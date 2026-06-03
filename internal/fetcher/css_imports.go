package fetcher

import "regexp"

// ExtractCSSImports returns every URL referenced from a CSS body
// via either:
//
//	@import url("./theme.css") / @import "./theme.css"
//	url(./files/inter-cyrillic-ext-wght-normal.woff2)
//	url("https://fonts/example.com/font.woff2")
//
// Used by the fetcher when it encounters a CSS-shaped body during
// its recursive walk — fonts + theme files referenced from CSS
// wouldn't get pulled otherwise, and esbuild's downstream walk
// would surface them as missing-in-cache errors at bundle time.
//
// Like ExtractImports, this is best-effort: we're not building a
// real CSS parser, just harvesting URLs the fetcher needs to chase.
// False negatives are tolerable (esbuild surfaces them as build
// errors with file + line). False positives are tolerable (a
// spurious URL fetch costs one wasted HTTP request).
//
// Skipped:
//   - data: URLs (already-inlined bytes)
//   - # URL fragments (intra-document references like SVG defs)
//
// The deduplication preserves first-appearance order for stable
// traversal under tests.
func ExtractCSSImports(src string) []string {
	seen := map[string]bool{}
	var out []string
	for _, re := range cssImportPatterns {
		for _, m := range re.FindAllStringSubmatch(src, -1) {
			u := m[1]
			if u == "" || u[0] == '#' {
				continue
			}
			if len(u) >= 5 && u[:5] == "data:" {
				continue
			}
			if !seen[u] {
				seen[u] = true
				out = append(out, u)
			}
		}
	}
	return out
}

// cssImportPatterns is the ordered list of regexes ExtractCSSImports
// applies. Two distinct shapes:
//
//  1. @import "spec";                  or   @import url("spec");
//  2. url(...) or url("...") or url('...')  inside any property
//
// The url(...) form is overwhelmingly common (every @font-face,
// every background-image). @import is rare in registry-served CSS
// because esm.sh tends to flatten them, but we cover it anyway for
// stylesheet packages that pass-through their original imports.
var cssImportPatterns = []*regexp.Regexp{
	// @import "..." or @import url("...")
	regexp.MustCompile(`@import\s+(?:url\s*\(\s*)?["']?([^"'() \t\n]+)["']?\s*\)?[\s;]`),
	// url(...) — unquoted, single-quoted, double-quoted
	regexp.MustCompile(`\burl\s*\(\s*["']?([^"'() \t\n]+)["']?\s*\)`),
}
