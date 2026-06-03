package viteless

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/fsnotify/fsnotify"

	"github.com/paulmanoni/viteless/internal/lockfile"
	"github.com/paulmanoni/viteless/internal/resolver"
	"github.com/paulmanoni/viteless/internal/sfc/vue"
)

// DevServer is a running unbundled dev server: it serves source modules
// transformed on the fly, pushes HMR updates as files change, and proxies
// unmatched requests (API calls) to the backend.
type DevServer struct {
	url       string
	httpSrv   *http.Server
	ln        net.Listener
	closeVue  func()
	closeConf func() // closes the vite.config JS-plugin sidecar, if any
	cancel    context.CancelFunc
	proc      *exec.Cmd // the real-vite child process, when delegating
	closed    sync.Once
	serveErr  chan error
}

// URL is the origin the SPA is served from (e.g. http://localhost:5173/).
func (d *DevServer) URL() string { return d.url }

// Wait blocks until ctx is cancelled or the server stops, then shuts the
// server down. Returns the serve error, if any (nil on clean shutdown).
func (d *DevServer) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		d.Close()
		return nil
	case err := <-d.serveErr:
		d.Close()
		return err
	}
}

// Close stops the server and disposes the Vue compiler. Idempotent.
func (d *DevServer) Close() error {
	d.closed.Do(func() {
		if d.cancel != nil {
			d.cancel()
		}
		if d.httpSrv != nil {
			_ = d.httpSrv.Close()
		}
		if d.closeVue != nil {
			d.closeVue()
		}
		if d.closeConf != nil {
			d.closeConf()
		}
		if d.proc != nil && d.proc.Process != nil {
			_ = d.proc.Process.Kill()
		}
	})
	return nil
}

