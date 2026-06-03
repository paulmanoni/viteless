// Package bundler wraps esbuild's Build API and wires the
// nexus-specific plugins (resolver + Vue SFC) so .jsx / .tsx /
// .vue entry points produce a single self-contained JS bundle
// without any Node-side toolchain.
//
// The bundler is the user-visible surface for "build my frontend":
// `nexus build` and `nexus dev` both drive it. CLI commands like
// `nexus add` don't go through here — they only touch the store +
// fetcher + lockfile.
//
// Plugin layering (registration order = first-crack order):
//
//  1. Resolver plugin (OnResolve) — intercepts bare imports,
//     looks them up in the lockfile, returns the on-disk path
//     of the cached blob. Falls through to esbuild's default
//     resolver for relative paths and unknown specs.
//  2. Vue SFC plugin (OnLoad on *.vue) — runs the .vue source
//     through @vue/compiler-sfc via Goja, returns the resulting
//     JS as if it were a regular module.
//  3. esbuild's native handlers cover .js / .ts / .jsx / .tsx /
//     .css / .json / etc.
package bundler

import (
	"fmt"
	"io"
	"strings"

	"github.com/evanw/esbuild/pkg/api"

	"github.com/paulmanoni/viteless/internal/lockfile"
	"github.com/paulmanoni/viteless/internal/store"
)

// vueRuntimeFlagsBanner is prepended to every emitted JS bundle.
// Defines Vue 3 esm-bundler's three compile-time globals at module
// entry, defensive against any code path Define's identifier
// substitution couldn't reach (e.g. a downstream module reading
// globalThis.__VUE_PROD_DEVTOOLS__ instead of the bare identifier).
//
// The typeof check makes it safe to re-run against an already-set
// window — useful when nexus dev hot-rebuilds incrementally and a
// page reload re-evals the banner.
//
// One line so esbuild keeps it on the first source line and stack
// traces from runtime-core stay readable.
const vueRuntimeFlagsBanner = `if(typeof globalThis.__VUE_PROD_DEVTOOLS__==="undefined"){globalThis.__VUE_PROD_DEVTOOLS__=false;globalThis.__VUE_OPTIONS_API__=true;globalThis.__VUE_PROD_HYDRATION_MISMATCH_DETAILS__=false;}`

