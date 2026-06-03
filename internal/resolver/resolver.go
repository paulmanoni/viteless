// Package resolver implements the esbuild plugin that intercepts
// bare imports in user code and resolves them against the project's
// nexus.lock — pointing esbuild at the corresponding cached blob in
// the disk store rather than letting it walk a non-existent
// node_modules tree.
//
// This is the seam that makes "import 'vue'" work without npm.
// esbuild walks the import graph; whenever it sees a bare specifier
// (anything that doesn't start with ./ ../ / or a known scheme), it
// asks the registered plugins via OnResolve. Our plugin:
//
//  1. Looks up "vue" in the lockfile — finds vue@3.4.21.
//  2. Reads the cached blob path from the store using the entry's
//     Resolved URL.
//  3. Returns that path; esbuild reads the file as if it were any
//     other module.
//
// Sub-path imports ("vue/dist/vue.esm.js") are handled by tracking
// the parent package's Resolved URL and joining the sub-path onto
// it, then looking THAT URL up in the lockfile (or store). v0.1
// requires sub-paths to be pre-fetched by the fetcher; runtime
// fetching during a build is intentionally not supported (would
// turn `nexus build` into a network operation).
//
// Relative imports (./foo) and unknown bare specs fall through to
// esbuild's default resolver, which lets it handle the user's own
// source tree the way it always has.
package resolver

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/evanw/esbuild/pkg/api"

	"github.com/paulmanoni/viteless/internal/lockfile"
	"github.com/paulmanoni/viteless/internal/store"
)

// Namespace is the esbuild namespace this plugin tags resolved
// entries with. Distinguishes our cached blobs from regular
// on-disk files in subsequent OnLoad events. v0.1 doesn't register
// any OnLoad — esbuild's default Loader inference from file
// extension is good enough for ESM blobs from esm.sh — but the
// namespace is reserved so future per-content-type handling (CSS
// injection, source map fix-up) has a stable hook to anchor on.
const Namespace = "nexus-deps"

// Options configures the plugin. Lockfile + Store are required;
// FetchOnDemand is optional but recommended.
type Options struct {
	Lockfile *lockfile.File
	Store    *store.Store

	// FetchOnDemand, when set, is invoked when a sub-path import
	// (e.g. `@apollo/client/core`) resolves to a URL that isn't
	// yet in the cache. The function is expected to fetch the
	// URL — typically via a fetcher.Fetcher — and populate the
	// store + (optionally) the lockfile. Returns the
	// POST-REDIRECT URL the fetcher actually stored under (esm.sh
	// 301-redirects e.g. `.css.mjs` → `.css`, so the cache key
	// differs from the asked-for URL); the resolver retries the
	// store lookup at this returned URL.
	//
	// Why this exists: `nexus install` walks the import graph
	// starting from package.json deps, which fetches package
	// ROOTS plus whatever esm.sh links to from those roots
	// (encoded /X-Z.../core.mjs URLs). When user code later
	// imports `@apollo/client/core` directly, that URL shape
	// (the clean sub-path form) isn't in the cache — esm.sh
	// serves it but we hadn't asked for it at install time.
	//
	// Without this hook the user sees a confusing "no cached
	// blob exists — run `nexus install`" error even though
	// they JUST ran nexus install. With it, sub-path imports
	// transparently fetch on first build and the lockfile
	// records them for repeatability.
	//
	// nil disables on-demand fetching — sub-path misses surface
	// the v0.1 error message unchanged. Useful for offline-only
	// builds where any cache miss should fail loud.
	FetchOnDemand func(url string) (resolvedURL string, err error)

	// DevSpecRewrite, when set, is consulted for every BARE package
	// spec in user code BEFORE the lockfile. If it returns a non-empty
	// URL, that URL is fetched on-demand (FetchOnDemand) and used
	// instead of the lockfile entry; the dev module's transitive
	// imports then resolve through the normal registry-internal path.
	//
	// `nexus dev` uses this to swap `vue` → its `.development.mjs`
	// esm.sh build so the Vue HMR runtime (__VUE_HMR_RUNTIME__, absent
	// from the production build) is present. Generic on purpose — the
	// resolver stays framework-agnostic; the Vue mapping lives in the
	// CLI.
	//
	// nil (production builds) → no rewriting; the lockfile's pinned
	// (production) URL is used unchanged. Requires FetchOnDemand to be
	// set; without it the rewrite is skipped and the lockfile wins.
	DevSpecRewrite func(spec, subpath string) string
}

