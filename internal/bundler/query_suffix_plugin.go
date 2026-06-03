package bundler

import (
	"encoding/base64"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// Query-suffix import support — Vite-style import attribute
// query strings that change how a file is loaded:
//
//	import shader from './shader.glsl?raw'      // → "string contents"
//	import url    from './image.png?url'        // → "/image-HASH.png"
//	import dataURL from './icon.svg?inline'     // → "data:image/svg+xml;base64,..."
//
// Without this plugin, esbuild treats the query as part of the
// filename ("shader.glsl?raw") and 404s on the filesystem
// lookup. With it, the query gets stripped + the file gets
// loaded in the mode the suffix requested.
//
// The three suffixes mirror Vite's convention. ?worker is NOT
// supported here — that needs separate Web Worker plumbing.

// Custom namespaces tagging the load mode. OnResolve assigns
// these so OnLoad knows which transform to apply without
// re-parsing the suffix.
const (
	nsQueryRaw    = "nexus-query-raw"
	nsQueryURL    = "nexus-query-url"
	nsQueryInline = "nexus-query-inline"
)

// NewQuerySuffixPlugin returns an esbuild plugin handling
// Vite-style query-suffix imports. Register it BEFORE the
// resolver plugin so its OnResolve fires first for matching
// paths.
//
// Importing the same file with different suffixes is allowed
// and treated as distinct modules — esbuild's module cache
// keys on (path, namespace) so `./img.png?raw` and
// `./img.png?url` coexist cleanly.
func NewQuerySuffixPlugin() api.Plugin {
	return api.Plugin{
		Name: "nexus-query-suffix",
		Setup: func(build api.PluginBuild) {
			// OnResolve: catch any import ending in one of
			// the recognized suffixes. We don't filter by
			// extension — the suffix decides, the extension
			// just affects ?url's loader choice.
			build.OnResolve(api.OnResolveOptions{
				Filter: `\?(raw|url|inline)$`,
			}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
				return resolveQuerySuffix(args)
			})
			// OnLoad for each mode in its own namespace.
			build.OnLoad(api.OnLoadOptions{
				Filter:    `.*`,
				Namespace: nsQueryRaw,
			}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
				return loadAsRaw(args.Path)
			})
			build.OnLoad(api.OnLoadOptions{
				Filter:    `.*`,
				Namespace: nsQueryURL,
			}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
				return loadAsURL(args.Path)
			})
			build.OnLoad(api.OnLoadOptions{
				Filter:    `.*`,
				Namespace: nsQueryInline,
			}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
				return loadAsInline(args.Path)
			})
		},
	}
}

// resolveQuerySuffix strips the ?<mode> suffix, resolves the
// bare path against args.ResolveDir, and returns a result in
// the matching namespace so the right OnLoad fires.
//
// Absolute paths in args.Path pass through unchanged after the
// strip. Relative paths get joined with ResolveDir. We don't
// do package resolution here — the operator wants a specific
// file, not an npm package; sending these through the resolver
// would be confusing (and esm.sh wouldn't understand the
// suffix anyway).
func resolveQuerySuffix(args api.OnResolveArgs) (api.OnResolveResult, error) {
	rawPath, mode := splitQuerySuffix(args.Path)
	if mode == "" {
		// Not actually a recognized suffix (filter false-positive
		// via regex anchoring nuance). Pass through to other
		// plugins.
		return api.OnResolveResult{}, nil
	}
	absPath := rawPath
	if !filepath.IsAbs(absPath) {
		// Relative paths resolve against the importing file's
		// directory. For path-less imports (bare specs with
		// suffix) we'd need package resolution — refuse those
		// rather than guessing.
		if !strings.HasPrefix(rawPath, "./") && !strings.HasPrefix(rawPath, "../") {
			return api.OnResolveResult{
				Errors: []api.Message{{
					Text: fmt.Sprintf("nexus-query: ?%s suffix requires a relative path (./%s or ../%s), got %q",
						mode, rawPath, rawPath, args.Path),
				}},
			}, nil
		}
		absPath = filepath.Join(args.ResolveDir, rawPath)
	}
	ns := ""
	switch mode {
	case "raw":
		ns = nsQueryRaw
	case "url":
		ns = nsQueryURL
	case "inline":
		ns = nsQueryInline
	}
	return api.OnResolveResult{
		Path:      absPath,
		Namespace: ns,
	}, nil
}

