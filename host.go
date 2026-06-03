// Package devserver implements viteless.Host backed by nexus's existing
// frontend machinery — the SFC compiler, the esbuild single-file transform,
// and the nexus.lock/.nexus-cache resolver. It is the seam that lets
// `nexus dev` serve the SPA unbundled (one module per URL, one Vue
// instance, real state-preserving HMR) instead of bundling, while
// `nexus build` keeps using the bundler unchanged.
//
// The Host plugs three operations into viteless:
//
//   - LoadModule  — bytes for a served URL: user source under Root, or a
//     cached dependency blob under DepPrefix.
//   - Transform   — one file → browser JS: .vue via the SFC compiler (+ an
//     HMR accept footer), .ts/.tsx/.jsx/.json via esbuild's
//     single-file Transform, .js/.mjs passthrough.
//   - ResolveImport — a specifier → the URL the browser should fetch:
//     relative/alias imports stay in the source tree; bare and
//     registry-internal imports go through the shared resolver
//     and are served from the cache under DepPrefix.
package viteless

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/evanw/esbuild/pkg/api"

	"github.com/paulmanoni/viteless/internal/resolver"
	"github.com/paulmanoni/viteless/internal/sfc/vue"
	"github.com/paulmanoni/viteless/internal/store"
)

// Alias maps an import specifier to a filesystem target, mirroring a
// tsconfig "paths" entry. Two shapes:
//
//   - Wildcard: Prefix "@/", Dir "<root>/src", Exact=false — from
//     "@/*": ["./src/*"]. The text after Prefix is joined onto Dir.
//   - Exact: Prefix "nexus-client", Dir "<root>/src/sdk/client.js",
//     Exact=true — from "nexus-client": ["src/sdk/client.js"]. The whole
//     specifier maps to that one file.
//
// The target is expected to live under Root so it can be served.
type Alias struct {
	Prefix string // import prefix (wildcard) or full specifier (exact)
	Dir    string // target dir (wildcard) or target file (exact)
	Exact  bool   // true → whole-specifier → single file mapping
}

// Config configures a Host.
type HostConfig struct {
	// Root is the directory served as the dev origin: index.html lives
	// here and a URL path "/x/y" maps to <Root>/x/y. Required.
	Root string

	// IndexHTML is an optional absolute path to the SPA shell when it
	// doesn't live under Root. The nexus layout keeps source under
	// islands.src/ (= Root) but the shell index.html under islands/ (the
	// build-output dir); without this the dev server has no index to serve
	// and the SPA can't boot. The entry <script> in it is rewritten to the
	// real source entry on serve (its checked-in /main.js points at the
	// production bundle, which doesn't exist in unbundled dev).
	IndexHTML string

	// Resolver carries the lockfile + store + on-demand/dev-rewrite hooks
	// the shared resolver uses to turn bare specs into cached blob URLs.
	Resolver resolver.Options

	// Compiler compiles .vue SFCs. nil disables Vue (a .vue request then
	// surfaces a transform error / overlay).
	Compiler vue.SFCCompiler

	// Aliases are tsconfig-style path aliases resolved against the source
	// tree before the dependency resolver is consulted.
	Aliases []Alias

	// Env carries import.meta.env.<NAME> substitutions (the VITE_* vars
	// from .env files). Inlined as esbuild Defines during Transform so a
	// real app's `import.meta.env.VITE_API` reads the value at dev time.
	Env map[string]string

	// Mode is the dev mode injected as import.meta.env.MODE alongside the
	// DEV/PROD booleans. Typically "development".
	Mode string

	// JSX selects the JSX transform for .jsx/.tsx. Zero value is classic
	// (React.createElement, needs React in scope); api.JSXAutomatic uses
	// the React 17+ runtime so modules need no `import React`.
	JSX api.JSX

	// JSXImportSource is the automatic-runtime import source ("react",
	// "preact", …). Ignored unless JSX == api.JSXAutomatic.
	JSXImportSource string

	// DepPrefix is the URL namespace cached dependency blobs are served
	// under. Defaults to "/@dep/".
	DepPrefix string

	// NodeModules, when set, switches dependency resolution from the
	// esm.sh CDN to this local node_modules directory: bare imports are
	// optimized (esbuild-bundled to ESM, with code-splitting so shared
	// deps stay singletons) and served under /@nm/. Set by Dev() when a
	// project has node_modules.
	NodeModules string

	// CacheRoot is the base directory for the node_modules optimizer's
	// on-disk cache (only used when NodeModules is set).
	CacheRoot string

	// Logf, if set, receives diagnostic messages (e.g. node_modules
	// optimize failures). Defaults to a no-op.
	Logf func(string, ...any)

	// Prebundle enables dependency pre-bundling: each npm package is
	// esbuild-bundled into one file on first request (intra-package
	// siblings inlined, cross-package imports kept external + shared), so
	// the browser fetches one file per dep instead of hundreds. Off by
	// default — a pure perf layer over the per-module path, which stays
	// the fallback for any package that fails to bundle.
	Prebundle bool
}