// New returns an esbuild plugin that wires OnResolve for every
// import esbuild encounters. Pass into Bundler.AddPlugin before
// any other plugin that also handles OnResolve.
func New(opts Options) (api.Plugin, error) {
	if opts.Lockfile == nil {
		return api.Plugin{}, errors.New("resolver: Lockfile is nil")
	}
	if opts.Store == nil {
		return api.Plugin{}, errors.New("resolver: Store is nil")
	}
	return api.Plugin{
		Name: "nexus-deps-resolver",
		Setup: func(build api.PluginBuild) {
			// OnResolve fires for every import esbuild
			// encounters. We narrow with a permissive filter
			// and decide internally whether to claim or pass
			// through — gives us one place to reason about
			// precedence.
			build.OnResolve(api.OnResolveOptions{Filter: ".*"}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
				return resolveOne(opts, args)
			})
			// OnLoad fires for every path we tagged with our
			// Namespace in OnResolve. esbuild can't infer the
			// loader from the path (cached blobs have no
			// extension — they're named after the content
			// hash), so we read the file ourselves and tell
			// esbuild what kind of content it is via the
			// Loader field.
			build.OnLoad(api.OnLoadOptions{
				Filter:    ".*",
				Namespace: Namespace,
			}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
				return loadOne(opts, args)
			})
		},
	}, nil
}