// Dev starts an unbundled native-ESM dev server for cfg. It wires the
// dependency resolver, the optional Vue SFC compiler, dep pre-bundling, a
// filesystem watcher driving HMR, and a reverse proxy to the backend, then
// returns once it is listening. Call DevServer.Wait to serve until a
// context is cancelled, or DevServer.Close to stop it.
func Dev(cfg DevConfig) (*DevServer, error) {
	if cfg.Root == "" {
		return nil, fmt.Errorf("viteless: Dev requires Root")
	}
	// esbuild (the node_modules optimizer) requires an absolute working
	// dir; absolutize Root so relative CLI args (./examples/foo) work.
	if abs, err := filepath.Abs(cfg.Root); err == nil {
		cfg.Root = abs
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	// Highest fidelity: if the real Vite is installed, just use it.
	if useRealVite(cfg.Root) {
		return devWithRealVite(cfg)
	}

	// A vite.config in the project fills any config the caller left unset
	// (aliases, proxy, recognized plugins). viteless can't run Vite, but it
	// reads the config's static surface — see LoadViteConfig.
	devMode := cfg.Mode
	if devMode == "" {
		devMode = "development"
	}
	var viteCfg *ViteConfig
	if vc, verr := LoadViteConfig(cfg.Root, devMode, logf); verr != nil {
		logf("viteless: vite.config ignored: %v", verr)
	} else if vc != nil {
		vc.applyDev(&cfg)
		viteCfg = vc // kept alive for the session; closed in DevServer.Close
	}

	servedRoot := cfg.Root

	fw := cfg.Framework
	if fw == FrameworkAuto {
		fw = detectFramework(servedRoot)
	}
	hasVue := fw == FrameworkVue
	depMode := resolveDepMode(cfg.DepMode, cfg.Root)
	useNodeModules := depMode == DepNodeModules

	// Read the lockfile for pins but pass an empty lockPath to the
	// on-demand fetcher: dev pulls dev-only URLs (vue.development.mjs) that
	// must never rewrite the committed lockfile.
	lockPath := cfg.LockfilePath
	if lockPath == "" {
		lockPath = filepath.Join(cfg.Root, lockfile.Filename)
	}
	lf, err := lockfile.LoadOrNew(lockPath)
	if err != nil {
		return nil, fmt.Errorf("viteless: load lockfile: %w", err)
	}
	st, err := openStore(cfg.CacheRoot)
	if err != nil {
		return nil, err
	}
	onDemand := newOnDemandFetch(lf, st, "", cfg.Registry, readProjectDeps(cfg.Root), logf)

	resOpts := resolver.Options{Lockfile: lf, Store: st, FetchOnDemand: onDemand}
	// The dev-only `vue` → vue.development.mjs rewrite is a CDN-resolver
	// concern; in node_modules mode the optimizer bundles vue's dev build
	// directly (NODE_ENV=development), so the HMR runtime is already there.
	if hasVue && !useNodeModules {
		resOpts.DevSpecRewrite = devVueRewrite(lf, onDemand, logf)
	}

	var compiler vue.SFCCompiler
	if hasVue {
		c, verr := newVueCompiler(st, cfg.Registry)
		if verr != nil {
			return nil, verr
		}
		compiler = c
	}

	aliases := cfg.Aliases
	if aliases == nil {
		aliases = esmAliases(servedRoot)
	}
	mode := cfg.Mode
	if mode == "" {
		mode = "development"
	}
	prebundle := cfg.Prebundle
	if !prebundle {
		// default on unless explicitly handled by caller; mirror Vite.
		prebundle = true
	}

	hostCfg := HostConfig{
		Root:      servedRoot,
		IndexHTML: cfg.IndexHTML,
		Resolver:  resOpts,
		Compiler:  compiler,
		Aliases:   aliases,
		Env:       cfg.Env,
		Mode:      mode,
		Prebundle: prebundle,
	}
	if useNodeModules {
		nmDir := cfg.NodeModules
		if nmDir == "" {
			nmDir = filepath.Join(cfg.Root, "node_modules")
		}
		hostCfg.NodeModules = nmDir
		hostCfg.CacheRoot = cfg.CacheRoot
		hostCfg.Logf = logf
		hostCfg.Prebundle = false // the optimizer replaces the CDN pre-bundler
		logf("viteless: resolving dependencies from %s", nmDir)
	}
	if fw == FrameworkReact {
		hostCfg.JSX = api.JSXAutomatic
	}
	host := NewDefaultHost(hostCfg)

	// Built-in tier-1 plugins run in dev so features that were build-only
	// (Tailwind) now work here too. Tailwind no-ops unless CSS uses its
	// directives, so it's free for non-Tailwind projects.
	devPlugins := make([]Plugin, 0, len(cfg.Plugins)+1)
	if cfg.Tailwind != TailwindOff {
		devPlugins = append(devPlugins, TailwindPlugin())
	}
	devPlugins = append(devPlugins, cfg.Plugins...)

	container := newPluginContainer(devPlugins)
	if container != nil {
		container.applyConfig(&ResolvedConfig{Root: cfg.Root, Mode: mode, Env: cfg.Env})
	}

	opts := []Option{}
	switch {
	case cfg.ProxyResolver != nil:
		opts = append(opts, WithProxyResolver(cfg.ProxyResolver))
	case cfg.ProxyTarget != "":
		opts = append(opts, WithProxy(cfg.ProxyTarget))
	}
	if container != nil {
		opts = append(opts, withPlugins(container))
	}
	srv := NewServer(host, opts...)

	ln, err := devListen(cfg.Addr)
	if err != nil {
		if compiler != nil {
			compiler.Close()
		}
		return nil, fmt.Errorf("viteless: bind: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	ctx, cancel := context.WithCancel(context.Background())
	d := &DevServer{
		url:      fmt.Sprintf("http://localhost:%d/", port),
		httpSrv:  &http.Server{Handler: srv.Handler()},
		ln:       ln,
		cancel:   cancel,
		serveErr: make(chan error, 1),
	}
	if viteCfg != nil {
		d.closeConf = viteCfg.Close
	}
	if compiler != nil {
		d.closeVue = compiler.Close
	}

	go func() {
		err := d.httpSrv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		d.serveErr <- err
	}()
	go devWatch(ctx, servedRoot, srv.HMR(), container, logf)

	logf("viteless: dev server on %s (proxy → backend)", d.url)
	return d, nil
}

// devListen binds the dev server, defaulting to the stable Vite port and
// falling back to an OS-assigned port if it's busy.
func devListen(addr string) (net.Listener, error) {
	if addr == "" {
		addr = "127.0.0.1:5173"
	} else if !strings.Contains(addr, ":") {
		addr = "127.0.0.1:" + addr
	} else if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	if ln, err := net.Listen("tcp", addr); err == nil {
		return ln, nil
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

// devWatch fans filesystem changes under servedRoot into HMR updates.
func devWatch(ctx context.Context, servedRoot string, hub *HMR, plugins *pluginContainer, logf func(string, ...any)) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return
	}
	defer w.Close()
	if err := watchRecursive(w, servedRoot); err != nil {
		logf("viteless: hmr watch setup: %v", err)
		return
	}
	debounce := map[string]time.Time{}
	const window = 80 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-w.Events:
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			now := time.Now()
			if last, ok := debounce[ev.Name]; ok && now.Sub(last) < window {
				continue
			}
			debounce[ev.Name] = now
			rel, rerr := filepath.Rel(servedRoot, ev.Name)
			if rerr != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			base := filepath.Base(ev.Name)
			urlPath := "/" + filepath.ToSlash(rel)
			defaultType := ""
			switch {
			case strings.HasSuffix(base, ".css"):
				defaultType = "css"
			case strings.HasSuffix(base, ".vue"), strings.HasSuffix(base, ".ts"),
				strings.HasSuffix(base, ".tsx"), strings.HasSuffix(base, ".jsx"),
				strings.HasSuffix(base, ".js"), strings.HasSuffix(base, ".mjs"):
				defaultType = "update"
			case base == "index.html":
				hub.Reload()
				continue
			}
			if defaultType == "" {
				continue
			}
			// A plugin handleHotUpdate hook may override the update set.
			if ups, ok := plugins.handleHotUpdate(&HotUpdate{File: ev.Name, Path: urlPath, Type: defaultType}); ok {
				for _, u := range ups {
					hub.Broadcast(u)
				}
				continue
			}
			hub.Broadcast(Update{Type: defaultType, Path: urlPath})
		case <-w.Errors:
		}
	}
}

// watchRecursive adds every source directory under dir to the watcher,
// skipping build/vcs/dependency dirs.
func watchRecursive(w *fsnotify.Watcher, dir string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() {
			return nil
		}
		switch info.Name() {
		case "node_modules", ".git", "dist", "islands":
			return filepath.SkipDir
		}
		_ = w.Add(path)
		return nil
	})
}

