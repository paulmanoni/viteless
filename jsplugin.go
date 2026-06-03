package viteless

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/evanw/esbuild/pkg/api"
)

// This file is the tier-2 JS-plugin runtime. Real Vite/Rollup plugins are
// JavaScript, so viteless runs them in a persistent Node sidecar (full
// fidelity — real Node APIs, the actual plugin packages) and bridges their
// universal hooks (resolveId / load / transform / transformIndexHtml) to the
// Go plugin container. When Node isn't available these plugins are reported
// as unsupported; the native tier-1 plugins (vue/react/tailwind) still work
// with zero Node.

// pluginMeta describes one config plugin discovered by the sidecar.
type pluginMeta struct {
	Name  string   `json:"name"`
	Hooks []string `json:"hooks"`
}

// readyMsg is the sidecar's first line: the resolved config + plugin list.
type readyMsg struct {
	Type    string         `json:"type"`
	Config  map[string]any `json:"config"`
	Plugins []pluginMeta   `json:"plugins"`
}

// nodePluginHost is a long-lived Node process holding a vite.config's plugin
// objects, answering hook calls over newline-delimited JSON on stdio.
type nodePluginHost struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	tmp     string
	config  map[string]any
	plugins []pluginMeta

	mu      sync.Mutex
	nextID  int
	pending map[int]chan rpcResp
	closed  bool
}

type rpcResp struct {
	Value any
	Err   string
}

//go:embed jsplugin_sidecar.mjs
var sidecarScript string

// startNodePluginHost bundles the config to ESM (real packages kept external
// so the sidecar imports the actual plugins) and starts the sidecar, reading
// the resolved config + plugin metadata from its first line.
func startNodePluginHost(cfgPath, root, mode string) (*nodePluginHost, error) {
	command := "serve"
	if mode == "production" {
		command = "build"
	}
	tmp := filepath.Join(filepath.Dir(cfgPath), ".viteless.config."+shortHashLong(cfgPath)+".mjs")
	r := api.Build(api.BuildOptions{
		EntryPoints:   []string{cfgPath},
		Bundle:        true,
		Write:         true,
		Outfile:       tmp,
		Platform:      api.PlatformNode,
		Format:        api.FormatESModule,
		Target:        api.ES2022,
		Packages:      api.PackagesExternal,
		AbsWorkingDir: root,
		LogLevel:      api.LogLevelSilent,
	})
	if len(r.Errors) > 0 {
		return nil, fmt.Errorf("bundle: %s", r.Errors[0].Text)
	}

	cmd := exec.Command("node", "--input-type=module", "-e", sidecarScript, tmp)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "VL_CMD="+command, "VL_MODE="+mode)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		os.Remove(tmp)
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		os.Remove(tmp)
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		os.Remove(tmp)
		return nil, err
	}

	h := &nodePluginHost{cmd: cmd, stdin: stdin, tmp: tmp, pending: map[int]chan rpcResp{}}
	reader := bufio.NewReader(stdout)

	// First line is the ready message (config + plugins).
	line, err := reader.ReadString('\n')
	if err != nil {
		h.Close()
		return nil, fmt.Errorf("sidecar handshake: %w", err)
	}
	var ready readyMsg
	if err := json.Unmarshal([]byte(line), &ready); err != nil || ready.Type != "ready" {
		h.Close()
		return nil, fmt.Errorf("sidecar handshake: bad ready message")
	}
	h.config = ready.Config
	h.plugins = ready.Plugins

	go h.dispatch(reader)
	return h, nil
}

// dispatch reads response lines and routes them to waiting callers.
func (h *nodePluginHost) dispatch(reader *bufio.Reader) {
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		var msg struct {
			ID    int    `json:"id"`
			Value any    `json:"value"`
			Err   string `json:"error"`
		}
		if json.Unmarshal([]byte(line), &msg) != nil {
			continue
		}
		h.mu.Lock()
		ch := h.pending[msg.ID]
		delete(h.pending, msg.ID)
		h.mu.Unlock()
		if ch != nil {
			ch <- rpcResp{Value: msg.Value, Err: msg.Err}
		}
	}
	// Stream closed: fail any in-flight calls.
	h.mu.Lock()
	for id, ch := range h.pending {
		ch <- rpcResp{Err: "sidecar exited"}
		delete(h.pending, id)
	}
	h.mu.Unlock()
}

// call invokes plugin[idx].hook(args...) in the sidecar.
func (h *nodePluginHost) call(idx int, hook string, args ...any) (any, error) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, fmt.Errorf("plugin host closed")
	}
	h.nextID++
	id := h.nextID
	ch := make(chan rpcResp, 1)
	h.pending[id] = ch
	h.mu.Unlock()

	req, _ := json.Marshal(map[string]any{"id": id, "plugin": idx, "hook": hook, "args": args})
	if _, err := h.stdin.Write(append(req, '\n')); err != nil {
		return nil, err
	}
	resp := <-ch
	if resp.Err != "" {
		return nil, fmt.Errorf("%s", resp.Err)
	}
	return resp.Value, nil
}

// Close stops the sidecar and removes the temp bundle. Idempotent.
func (h *nodePluginHost) Close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	h.mu.Unlock()
	if h.stdin != nil {
		_ = h.stdin.Close()
	}
	if h.cmd != nil && h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
		_ = h.cmd.Wait()
	}
	if h.tmp != "" {
		_ = os.Remove(h.tmp)
	}
}

// jsPlugin bridges one sidecar-hosted plugin to the Go plugin container. It
// implements every capability interface but short-circuits (no IPC) for
// hooks the plugin doesn't actually have.
type jsPlugin struct {
	host  *nodePluginHost
	index int
	name  string
	hooks map[string]bool
}

func newJSPlugin(host *nodePluginHost, index int, name string, hooks []string) *jsPlugin {
	set := make(map[string]bool, len(hooks))
	for _, h := range hooks {
		set[h] = true
	}
	return &jsPlugin{host: host, index: index, name: name, hooks: set}
}

func (p *jsPlugin) Name() string {
	if p.name != "" {
		return p.name
	}
	return "js-plugin"
}

func (p *jsPlugin) ResolveId(spec, importer string) (string, bool) {
	if !p.hooks["resolveId"] {
		return "", false
	}
	v, err := p.host.call(p.index, "resolveId", spec, importer)
	if err != nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok && s != ""
}

func (p *jsPlugin) Load(id string) (string, bool, error) {
	if !p.hooks["load"] {
		return "", false, nil
	}
	v, err := p.host.call(p.index, "load", id)
	if err != nil {
		return "", false, err
	}
	s, ok := v.(string)
	return s, ok && s != "", nil
}

func (p *jsPlugin) Transform(code, id string) (string, bool, error) {
	if !p.hooks["transform"] {
		return code, false, nil
	}
	v, err := p.host.call(p.index, "transform", code, id)
	if err != nil {
		return "", false, err
	}
	if s, ok := v.(string); ok {
		return s, true, nil
	}
	return code, false, nil
}

func (p *jsPlugin) TransformIndexHtml(html string) (string, error) {
	if !p.hooks["transformIndexHtml"] {
		return html, nil
	}
	v, err := p.host.call(p.index, "transformIndexHtml", html)
	if err != nil {
		return "", err
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	return html, nil
}

// isNativeRuntimeName reports whether a plugin name is handled by a viteless
// tier-1 native plugin (so it should not be run as JS).
func isNativeRuntimeName(name string) bool {
	return knownViteRuntimeNames[name] != "" || knownVitePlugins[name] != ""
}
