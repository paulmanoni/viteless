package viteless

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// pluginBridge adapts the plugin container to an esbuild plugin so the same
// resolveId / load / transform hooks that run in the dev server also run in
// the production build. It is registered AFTER the built-in esbuild plugins
// (resolver, vue, tailwind, glob, query) so those keep first crack, and is
// a no-op for hook kinds no plugin implements (non-plugin builds are
// byte-unaffected).
func pluginBridge(c *pluginContainer) api.Plugin {
	const ns = "viteless-plugin"
	var hasResolve, hasLoad, hasTransform bool
	for _, p := range c.plugins {
		if _, ok := p.(ResolveIdPlugin); ok {
			hasResolve = true
		}
		if _, ok := p.(LoadPlugin); ok {
			hasLoad = true
		}
		if _, ok := p.(TransformPlugin); ok {
			hasTransform = true
		}
	}
	return api.Plugin{
		Name: "viteless-plugins",
		Setup: func(b api.PluginBuild) {
			if hasResolve || hasLoad {
				b.OnResolve(api.OnResolveOptions{Filter: `.*`}, func(a api.OnResolveArgs) (api.OnResolveResult, error) {
					if id, ok := c.resolveId(a.Path, a.Importer); ok {
						return api.OnResolveResult{Path: id, Namespace: ns}, nil
					}
					return api.OnResolveResult{}, nil
				})
				b.OnLoad(api.OnLoadOptions{Filter: `.*`, Namespace: ns}, func(a api.OnLoadArgs) (api.OnLoadResult, error) {
					code, ok, err := c.load(a.Path)
					if err != nil {
						return api.OnLoadResult{}, err
					}
					if !ok {
						return api.OnLoadResult{}, nil
					}
					if hasTransform {
						if out, terr := c.transform(code, a.Path); terr == nil {
							code = out
						} else {
							return api.OnLoadResult{}, terr
						}
					}
					loader := loaderForID(a.Path)
					return api.OnLoadResult{Contents: &code, Loader: loader}, nil
				})
			}
			if hasTransform {
				// Catch-all transform for on-disk code modules. Registered
				// last, so a built-in plugin (vue/tailwind/glob) that already
				// claimed the file wins; otherwise we read it, run the
				// transform chain, and hand esbuild the result with the
				// matching loader.
				b.OnLoad(api.OnLoadOptions{Filter: `\.(tsx?|jsx?|mjs|cjs|css)$`, Namespace: "file"}, func(a api.OnLoadArgs) (api.OnLoadResult, error) {
					body, err := os.ReadFile(a.Path)
					if err != nil {
						return api.OnLoadResult{}, err
					}
					out, terr := c.transform(string(body), a.Path)
					if terr != nil {
						return api.OnLoadResult{}, terr
					}
					loader := loaderForID(a.Path)
					return api.OnLoadResult{Contents: &out, Loader: loader, ResolveDir: filepath.Dir(a.Path)}, nil
				})
			}
		},
	}
}

// loaderForID maps a module id's extension to the esbuild loader.
func loaderForID(id string) api.Loader {
	if i := strings.IndexAny(id, "?#"); i >= 0 {
		id = id[:i]
	}
	switch strings.ToLower(filepath.Ext(id)) {
	case ".ts":
		return api.LoaderTS
	case ".tsx":
		return api.LoaderTSX
	case ".jsx":
		return api.LoaderJSX
	case ".css":
		return api.LoaderCSS
	case ".json":
		return api.LoaderJSON
	default:
		return api.LoaderJS
	}
}