// devVueRewrite returns a resolver DevSpecRewrite that swaps bare `vue`
// for its esm.sh .development.mjs build so __VUE_HMR_RUNTIME__ is present
// (it's absent from the production build). Returns nil — leaving the
// pinned production vue in place — when the dev build can't be pre-warmed.
func devVueRewrite(lf *lockfile.File, onDemand func(string) (string, error), logf func(string, ...any)) func(spec, subpath string) string {
	pkg, err := lf.Resolve("vue", "")
	if err != nil {
		var ae *lockfile.AmbiguousError
		if errors.As(err, &ae) && len(ae.Versions) > 0 {
			pkg, err = lf.Resolve("vue", ae.Versions[len(ae.Versions)-1])
		}
		if err != nil {
			return nil
		}
	}
	if pkg.Version == "" {
		return nil
	}
	devURL := "https://esm.sh/vue@" + pkg.Version + "/es2022/vue.development.mjs"
	resolved := devURL
	if onDemand != nil {
		canonical, ferr := onDemand(devURL)
		if ferr != nil {
			logf("viteless: vue dev build prefetch failed (%v); Vue HMR disabled, using production vue", ferr)
			return nil
		}
		if canonical != "" {
			resolved = canonical
		}
	}
	return func(spec, subpath string) string {
		if spec == "vue" && subpath == "" {
			return resolved
		}
		return ""
	}
}