// Host implements viteless.Host.
type DefaultHost struct {
	root      string
	indexHTML string
	depPrefix string
	res       resolver.Options
	compiler  vue.SFCCompiler
	aliases   []Alias
	defines   map[string]string
	jsx       api.JSX
	jsxSource string

	// deps maps a served DepPrefix path → the canonical registry URL its
	// bytes are cached under. Populated by ResolveImport (which always
	// runs before the browser fetches the resolved URL) and read by
	// LoadModule; a deterministic encode/decode is the fallback.
	deps sync.Map

	// nm is the node_modules dependency optimizer (nil unless NodeModules
	// dep mode is active). When set it replaces the CDN resolver for bare
	// imports.
	nm *nmOptimizer

	// pre is the dependency pre-bundler (nil when Prebundle is off). It
	// tracks each package's entry set internally and builds them grouped
	// with code-splitting; the Host only encodes/decodes the served paths.
	pre *prebundler
}

// New builds a Host from cfg.
func NewDefaultHost(cfg HostConfig) *DefaultHost {
	dp := cfg.DepPrefix
	if dp == "" {
		dp = "/@dep/"
	}
	h := &DefaultHost{
		root:      cfg.Root,
		indexHTML: cfg.IndexHTML,
		depPrefix: dp,
		res:       cfg.Resolver,
		compiler:  cfg.Compiler,
		aliases:   cfg.Aliases,
		defines:   buildDefines(cfg.Env, cfg.Mode),
		jsx:       cfg.JSX,
		jsxSource: cfg.JSXImportSource,
	}
	if cfg.NodeModules != "" {
		cacheRoot := cfg.CacheRoot
		if cacheRoot == "" {
			cacheRoot = store.DefaultRoot()
		}
		cacheDir := filepath.Join(cacheRoot, "nm-optimize", shortHashLong(cfg.Root))
		h.nm = newNMOptimizer(cfg.Root, cacheDir, true, cfg.Logf)
	} else if cfg.Prebundle {
		// Pre-bundling is the CDN-mode perf layer; node_modules mode uses
		// the optimizer instead.
		h.pre = newPrebundler(h)
	}
	return h
}

// buildDefines composes the import.meta.env.* substitution map handed to
// esbuild's Transform. Mirrors the bundler's dev Defines so unbundled dev
// and `nexus build` expose the same env surface: MODE/DEV/PROD plus each
// caller-supplied VITE_* var, all JSON-encoded to valid JS literals.
func buildDefines(env map[string]string, mode string) map[string]string {
	d := map[string]string{}
	if mode != "" {
		d["import.meta.env.MODE"] = jsLit(mode)
		d["import.meta.env.DEV"] = boolLit(mode == "development")
		d["import.meta.env.PROD"] = boolLit(mode == "production")
		d["import.meta.env.BASE_URL"] = jsLit("/")
	}
	for k, v := range env {
		d["import.meta.env."+k] = jsLit(v)
	}
	if len(d) == 0 {
		return nil
	}
	return d
}

