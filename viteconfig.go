package viteless

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/paulmanoni/qjs"
)

// ViteConfig is the subset of a vite.config.{js,ts} that viteless maps onto
// its own configuration. viteless can't run Vite, but a Vite config is just
// a module returning an object — we evaluate it in QuickJS (with `vite` and
// the known plugins shimmed) and read the static fields back.
type ViteConfig struct {
	Base    string            // → PublicPath
	OutDir  string            // → OutDir
	Root    string            // → SrcDir hint
	Aliases []Alias           // resolve.alias → Aliases
	Proxy   string            // first server.proxy target → ProxyTarget
	Define  map[string]string // define / env → Env
	Plugins []Plugin          // recognized tier-1 native + tier-2 JS plugins
	Unknown []string          // unrecognized plugin names (reported, not run)

	closer func() // closes the Node plugin sidecar, if one is running
}

// Close releases the Node plugin sidecar backing any tier-2 JS plugins.
// Callers that ran JS plugins must keep the ViteConfig alive for the whole
// run (the dev session / the build) and Close it afterwards. Safe on nil.
func (vc *ViteConfig) Close() {
	if vc != nil && vc.closer != nil {
		vc.closer()
	}
}

// viteConfigNames is searched in order: viteless's own config name wins
// (it can import { defineConfig } from 'viteless' with nothing installed),
// then vite.config.* for drop-in compatibility with existing Vite projects.
var viteConfigNames = []string{
	"viteless.config.ts", "viteless.config.mts", "viteless.config.js",
	"viteless.config.mjs", "viteless.config.cjs",
	"vite.config.ts", "vite.config.mts", "vite.config.js",
	"vite.config.mjs", "vite.config.cjs",
}