// esmAliases derives import aliases from the project's tsconfig/jsconfig
// "paths", falling back to the "@/" → src convention.
func esmAliases(servedRoot string) []Alias {
	def := func() []Alias {
		srcSub := filepath.Join(servedRoot, "src")
		if info, err := os.Stat(srcSub); err == nil && info.IsDir() {
			return []Alias{{Prefix: "@/", Dir: srcSub}}
		}
		return []Alias{{Prefix: "@/", Dir: servedRoot}}
	}
	paths, baseURL, ok := readTSConfigPaths(filepath.Join(servedRoot, "tsconfig.json"))
	if !ok {
		paths, baseURL, ok = readTSConfigPaths(filepath.Join(servedRoot, "jsconfig.json"))
	}
	if !ok || len(paths) == 0 {
		return def()
	}
	base := filepath.Join(servedRoot, filepath.FromSlash(baseURL))
	var aliases []Alias
	for key, targets := range paths {
		if len(targets) == 0 {
			continue
		}
		target := targets[0]
		if strings.HasSuffix(key, "/*") && strings.HasSuffix(target, "/*") {
			prefix := strings.TrimSuffix(key, "*")
			dir := filepath.Join(base, filepath.FromSlash(strings.TrimSuffix(target, "*")))
			aliases = append(aliases, Alias{Prefix: prefix, Dir: dir})
		} else if !strings.Contains(key, "*") {
			aliases = append(aliases, Alias{Prefix: key, Dir: filepath.Join(base, filepath.FromSlash(target)), Exact: true})
		}
	}
	if len(aliases) == 0 {
		return def()
	}
	hasAt := false
	for _, a := range aliases {
		if a.Prefix == "@/" {
			hasAt = true
		}
	}
	if !hasAt {
		aliases = append(aliases, def()...)
	}
	return aliases
}

// readTSConfigPaths extracts compilerOptions.paths + baseUrl, tolerating
// JSONC (comments + trailing commas).
func readTSConfigPaths(file string) (paths map[string][]string, baseURL string, ok bool) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, "", false
	}
	var cfg struct {
		CompilerOptions struct {
			BaseURL string              `json:"baseUrl"`
			Paths   map[string][]string `json:"paths"`
		} `json:"compilerOptions"`
	}
	if err := json.Unmarshal(stripJSONC(raw), &cfg); err != nil {
		return nil, "", false
	}
	base := cfg.CompilerOptions.BaseURL
	if base == "" {
		base = "."
	}
	return cfg.CompilerOptions.Paths, base, true
}

var trailingCommaRE = regexp.MustCompile(`,(\s*[}\]])`)

// stripJSONC removes // and /* */ comments and trailing commas, preserving
// string literals, so a JSONC tsconfig parses as plain JSON.
func stripJSONC(b []byte) []byte {
	var out []byte
	inStr, esc, line, block := false, false, false, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case line:
			if c == '\n' {
				line = false
				out = append(out, c)
			}
		case block:
			if c == '*' && i+1 < len(b) && b[i+1] == '/' {
				block = false
				i++
			}
		case inStr:
			out = append(out, c)
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
		case c == '"':
			inStr = true
			out = append(out, c)
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			line = true
			i++
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			block = true
			i++
		default:
			out = append(out, c)
		}
	}
	return trailingCommaRE.ReplaceAll(out, []byte("$1"))
}