// Options configures one Build call. All fields are optional
// except Entries; sensible defaults are filled in by applyDefaults.
type Options struct {
	// Entries lists the absolute or project-relative paths of the
	// modules to bundle. Each becomes one output bundle. Most
	// users supply a single entry (e.g. "frontend/app.tsx");
	// multi-entry is supported for sites with several independent
	// island bundles per page.
	Entries []string

	// OutDir is the directory bundles are written into. Defaults
	// to "islands" — matches the convention the rest of the
	// framework expects (template.WithStatic("islands") serves it).
	OutDir string

	// Minify toggles esbuild's minifier. Defaults to true for
	// production-ish output; tests + dev mode flip it off for
	// readable output.
	Minify bool

	// Sourcemap controls source-map emission. Defaults to
	// api.SourceMapLinked (separate .map file alongside the
	// bundle, referenced by a //# sourceMappingURL comment).
	Sourcemap api.SourceMap

	// Watch enables esbuild's native watch mode. When true, Build
	// returns the first build's result immediately and esbuild
	// continues watching in the background. The OnRebuild
	// callback (if any) fires after each subsequent rebuild.
	// Caller must hold a reference to the returned Context to
	// stop the watcher cleanly.
	Watch bool

	// OnRebuild is the watch-mode callback fired after each
	// successful rebuild. Ignored when Watch is false.
	OnRebuild func(api.BuildResult)

	// Lockfile is the deps lockfile the resolver plugin consults.
	// Required when bundling code with bare imports; nil is fine
	// for pure-relative-import bundles (rare).
	Lockfile *lockfile.File

	// Store is the disk cache the resolver plugin reads cached
	// blobs from. Required when Lockfile is non-nil.
	Store *store.Store

	// Target is the JS language level esbuild targets. Defaults
	// to api.ES2022 — covers every browser less than 3 years old.
	Target api.Target

	// LogTo is where esbuild's diagnostics get printed. When nil,
	// esbuild's LogLevel is set to Silent and the caller reads
	// Errors/Warnings off the BuildResult instead. CLI sets this
	// to os.Stderr for interactive feedback.
	LogTo io.Writer

	// TSConfig is the project's tsconfig.json (or jsconfig.json)
	// path. esbuild reads its compilerOptions.paths and honors
	// them during resolution, so the Vite-classic
	//
	//	"paths": { "@/*": ["islands.src/src/*"] }
	//
	// pattern Just Works in nexus dev / nexus build without
	// requiring the operator to rewrite every `@/views/...`
	// import to a relative path.
	//
	// Empty disables tsconfig integration — esbuild falls back
	// to its standard relative-/-absolute-only resolution. The
	// CLI auto-detects ./tsconfig.json in the project root when
	// this is unset, so library callers usually don't set it.
	//
	// Important: esbuild interprets the path field's "baseUrl"
	// relative to the tsconfig location, not the working dir.
	// Pass an absolute path here to avoid surprises in
	// monorepo-style layouts where the bundler runs from a
	// subdirectory.
	TSConfig string

	// Env carries the build-time environment variables exposed
	// to bundled code via `import.meta.env.<NAME>`. Each entry
	// becomes an esbuild Define substitution that inlines the
	// value as a JSON-encoded literal at bundle time.
	//
	// Vite-port projects typically reference this as:
	//
	//	const api = import.meta.env.VITE_GATEWAY_API
	//
	// With Env["VITE_GATEWAY_API"] = "https://api.example.com"
	// set, every occurrence of that expression is replaced with
	// the literal string at compile time. Browser bundle has no
	// runtime lookup — same shape Vite emits.
	//
	// SECURITY: this is build-time substitution. Anything in
	// Env ends up in the public bundle. Don't put secrets here
	// — use runtime config (nexus.Get[T]) for anything
	// sensitive. The Vite convention is to only expose vars
	// prefixed `VITE_` to prevent accidental leakage of server
	// secrets that happen to be in the process env.
	//
	// nil disables import.meta.env substitution. Code that
	// reads `import.meta.env.X` will see undefined at runtime
	// (browser default), matching pre-v0.89 behavior.
	Env map[string]string

	// Mode is the build mode injected as `import.meta.env.MODE`
	// alongside the boolean flags `import.meta.env.DEV` /
	// `import.meta.env.PROD`. Vite convention values are
	// "development" or "production"; anything else just lands
	// as MODE verbatim with PROD=false / DEV=false.
	//
	// Empty disables all three substitutions — code reading
	// MODE/DEV/PROD sees undefined. Most callers want
	// "development" (nexus dev) or "production" (nexus build).
	Mode string

	// PublicPath is the URL prefix esbuild stamps onto every
	// file-loaded asset reference (image, font, SVG) so the
	// browser can resolve them regardless of which SPA route
	// is active. Default "/" (root-absolute) when empty.
	//
	// Override when deploying under a sub-path:
	//
	//	PublicPath: "/admin/"      // assets resolve to /admin/foo-HASH.png
	//	PublicPath: "/v2/"
	//
	// Must include the trailing slash — esbuild concatenates
	// it directly with the hashed filename. Missing slash =
	// "/adminfoo-HASH.png" 404.
	//
	// Also exposed as `import.meta.env.BASE_URL` for Vite-port
	// projects that read it at runtime (e.g. building absolute
	// URLs for fetch requests or router base-path config).
	PublicPath string

	// Splitting enables esbuild's code splitting. When on, code
	// shared across multiple entries — and anything behind a
	// dynamic `import()` — is hoisted into separate chunk files
	// (under "<OutDir>/chunks/") instead of being duplicated into
	// every bundle. This is what makes route-level lazy loading
	// (`import('./views/Settings.vue')`) actually shrink the
	// initial payload, matching Vite's default behavior.
	//
	// Requires the ES-module output format (always set) and an
	// output directory (always set), both of which the bundler
	// already guarantees. Safe to leave on for single-entry
	// projects with no dynamic imports: esbuild simply emits the
	// one entry file and no chunks, so output is byte-identical to
	// the unsplit build.
	//
	// Off by default so library callers that depend on a single
	// self-contained bundle file aren't surprised; the CLI
	// (`nexus build` / `nexus dev`) turns it on.
	Splitting bool

	// Inject lists files esbuild auto-imports into every entry point
	// (esbuild's --inject). Used in dev to seed a Vue HMR bridge: a
	// tiny module that does `import * as V from "vue"; globalThis
	// .__nexus_vue__ = V`, so the running app and separately-served HMR
	// update modules share ONE Vue instance (Vue's HMR runtime requires
	// it). The `import "vue"` is a bare spec, so it dedups to the same
	// instance the app already bundles.
	//
	// Empty (production builds) → no injection; the bundle is unchanged.
	Inject []string

	// JSX selects the JSX transform esbuild applies to .jsx/.tsx.
	// Zero value (api.JSXTransform) is the classic
	// React.createElement form — requires an in-scope `React`.
	// Set api.JSXAutomatic for the React 17+ automatic runtime
	// (no `import React` needed), paired with JSXImportSource.
	JSX api.JSX

	// JSXImportSource is the module the automatic runtime imports
	// the JSX helpers from — "react" (esbuild's default when empty
	// under JSXAutomatic), "preact", "solid-js", etc. Ignored
	// unless JSX == api.JSXAutomatic.
	JSXImportSource string

	// AbsWorkingDir sets esbuild's resolution base directory. The
	// DepNodeModules path points this at the frontend root so
	// esbuild's native node_modules resolution finds bare imports
	// without the esm.sh resolver plugin. Empty = esbuild default
	// (process cwd).
	AbsWorkingDir string

	// Alias maps an import prefix to a target path (esbuild's alias). Used
	// for tsconfig-style aliases sourced from a vite.config resolve.alias —
	// "@" → "<root>/src" resolves "@/x" → "<root>/src/x".
	Alias map[string]string
}