// resolveOne is the core of the plugin. Implementation factored
// out as a non-method function so the tests can drive it directly
// without standing up an esbuild build context.
//
// Return semantics, per esbuild's plugin docs:
//
//   - {Path: "..."} → we claim this import; esbuild reads that
//     path
//   - {} (zero) → pass through to esbuild's default resolver
//   - {Errors: [...]} → fail the build with our diagnostic
//
// Decision tree:
//
//  1. Importer is one of our cached blobs (args.Namespace ==
//     Namespace OR args.Importer matches a known cache path) →
//     all imports are CDN-internal; resolve against the importer's
//     URL and look up in the store. This is the "registry-served
//     ESM transitively references siblings" case.
//  2. Importer is user code → relative/absolute paths fall
//     through to esbuild's default; bare specs go through the
//     lockfile.
func resolveOne(opts Options, args api.OnResolveArgs) (api.OnResolveResult, error) {
	p := args.Path


	// --- registry-internal resolution path ---------------------------
	// When the importer file is in our namespace, args.Importer
	// IS the registry URL we returned at the previous OnResolve
	// (the URL-as-Path scheme means we don't need a blob-path
	// reverse index — esbuild threads the path verbatim into
	// child resolve calls).
	//
	// Every relative / absolute-path / absolute-URL import inside
	// such a file refers to a sibling in the registry. Resolve
	// against the importer URL and look up the result in the store.
	if args.Namespace == Namespace && args.Importer != "" {
		// Two-attempt lookup: esm.sh's URL conventions mix
		// query-bearing and query-free sibling URLs. A stub at
		// /pkg@1.0.0 might `import "/dep@^2?target=es2022"`,
		// and that dep's body in turn imports a SIBLING that
		// path-encodes its variant (/dep@2.0.0/es2022/x.mjs,
		// no query). resolveRegistryURL inherits the importer's
		// query (right for the Vue compile bootstrap where every
		// fetched URL carries ?target=es2015), but for esm.sh's
		// general conventions the right lookup is query-free.
		// We try inherited first, then strip query and retry.
		// On both misses we surface a clear error.
		absURL, err := resolveRegistryURL(args.Importer, p)
		if err == nil && absURL != "" {
			if _, _, gerr := opts.Store.Get(absURL); gerr == nil {
				return api.OnResolveResult{Path: absURL, Namespace: Namespace}, nil
			}
			if stripped := stripQuery(absURL); stripped != absURL {
				if _, _, gerr := opts.Store.Get(stripped); gerr == nil {
					return api.OnResolveResult{Path: stripped, Namespace: Namespace}, nil
				}
			}
			// On-demand fetch: install-time recursion didn't
			// reach this URL (typically a vuetify lib/util
			// helper, or a transitive utility that esm.sh
			// links only when a specific top-level export
			// from the project's code actually uses it).
			// Pull now + retry — the same hook the sub-path
			// branch below uses. esm.sh routinely
			// 301-redirects internal paths (e.g. .css.mjs →
			// .css), so the cache key the fetcher used may
			// differ from absURL — use the returned
			// post-redirect URL for the retry.
			if opts.FetchOnDemand != nil {
				if canonical, ferr := opts.FetchOnDemand(absURL); ferr == nil {
					key := absURL
					if canonical != "" {
						key = canonical
					}
					if _, _, retryErr := opts.Store.Get(key); retryErr == nil {
						return api.OnResolveResult{Path: key, Namespace: Namespace}, nil
					}
				}
			}
		}
		// Registry-shaped import didn't hit the cache → clear
		// error rather than letting esbuild try a useless
		// filesystem lookup.
		if strings.HasPrefix(p, "/") || strings.HasPrefix(p, ".") || strings.Contains(p, "://") {
			return api.OnResolveResult{
				Errors: []api.Message{{
					Text: fmt.Sprintf("nexus-deps: %s (imported from %s → %s) is not in the cache — run `nexus install`",
						p, args.Importer, absURL),
				}},
			}, nil
		}
		// Bare spec inside a registry-served file — fall through
		// to the lockfile branch below.
	}

	// Relative imports always belong to esbuild's default
	// resolver — user code referencing user code.
	if strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") || p == "." || p == ".." {
		return api.OnResolveResult{}, nil
	}
	// Absolute paths are esbuild's job too (rare in user code,
	// common when esbuild re-enters with a fully-resolved path).
	if strings.HasPrefix(p, "/") {
		return api.OnResolveResult{}, nil
	}
	// Protocol-prefixed paths (http://, data:, etc.) — pass.
	if strings.Contains(p, "://") || strings.HasPrefix(p, "data:") {
		return api.OnResolveResult{}, nil
	}

	// Split off any sub-path: "vue/dist/vue.esm.js" → ("vue",
	// "dist/vue.esm.js"). Scoped packages start with "@" and
	// their first segment is part of the spec ("@vue/runtime-dom"
	// is the spec, "@vue/runtime-dom/foo.js" splits as
	// ("@vue/runtime-dom", "foo.js")).
	spec, subpath := splitSpec(p)

	// Dev-mode spec rewrite (e.g. vue → vue.development.mjs for HMR).
	// Consulted before the lockfile; on any miss we fall through to the
	// normal lockfile path so a bad rewrite never breaks resolution.
	//
	// HOT PATH IS READ-ONLY: the dev URL is pre-warmed into the store by
	// the caller (single-threaded, before esbuild's parallel build), so
	// the common case is a concurrent-safe Store.Get. FetchOnDemand
	// (which writes the lockfile map) is only a cold-miss fallback — it
	// must not run during the parallel build, where it would race the
	// concurrent lf.Resolve reads ("concurrent map iteration and map
	// write"). A warm pre-fetch keeps us off that branch.
	if opts.DevSpecRewrite != nil {
		if devURL := opts.DevSpecRewrite(spec, subpath); devURL != "" {
			if _, _, gerr := opts.Store.Get(devURL); gerr == nil {
				return api.OnResolveResult{Path: devURL, Namespace: Namespace}, nil
			}
			if opts.FetchOnDemand != nil {
				if canonical, ferr := opts.FetchOnDemand(devURL); ferr == nil && canonical != "" {
					if _, _, gerr := opts.Store.Get(canonical); gerr == nil {
						return api.OnResolveResult{Path: canonical, Namespace: Namespace}, nil
					}
				}
			}
		}
	}

	pkg, err := opts.Lockfile.Resolve(spec, "")
	if err != nil {
		if errors.Is(err, lockfile.ErrNotResolved) {
			// Not in our lockfile — but the user might have
			// imported something that's a valid esm.sh path
			// even though their `nexus install` didn't fetch
			// it. Common case: `import "@mdi/font/css/foo.css"`
			// where @mdi/font is stored under the file-path
			// spec but not under the bare package name.
			//
			// Skip on-demand for tsconfig-paths aliases
			// (`@/`, `~/`, etc.) — those are user-defined
			// path prefixes that esbuild resolves against
			// the tsconfig's paths map. Sending them to
			// esm.sh would 400; the right resolver is
			// esbuild's default which we fall through to.
			if opts.FetchOnDemand != nil && looksLikePackageImport(p) {
				// Subpath import of an un-pinned package
				// (`react-dom/client`, `react/jsx-runtime`). Fetching
				// the subpath SPEC directly collapses to the package
				// ROOT, which lacks the subpath's exports (the root
				// react-dom has no `createRoot`). So resolve the
				// package root first, then join the subpath against it
				// and fetch THAT exact module.
				if subpath != "" {
					if root, ferr := opts.FetchOnDemand(spec); ferr == nil && root != "" {
						target := joinSubpath(root, subpath)
						if canon, terr := opts.FetchOnDemand(target); terr == nil && canon != "" {
							if _, _, gerr := opts.Store.Get(canon); gerr == nil {
								return api.OnResolveResult{Path: canon, Namespace: Namespace}, nil
							}
						}
					}
				}
				// Root import, or subpath fallback: fetch the path
				// as-is (covers `@mdi/font/css/foo.css`-style file
				// imports the fetcher stores under the full path).
				if canonical, fetchErr := opts.FetchOnDemand(p); fetchErr == nil && canonical != "" {
					if _, _, retryErr := opts.Store.Get(canonical); retryErr == nil {
						return api.OnResolveResult{
							Path:      canonical,
							Namespace: Namespace,
						}, nil
					}
				}
			}
			// Last resort: let esbuild's default resolver
			// have a go. If the user wrote a typo it'll
			// surface as a normal "could not resolve" from
			// esbuild itself.
			return api.OnResolveResult{}, nil
		}
		var ae *lockfile.AmbiguousError
		if errors.As(err, &ae) {
			return api.OnResolveResult{
				Errors: []api.Message{{Text: ae.Error()}},
			}, nil
		}
		return api.OnResolveResult{}, fmt.Errorf("resolver: lockfile lookup for %q: %w", spec, err)
	}

	// Resolve to the registry URL. For root-package imports the
	// entry's Resolved URL is what we hand to the store. For
	// sub-path imports we join the sub-path against the resolved
	// URL and look THAT up — the fetcher should have recursed
	// into it at `nexus add` time.
	targetURL := pkg.Resolved
	if subpath != "" {
		targetURL = joinSubpath(pkg.Resolved, subpath)
	}
	// Confirm the blob is present before claiming the import —
	// otherwise the build proceeds through OnLoad and surfaces
	// the error there with worse context. We discard the meta
	// here; OnLoad re-fetches it via the same Path (= URL) key.
	if _, _, err := opts.Store.Get(targetURL); err != nil {
		if errors.Is(err, store.ErrNotCached) {
			// Sub-path / file imports are fetched WITHOUT the
			// `?external=` query (fetcher.specToURL's file-path
			// branch strips it — external only affects a
			// package's OWN js imports, which a direct file
			// fetch has none of). But targetURL inherits the
			// root entry's Resolved query via joinSubpath, so
			// when External is set the lookup key carries an
			// `?external=` the stored blob never had. Retry the
			// query-stripped key — it matches how the fetcher
			// actually stored the file blob. Additive: only runs
			// on an otherwise-fatal miss, so it can't change a
			// currently-resolving build.
			if subpath != "" {
				if bare := stripQuery(targetURL); bare != targetURL {
					if _, _, bareErr := opts.Store.Get(bare); bareErr == nil {
						return api.OnResolveResult{
							Path:      bare,
							Namespace: Namespace,
						}, nil
					}
				}
			}
			// On-demand fetch path: sub-path imports
			// (`@apollo/client/core`, `vuetify/styles`)
			// reference URLs the install-time graph walk
			// didn't visit. If FetchOnDemand is wired, pull
			// the URL now + retry the cache lookup.
			if subpath != "" && opts.FetchOnDemand != nil {
				if canonical, fetchErr := opts.FetchOnDemand(targetURL); fetchErr == nil {
					key := targetURL
					if canonical != "" {
						key = canonical
					}
					if _, _, retryErr := opts.Store.Get(key); retryErr == nil {
						return api.OnResolveResult{
							Path:      key,
							Namespace: Namespace,
						}, nil
					}
				}
				// Fetch failed or the cache still doesn't
				// have it — fall through to the clear error
				// so the user gets actionable output.
			}
			return api.OnResolveResult{
				Errors: []api.Message{{
					Text: fmt.Sprintf("nexus-deps: %s resolves to %s but no cached blob exists — run `nexus install` to populate the cache",
						spec, targetURL),
				}},
			}, nil
		}
		return api.OnResolveResult{}, fmt.Errorf("resolver: store lookup for %s: %w", targetURL, err)
	}

	// Return the URL as the Path: esbuild treats it opaquely
	// inside our namespace, but uses its BASENAME for output
	// filenames (so an asset import of /inter.woff2 emits
	// "inter-HASH.woff2" alongside the JS bundle instead of
	// the hash-named file the blob path produced).
	return api.OnResolveResult{
		Path:      targetURL,
		Namespace: Namespace,
	}, nil
}

