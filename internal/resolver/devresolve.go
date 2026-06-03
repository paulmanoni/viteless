package resolver

import (
	"errors"
	"fmt"
	neturl "net/url"
	"regexp"
	"strings"

	"github.com/paulmanoni/viteless/internal/lockfile"
)

// ResolveURL maps an import specifier to the canonical store URL its bytes
// are cached under — the same decision tree as the esbuild plugin's
// resolveOne, but returning a plain URL instead of an api.OnResolveResult.
//
// The unbundled dev server (frontend/devserver) uses this so it resolves
// bare and registry-internal imports IDENTICALLY to `nexus build`: one
// code path, one set of helpers (splitSpec / joinSubpath / DevSpecRewrite /
// resolveRegistryURL / FetchOnDemand). It then wraps the returned URL as a
// browser-fetchable /@dep/ path.
//
// importerURL is the real registry URL of the importing module when that
// module is itself a cached dep blob (so a relative/absolute import inside
// it resolves against its registry siblings). Pass "" when the importer is
// user source — then only bare specs resolve here and relative/alias
// imports are left to the caller (filesystem).
//
// Returns:
//
//   - (url, true,  nil) — resolved to a cached (or just-fetched) blob URL
//   - ("",  false, nil) — not the resolver's concern; the caller handles it
//     (relative/alias/user code → its own source tree)
//   - ("",  false, err) — a hard error (ambiguous version, lockfile fault)
func (o Options) ResolveURL(spec, importerURL string) (string, bool, error) {
	if o.Lockfile == nil || o.Store == nil {
		return "", false, errors.New("resolver: ResolveURL needs Lockfile + Store")
	}
	p := spec

	// --- registry-internal resolution -------------------------------
	// The importer is one of our cached dep blobs, so every relative /
	// absolute-path / absolute-URL import refers to a registry sibling.
	// Resolve against the importer URL and look the result up in the
	// store (two-attempt: inherited-query, then query-stripped), with an
	// on-demand fetch as the cold-miss fallback. Mirrors resolveOne's
	// namespace branch.
	if importerURL != "" {
		if absURL, err := resolveRegistryURL(importerURL, p); err == nil && absURL != "" {
			if u, ok := o.lookupOrFetch(absURL); ok {
				return u, true, nil
			}
		}
		// A bare spec inside a dep blob falls through to the lockfile
		// branch below (same as resolveOne).
	}

	// Relative / absolute-path / protocol imports from USER code are not
	// the resolver's job — the dev server resolves those against the
	// project's own source tree (or leaves them to the browser).
	if importerURL == "" {
		switch {
		case strings.HasPrefix(p, "./"), strings.HasPrefix(p, "../"), p == ".", p == "..":
			return "", false, nil
		case strings.HasPrefix(p, "/"):
			return "", false, nil
		case strings.Contains(p, "://"), strings.HasPrefix(p, "data:"):
			return "", false, nil
		}
	}

	spec, subpath := splitSpec(p)

	// Dev-mode spec rewrite (vue → vue.development.mjs). Consulted before
	// the lockfile; on any miss we fall through so a bad rewrite never
	// breaks resolution.
	if o.DevSpecRewrite != nil {
		if devURL := o.DevSpecRewrite(spec, subpath); devURL != "" {
			if u, ok := o.lookupOrFetch(devURL); ok {
				return u, true, nil
			}
		}
	}

	pkg, err := o.Lockfile.Resolve(spec, "")
	if err != nil {
		if errors.Is(err, lockfile.ErrNotResolved) {
			// Not in the lockfile, but it may be a valid esm.sh path the
			// install-time walk didn't reach. Skip tsconfig-style aliases
			// (@/, ~/, #…) — esm.sh would 400 those; the caller resolves
			// them against the source tree.
			if o.FetchOnDemand != nil && looksLikePackageImport(p) {
				// Subpath import of an un-pinned package
				// (`react/jsx-runtime`, `react-dom/client`): fetching the
				// subpath spec directly collapses to the package ROOT,
				// which lacks the subpath's exports (the root `react` has
				// no `jsx`/`jsxs`). Resolve the package root first, then
				// join the subpath against its clean URL and fetch that.
				if subpath != "" {
					if root, ferr := o.FetchOnDemand(spec); ferr == nil && root != "" {
						if u, ok := o.lookupOrFetch(joinSubpath(root, subpath)); ok {
							return u, true, nil
						}
					}
				}
				if u, ok := o.lookupOrFetch(p); ok {
					return u, true, nil
				}
			}
			return "", false, nil
		}
		var ae *lockfile.AmbiguousError
		if errors.As(err, &ae) {
			return "", false, ae
		}
		return "", false, fmt.Errorf("resolver: lockfile lookup for %q: %w", spec, err)
	}

	targetURL := pkg.Resolved
	if subpath != "" {
		targetURL = joinSubpath(pkg.Resolved, subpath)
	}
	if u, ok := o.lookupOrFetch(targetURL); ok {
		return u, true, nil
	}
	return "", false, fmt.Errorf("resolver: %s resolves to %s but no cached blob exists — run `nexus install`", spec, targetURL)
}