// assetLoaders is the default per-extension loader map handed to
// esbuild. Images + fonts go through the file loader (emitted to
// outDir with a content-hashed name, import returns the public URL
// string) so projects can do:
//
//	import flagPNG from "@/assets/flag.png"  // → "/flag-A1B2C3.png"
//	import "@mdi/font/css/icons.css"         // → bundled CSS
//
// Inline-friendly cases (.txt, .json) use loaders that produce JS
// values directly. SVG defaults to `file` rather than `dataurl`
// because most apps use it as <img src> not as inline CSS — easier
// to override per-project later if needed.
//
// Operators wanting different behavior (e.g. inline tiny PNGs as
// data URLs) can override via a future Options.Loaders field; for
// now the defaults cover the common Vite-port case.
var assetLoaders = map[string]api.Loader{
	// Images.
	".png":  api.LoaderFile,
	".jpg":  api.LoaderFile,
	".jpeg": api.LoaderFile,
	".gif":  api.LoaderFile,
	".webp": api.LoaderFile,
	".avif": api.LoaderFile,
	".svg":  api.LoaderFile,
	".ico":  api.LoaderFile,
	// Fonts.
	".woff":  api.LoaderFile,
	".woff2": api.LoaderFile,
	".ttf":   api.LoaderFile,
	".otf":   api.LoaderFile,
	".eot":   api.LoaderFile,
	// Misc.
	".txt":  api.LoaderText,
	".json": api.LoaderJSON,
	// CSS Modules: `.module.css` imports yield an object of
	// hashed class names that JS code references by their
	// original short name, e.g.
	//
	//	import styles from "./Button.module.css"
	//	<button class={styles.primary}>
	//
	// esbuild's local-css loader does the scoping — each class
	// name gets a content-hash suffix so collisions across
	// modules are impossible. Plain `.css` files stay
	// globally-scoped via esbuild's native CSS bundling, so
	// existing imports `import "./styles.css"` keep working.
	".module.css": api.LoaderLocalCSS,
	// .css is handled natively by esbuild without a loader entry
	// (CSS bundling is built in). Listing it here would override
	// that to "treat as raw text" — DON'T add it.
}

