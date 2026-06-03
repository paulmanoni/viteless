package viteless

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/evanw/esbuild/pkg/api"

	"github.com/paulmanoni/viteless/internal/bundler"
	"github.com/paulmanoni/viteless/internal/fetcher"
	"github.com/paulmanoni/viteless/internal/lockfile"
	"github.com/paulmanoni/viteless/internal/resolver"
	"github.com/paulmanoni/viteless/internal/store"
)

// Build produces a production bundle for cfg into a dist directory,
// embedding-ready (one binary's //go:embed can pick it up). It wires
// esbuild with the dependency resolver, the optional Vue SFC compiler,
// Tailwind, and the Vite-compat plugins (import.meta.glob, ?url/?raw,
// workers), then emits an index.html that references the built entry.
//
// A nil Go error with a non-empty BuildResult.Errors means the
// configuration was valid but the bundle failed — inspect Errors.
func Build(cfg BuildConfig) (BuildResult, error) {
	if cfg.Root == "" {
		return BuildResult{}, fmt.Errorf("viteless: Build requires Root")
	}
	// esbuild requires an absolute working dir; absolutize Root up front so
	// relative CLI args (./examples/foo) work in node_modules mode.
	if abs, err := filepath.Abs(cfg.Root); err == nil {
		cfg.Root = abs
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	// Highest fidelity: if the real Vite is installed, delegate the build.
	if useRealVite(cfg.Root) {
		return buildWithRealVite(cfg)
	}

	// A vite.config fills any unset build config (base, outDir, recognized
	// plugins) before entry detection + output paths are derived.
	buildMode := cfg.Mode
	if buildMode == "" {
		buildMode = "production"
	}
	if vc, verr := LoadViteConfig(cfg.Root, buildMode, logf); verr != nil {
		logf("viteless: vite.config ignored: %v", verr)
	} else if vc != nil {
		vc.applyBuild(&cfg)
		defer vc.Close() // stop the JS-plugin sidecar (if any) after the build
	}

	// Resolve the entry set + their directory.
	srcDir := cfg.SrcDir
	if srcDir == "" {
		srcDir = cfg.Root
	}
	entries := cfg.Entries
	if len(entries) == 0 {
		d, found, err := findFrontendEntries(srcDir)
		if err != nil {
			return BuildResult{}, err
		}
		if len(found) == 0 {
			return BuildResult{}, fmt.Errorf("viteless: no entry files (.ts/.tsx/.jsx/.js) found under %s", srcDir)
		}
		srcDir, entries = d, found
	}

	outDir := cfg.OutDir
	if outDir == "" {
		outDir = filepath.Join(cfg.Root, "dist")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return BuildResult{}, fmt.Errorf("viteless: mkdir %s: %w", outDir, err)
	}

	fw := cfg.Framework
	if fw == FrameworkAuto {
		fw = detectFramework(srcDir)
	}

	b := bundler.New()

	depMode := resolveDepMode(cfg.DepMode, cfg.Root)

	// The store is always opened: it backs the @vue/compiler-sfc bootstrap
	// even in node_modules mode (a build-time tool, fetched + cached once).
	st, err := openStore(cfg.CacheRoot)
	if err != nil {
		return BuildResult{}, err
	}

	// Dependency resolution. CDN mode adds the esm.sh resolver plugin;
	// NodeModules mode omits it and lets esbuild's native resolution walk
	// node_modules (AbsWorkingDir is set on the build options below).
	var lf *lockfile.File
	if depMode == DepCDN {
		lockPath := cfg.LockfilePath
		if lockPath == "" {
			lockPath = filepath.Join(cfg.Root, lockfile.Filename)
		}
		lf, err = lockfile.LoadOrNew(lockPath)
		if err != nil {
			return BuildResult{}, fmt.Errorf("viteless: load lockfile: %w", err)
		}
		rp, rerr := resolver.New(resolver.Options{
			Lockfile:      lf,
			Store:         st,
			FetchOnDemand: newOnDemandFetch(lf, st, lockPath, cfg.Registry, readProjectDeps(cfg.Root), logf),
		})
		if rerr != nil {
			return BuildResult{}, fmt.Errorf("viteless: build resolver: %w", rerr)
		}
		b.AddPlugin(rp)
	}

	// Vue SFC plugin: compiles .vue via @vue/compiler-sfc in QuickJS.
	if fw == FrameworkVue {
		p, closeVue, err := vueBuildPlugin(lf, st, cfg.Registry)
		if err != nil {
			return BuildResult{}, err
		}
		if p != nil {
			b.AddPlugin(*p)
			defer closeVue()
		}
	}

	// Tailwind (no-op unless directives are present and enabled).
	if cfg.Tailwind != TailwindOff {
		b.AddPlugin(bundler.NewTailwindPlugin())
	}
	// Vite-compat plugins.
	b.AddPlugin(bundler.NewImportMetaGlobPlugin())
	b.AddPlugin(bundler.NewQuerySuffixPlugin())
	// User/config plugins via the esbuild bridge — after the built-ins so
	// they keep first crack, before the worker so nested builds inherit it.
	container := newPluginContainer(cfg.Plugins)
	if container != nil {
		container.applyConfig(&ResolvedConfig{Root: cfg.Root, Mode: cfg.Mode, Env: cfg.Env})
		b.AddPlugin(pluginBridge(container))
	}
	// Worker plugin LAST so its sub-build inherits every other plugin.
	b.AddPlugin(bundler.NewWorkerPlugin(bundler.WorkerPluginOptions{
		OutDir:        outDir,
		PublicPath:    cfg.PublicPath,
		NestedPlugins: append([]api.Plugin(nil), b.Plugins...),
	}))

	mode := cfg.Mode
	if mode == "" {
		mode = "production"
	}

	opts := bundler.Options{
		Entries:    entries,
		OutDir:     outDir,
		Minify:     derefOr(cfg.Minify, true),
		Splitting:  derefOr(cfg.Splitting, true),
		Lockfile:   lf,
		Store:      st,
		TSConfig:   cfg.TSConfig,
		Env:        cfg.Env,
		Mode:       mode,
		PublicPath: cfg.PublicPath,
		Alias:      esbuildAliases(cfg.Aliases),
	}
	if !derefOr(cfg.Sourcemap, true) {
		opts.Sourcemap = api.SourceMapNone
	}
	if depMode == DepNodeModules {
		opts.AbsWorkingDir = cfg.Root
	}
	if fw == FrameworkReact {
		opts.JSX = api.JSXAutomatic
	}

	res, err := b.Build(opts)
	if err != nil {
		return BuildResult{}, err
	}

	br := BuildResult{OutDir: outDir}
	for _, f := range res.OutputFiles {
		br.OutputFiles = append(br.OutputFiles, f.Path)
	}
	for _, m := range res.Errors {
		br.Errors = append(br.Errors, formatMsg(m))
	}
	for _, m := range res.Warnings {
		br.Warnings = append(br.Warnings, formatMsg(m))
	}
	if len(br.Errors) > 0 {
		return br, nil // config was valid; the build itself failed
	}

	// Mirror public/* into the output dir (favicons, manifest, etc.).
	for _, candidate := range []string{
		filepath.Join(cfg.Root, "public"),
		filepath.Join(srcDir, "public"),
	} {
		if n, perr := bundler.CopyPublicDir(candidate, outDir); perr == nil && n > 0 {
			break
		}
	}

	// Emit the production index.html referencing the built entry/CSS.
	if err := writeProductionIndex(cfg.Root, srcDir, outDir, entries, cfg.PublicPath, container); err != nil {
		return br, fmt.Errorf("viteless: write index.html: %w", err)
	}
	logf("viteless: built %d entr%s → %s", len(entries), plural(len(entries)), outDir)
	return br, nil
}

func formatMsg(m api.Message) string {
	if m.Location != nil {
		return fmt.Sprintf("%s:%d:%d: %s", m.Location.File, m.Location.Line, m.Location.Column, m.Text)
	}
	return m.Text
}

func derefOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// openStore opens the content-addressed cache at root (or the default).
func openStore(root string) (*store.Store, error) {
	if root == "" {
		root = store.DefaultRoot()
	}
	st, err := store.New(root)
	if err != nil {
		return nil, fmt.Errorf("viteless: open cache %s: %w", root, err)
	}
	return st, nil
}

// newOnDemandFetch returns a resolver FetchOnDemand hook that pulls
// missing dependency URLs from the registry, caches them in the store,
// and records them in the lockfile for repeatability. Returns nil when
// lf/st are nil (no CDN resolution).
func newOnDemandFetch(lf *lockfile.File, st *store.Store, lockPath, registry string, deps map[string]string, logf func(string, ...any)) func(string) (string, error) {
	if lf == nil || st == nil {
		return nil
	}
	if registry == "" {
		registry = os.Getenv("NEXUS_REGISTRY")
	}
	if registry == "" {
		registry = fetcher.DefaultRegistry
	}
	f := fetcher.New(st, registry)
	f.External = []string{"vue", "react", "react-dom"}

	var mu sync.Mutex
	return func(reqURL string) (string, error) {
		res, err := f.Fetch(context.Background(), pinVersion(reqURL, deps))
		if err != nil {
			return "", err
		}
		mu.Lock()
		lf.Add(res.Root)
		for _, t := range res.Transitive {
			lf.Add(t)
		}
		if lockPath != "" {
			if werr := lf.Save(lockPath); werr != nil {
				logf("viteless: warning: lockfile save after on-demand fetch failed: %v", werr)
			}
		}
		mu.Unlock()
		return res.Root.Resolved, nil
	}
}

// readProjectDeps reads <root>/package.json and returns the merged
// dependencies + devDependencies (name → version range). Returns nil when
// there's no package.json. Used to pin CDN dependency versions to what the
// project declares instead of always pulling the registry's latest.
func readProjectDeps(root string) map[string]string {
	b, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return nil
	}
	var pj struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if json.Unmarshal(b, &pj) != nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range pj.DevDependencies {
		out[k] = v
	}
	for k, v := range pj.Dependencies {
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// pinVersion rewrites a bare, unversioned package request to its
// package.json-declared range (e.g. "react" → "react@^19"), so esm.sh
// resolves the version the project pinned. Only the unscoped package root
// is pinned — subpaths inherit the version once the root is in the
// lockfile; scoped names and already-versioned/URL requests pass through.
func pinVersion(reqURL string, deps map[string]string) string {
	if deps == nil || strings.ContainsAny(reqURL, "/:") {
		return reqURL
	}
	if len(reqURL) > 1 && strings.Contains(reqURL[1:], "@") {
		return reqURL // already carries a version
	}
	if rng, ok := deps[reqURL]; ok && rng != "" {
		return reqURL + "@" + rng
	}
	return reqURL
}

// collectFrontendEntries returns one entry path per top-level JS/TS file
// in srcDir. .vue/.css/.json are colocated dependencies, not entries.
func collectFrontendEntries(srcDir string) ([]string, error) {
	dirents, err := os.ReadDir(srcDir)
	if err != nil {
		return nil, fmt.Errorf("viteless: read %s: %w", srcDir, err)
	}
	var entries []string
	for _, e := range dirents {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || isConfigFile(e.Name()) {
			continue
		}
		switch filepath.Ext(e.Name()) {
		case ".jsx", ".tsx", ".ts", ".js":
			entries = append(entries, filepath.Join(srcDir, e.Name()))
		}
	}
	return entries, nil
}

// isConfigFile reports whether name is a tooling config (vite.config.ts,
// viteless.config.ts, tailwind.config.js, …) that must never be treated as
// an application entry — bundling one would try to resolve its `import
// 'vite'`/'viteless' and fail.
func isConfigFile(name string) bool {
	lower := strings.ToLower(name)
	for _, suffix := range []string{".config.ts", ".config.mts", ".config.js", ".config.mjs", ".config.cjs"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return strings.HasPrefix(lower, "vite-env") || lower == "env.d.ts"
}

// nestedEntryFallbacks are the conventional subdirectories checked when
// the top level of the search root has no entries.
var nestedEntryFallbacks = []string{"src", "app", "client"}

// findFrontendEntries returns the directory + entry list to drive the
// bundler, descending into a conventional subdir when the top level has
// none.
func findFrontendEntries(srcDir string) (string, []string, error) {
	entries, err := collectFrontendEntries(srcDir)
	if err != nil {
		return srcDir, nil, err
	}
	if len(entries) > 0 {
		return srcDir, entries, nil
	}
	for _, name := range nestedEntryFallbacks {
		sub := filepath.Join(srcDir, name)
		if info, statErr := os.Stat(sub); statErr != nil || !info.IsDir() {
			continue
		}
		if nested, nestedErr := collectFrontendEntries(sub); nestedErr == nil && len(nested) > 0 {
			return sub, nested, nil
		}
	}
	return srcDir, nil, nil
}

// hasVueSources reports whether srcDir (recursively) contains any .vue
// file — main.ts may transitively import an App.vue even when no entry
// is itself a .vue.
func hasVueSources(srcDir string) bool {
	found := false
	_ = filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".vue") {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// detectFramework infers the framework from the source tree: a .vue file
// wins (Vue), then a .tsx/.jsx entry (React), else Vanilla.
func detectFramework(srcDir string) Framework {
	if hasVueSources(srcDir) {
		return FrameworkVue
	}
	jsx := false
	_ = filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".tsx") || strings.HasSuffix(path, ".jsx") {
			jsx = true
			return filepath.SkipAll
		}
		return nil
	})
	if jsx {
		return FrameworkReact
	}
	return FrameworkVanilla
}

// moduleScriptRE matches the first module <script src="…"> in an HTML
// shell so the production entry name can be substituted in.
var moduleScriptRE = regexp.MustCompile(`(<script[^>]*\bsrc=")([^"]+)("[^>]*>)`)

// writeProductionIndex emits outDir/index.html from the source shell,
// repointing its module script at the built entry (/<name>.js) and
// adding a stylesheet link when the entry produced a CSS sibling.
func writeProductionIndex(root, srcDir, outDir string, entries []string, publicPath string, plugins *pluginContainer) error {
	base := strings.TrimSuffix(filepath.Base(entries[0]), filepath.Ext(entries[0]))
	pp := publicPath
	if pp == "" {
		pp = "/"
	}
	if !strings.HasSuffix(pp, "/") {
		pp += "/"
	}
	jsHref := pp + base + ".js"
	cssHref := pp + base + ".css"
	hasCSS := false
	if _, err := os.Stat(filepath.Join(outDir, base+".css")); err == nil {
		hasCSS = true
	}

	var html string
	for _, p := range []string{filepath.Join(root, "index.html"), filepath.Join(srcDir, "index.html")} {
		if b, err := os.ReadFile(p); err == nil {
			html = string(b)
			break
		}
	}
	// Plugin transformIndexHtml hooks (Vite parity) run on the shell.
	if plugins != nil && html != "" {
		if out, err := plugins.transformIndexHtml(html); err == nil {
			html = out
		} else {
			return err
		}
	}
	if html == "" {
		html = "<!doctype html>\n<html>\n<head><meta charset=\"utf-8\"></head>\n<body>\n<div id=\"app\"></div>\n<script type=\"module\" src=\"" + jsHref + "\"></script>\n</body>\n</html>\n"
	} else if moduleScriptRE.MatchString(html) {
		replaced := false
		html = moduleScriptRE.ReplaceAllStringFunc(html, func(m string) string {
			if replaced {
				return m
			}
			replaced = true
			sub := moduleScriptRE.FindStringSubmatch(m)
			return sub[1] + jsHref + sub[3]
		})
	} else {
		html = strings.Replace(html, "</body>", "<script type=\"module\" src=\""+jsHref+"\"></script>\n</body>", 1)
	}
	if hasCSS && !strings.Contains(html, cssHref) {
		link := "<link rel=\"stylesheet\" href=\"" + cssHref + "\">"
		if strings.Contains(html, "</head>") {
			html = strings.Replace(html, "</head>", link+"\n</head>", 1)
		} else {
			html = link + "\n" + html
		}
	}
	return os.WriteFile(filepath.Join(outDir, "index.html"), []byte(html), 0o644)
}
