package bundler

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/evanw/esbuild/pkg/api"
)

// Web Worker plugin support.
//
// Vite-style worker imports:
//
//	import MyWorker from './heavy.ts?worker'
//	const w = new MyWorker()
//	w.postMessage('start')
//
// The `?worker` suffix tells the bundler to:
//  1. Run a SUB-BUILD that bundles `heavy.ts` (and everything
//     it imports) as a standalone module-format file.
//  2. Write the sub-build's output into the parent's OutDir
//     with a content-hashed filename (worker-<hash>.js).
//  3. Generate a JS module exporting a Worker subclass whose
//     constructor points at the bundled worker URL.
//
// At runtime the operator gets:
//
//	new MyWorker() === new Worker("/worker-A1B2C3.js", { type: "module" })
//
// Sub-build inherits the parent's plugins (resolver, sass,
// tailwind, glob, etc.) so workers can use the same imports
// the main bundle does — bare specs from the lockfile, SCSS
// (rare but possible in inline workers), etc. The worker
// plugin itself is excluded from the sub-build's plugin list
// to prevent infinite recursion if a worker imports another
// `?worker`.
//
// Limitations:
//   - `?worker&inline` (blob URL) isn't supported in v1.
//     Operators wanting inlined workers should use the URL
//     constructor pattern directly.
//   - `?sharedworker` isn't supported. SharedWorker has lower
//     uptake; revisit when there's demand.
//   - Workers can't recursively import other ?worker files.

// nsWorker is the OnLoad namespace the worker plugin tags
// resolved entries with so its loader runs.
const nsWorker = "nexus-worker"

// WorkerPluginOptions configures NewWorkerPlugin. OutDir +
// PublicPath are required; NestedPlugins should be the same
// plugin slice the parent bundler uses MINUS the worker plugin
// itself (to avoid recursion).
type WorkerPluginOptions struct {
	// OutDir is the parent build's output directory. Worker
	// bundles get written here alongside the main JS.
	OutDir string

	// PublicPath is the URL prefix the runtime fetches worker
	// bundles from. Matches the parent build's PublicPath so
	// `new Worker("/foo/worker-HASH.js")` works under any
	// deployment path.
	PublicPath string

	// NestedPlugins is the slice of plugins the sub-build
	// uses to resolve worker imports. Should NOT include the
	// worker plugin itself.
	NestedPlugins []api.Plugin
}

// NewWorkerPlugin returns an esbuild plugin handling
// `?worker` suffix imports. Register it AFTER all other
// plugins so the sub-build can inherit them via NestedPlugins.
func NewWorkerPlugin(opts WorkerPluginOptions) api.Plugin {
	// Per-plugin instance cache: workers that resolve to the
	// same source path emit ONE bundle, not N. Without this a
	// worker imported from 5 different places would bundle 5
	// times into 5 distinct hashed files.
	var (
		cacheMu sync.Mutex
		cache   = map[string]string{} // src abs path → emitted URL
	)
	return api.Plugin{
		Name: "nexus-worker",
		Setup: func(build api.PluginBuild) {
			build.OnResolve(api.OnResolveOptions{
				Filter: `\?worker$`,
			}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
				bare := strings.TrimSuffix(args.Path, "?worker")
				if !strings.HasPrefix(bare, "./") && !strings.HasPrefix(bare, "../") {
					return api.OnResolveResult{
						Errors: []api.Message{{
							Text: fmt.Sprintf("nexus-worker: ?worker suffix requires a relative path, got %q", args.Path),
						}},
					}, nil
				}
				return api.OnResolveResult{
					Path:      filepath.Join(args.ResolveDir, bare),
					Namespace: nsWorker,
				}, nil
			})
			build.OnLoad(api.OnLoadOptions{
				Filter:    `.*`,
				Namespace: nsWorker,
			}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
				cacheMu.Lock()
				cached, ok := cache[args.Path]
				cacheMu.Unlock()
				if ok {
					return workerWrapperJS(cached), nil
				}
				url, err := bundleWorker(args.Path, opts)
				if err != nil {
					return api.OnLoadResult{
						Errors: []api.Message{{Text: err.Error()}},
					}, nil
				}
				cacheMu.Lock()
				cache[args.Path] = url
				cacheMu.Unlock()
				return workerWrapperJS(url), nil
			})
		},
	}
}

