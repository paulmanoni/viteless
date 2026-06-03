package bundler

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/evanw/esbuild/pkg/api"
)

// NewImportMetaGlobPlugin returns an esbuild plugin that
// rewrites `import.meta.glob(...)` calls in JS/TS source files
// into object literals mapping each matched path to a dynamic
// import. Mirrors Vite's compile-time glob behavior, used
// heavily by Vue Router for page auto-discovery + component
// registration patterns:
//
//	// router/index.ts
//	const pages = import.meta.glob('./pages/**/*.vue')
//	const routes = Object.entries(pages).map(([path, loader]) => ({
//	    path: pathToRoute(path),
//	    component: loader,
//	}))
//
// Architecture: OnLoad intercepts user JS/TS files (the `file`
// namespace — registry-served blobs are skipped since they
// won't contain glob calls). For each file the rewriter scans
// for `import.meta.glob` invocations (token-aware: skips
// strings + comments), expands the glob against the file's
// directory, and rewrites the call to the inline object
// literal.
//
// Files WITHOUT glob calls pass through with zero overhead —
// rewriteGlobCalls returns the input unchanged + a zero count.
//
// Limitations operators should know about:
//   - Patterns must be string LITERALS, not variables
//   - Object alternation `{a,b}` in patterns isn't supported
//   - eager: true isn't supported (surfaces a clear error)
//
// .vue files are NOT intercepted here — the vue SFC plugin
// returns compiled JS in its own OnLoad, and esbuild doesn't
// re-run OnLoad on the result. To support import.meta.glob
// inside <script setup> blocks, the vue plugin would need to
// call rewriteGlobCalls on its compiled output. Future work.
func NewImportMetaGlobPlugin() api.Plugin {
	return api.Plugin{
		Name: "nexus-import-meta-glob",
		Setup: func(build api.PluginBuild) {
			build.OnLoad(api.OnLoadOptions{
				// JS, TS, JSX, TSX, MJS, CJS — every shape
				// user code commonly takes. Vue files are
				// handled by the vue plugin (which we don't
				// post-process today).
				Filter:    `\.(jsx?|tsx?|mjs|cjs)$`,
				Namespace: "file",
			}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
				body, err := os.ReadFile(args.Path) // #nosec G304 -- esbuild-supplied source path
				if err != nil {
					return api.OnLoadResult{}, fmt.Errorf("nexus-glob: read %s: %w", args.Path, err)
				}
				rewritten, n, err := rewriteGlobCalls(string(body), filepath.Dir(args.Path))
				if err != nil {
					return api.OnLoadResult{
						Errors: []api.Message{{
							Text: fmt.Sprintf("nexus-glob: rewrite %s: %v", args.Path, err),
						}},
					}, nil
				}
				if n == 0 {
					// No globs in this file — pass through
					// to esbuild's native handler. Returning
					// zero result tells esbuild "I didn't
					// claim this; ask other plugins or use
					// your default."
					return api.OnLoadResult{}, nil
				}
				loader := loaderForPath(args.Path)
				return api.OnLoadResult{
					Contents:   &rewritten,
					Loader:     loader,
					ResolveDir: filepath.Dir(args.Path),
				}, nil
			})
		},
	}
}

// loaderForPath picks the esbuild Loader corresponding to the
// file's extension. Required because once we return Contents
// in OnLoad, esbuild can no longer infer from the path alone.
func loaderForPath(path string) api.Loader {
	switch filepath.Ext(path) {
	case ".ts":
		return api.LoaderTS
	case ".tsx":
		return api.LoaderTSX
	case ".jsx":
		return api.LoaderJSX
	case ".cjs", ".mjs", ".js":
		return api.LoaderJS
	default:
		return api.LoaderJS
	}
}