// Bundler holds plugins applied across every Build call. The user
// constructs one per project (or per CLI invocation), registers
// plugins via AddPlugin, then calls Build with per-invocation
// Options.
type Bundler struct {
	// Plugins is the list of esbuild plugins applied to every
	// Build call. Populated via AddPlugin. The order matters —
	// earlier plugins get first dibs at OnResolve/OnLoad events
	// for matching paths. Resolver should come BEFORE the Vue SFC
	// plugin so a bare "vue" import resolves before any .vue
	// load handler sees it.
	Plugins []api.Plugin
}

// New returns a Bundler with no plugins registered.
func New() *Bundler { return &Bundler{} }

// AddPlugin registers a plugin applied to every Build call.
// Plugins run in registration order.
func (b *Bundler) AddPlugin(p api.Plugin) {
	b.Plugins = append(b.Plugins, p)
}

// Result wraps esbuild's BuildResult plus the watch-mode Context
// (nil when Watch was false). Callers in watch mode hold onto Ctx
// and call Stop() / Dispose() to tear down the watcher cleanly.
type Result struct {
	api.BuildResult
	Ctx api.BuildContext
}

// Build produces bundles for opts.Entries. Returns the esbuild
// BuildResult so callers can inspect diagnostics + output files.
// Errors only surface when the configuration itself is invalid;
// per-file build diagnostics land in Result.Errors and don't
// short-circuit to a Go error.
func (b *Bundler) Build(opts Options) (Result, error) {
	if err := opts.validate(); err != nil {
		return Result{}, err
	}
	b.applyDefaults(&opts)

	logLevel := api.LogLevelSilent
	if opts.LogTo != nil {
		logLevel = api.LogLevelInfo
	}

	buildOpts := api.BuildOptions{
		EntryPoints:       opts.Entries,
		Outdir:            opts.OutDir,
		Bundle:            true,
		Write:             true,
		Format:            api.FormatESModule,
		Target:            opts.Target,
		Sourcemap:         opts.Sourcemap,
		// Code splitting hoists shared + dynamically-imported code
		// into chunk files instead of duplicating it per entry.
		// ChunkNames only takes effect when Splitting is true, so
		// setting it unconditionally is harmless. The content hash
		// makes chunks safe to cache far-future.
		Splitting:  opts.Splitting,
		ChunkNames: "chunks/[name]-[hash]",
		MinifyWhitespace:  opts.Minify,
		MinifyIdentifiers: opts.Minify,
		MinifySyntax:      opts.Minify,
		Plugins:           b.Plugins,
		LogLevel:          logLevel,
		// esbuild reads compilerOptions.paths from the given
		// tsconfig and applies them during resolution. Empty
		// string disables the integration cleanly.
		Tsconfig: opts.TSConfig,
		// Asset loaders for files imports typically reference
		// from Vue / React code:
		//
		//   import flag from "@/assets/flag.png"  → URL string
		//   import "./styles.css"                 → bundled CSS
		//
		// `file` loader emits the asset into outDir with a
		// content-hashed filename and the import returns the
		// public URL string. `css` loader bundles via esbuild's
		// native CSS pipeline so @import + url() recurse.
		// `text`/`json` cover the smaller cases.
		//
		// Without these, esbuild errors with "No loader is
		// configured for .png files" which the operator can't
		// fix without dropping into nexus internals — exactly
		// the Vite-port friction this is meant to remove.
		Loader: assetLoaders,
		// PublicPath prefixes file-loader output (image, font,
		// SVG references) with the site root so assets resolve
		// regardless of which SPA route is active. Default "/"
		// when caller didn't set it. See Options.PublicPath
		// godoc for sub-path deployment details.
		PublicPath: defaultPublicPath(opts.PublicPath),
		// Vue's esm-bundler distribution (which is what esm.sh
		// serves) reads three compile-time flags as bare global
		// identifiers — without build-time substitution they
		// surface in the browser as ReferenceErrors at module
		// load (the very first line of runtime-core.mjs does
		// `__VUE_PROD_DEVTOOLS__ && something`).
		//
		// Defaults match Vue's recommended production values:
		//   - PROD_DEVTOOLS=false       no Vue devtools hook
		//   - OPTIONS_API=true          keep Options API support
		//                               (set false if your app
		//                               is composition-only and
		//                               you want a smaller bundle)
		//   - HYDRATION_MISMATCH=false  drop SSR hydration debug
		//                               output from prod bundles
		//
		// Harmless for non-Vue projects — the identifiers just
		// don't appear in non-Vue code, so the Define has no
		// effect. Caller-supplied Define entries (future
		// option) would merge here rather than replace.
		Define: buildDefines(opts),
		// Runtime polyfill as a banner — runs at the top of
		// every emitted JS file BEFORE any imports execute, so
		// even if a Vue chunk somehow slipped past Define's
		// identifier substitution (rare; can happen when a
		// downstream module accesses the flag via globalThis
		// instead of as a bare identifier), the globals are
		// already set. Belt + suspenders.
		//
		// Idempotent (typeof check) so re-running the bundle
		// in dev mode against an already-set-up window object
		// is harmless. Cost: ~250 bytes per bundle, trivially
		// shaken away from non-browser builds since they'd
		// access globalThis anyway.
		Banner: map[string]string{
			"js": vueRuntimeFlagsBanner,
		},
		// Dev-only HMR bridge (empty in production). Auto-imported into
		// every entry so globalThis.__nexus_vue__ points at the app's
		// own Vue instance — shared with HMR update modules.
		Inject: opts.Inject,
		// JSX transform: classic by default; automatic runtime for
		// React 17+ / Preact / Solid when the caller sets it, so
		// JSX/TSX entries need no explicit `import React`.
		JSX:             opts.JSX,
		JSXImportSource: opts.JSXImportSource,
		AbsWorkingDir:   opts.AbsWorkingDir,
		Alias:           opts.Alias,
	}

	if opts.Watch {
		// Wire an OnEnd plugin so EVERY build (initial + every file-
		// change rebuild) flows through opts.OnRebuild. esbuild's
		// auto-rebuild loop never calls our code back otherwise —
		// the v0.71.3 path only invoked opts.OnRebuild manually for
		// the initial Rebuild(), so file-change rebuilds wrote (or
		// failed to write) silently with no log output. OnEnd is the
		// supported hook for "run after every build, including the
		// ones esbuild triggers itself."
		if opts.OnRebuild != nil {
			cb := opts.OnRebuild
			buildOpts.Plugins = append(buildOpts.Plugins, api.Plugin{
				Name: "nexus-bundler-onend",
				Setup: func(build api.PluginBuild) {
					build.OnEnd(func(result *api.BuildResult) (api.OnEndResult, error) {
						cb(*result)
						return api.OnEndResult{}, nil
					})
				},
			})
		}

		ctx, ctxErr := api.Context(buildOpts)
		// ctxErr is *ContextError — nil-check the pointer before
		// dereferencing the Errors field, otherwise a nil ctxErr
		// (which esbuild returns when api.Context succeeded) would
		// panic with SIGSEGV at the len(ctxErr.Errors) site.
		if ctxErr != nil && len(ctxErr.Errors) > 0 {
			return Result{}, fmt.Errorf("bundler: esbuild context: %s", ctxErr.Errors[0].Text)
		}
		// Defense-in-depth against esbuild API changes — a future
		// version could return (nil, nil) on an error path we
		// don't recognize, and we'd rather surface a clear message
		// than crash inside ctx.Rebuild().
		if ctx == nil {
			return Result{}, fmt.Errorf("bundler: esbuild returned nil watch context")
		}
		// Watch BEFORE Rebuild so esbuild arms its file-change
		// watcher before the initial bundle finishes. Watch does NOT
		// trigger a build itself; Rebuild runs the first one
		// synchronously and primes the dependency graph esbuild
		// watches afterwards.
		if err := ctx.Watch(api.WatchOptions{}); err != nil {
			return Result{}, fmt.Errorf("bundler: watch: %w", err)
		}
		first := ctx.Rebuild()
		return Result{BuildResult: first, Ctx: ctx}, nil
	}

	res := api.Build(buildOpts)
	return Result{BuildResult: res}, nil
}