// loadOne reads the cached blob and tells esbuild what kind of
// content it is. args.Path is the URL the resolver returned;
// store.Get round-trips it to the on-disk blob path + the meta
// we stashed at fetch time (Content-Type, etc.).
//
// Loader dispatch follows loaderFor's rules (see its godoc): JS,
// CSS, JSON, font/image binary assets, generic binary, JS
// fallback. The URL doubles as the input to the extension-sniff
// branch for when Content-Type was missing or octet-stream.
func loadOne(opts Options, args api.OnLoadArgs) (api.OnLoadResult, error) {
	blob, meta, err := opts.Store.Get(args.Path)
	if err != nil {
		return api.OnLoadResult{}, fmt.Errorf("resolver: store lookup %s: %w", args.Path, err)
	}
	body, err := os.ReadFile(blob)
	if err != nil {
		return api.OnLoadResult{}, fmt.Errorf("resolver: read cached blob %s: %w", blob, err)
	}
	contents := string(body)
	return api.OnLoadResult{
		Contents: &contents,
		Loader:   loaderFor(meta.ContentType, args.Path),
	}, nil
}

// loaderFor maps a (Content-Type, URL) pair to the esbuild Loader
// esbuild should treat the cached blob as. Two signals because
// neither is reliable alone:
//
//   - Content-Type is the registry's hint about the bytes' shape.
//     esm.sh sets it accurately for JS/CSS/JSON but routinely
//     serves binary assets (woff2, png) as application/octet-stream
//     when its auto-detection misses.
//   - URL extension is the last-ditch fallback. Browsers do the
//     same dance — mime-sniff first, then trust the extension.
//
// Resolution order:
//
//  1. JS / TS / ES content-types → LoaderJS
//  2. CSS content-types → LoaderCSS
//  3. JSON content-types → LoaderJSON
//  4. Font content-types OR known font extensions → LoaderFile
//     (esbuild copies the bytes to outdir as a separate asset,
//     replaces the import expression with the public URL string;
//     CSS @font-face url() refs work the same way)
//  5. Image content-types OR known image extensions → LoaderFile
//  6. Generic binary (octet-stream + no JS-shape ext) → LoaderFile
//  7. Fallthrough: LoaderJS, because most blobs in our store
//     genuinely are JS and a syntax error from esbuild is more
//     useful than silent corruption.
//
// LoaderFile is the right answer for fonts + images: esbuild
// emits them as standalone files alongside the JS bundle and
// rewrites the import to a URL string. The downstream browser
// fetches the font over HTTP, which is what every Vue/React app
// already expects.
func loaderFor(contentType, urlStr string) api.Loader {
	ct := strings.ToLower(contentType)
	switch {
	case strings.Contains(ct, "javascript"),
		strings.Contains(ct, "ecmascript"),
		strings.Contains(ct, "typescript"):
		return api.LoaderJS
	case strings.Contains(ct, "css"):
		return api.LoaderCSS
	case strings.Contains(ct, "json"):
		return api.LoaderJSON
	case strings.Contains(ct, "font/"),
		strings.Contains(ct, "application/font"),
		strings.Contains(ct, "application/vnd.ms-fontobject"):
		return api.LoaderFile
	case strings.HasPrefix(ct, "image/"):
		return api.LoaderFile
	}

	// Content-Type either missing or ambiguous (octet-stream is
	// the common case for fonts on esm.sh). Sniff the URL's path
	// extension to dispatch binary asset shapes that JS can't
	// possibly handle.
	if loader, ok := loaderFromExtension(urlStr); ok {
		return loader
	}

	return api.LoaderJS
}

