package viteless

import (
	"os"
	"path/filepath"
)

// This file defines the high-level configuration surface shared by the
// batteries-included entry points Dev() (dev.go) and Build() (build.go).
// The low-level Host/Server API in server.go stays available for callers
// who want to assemble their own toolchain.

// Framework selects which transform path a project's source flows
// through. Auto inspects the source tree (a .vue file → Vue; a .tsx/.jsx
// entry → React; otherwise Vanilla) so most callers never set it.
type Framework int

const (
	// FrameworkAuto detects the framework from the source tree.
	FrameworkAuto Framework = iota
	// FrameworkVue compiles .vue SFCs via the embedded QuickJS
	// @vue/compiler-sfc and injects the Vue HMR runtime.
	FrameworkVue
	// FrameworkReact targets the React automatic JSX runtime — .jsx/
	// .tsx need no explicit `import React`. (react-refresh HMR is not
	// wired yet; React edits trigger a full reload.)
	FrameworkReact
	// FrameworkVanilla is plain TS/JS with esbuild — no framework
	// transform beyond type stripping and bundling.
	FrameworkVanilla
)

// DepMode chooses where third-party packages are sourced from.
type DepMode int

const (
	// DepAuto (the default) detects how to source dependencies: if the
	// project has a node_modules directory (the user ran `npm install`),
	// it behaves as DepNodeModules; otherwise DepCDN. This makes viteless
	// drop-in for both npm-managed projects and zero-install ones.
	DepAuto DepMode = iota
	// DepCDN resolves bare imports from the esm.sh registry, pins them
	// in the lockfile, and caches blobs in the content-addressed store.
	// No node_modules and no npm required.
	DepCDN
	// DepNodeModules resolves bare imports from a local node_modules
	// directory (offline / private packages). Reintroduces an npm install
	// step but no Node runtime.
	DepNodeModules
)

// resolveDepMode collapses DepAuto into a concrete mode for root: it picks
// DepNodeModules when <root>/node_modules exists, else DepCDN.
func resolveDepMode(m DepMode, root string) DepMode {
	if m != DepAuto {
		return m
	}
	if fi, err := os.Stat(filepath.Join(root, "node_modules")); err == nil && fi.IsDir() {
		return DepNodeModules
	}
	return DepCDN
}

// TailwindMode controls the Tailwind CSS pass.
type TailwindMode int

const (
	// TailwindAuto enables Tailwind when a standalone `tailwindcss`
	// binary is on PATH and a CSS file uses Tailwind directives.
	TailwindAuto TailwindMode = iota
	// TailwindOn forces the Tailwind pass (errors if the binary is
	// missing).
	TailwindOn
	// TailwindOff disables the Tailwind pass entirely.
	TailwindOff
)

// DevConfig configures Dev() — the unbundled, HMR dev server. Only Root
// is required; everything else has a sensible default.
type DevConfig struct {
	Root          string            // dir holding index.html + the source tree (required)
	IndexHTML     string            // explicit shell path when it lives outside Root
	Addr          string            // listen address; default "127.0.0.1:5173"
	ProxyTarget   string            // backend origin for unmatched requests, e.g. "http://127.0.0.1:8080"
	ProxyResolver func() string     // lazy backend origin (overrides ProxyTarget when set)
	Aliases       []Alias           // tsconfig "paths"; auto-parsed from Root/tsconfig.json when nil
	Env           map[string]string // import.meta.env.VITE_* substitutions
	Mode          string            // "development" (default)
	Framework     Framework         // FrameworkAuto by default
	DepMode       DepMode           // DepCDN by default
	NodeModules   string            // node_modules dir when DepMode==DepNodeModules; default Root/node_modules
	Tailwind      TailwindMode      // TailwindAuto by default
	CacheRoot     string            // content-addressed store root; default store.DefaultRoot()
	Registry      string            // esm.sh override; default $NEXUS_REGISTRY or fetcher.DefaultRegistry
	Prebundle     bool              // dependency pre-bundling; default true
	LockfilePath  string            // lockfile path; default Root/nexus.lock
	Plugins       []Plugin          // user/config/built-in plugins (run in dev + build)
	Logf          func(string, ...any)
}

// BuildConfig configures Build() — the production bundle written to a
// dist directory. Only Root is required.
type BuildConfig struct {
	Root         string            // frontend root (required)
	SrcDir       string            // entry search root; default auto (Root, then Root/{src,app,client})
	Entries      []string          // explicit entry files; default auto (one per top-level .ts/.tsx/.jsx/.js)
	OutDir       string            // output dir; default Root/dist
	Framework    Framework         // FrameworkAuto by default
	DepMode      DepMode           // DepCDN by default
	NodeModules  string            // node_modules dir when DepMode==DepNodeModules
	Tailwind     TailwindMode      // TailwindAuto by default
	Aliases      []Alias           // tsconfig "paths"; or set TSConfig
	TSConfig     string            // tsconfig/jsconfig path esbuild reads paths from
	Env          map[string]string // import.meta.env.VITE_* substitutions
	Mode         string            // "production" (default)
	PublicPath   string            // asset URL prefix; default "/"
	Minify       *bool             // default true in production
	Sourcemap    *bool             // default true
	Splitting    *bool             // default true
	CacheRoot    string            // store root; default store.DefaultRoot()
	Registry     string            // esm.sh override
	LockfilePath string            // lockfile path; default Root/nexus.lock
	Plugins      []Plugin          // user/config/built-in plugins (run in dev + build)
	Logf         func(string, ...any)
}

// BuildResult reports the outcome of Build(). Errors carries esbuild's
// per-file diagnostics — a non-empty Errors means the bundle is not
// usable even though Build() returned a nil Go error (Go errors are
// reserved for invalid configuration).
type BuildResult struct {
	OutDir      string
	OutputFiles []string
	Errors      []string
	Warnings    []string
}