func findViteConfig(root string) string {
	for _, n := range viteConfigNames {
		p := filepath.Join(root, n)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// LoadViteConfig finds, bundles, evaluates, and maps a vite.config in root.
// mode is "development" or "production". Returns (nil, nil) when there is no
// config file.
func LoadViteConfig(root, mode string, logf func(string, ...any)) (*ViteConfig, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	cfgPath := findViteConfig(root)
	if cfgPath == "" {
		return nil, nil
	}

	// Prefer real Node when it's available: a persistent sidecar evaluates
	// the config exactly as Vite would (real node: builtins, real plugins)
	// AND keeps the plugins alive so their JS hooks can run (tier-2). Fall
	// back to the embedded QuickJS shim (zero-Node) when Node is absent or
	// the Node path fails (e.g. a plugin package isn't installed).
	if nodeOnPath() {
		if host, err := startNodePluginHost(cfgPath, root, mode); err == nil {
			vc := mapViteConfig(host.config, root, logf)
			// Make non-native plugins with runnable hooks executable via the
			// sidecar, and drop them from the unsupported list.
			runnable := map[string]bool{}
			for i, pm := range host.plugins {
				if isNativeRuntimeName(pm.Name) || len(pm.Hooks) == 0 {
					continue
				}
				vc.Plugins = append(vc.Plugins, newJSPlugin(host, i, pm.Name, pm.Hooks))
				runnable[pm.Name] = true
				logf("viteless: JS plugin %q running via node (hooks: %s)", displayName(pm.Name), strings.Join(pm.Hooks, ","))
			}
			if len(runnable) > 0 {
				var still []string
				for _, u := range vc.Unknown {
					if !runnable[u] {
						still = append(still, u)
					}
				}
				vc.Unknown = still
				vc.closer = host.Close // keep the sidecar alive for the run
			} else {
				host.Close() // no JS plugins to run — don't keep Node around
			}
			reportUnknown(vc, logf)
			logf("viteless: loaded %s (via node)", filepath.Base(cfgPath))
			return vc, nil
		} else {
			logf("viteless: node config eval failed (%v); falling back to the zero-Node evaluator", err)
		}
	}

	bundle, err := bundleViteConfig(cfgPath, root)
	if err != nil {
		return nil, fmt.Errorf("viteless: bundle %s: %w", filepath.Base(cfgPath), err)
	}
	raw, err := evalViteConfig(bundle, cfgPath, root, mode)
	if err != nil {
		return nil, fmt.Errorf("viteless: evaluate %s: %w", filepath.Base(cfgPath), err)
	}
	vc := mapViteConfig(raw, root, logf)
	reportUnknown(vc, logf)
	logf("viteless: loaded %s", filepath.Base(cfgPath))
	return vc, nil
}

// displayName gives an anonymous plugin a readable label.
func displayName(name string) string {
	if name == "" {
		return "(anonymous)"
	}
	return name
}

// nodeOnPath reports whether a `node` binary is available.
func nodeOnPath() bool {
	_, err := exec.LookPath("node")
	return err == nil
}


// knownVitePlugins maps a Vite plugin package name to the viteless tier-1
// tag the shim stamps on it, so the config can be evaluated without running
// the plugin's real JS (its work is done natively by viteless).
var knownVitePlugins = map[string]string{
	"@vitejs/plugin-vue":       "vue",
	"@vitejs/plugin-vue-jsx":   "vue",
	"@vitejs/plugin-react":     "react",
	"@vitejs/plugin-react-swc": "react",
	"@tailwindcss/vite":        "tailwind",
}

// knownViteRuntimeNames recognizes plugins by the `name` they expose at
// runtime — used for the Node evaluation path, where the config returns real
// plugin objects rather than the shim's import-source tags.
var knownViteRuntimeNames = map[string]string{
	"vite:vue":           "vue",
	"vite:vue-jsx":       "vue",
	"vite:react-babel":   "react",
	"vite:react-refresh": "react",
	"vite:react-swc":     "react",
	"@tailwindcss/vite":  "tailwind",
	"@tailwindcss/postcss": "tailwind",
}

// bundleViteConfig esbuild-bundles the config file into a single IIFE that
// assigns the resolved config to globalThis.__viteless_config. The `vite`
// import and the known plugins are shimmed (so their real JS never runs),
// node builtins get minimal shims, and any other bare import becomes a
// tagged no-op so evaluation never crashes on an unknown plugin.
func bundleViteConfig(cfgPath, root string) ([]byte, error) {
	const (
		nsVite    = "vl-vite"
		nsNative  = "vl-native"
		nsNode    = "vl-node"
		nsUnknown = "vl-unknown"
		nsEntry   = "vl-entry"
	)
	adapter := `import __cfg from "__vite_user_config__";
globalThis.__viteless_config = (typeof __cfg === "function")
  ? __cfg({ command: globalThis.__viteless_command, mode: globalThis.__viteless_mode })
  : __cfg;
`
	shim := api.Plugin{
		Name: "viteless-config-shims",
		Setup: func(b api.PluginBuild) {
			b.OnResolve(api.OnResolveOptions{Filter: `^__vite_user_config__$`}, func(a api.OnResolveArgs) (api.OnResolveResult, error) {
				return api.OnResolveResult{Path: cfgPath, Namespace: "file"}, nil
			})
			b.OnResolve(api.OnResolveOptions{Filter: `.*`}, func(a api.OnResolveArgs) (api.OnResolveResult, error) {
				spec := a.Path
				// Relative/absolute imports from the config (local files) →
				// let esbuild resolve them normally.
				if strings.HasPrefix(spec, ".") || strings.HasPrefix(spec, "/") || filepath.IsAbs(spec) {
					return api.OnResolveResult{}, nil
				}
				switch {
				case spec == "vite" || spec == "viteless":
					return api.OnResolveResult{Path: spec, Namespace: nsVite}, nil
				case knownVitePlugins[spec] != "":
					return api.OnResolveResult{Path: spec, Namespace: nsNative}, nil
				case isNodeBuiltin(spec):
					return api.OnResolveResult{Path: spec, Namespace: nsNode}, nil
				default:
					return api.OnResolveResult{Path: spec, Namespace: nsUnknown}, nil
				}
			})
			b.OnLoad(api.OnLoadOptions{Filter: `.*`, Namespace: nsVite}, func(a api.OnLoadArgs) (api.OnLoadResult, error) {
				c := viteShimJS
				return api.OnLoadResult{Contents: &c, Loader: api.LoaderJS}, nil
			})
			b.OnLoad(api.OnLoadOptions{Filter: `.*`, Namespace: nsNative}, func(a api.OnLoadArgs) (api.OnLoadResult, error) {
				tag := knownVitePlugins[a.Path]
				c := fmt.Sprintf("export default function(){ return { name: %q, __viteless: %q }; }\n", a.Path, tag)
				return api.OnLoadResult{Contents: &c, Loader: api.LoaderJS}, nil
			})
			b.OnLoad(api.OnLoadOptions{Filter: `.*`, Namespace: nsNode}, func(a api.OnLoadArgs) (api.OnLoadResult, error) {
				c := nodeShimFor(strings.TrimPrefix(a.Path, "node:"))
				return api.OnLoadResult{Contents: &c, Loader: api.LoaderJS}, nil
			})
			b.OnLoad(api.OnLoadOptions{Filter: `.*`, Namespace: nsUnknown}, func(a api.OnLoadArgs) (api.OnLoadResult, error) {
				// Unknown bare import — tag it so an unrecognized plugin in
				// plugins[] is reported (tier-3) instead of crashing the
				// config. Provides a callable default + a Proxy for named
				// imports so `import { x } from 'pkg'` doesn't fail to bundle.
				c := fmt.Sprintf("const __u = function(){ return { name: %q, __viteless: \"unknown\" }; };\nexport default __u;\nexport const __viteless_unknown = %q;\n", a.Path, a.Path)
				return api.OnLoadResult{Contents: &c, Loader: api.LoaderJS}, nil
			})
		},
	}

	r := api.Build(api.BuildOptions{
		Stdin: &api.StdinOptions{
			Contents:   adapter,
			ResolveDir: root,
			Sourcefile: "viteless-config-adapter.js",
			Loader:     api.LoaderJS,
		},
		Bundle:   true,
		Write:    false,
		Format:   api.FormatIIFE,
		Target:   api.ES2022,
		Platform: api.PlatformNeutral,
		Plugins:  []api.Plugin{shim},
		LogLevel: api.LogLevelSilent,
		Define: map[string]string{
			"__dirname":            jsLit(filepath.Dir(cfgPath)),
			"__filename":           jsLit(cfgPath),
			"import.meta.url":      jsLit("file://" + cfgPath),
			"process.env.NODE_ENV": `"development"`,
		},
	})
	if len(r.Errors) > 0 {
		return nil, fmt.Errorf("%s", r.Errors[0].Text)
	}
	if len(r.OutputFiles) == 0 {
		return nil, fmt.Errorf("no output")
	}
	return r.OutputFiles[0].Contents, nil
}

// evalViteConfig runs the bundled config in QuickJS and returns the config
// object as a decoded map.
func evalViteConfig(bundle []byte, cfgPath, root, mode string) (map[string]any, error) {
	rt, err := qjs.New()
	if err != nil {
		return nil, err
	}
	defer rt.Close()
	ctx := rt.Context()

	command := "serve"
	if mode == "production" {
		command = "build"
	}
	prelude := fmt.Sprintf(`
		globalThis.__viteless_mode = %q;
		globalThis.__viteless_command = %q;
		if (typeof globalThis.process === 'undefined') globalThis.process = {};
		globalThis.process.env = globalThis.process.env || { NODE_ENV: 'development' };
		globalThis.process.cwd = function(){ return %q; };
		globalThis.process.platform = 'linux';
		if (typeof globalThis.global === 'undefined') globalThis.global = globalThis;
	`, mode, command, root)
	if _, err := ctx.Eval("viteless-config-prelude.js", qjs.Code(prelude)); err != nil {
		return nil, fmt.Errorf("prelude: %w", err)
	}
	if _, err := ctx.Eval("vite.config.bundle.js", qjs.Code(string(bundle))); err != nil {
		return nil, err
	}
	v, err := ctx.Eval("viteless-config-read.js", qjs.Code(`JSON.stringify(globalThis.__viteless_config || {})`))
	if err != nil {
		return nil, err
	}
	defer v.Free()
	var out map[string]any
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	return out, nil
}

// mapViteConfig translates the decoded config object into a ViteConfig.
func mapViteConfig(raw map[string]any, root string, logf func(string, ...any)) *ViteConfig {
	vc := &ViteConfig{Define: map[string]string{}}

	if s, ok := raw["base"].(string); ok {
		vc.Base = s
	}
	if s, ok := raw["root"].(string); ok {
		vc.Root = s
	}
	if build, ok := raw["build"].(map[string]any); ok {
		if s, ok := build["outDir"].(string); ok {
			vc.OutDir = s
		}
	}
	if resolve, ok := raw["resolve"].(map[string]any); ok {
		vc.Aliases = parseAliases(resolve["alias"], root)
	}
	if server, ok := raw["server"].(map[string]any); ok {
		vc.Proxy = firstProxyTarget(server["proxy"])
	}
	if def, ok := raw["define"].(map[string]any); ok {
		for k, val := range def {
			if b, err := json.Marshal(val); err == nil {
				vc.Define[k] = string(b)
			}
		}
	}
	// Plugins: recognize tier-1 natives; report the rest. The QuickJS path
	// stamps a __viteless tag (recognized by import source); the Node path
	// returns real plugin objects, recognized by their runtime `name`.
	for _, p := range pluginList(raw["plugins"]) {
		obj, ok := p.(map[string]any)
		if !ok {
			continue
		}
		tag, _ := obj["__viteless"].(string)
		name, _ := obj["name"].(string)
		if tag == "" {
			tag = knownViteRuntimeNames[name]
		}
		switch tag {
		case "tailwind":
			vc.Plugins = append(vc.Plugins, TailwindPlugin())
		case "vue", "react":
			// Handled natively by framework detection; nothing to add.
		case "unknown", "":
			if name != "" {
				vc.Unknown = append(vc.Unknown, name)
			}
		}
	}
	return vc
}

// reportUnknown logs the plugins viteless can neither run natively nor via
// the Node sidecar.
func reportUnknown(vc *ViteConfig, logf func(string, ...any)) {
	for _, u := range vc.Unknown {
		logf("viteless: plugin %q is not supported (needs Node to run its JS) — skipped", u)
	}
}

// parseAliases turns a Vite resolve.alias (object or array form) into viteless
// Alias entries. Wildcards aren't used by Vite aliases (they're prefix maps),
// so each becomes a prefix→dir mapping.
func parseAliases(v any, root string) []Alias {
	var out []Alias
	add := func(find, replace string) {
		if find == "" || replace == "" {
			return
		}
		dir := replace
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(root, dir)
		}
		// Vite aliases are typically exact-prefix ("@" → src). Treat "@/..."
		// style by giving the prefix a trailing slash when the alias is a
		// bare token.
		prefix := find
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		out = append(out, Alias{Prefix: prefix, Dir: dir})
	}
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if s, ok := t[k].(string); ok {
				add(k, s)
			}
		}
	case []any:
		for _, e := range t {
			if m, ok := e.(map[string]any); ok {
				find, _ := m["find"].(string)
				replace, _ := m["replacement"].(string)
				add(find, replace)
			}
		}
	}
	return out
}