// bundleWorker runs esbuild on srcPath as a standalone entry,
// writes the result into opts.OutDir with a content-hashed
// filename, and returns the public URL (PublicPath + filename)
// the runtime fetches it from.
//
// The sub-build inherits opts.NestedPlugins so the worker can
// use the same imports as the main bundle (resolver, sass,
// etc.). Minification + sourcemaps follow the parent's
// conventions implicitly — sub-build outputs separately and
// the worker's own .map gets emitted alongside the .js.
func bundleWorker(srcPath string, opts WorkerPluginOptions) (string, error) {
	result := api.Build(api.BuildOptions{
		EntryPoints: []string{srcPath},
		Bundle:      true,
		Write:       false,
		Format:      api.FormatESModule,
		Target:      api.ES2022,
		Plugins:     opts.NestedPlugins,
		// Inline source maps — Linked maps need an output path
		// for the sidecar .map, which Write=false sub-builds
		// don't have. Inline keeps debugging working at the
		// cost of larger worker bytes; acceptable for dev +
		// not a concern in prod minify mode.
		Sourcemap: api.SourceMapInline,
		LogLevel:  api.LogLevelSilent,
	})
	if len(result.Errors) > 0 {
		return "", fmt.Errorf("nexus-worker: bundle %s: %s", srcPath, result.Errors[0].Text)
	}
	if len(result.OutputFiles) == 0 {
		return "", fmt.Errorf("nexus-worker: bundle %s produced no output", srcPath)
	}
	// Pick the JS output. With Write=false + no Outdir, esbuild
	// names OutputFiles with internal paths that don't always
	// end in `.js` — but the JS output is the file that is NOT
	// the sourcemap. Inline sourcemaps are baked in so there's
	// only one OutputFile per entry here.
	var jsBytes []byte
	for _, f := range result.OutputFiles {
		if !strings.HasSuffix(f.Path, ".map") {
			jsBytes = f.Contents
			break
		}
	}
	if jsBytes == nil {
		return "", fmt.Errorf("nexus-worker: bundle %s produced no JS output", srcPath)
	}
	// Content hash for repeatable filenames + cache busting.
	sum := sha256.Sum256(jsBytes)
	hash := hex.EncodeToString(sum[:])[:8]
	base := workerOutputBaseName(srcPath, hash)
	dst := filepath.Join(opts.OutDir, base)
	if err := os.WriteFile(dst, jsBytes, 0o644); err != nil {
		return "", fmt.Errorf("nexus-worker: write %s: %w", dst, err)
	}
	publicPath := opts.PublicPath
	if publicPath == "" {
		publicPath = "/"
	}
	return publicPath + base, nil
}

// workerOutputBaseName generates a hashed filename for the
// emitted worker bundle. Uses the source basename (sans
// extension) as a prefix so operators can tell which file the
// emitted bundle came from when browsing outDir.
//
//	heavy.ts + a1b2c3d4  →  heavy.worker-a1b2c3d4.js
func workerOutputBaseName(srcPath, hash string) string {
	base := filepath.Base(srcPath)
	if dot := strings.LastIndexByte(base, '.'); dot > 0 {
		base = base[:dot]
	}
	return fmt.Sprintf("%s.worker-%s.js", base, hash)
}

// workerWrapperJS produces the OnLoadResult containing the JS
// module that the parent bundle imports. The default export is
// a Worker subclass whose constructor passes the bundled
// worker's URL to super().
//
// We use a subclass (not just `() => new Worker(url)`) so the
// operator can do `class MyWorker extends BaseWorker {}` if
// they want, and `instanceof Worker` works as expected.
//
// `type: "module"` is the modern default — workers can use
// `import` statements. Most Vite-port projects assume this.
// Operators wanting classic (script-mode) workers can pass
// `{ type: "classic" }` as constructor opts.
func workerWrapperJS(url string) api.OnLoadResult {
	js := fmt.Sprintf(`export default class extends Worker {
	constructor(opts) {
		super(%q, { type: "module", ...opts });
	}
}`, url)
	return api.OnLoadResult{
		Contents: &js,
		Loader:   api.LoaderJS,
	}
}