func jsLit(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

func boolLit(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// LoadModule returns the source bytes + kind for a served URL path.
func (h *DefaultHost) LoadModule(urlPath string) ([]byte, string, bool) {
	if h.nm != nil && strings.HasPrefix(urlPath, nmPrefix) {
		if b, ok := h.nm.load(urlPath); ok {
			return b, "js", true
		}
		return nil, "", false
	}
	if h.pre != nil && strings.HasPrefix(urlPath, PrebundlePrefix) {
		return h.loadPrebundle(urlPath)
	}
	if strings.HasPrefix(urlPath, h.depPrefix) {
		return h.loadDep(urlPath)
	}
	return h.loadSource(urlPath)
}

// loadPrebundle serves a file from a package's grouped, code-split build.
// The path is /@pre/<pkg@ver>/<basename>, where basename is an entry
// (e-<hash>.js) or a shared chunk (chunk-<hash>.js). It splits the package
// key from the basename and asks the prebundler, which builds/rebuilds the
// whole package lazily. On a build error it returns ok=false (404); but
// ResolveImport only points entries here, and chunks are only referenced by
// an already-built entry, so a miss is rare.
func (h *DefaultHost) loadPrebundle(urlPath string) ([]byte, string, bool) {
	rest := strings.TrimPrefix(urlPath, PrebundlePrefix)
	slash := strings.LastIndexByte(rest, '/')
	if slash < 0 {
		return nil, "", false
	}
	pkg, base := rest[:slash], rest[slash+1:]
	js, err := h.pre.serve(pkg, base)
	if err != nil {
		return nil, "", false
	}
	return []byte(js), "js", true
}

// loadSource serves a file from the project source tree under Root.
func (h *DefaultHost) loadSource(urlPath string) ([]byte, string, bool) {
	clean := path.Clean(urlPath)
	if clean == "/" || clean == "." {
		clean = "/index.html"
	}
	// Reject traversal escapes: the cleaned path must stay rooted.
	if strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return nil, "", false
	}
	rel := filepath.FromSlash(strings.TrimPrefix(clean, "/"))
	fsPath := filepath.Join(h.root, rel)
	// Defense in depth: ensure the resolved path is still within Root.
	if r, err := filepath.Rel(h.root, fsPath); err != nil || strings.HasPrefix(r, "..") {
		return nil, "", false
	}
	body, err := os.ReadFile(fsPath)
	if err != nil {
		// Shell fallback: the SPA index.html may live outside Root (nexus
		// keeps source under islands.src/ but the shell under islands/).
		// Serve the configured IndexHTML for the "/" → /index.html request
		// so the dev server boots the app instead of proxying the broken
		// production shell. Entry-script rewrite (below) re-points it at the
		// source entry, resolved against Root.
		if clean == "/index.html" && h.indexHTML != "" {
			if ib, ierr := os.ReadFile(h.indexHTML); ierr == nil {
				return h.rewriteEntryScripts(injectVueFlags(ib)), "html", true
			}
		}
		// public/ convention: assets the HTML hard-codes (favicons, PWA
		// icons, manifest.json) live in <Root>/public and are served at
		// the site root — e.g. <link href="/arm-192.png"> resolves to
		// <Root>/public/arm-192.png. Mirrors Vite + the bundler's
		// CopyPublicDir step. Only consulted on a direct-path miss.
		pubPath := filepath.Join(h.root, "public", rel)
		if pr, perr := filepath.Rel(filepath.Join(h.root, "public"), pubPath); perr == nil && !strings.HasPrefix(pr, "..") {
			if pb, perr := os.ReadFile(pubPath); perr == nil {
				return pb, kindForExt(strings.ToLower(path.Ext(clean))), true
			}
		}
		return nil, "", false
	}
	ext := strings.ToLower(path.Ext(clean))
	kind := kindForExt(ext)
	switch {
	case kind == "html":
		body = injectVueFlags(body)
		body = h.rewriteEntryScripts(body)
	}
	return body, kind, true
}

// entryScriptRE matches a module script tag's src attribute pointing at a
// root-absolute .js entry — e.g. <script type="module" src="/main.js">.
var entryScriptRE = regexp.MustCompile(`(<script[^>]*\bsrc=")(/[^"]+\.js)(")`)

// rewriteEntryScripts fixes the dev-mode entry mismatch: the project's
// checked-in index.html references the PRODUCTION bundle name ("/main.js",
// what `nexus build` emits), but in the unbundled dev server only the
// SOURCE entry ("/main.ts") exists — so the browser's request for /main.js
// 404s and the SPA never boots. When a referenced /X.js has no on-disk file
// but a /X.ts (or .tsx/.jsx/.mjs) sibling does, rewrite the tag to the
// source path so it flows through the normal transform pipeline. Vite does
// the same by having index.html point at the source entry directly.
func (h *DefaultHost) rewriteEntryScripts(html []byte) []byte {
	return entryScriptRE.ReplaceAllFunc(html, func(m []byte) []byte {
		sub := entryScriptRE.FindSubmatch(m)
		jsURL := string(sub[2]) // "/main.js"
		rel := strings.TrimPrefix(path.Clean(jsURL), "/")
		// Already on disk as-is → leave it (a real prebuilt bundle).
		if fi, err := os.Stat(filepath.Join(h.root, filepath.FromSlash(rel))); err == nil && !fi.IsDir() {
			return m
		}
		base := strings.TrimSuffix(jsURL, ".js")
		for _, ext := range []string{".ts", ".tsx", ".jsx", ".mjs", ".js"} {
			cand := strings.TrimPrefix(base+ext, "/")
			if fi, err := os.Stat(filepath.Join(h.root, filepath.FromSlash(cand))); err == nil && !fi.IsDir() {
				return []byte(string(sub[1]) + base + ext + string(sub[3]))
			}
		}
		return m
	})
}

// vueFlagsScript defines Vue 3 esm-bundler's compile-time feature flags as
// globals. The esm.sh Vue build reads these and warns when they're absent
// (the production bundler sets them via Define/banner). A classic inline
// script in <head> runs before any deferred module — so the flags exist by
// the time vue.development.mjs evaluates. Values mirror the bundler's dev
// defaults (frontend/deps/bundler vueRuntimeFlagsBanner).
const vueFlagsScript = `<script>` +
	`globalThis.__VUE_OPTIONS_API__=true;` +
	`globalThis.__VUE_PROD_DEVTOOLS__=false;` +
	`globalThis.__VUE_PROD_HYDRATION_MISMATCH_DETAILS__=false;` +
	`</script>`

// injectVueFlags inserts the feature-flag script as early as possible in
// the HTML head (before the first <script>/<link>, else before </head>),
// so it executes ahead of the entry module graph.
func injectVueFlags(html []byte) []byte {
	h := string(html)
	if strings.Contains(h, "__VUE_OPTIONS_API__") {
		return html // already present
	}
	if i := strings.Index(h, "<script"); i >= 0 {
		return []byte(h[:i] + vueFlagsScript + "\n  " + h[i:])
	}
	if i := strings.Index(h, "</head>"); i >= 0 {
		return []byte(h[:i] + vueFlagsScript + "\n" + h[i:])
	}
	return []byte(vueFlagsScript + "\n" + h)
}

// loadDep serves a cached dependency blob, reverse-mapping the served path
// to its canonical registry URL and reading it from the store.
func (h *DefaultHost) loadDep(urlPath string) ([]byte, string, bool) {
	canonical, ok := h.canonicalFor(urlPath)
	if !ok {
		return nil, "", false
	}
	// Resolve the canonical URL to its actual cache key. LocateBlobURL
	// applies the store / query-strip / on-demand lookups AND the
	// semver-range fallback — so a /@dep/ path that decodes to an
	// unresolved range URL (e.g. `graphql@^15.0.0 || ^16.0.0`, which a
	// previously-cached blob baked into its imports) resolves to the
	// concrete version the lockfile pinned instead of 404ing.
	key, ok := h.res.LocateBlobURL(canonical)
	if !ok {
		return nil, "", false
	}
	blob, meta, err := h.res.Store.Get(key)
	if err != nil {
		return nil, "", false
	}
	body, err := os.ReadFile(blob)
	if err != nil {
		return nil, "", false
	}
	kind := kindForContentType(meta.ContentType, key)
	// finishDep rebases CSS url()s against the resolved key (its real
	// registry location) so font/asset refs resolve correctly.
	return h.finishDep(body, kind, key), kind, true
}

// finishDep post-processes a dep blob before it's served. For CSS it
// rebases relative url() references (fonts, images) to absolute /@dep/
// URLs: the CSS is injected as a <style> tag, so a relative url() like
// `../fonts/x.woff2` would otherwise resolve against the PAGE origin
// (localhost:5173) and 404. Resolving each against the CSS's own registry
// URL — exactly how the bundler's registry-internal resolver handles it —
// points the browser at the cached font blob instead.
func (h *DefaultHost) finishDep(body []byte, kind, importerURL string) []byte {
	if kind != "css" {
		return body
	}
	return []byte(rewriteCSSURLs(string(body), func(ref string) string {
		// Leave absolute URLs, data:, and bare #fragment refs untouched.
		if ref == "" || strings.HasPrefix(ref, "data:") || strings.HasPrefix(ref, "#") ||
			strings.Contains(ref, "://") {
			return ref
		}
		// Resolve the WHOLE ref (query included — esm.sh keys font
		// variants on ?v=…) against the CSS's registry URL. ResolveURL's
		// registry-internal branch joins it via resolveRegistryURL and
		// fetches the blob on demand, so the font is in the cache by the
		// time the browser requests the rewritten /@dep/ URL.
		if u, ok, _ := h.res.ResolveURL(ref, importerURL); ok {
			return h.toDepPath(u)
		}
		return ref
	}))
}

// cssURLRE matches a CSS url() token, capturing the (optionally quoted)
// reference. Handles url(x), url('x'), url("x"). The captured groups are
// the three quote variants; exactly one is non-empty per match.
var cssURLRE = regexp.MustCompile(`url\(\s*(?:"([^"]*)"|'([^']*)'|([^)'"\s]*))\s*\)`)

// rewriteCSSURLs rewrites every url() reference in css via map. The
// rewritten value is re-emitted double-quoted. @import url()s and font/
// image refs all flow through. map receives the raw reference (no quotes)
// and returns the replacement.
func rewriteCSSURLs(css string, mapRef func(string) string) string {
	return cssURLRE.ReplaceAllStringFunc(css, func(m string) string {
		sub := cssURLRE.FindStringSubmatch(m)
		ref := sub[1]
		if ref == "" {
			ref = sub[2]
		}
		if ref == "" {
			ref = sub[3]
		}
		out := mapRef(ref)
		return `url("` + out + `")`
	})
}

// Transform compiles one source file to browser JS. viteless only calls
// this for "js"-kind modules; dispatch is by extension.
func (h *DefaultHost) Transform(urlPath string, src []byte) ([]byte, error) {
	// Strip any query (?t=…) viteless appends on hot re-imports.
	clean := urlPath
	if i := strings.IndexByte(clean, '?'); i >= 0 {
		clean = clean[:i]
	}
	ext := strings.ToLower(path.Ext(clean))
	switch ext {
	case ".vue":
		return h.transformVue(clean, src)
	case ".ts", ".tsx", ".jsx", ".json":
		return h.transformEsbuild(clean, src, ext)
	default:
		// .js / .mjs — already browser JS; passthrough (imports are
		// rewritten by viteless afterwards).
		return src, nil
	}
}

func (h *DefaultHost) transformVue(urlPath string, src []byte) ([]byte, error) {
	if h.compiler == nil {
		return nil, fmt.Errorf("no Vue SFC compiler wired for %s", urlPath)
	}
	// SASS/SCSS preprocessing is unsupported: the QuickJS adapter has no
	// preprocessor, so inline `<style lang="scss">` is passed through as-is.
	// Plain CSS and Tailwind are the supported style paths.
	res, err := h.compiler.Compile(string(src), urlPath)
	if err != nil {
		return nil, err
	}
	if len(res.Errors) > 0 {
		var b strings.Builder
		for i, e := range res.Errors {
			if i > 0 {
				b.WriteByte('\n')
			}
			fmt.Fprintf(&b, "%s (%d:%d)", e.Message, e.Line, e.Column)
		}
		return nil, fmt.Errorf("%s", b.String())
	}
	// @vue/compiler-sfc emits TypeScript when the SFC uses
	// `<script setup lang="ts">` — and even pure-JS SFCs get a typed
	// template-render wrapper (`(_ctx: any, _cache: any) => …`). The
	// browser can't parse those annotations, so strip them with esbuild's
	// single-file TS transform (the bundler path does the same via
	// api.LoaderTS). Without this the served module dies with
	// "missing ) after formal parameters" on the first typed signature.
	stripped, err := h.transformEsbuild(urlPath, []byte(res.Code), ".ts")
	if err != nil {
		return nil, err
	}
	// The compiled SFC already sets __sfc__.__hmrId and registers the
	// component with the Vue HMR runtime (createRecord). Append the
	// accept footer (plain JS) so an edited module hot-swaps the live
	// component in place (state preserved) instead of forcing a reload.
	return append(stripped, []byte(vueHMRFooter)...), nil
}

// vueHMRFooter wires the standard Vue SFC hot-update path. It reads the
// runtime off globalThis (the dev Vue build installs it there) so it works
// in an unbundled module where a bare __VUE_HMR_RUNTIME__ would be
// undefined.
const vueHMRFooter = `
if (import.meta.hot) {
  import.meta.hot.accept((mod) => {
    const R = globalThis.__VUE_HMR_RUNTIME__;
    if (!mod || !R) { return; }
    const c = mod.default;
    if (c && c.__hmrId) { R.reload(c.__hmrId, c); }
  });
}
`

// transformEsbuild runs esbuild's single-file Transform to strip TS types /
// compile JSX / turn JSON into an ESM default export.
func (h *DefaultHost) transformEsbuild(urlPath string, src []byte, ext string) ([]byte, error) {
	loader := api.LoaderTS
	switch ext {
	case ".tsx":
		loader = api.LoaderTSX
	case ".jsx":
		loader = api.LoaderJSX
	case ".json":
		loader = api.LoaderJSON
	}
	r := api.Transform(string(src), api.TransformOptions{
		Loader:          loader,
		Format:          api.FormatESModule,
		Target:          api.ES2022,
		Sourcefile:      urlPath,
		Sourcemap:       api.SourceMapInline,
		Define:          h.defines,
		JSX:             h.jsx,
		JSXImportSource: h.jsxSource,
	})
	if len(r.Errors) > 0 {
		return nil, fmt.Errorf("%s: %s", urlPath, r.Errors[0].Text)
	}
	return r.Code, nil
}

// ResolveImport maps an import specifier to the URL the browser should
// fetch. importerURL is the served path of the importing module.
func (h *DefaultHost) ResolveImport(spec string, kind SpecKind, importerURL string) string {
	// node_modules mode: the optimizer owns bare imports.
	if h.nm != nil {
		// Inside an optimized bundle: keep its relative chunk imports
		// within /@nm/; route any (rare) bare import back to the optimizer.
		if strings.HasPrefix(importerURL, nmPrefix) {
			switch kind {
			case SpecRelative:
				return path.Clean(path.Join(path.Dir(importerURL), spec))
			case SpecBare:
				return h.nm.resolve(spec)
			default:
				return ""
			}
		}
		// User code: alias wins, then the optimizer; relative/absolute
		// fall through to the normal source-tree resolution below.
		if kind == SpecBare {
			if served, ok := h.resolveAlias(spec); ok {
				return served
			}
			return h.nm.resolve(spec)
		}
	}

	importerIsDep := strings.HasPrefix(importerURL, h.depPrefix)

	// Registry-internal imports: the importer is a cached dep blob, so a
	// relative/absolute/bare import resolves against its registry
	// siblings via the shared resolver.
	if importerIsDep {
		realImporter, _ := h.canonicalFor(importerURL)
		if u, ok, _ := h.res.ResolveURL(spec, realImporter); ok {
			return h.toDepPath(u)
		}
		return "" // leave untouched; the browser will surface the miss
	}

	// User code.
	switch kind {
	case SpecRelative:
		// Resolve against the importer's served directory; stays in the
		// source tree (served by loadSource). Apply Vite-style extension +
		// index resolution so `./router` finds router.ts or router/index.ts.
		served := path.Clean(path.Join(path.Dir(importerURL), spec))
		return h.resolveSourceURL(served)
	case SpecAbsolute:
		// A root-absolute path ("/foo") is already a served URL; an
		// absolute https:// URL is a registry reference.
		if strings.Contains(spec, "://") {
			if u, ok, _ := h.res.ResolveURL(spec, ""); ok {
				return h.toDepPath(u)
			}
		}
		return "" // "/foo" — leave as-is, loadSource handles it
	default: // SpecBare
		// tsconfig-style alias (e.g. "@/views/Foo.vue") → source tree.
		if served, ok := h.resolveAlias(spec); ok {
			return served
		}
		// Real package import → shared resolver → cached blob URL.
		if u, ok, _ := h.res.ResolveURL(spec, ""); ok {
			// Pre-bundle eligible packages (everything except the Vue
			// family) into one file, collapsing the intra-package fan-out.
			// Only JS entries are bundled — a CSS-only package like
			// @mdi/font is served per-module as a style. A failed build
			// later falls back to per-module via loadDep.
			if h.pre != nil && prebundleEligible(u) && h.depURLIsJS(u) {
				return h.toPrebundle(u)
			}
			return h.toDepPath(u)
		}
		return ""
	}
}

// toPrebundle registers a package entry with the prebundler and returns its
// served path /@pre/<pkg@ver>/e-<hash>.js. Registration accumulates the
// package's entry set so the next build covers every sub-path entry the app
// imports; the actual (grouped, code-split) build runs lazily on first
// fetch. The e-<hash> basename is stable per entry URL.
func (h *DefaultHost) toPrebundle(entryStoreURL string) string {
	pkg, base := h.pre.register(entryStoreURL)
	return PrebundlePrefix + pkg + "/" + base
}

// resolveAlias rewrites a tsconfig-style aliased import to its served path
// under Root. Returns ok=false when no alias matches. Exact aliases
// (whole-specifier → one file, e.g. "nexus-client/vue") are tried before
// wildcard prefixes so a specific entry wins over a broad "@/"-style one.
func (h *DefaultHost) resolveAlias(spec string) (string, bool) {
	// Exact matches first.
	for _, a := range h.aliases {
		if a.Exact && spec == a.Prefix {
			if served, ok := h.aliasServedPath(a.Dir); ok {
				return served, true
			}
		}
	}
	// Then wildcard prefixes (longest prefix wins, so a deeper alias isn't
	// shadowed by a shorter one like a bare "@/").
	best := -1
	var bestAlias Alias
	for _, a := range h.aliases {
		if a.Exact || a.Prefix == "" || !strings.HasPrefix(spec, a.Prefix) {
			continue
		}
		if len(a.Prefix) > best {
			best, bestAlias = len(a.Prefix), a
		}
	}
	if best < 0 {
		return "", false
	}
	rest := strings.TrimPrefix(spec, bestAlias.Prefix)
	abs := filepath.Join(bestAlias.Dir, filepath.FromSlash(rest))
	return h.aliasServedPath(abs)
}

// aliasServedPath turns an absolute filesystem target into a served URL
// path (with extension/index resolution), or ok=false when it escapes Root.
func (h *DefaultHost) aliasServedPath(abs string) (string, bool) {
	rel, err := filepath.Rel(h.root, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", false // target outside Root — can't serve it
	}
	return h.resolveSourceURL("/" + filepath.ToSlash(rel)), true
}

// sourceResolveExts is the extension probe order for extensionless imports,
// matching Vite's default resolve.extensions (TS first, then JS, JSX/TSX,
// then .vue/.json/.mjs).
var sourceResolveExts = []string{".ts", ".tsx", ".js", ".jsx", ".vue", ".mjs", ".json"}

// resolveSourceURL applies Vite-style extension + index resolution to a
// served source URL. Browsers request exactly the URL we return, so an
// extensionless import like "/src/router" must be rewritten to the file
// that actually exists ("/src/router/index.ts") or the fetch 404s. If the
// path already resolves to a file (has an extension that exists, or is an
// exact hit), it's returned unchanged; if nothing matches, the original is
// returned so the miss surfaces normally.
func (h *DefaultHost) resolveSourceURL(urlPath string) string {
	clean := strings.TrimPrefix(path.Clean(urlPath), "/")
	abs := filepath.Join(h.root, filepath.FromSlash(clean))

	// Exact file hit (import already carried a real extension).
	if fi, err := os.Stat(abs); err == nil && !fi.IsDir() {
		return urlPath
	}
	// <path><ext>
	for _, ext := range sourceResolveExts {
		if fi, err := os.Stat(abs + ext); err == nil && !fi.IsDir() {
			return urlPath + ext
		}
	}
	// <path>/index<ext>
	for _, ext := range sourceResolveExts {
		if fi, err := os.Stat(filepath.Join(abs, "index"+ext)); err == nil && !fi.IsDir() {
			return strings.TrimRight(urlPath, "/") + "/index" + ext
		}
	}
	// TS-style .js → .ts mapping: `import './x.js'` where only x.ts exists
	// (TS source authored with explicit .js extensions). Swap the extension
	// and retry the on-disk probe.
	if jsExt := path.Ext(clean); jsExt == ".js" || jsExt == ".jsx" || jsExt == ".mjs" {
		stem := strings.TrimSuffix(urlPath, jsExt)
		stemAbs := strings.TrimSuffix(abs, jsExt)
		for _, ext := range []string{".ts", ".tsx"} {
			if fi, err := os.Stat(stemAbs + ext); err == nil && !fi.IsDir() {
				return stem + ext
			}
		}
	}
	// No on-disk match — return as-is; loadSource will 404 and the dev
	// sees a clear missing-module error rather than a silent wrong path.
	return urlPath
}

// toDepPath encodes a canonical registry URL into a served DepPrefix path
// and records the mapping for the reverse lookup.
//
//	https://esm.sh/vue@3.5.13/es2022/vue.mjs → /@dep/https/esm.sh/vue@3.5.13/es2022/vue.mjs
//
// A "?query" (rare for stored dep URLs) is folded into the path so it
// survives as part of r.URL.Path; the recorded mapping is authoritative.
func (h *DefaultHost) toDepPath(canonical string) string {
	p := strings.Replace(canonical, "://", "/", 1)
	p = strings.ReplaceAll(p, "?", "/__q__/")
	sp := h.depPrefix + p
	h.deps.Store(sp, canonical)
	return sp
}

// canonicalFor reverses toDepPath. The recorded mapping wins; the
// deterministic decode is the fallback for a cold LoadModule.
func (h *DefaultHost) canonicalFor(servedPath string) (string, bool) {
	if v, ok := h.deps.Load(servedPath); ok {
		return v.(string), true
	}
	rest := strings.TrimPrefix(servedPath, h.depPrefix)
	if rest == servedPath {
		return "", false
	}
	rest = strings.ReplaceAll(rest, "/__q__/", "?")
	rest = strings.Replace(rest, "/", "://", 1)
	return rest, true
}

// kindForExt classifies a source file by extension into a viteless module
// kind. Unknown extensions are treated as assets (served as a URL export).
func kindForExt(ext string) string {
	switch ext {
	case ".html", ".htm":
		return "html"
	case ".css", ".scss", ".sass":
		return "css"
	case ".vue", ".ts", ".tsx", ".jsx", ".js", ".mjs", ".json":
		return "js"
	default:
		return "asset"
	}
}

// kindForContentType classifies a cached dependency blob. The URL's
// extension is the fallback when the Content-Type is missing/ambiguous.
func kindForContentType(ct, url string) string {
	lc := strings.ToLower(ct)
	switch {
	case strings.Contains(lc, "css"):
		return "css"
	case strings.Contains(lc, "javascript"), strings.Contains(lc, "ecmascript"), strings.Contains(lc, "typescript"), strings.Contains(lc, "json"):
		return "js"
	case strings.HasPrefix(lc, "font/"), strings.HasPrefix(lc, "image/"), strings.Contains(lc, "application/font"):
		return "asset"
	}
	switch strings.ToLower(path.Ext(stripURLQuery(url))) {
	case ".css":
		return "css"
	case ".woff", ".woff2", ".ttf", ".otf", ".eot", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".ico", ".avif":
		return "asset"
	default:
		return "js"
	}
}

func stripURLQuery(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}