func firstProxyTarget(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch t := m[k].(type) {
		case string:
			return t
		case map[string]any:
			if s, ok := t["target"].(string); ok {
				return s
			}
		}
	}
	return ""
}

func pluginList(v any) []any {
	switch t := v.(type) {
	case []any:
		return flattenPlugins(t)
	default:
		return nil
	}
}

// flattenPlugins flattens nested plugin arrays (Vite allows [[a,b],c]).
func flattenPlugins(in []any) []any {
	var out []any
	for _, e := range in {
		if sub, ok := e.([]any); ok {
			out = append(out, flattenPlugins(sub)...)
		} else if e != nil {
			out = append(out, e)
		}
	}
	return out
}

// applyDev merges a vite.config into a DevConfig, filling only fields the
// caller left unset (explicit DevConfig wins).
func (vc *ViteConfig) applyDev(cfg *DevConfig) {
	if cfg.Aliases == nil && len(vc.Aliases) > 0 {
		cfg.Aliases = vc.Aliases
	}
	if cfg.ProxyTarget == "" && cfg.ProxyResolver == nil && vc.Proxy != "" {
		cfg.ProxyTarget = vc.Proxy
	}
	if len(vc.Plugins) > 0 {
		cfg.Plugins = append(append([]Plugin{}, vc.Plugins...), cfg.Plugins...)
	}
}