// loaderFromExtension maps a URL or path's lowercased trailing
// extension to a binary-safe loader. Returns ok=false when the
// extension doesn't match a known binary shape, so the caller
// falls back to the JS default.
//
// The extension set is the modern web baseline:
//   - fonts: woff2, woff, ttf, otf, eot
//   - images: png, jpg, jpeg, gif, webp, svg, ico, avif
//   - generic binary fallback: covered by the octet-stream branch
//     in loaderFor; this function is for cases where Content-Type
//     was outright absent.
func loaderFromExtension(urlStr string) (api.Loader, bool) {
	// Strip query string before extension sniff — esm.sh URLs
	// carry "?target=es2015" etc. that would mask the real ext.
	if i := strings.Index(urlStr, "?"); i >= 0 {
		urlStr = urlStr[:i]
	}
	lower := strings.ToLower(urlStr)
	switch {
	case strings.HasSuffix(lower, ".woff2"),
		strings.HasSuffix(lower, ".woff"),
		strings.HasSuffix(lower, ".ttf"),
		strings.HasSuffix(lower, ".otf"),
		strings.HasSuffix(lower, ".eot"):
		return api.LoaderFile, true
	case strings.HasSuffix(lower, ".png"),
		strings.HasSuffix(lower, ".jpg"),
		strings.HasSuffix(lower, ".jpeg"),
		strings.HasSuffix(lower, ".gif"),
		strings.HasSuffix(lower, ".webp"),
		strings.HasSuffix(lower, ".svg"),
		strings.HasSuffix(lower, ".ico"),
		strings.HasSuffix(lower, ".avif"):
		return api.LoaderFile, true
	case strings.HasSuffix(lower, ".js"),
		strings.HasSuffix(lower, ".mjs"),
		strings.HasSuffix(lower, ".cjs"),
		strings.HasSuffix(lower, ".ts"),
		strings.HasSuffix(lower, ".tsx"),
		strings.HasSuffix(lower, ".jsx"):
		return api.LoaderJS, true
	case strings.HasSuffix(lower, ".css"):
		return api.LoaderCSS, true
	case strings.HasSuffix(lower, ".json"):
		return api.LoaderJSON, true
	}
	return api.LoaderDefault, false
}