// splitQuerySuffix breaks a path with a query suffix into the
// bare path and the recognized mode. Returns ("", "") when the
// path has no recognized suffix.
//
//	splitQuerySuffix("./foo.glsl?raw") → ("./foo.glsl", "raw")
//	splitQuerySuffix("./foo.glsl")     → ("", "")
func splitQuerySuffix(p string) (string, string) {
	q := strings.LastIndexByte(p, '?')
	if q < 0 {
		return "", ""
	}
	mode := p[q+1:]
	switch mode {
	case "raw", "url", "inline":
		return p[:q], mode
	}
	return "", ""
}

// loadAsRaw reads the file as text + returns a JS module whose
// default export is the string contents. Useful for shaders,
// SQL, markdown content, anything you want INLINE in the JS
// bundle as a string literal.
//
// We use JS-string-quoted output (via jsonString) so embedded
// quotes/backslashes/newlines round-trip safely. Large files
// inflate the bundle — operators reading 10MB of CSV via ?raw
// should think about ?url + fetch instead. We don't enforce
// a size limit; operator's intent.
func loadAsRaw(path string) (api.OnLoadResult, error) {
	body, err := os.ReadFile(path) // #nosec G304 -- operator-supplied import path
	if err != nil {
		return api.OnLoadResult{
			Errors: []api.Message{{
				Text: fmt.Sprintf("nexus-query: ?raw read %s: %v", path, err),
			}},
		}, nil
	}
	js := "export default " + jsonString(string(body))
	return api.OnLoadResult{
		Contents: &js,
		Loader:   api.LoaderJS,
	}, nil
}

// loadAsURL emits the file via esbuild's file loader by
// returning it in the default namespace + setting Loader to
// File. The default export becomes the public URL string
// (subject to PublicPath prefix).
//
// Equivalent to `import x from "./img.png"` for files the
// asset-loader map already covers; the value of ?url is
// suffix-driven explicit intent + works for ANY extension
// (including .ts/.js where the default loader would compile).
func loadAsURL(path string) (api.OnLoadResult, error) {
	body, err := os.ReadFile(path) // #nosec G304 -- operator-supplied import path
	if err != nil {
		return api.OnLoadResult{
			Errors: []api.Message{{
				Text: fmt.Sprintf("nexus-query: ?url read %s: %v", path, err),
			}},
		}, nil
	}
	s := string(body)
	return api.OnLoadResult{
		Contents: &s,
		Loader:   api.LoaderFile,
		// ResolveDir is the file's directory — irrelevant for
		// file loader (no nested imports), but kept for
		// completeness.
		ResolveDir: filepath.Dir(path),
	}, nil
}

// loadAsInline reads the file as bytes, base64-encodes them,
// and exports a `data:<mime>;base64,<encoded>` URL as the
// default. Useful for small images you want inlined in the
// bundle to avoid an extra HTTP round-trip.
//
// MIME type is guessed from the file extension via Go's stdlib
// mime package; unknowns fall back to application/octet-stream.
// Large files inflate the bundle ~33% from base64 overhead;
// same operator-trust principle as ?raw.
func loadAsInline(path string) (api.OnLoadResult, error) {
	body, err := os.ReadFile(path) // #nosec G304 -- operator-supplied import path
	if err != nil {
		return api.OnLoadResult{
			Errors: []api.Message{{
				Text: fmt.Sprintf("nexus-query: ?inline read %s: %v", path, err),
			}},
		}, nil
	}
	mimeType := mime.TypeByExtension(filepath.Ext(path))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	encoded := base64.StdEncoding.EncodeToString(body)
	dataURL := "data:" + mimeType + ";base64," + encoded
	js := "export default " + jsonString(dataURL)
	return api.OnLoadResult{
		Contents: &js,
		Loader:   api.LoaderJS,
	}, nil
}