// applyBuild merges a vite.config into a BuildConfig, filling only unset
// fields.
func (vc *ViteConfig) applyBuild(cfg *BuildConfig) {
	if cfg.PublicPath == "" && vc.Base != "" {
		cfg.PublicPath = vc.Base
	}
	if cfg.OutDir == "" && vc.OutDir != "" {
		out := vc.OutDir
		if !filepath.IsAbs(out) {
			out = filepath.Join(cfg.Root, out)
		}
		cfg.OutDir = out
	}
	if cfg.Aliases == nil && len(vc.Aliases) > 0 {
		cfg.Aliases = vc.Aliases
	}
	if len(vc.Plugins) > 0 {
		cfg.Plugins = append(append([]Plugin{}, vc.Plugins...), cfg.Plugins...)
	}
}

// esbuildAliases converts viteless prefix aliases into esbuild's Alias map.
// esbuild matches an alias key both exactly and as a path prefix, so
// "@" → "/abs/src" resolves "@/components/X.vue" → "/abs/src/components/X.vue".
func esbuildAliases(aliases []Alias) map[string]string {
	if len(aliases) == 0 {
		return nil
	}
	m := make(map[string]string, len(aliases))
	for _, a := range aliases {
		key := strings.TrimSuffix(a.Prefix, "/")
		if key == "" {
			continue
		}
		m[key] = a.Dir
	}
	return m
}

func isNodeBuiltin(spec string) bool {
	s := strings.TrimPrefix(spec, "node:")
	switch s {
	case "path", "url", "fs", "os", "process", "util", "module":
		return true
	}
	return false
}