func (o *Options) validate() error {
	if len(o.Entries) == 0 {
		return fmt.Errorf("bundler: at least one entry required")
	}
	if o.Lockfile != nil && o.Store == nil {
		return fmt.Errorf("bundler: Lockfile provided without Store — resolver plugin would have nowhere to read cached blobs")
	}
	return nil
}

func (b *Bundler) applyDefaults(o *Options) {
	if o.OutDir == "" {
		o.OutDir = "islands"
	}
	if o.Target == 0 {
		o.Target = api.ES2022
	}
	if o.Sourcemap == 0 {
		o.Sourcemap = api.SourceMapLinked
	}
}

// buildDefines composes the full esbuild Define map for one
// Build call. Three sources of substitutions, in precedence
// order (later wins):
//
//  1. Vue compile-time globals (always emitted; harmless when
//     no Vue code in the bundle since the identifiers don't
//     appear)
//  2. import.meta.env.{MODE,DEV,PROD} from Options.Mode
//  3. import.meta.env.<NAME> for each entry in Options.Env
//
// Env values are JSON-encoded so quotes/escapes are handled
// correctly — esbuild Define values must be valid JS literals,
// not raw strings. `"foo"` is correct; `foo` would be treated
// as an identifier.
//
// import.meta.env.* keys use the full dotted form esbuild
// supports natively. Vite-port projects' references like
//
//	const api = import.meta.env.VITE_GATEWAY_API
//
// match these keys at the source level + get substituted
// inline.
func buildDefines(opts Options) map[string]string {
	d := map[string]string{
		"__VUE_PROD_DEVTOOLS__":                   "false",
		"__VUE_OPTIONS_API__":                     "true",
		"__VUE_PROD_HYDRATION_MISMATCH_DETAILS__": "false",
	}
	if opts.Mode != "" {
		d["import.meta.env.MODE"] = jsonString(opts.Mode)
		d["import.meta.env.DEV"] = boolLit(opts.Mode == "development")
		d["import.meta.env.PROD"] = boolLit(opts.Mode == "production")
		// Vite also exposes a BASE_URL convention — derived
		// from the same PublicPath the file loader uses so
		// runtime URL building stays consistent with what the
		// bundler emits at compile time.
		d["import.meta.env.BASE_URL"] = jsonString(defaultPublicPath(opts.PublicPath))
	}
	for k, v := range opts.Env {
		d["import.meta.env."+k] = jsonString(v)
	}
	return d
}