// stripQuery returns u with any "?..." query string removed.
// Used by resolveOne's two-attempt lookup: when the inherited-
// query URL isn't in the cache, the query-stripped form often is
// because esm.sh sibling URLs frequently path-encode their
// variant (e.g. /pkg@1/es2022/x.mjs) and don't need a query.
func stripQuery(u string) string {
	if i := strings.Index(u, "?"); i >= 0 {
		return u[:i]
	}
	return u
}

// resolveRegistryURL joins an import specifier (which may be
// relative, absolute-path, or absolute-URL) against the importer's
// origin URL. The result is an absolute URL we can look up in the
// store.
//
//	importer = https://esm.sh/x@1?target=es2015
//	imp      = ./y.mjs           → https://esm.sh/y.mjs?target=es2015
//	imp      = /node/buffer.mjs  → https://esm.sh/node/buffer.mjs?target=es2015
//	imp      = https://x/y       → https://x/y (unchanged)
//	imp      = bare-name         → ""  (not registry-internal)
//
// The importer's QUERY STRING is preserved on the result when the
// import is a relative or absolute-path reference. esm.sh keys its
// content variants on query (`?target=es2015`, `?bundle`, etc.),
// and our fetcher stored sibling files with the same query — the
// resolver has to use the same key shape or every cross-file
// import inside a lowered bundle fails to find its peer.
func resolveRegistryURL(importerURL, imp string) (string, error) {
	if strings.HasPrefix(imp, "http://") || strings.HasPrefix(imp, "https://") {
		return imp, nil
	}
	if strings.HasPrefix(imp, ".") || strings.HasPrefix(imp, "/") {
		base, err := url.Parse(importerURL)
		if err != nil {
			return "", err
		}
		rel, err := url.Parse(imp)
		if err != nil {
			return "", err
		}
		joined := base.ResolveReference(rel)
		// RFC 3986 §5.2.2 says the merged target inherits NOTHING
		// from the base's query — only scheme/host/path. We
		// reattach the base query when the import didn't carry
		// one of its own, since registry-internal siblings live
		// under the same content variant.
		if joined.RawQuery == "" && base.RawQuery != "" {
			joined.RawQuery = base.RawQuery
		}
		return joined.String(), nil
	}
	// Bare spec — caller already had a chance via the lockfile
	// path. Returning "" signals "not a registry-internal import".
	return "", errors.New("bare spec — not a registry-internal import")
}