// LocateBlobURL returns the canonical cache key a registry URL's bytes live
// under, applying the same lookups + semver-range fallback ResolveURL uses.
// The dev server calls this when serving a /@dep/ path whose decoded URL
// misses the store directly — notably an unresolved range URL like
// `.../graphql@^15.0.0 || ^16.0.0?target=es2022` that a previously-cached
// blob baked into its imports. Returns ("", false) when the bytes can't be
// located or fetched.
func (o Options) LocateBlobURL(url string) (string, bool) {
	if o.Store == nil {
		return "", false
	}
	return o.lookupOrFetch(url)
}

// lookupOrFetch returns the cache key a URL's bytes live under. It tries
// the URL as-is, then its query-stripped form (esm.sh path-encodes sibling
// variants without a query), then — when FetchOnDemand is wired — pulls the
// URL and retries at the post-redirect canonical key. Returns ("", false)
// when the bytes can't be located or fetched.
//
// Unlike resolveOne's hot path (which keeps fetching off the parallel
// build to avoid the lockfile write race), ResolveURL runs on the dev
// server's single-flight request goroutines, so an on-demand fetch here is
// safe.
func (o Options) lookupOrFetch(url string) (string, bool) {
	if _, _, err := o.Store.Get(url); err == nil {
		return url, true
	}
	if stripped := stripQuery(url); stripped != url {
		if _, _, err := o.Store.Get(stripped); err == nil {
			return stripped, true
		}
	}
	if o.FetchOnDemand != nil {
		if canonical, err := o.FetchOnDemand(url); err == nil {
			key := url
			if canonical != "" {
				key = canonical
			}
			if _, _, err := o.Store.Get(key); err == nil {
				return key, true
			}
		}
	}
	// Semver-range fallback. esm.sh links some transitive deps by an
	// UNRESOLVED range, e.g. `.../graphql@^15.0.0 || ^16.0.0?target=es2022`
	// (it 302-redirects those at fetch time, so the bytes are cached under
	// the concrete version, not the range). The range URL itself has
	// spaces/`||` and isn't a cache key, so the lookups above all miss.
	// Pin it to the concrete version the lockfile already resolved for that
	// package and retry — the lockfile is the source of truth.
	if pkg, ok := esmRangePackage(url); ok {
		if p, err := o.Lockfile.Resolve(pkg, ""); err == nil && p.Resolved != "" {
			if u, ok := o.lookupOrFetchConcrete(p.Resolved); ok {
				return u, true
			}
		}
	}
	return "", false
}

// lookupOrFetchConcrete is lookupOrFetch WITHOUT the range fallback, used to
// resolve the lockfile's already-concrete URL (avoids any recursion).
func (o Options) lookupOrFetchConcrete(url string) (string, bool) {
	if _, _, err := o.Store.Get(url); err == nil {
		return url, true
	}
	if stripped := stripQuery(url); stripped != url {
		if _, _, err := o.Store.Get(stripped); err == nil {
			return stripped, true
		}
	}
	if o.FetchOnDemand != nil {
		if canonical, err := o.FetchOnDemand(url); err == nil && canonical != "" {
			if _, _, err := o.Store.Get(canonical); err == nil {
				return canonical, true
			}
		}
	}
	return "", false
}

// esmVersionRE captures an esm.sh URL's package + version-spec segment:
//
//	https://esm.sh/graphql@^15.0.0 || ^16.0.0?target=es2022
//	https://esm.sh/@scope/pkg@^1.0.0/sub.mjs
//
// Group 1 = package name (incl. scope), group 2 = the version spec up to
// the next /, ? or end. Handles %-encoded forms too (decoded before match).
var esmVersionRE = regexp.MustCompile(`esm\.sh/((?:@[^/]+/)?[^/@]+)@([^/?]+)`)

// esmRangePackage reports whether url is an esm.sh package URL whose version
// segment is an unresolved SEMVER RANGE (not a concrete x.y.z), returning the
// bare package name to re-resolve against the lockfile. A range contains any
// of ^ ~ || * > < x or whitespace (after percent-decoding); a bare digits
// version does not.
func esmRangePackage(url string) (string, bool) {
	dec, err := neturl.QueryUnescape(url)
	if err != nil {
		dec = url
	}
	m := esmVersionRE.FindStringSubmatch(dec)
	if m == nil {
		return "", false
	}
	pkg, ver := m[1], m[2]
	if strings.ContainsAny(ver, "^~*<> |") || strings.Contains(ver, "||") {
		return pkg, true
	}
	return "", false
}