// jsonString quotes s as a valid JS string literal. We can't
// use the stdlib's strconv.Quote because that uses Go-flavored
// escapes; we need JSON-compatible escapes (which the JS parser
// also accepts). encoding/json.Marshal of a string produces
// exactly this — a double-quoted, JSON-escaped string.
func jsonString(s string) string {
	// Minimal manual escaping covers the common cases without
	// pulling encoding/json into the import set just for this.
	// Vite env values are typically URLs, identifiers, simple
	// strings — no embedded backslashes / control chars.
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 {
				// Fall back to \uXXXX for control chars.
				fmt.Fprintf(&b, `\u%04x`, c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// boolLit produces a JS boolean literal — "true" or "false".
func boolLit(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// defaultPublicPath returns the operator's PublicPath when set
// or "/" otherwise. Centralized so the file-loader prefix and
// the import.meta.env.BASE_URL substitution stay in sync — an
// operator can't accidentally have assets emitted at /admin/ but
// runtime BASE_URL reading "/" (or vice versa).
//
// We don't validate the trailing slash; operators who omit it
// will see 404s in the browser and the error path is
// self-explanatory. Forcing the slash silently could mask
// genuine "I meant `admin` not `/admin/`" typos.
func defaultPublicPath(p string) string {
	if p == "" {
		return "/"
	}
	return p
}