// splitSpec separates a bare import path into (package, subpath).
//
// looksLikePackageImport screens out imports that LOOK bare to
// the resolver but are actually tsconfig-paths aliases or other
// non-package prefixes that have no chance of resolving on
// esm.sh. Sending them to FetchOnDemand wastes a network round
// trip (400 from esm.sh) and clutters operator output with
// noisy "on-demand fetch failed" warnings.
//
// Rejected shapes:
//
//	"@/foo"        ← tsconfig alias (no scope name between @ and /)
//	"@/"
//	"~/foo"        ← tilde-style alias (vue-cli convention)
//	"#foo"         ← node imports field (rare in browser code)
//
// Accepted shapes:
//
//	"vue"
//	"@vue/runtime-dom"
//	"@apollo/client/core"
//	"@mdi/font/css/foo.css"
//
// The distinction: a real package import is either an unscoped
// name (no leading @) OR a scoped name where the scope segment
// between "@" and "/" is non-empty. Tsconfig aliases like "@/"
// have an EMPTY scope segment, which the npm registry would
// also reject.
func looksLikePackageImport(p string) bool {
	if p == "" {
		return false
	}
	// Tilde aliases + node-imports never go to esm.sh.
	if p[0] == '~' || p[0] == '#' {
		return false
	}
	if p[0] == '@' {
		// Scoped form. Need a non-empty scope segment between
		// "@" and the first "/" — otherwise it's a tsconfig
		// alias masquerading as a scoped package.
		slash := strings.IndexByte(p, '/')
		if slash <= 1 {
			// slash == -1: just "@" or "@scope" — no path at all
			// slash == 1: "@/..." — empty scope, classic tsconfig alias
			return false
		}
	}
	return true
}

//	"vue"                          → ("vue", "")
//	"vue/dist/vue.esm.js"          → ("vue", "dist/vue.esm.js")
//	"@vue/runtime-dom"             → ("@vue/runtime-dom", "")
//	"@vue/runtime-dom/foo/bar.js"  → ("@vue/runtime-dom", "foo/bar.js")
//
// Scoped packages reserve the first TWO path segments for the
// spec; unscoped reserve only the first.
func splitSpec(path string) (spec, subpath string) {
	if strings.HasPrefix(path, "@") {
		// Scoped: take @scope/name as the spec.
		parts := strings.SplitN(path, "/", 3)
		if len(parts) < 2 {
			return path, ""
		}
		spec = parts[0] + "/" + parts[1]
		if len(parts) == 3 {
			subpath = parts[2]
		}
		return
	}
	// Unscoped: first segment is the spec.
	if i := strings.Index(path, "/"); i >= 0 {
		return path[:i], path[i+1:]
	}
	return path, ""
}

// joinSubpath appends a sub-path to a package's resolved URL,
// preserving query strings. Used to convert "vue/dist/vue.esm.js"
// + the lockfile's "https://esm.sh/vue@3.4.21" into
// "https://esm.sh/vue@3.4.21/dist/vue.esm.js".
//
// The path append happens BEFORE the query string. A naive string
// concat would produce garbage like
// "https://esm.sh/vue@3.4.21?external=vue/dist/vue.esm.js" — the
// "/dist/vue.esm.js" winds up inside the query string and breaks
// the lookup. Parsing through net/url and mutating Path keeps
// query-bearing URLs (post-?external= addition) sound.
//
// The esm.sh URL form for sub-paths is exactly this concatenation
// (on the path), which is why our fetcher's recursive traversal
// would have already fetched these sub-blobs at `nexus add` time.
func joinSubpath(resolvedURL, sub string) string {
	u, err := url.Parse(resolvedURL)
	if err != nil {
		// Fall back to naive concat — the subsequent store lookup
		// will fail and the user gets a clear "not cached" error
		// rather than a silent wrong-URL hit.
		return strings.TrimRight(resolvedURL, "/") + "/" + strings.TrimLeft(sub, "/")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.TrimLeft(sub, "/")
	return u.String()
}
