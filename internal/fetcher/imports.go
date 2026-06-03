package fetcher

import (
	"regexp"
)

// ExtractImports returns every import specifier referenced from the
// given ESM source. Includes:
//
//	import 'spec'
//	import x from 'spec'
//	import x, { y } from 'spec'
//	import * as x from 'spec'
//	export { x } from 'spec'
//	export * from 'spec'
//	import('spec')                  // dynamic
//
// Each specifier appears at most once in the returned slice; first-
// appearance order is preserved so callers walking the result get a
// stable traversal.
//
// We're not parsing JavaScript — that's esbuild's job at bundle
// time. We're harvesting bare specs the fetcher needs to chase
// recursively. False negatives (a clever import wrapped in eval())
// are fine: esbuild's resolver plugin still triggers a late fetch
// for them. False positives (matching a "from" inside a string
// literal or comment) are NOT fine — they cause spurious fetches.
//
// Defense: build a code/string/comment mask over the source, run
// the regexes against the ORIGINAL bytes (so quoted specs match),
// then drop any match whose KEYWORD position lies inside a string
// or comment. A real import begins with the `import` or `export`
// keyword at code-level — the spec inside its quotes is fine to
// capture even though it's "inside a string".
func ExtractImports(src string) []string {
	mask := buildMask(src)

	seen := map[string]bool{}
	var out []string
	for _, re := range importPatterns {
		for _, m := range re.FindAllStringSubmatchIndex(src, -1) {
			// m[0]/m[1] is the full match; m[2]/m[3] is group 1
			// (the spec). The keyword (import/export) is at m[0].
			start := m[0]
			if start < len(mask) && mask[start] != codeRegion {
				continue
			}
			spec := src[m[2]:m[3]]
			if spec == "" {
				continue
			}
			if !seen[spec] {
				seen[spec] = true
				out = append(out, spec)
			}
		}
	}
	return out
}

// importPatterns lists the regexes ExtractImports applies, in the
// order matches are reported. The grouping (spec) is always group 1.
var importPatterns = []*regexp.Regexp{
	// import ... from "spec"   /   import "spec"
	// export ... from "spec"   /   export * from "spec"
	//
	// Whitespace is OPTIONAL throughout (\s*) because minified
	// ESM bundles from esm.sh collapse it:
	//
	//   }import{EventEmitter as g}from"/node/events.mjs";
	//
	// has zero whitespace between `import` and `{` and between
	// `}` and `from`. The \b after import|export keeps
	// `importExport` from false-matching.
	//
	// Leading-separator class is wide for the same reason —
	// `}import` and `)import` show up routinely after minifiers
	// drop newlines.
	regexp.MustCompile(`(?m)(?:^|[\s;{}(),])(?:import|export)\b\s*(?:[^;'"]*?\s*from\s*)?['"]([^'"\n]+)['"]`),
	// dynamic import("spec")
	regexp.MustCompile(`(?m)import\s*\(\s*['"]([^'"\n]+)['"]\s*\)`),
}

// codeRegion / stringRegion / commentRegion are the values
// buildMask paints over the source. Callers check mask[i] to know
// which lexical region byte i belongs to.
const (
	codeRegion    = 0
	stringRegion  = 's'
	commentRegion = 'c'
)

// buildMask walks src once and returns a parallel byte slice tagged
// with the lexical region for each input byte. Single-pass state
// machine: code → (string|line-comment|block-comment) → code.
//
// String handling honors \-escapes so that  "\""  doesn't terminate
// the string early. Template literals (`…`) are treated as opaque
// strings; their ${…} substitution can contain real code, but no
// esm.sh-generated module uses templates around imports — and a
// pathological case here just produces false negatives (a missed
// import), not false positives (a spurious fetch).
func buildMask(src string) []byte {
	mask := make([]byte, len(src))

	const (
		stCode = iota
		stStr
		stLine
		stBlock
	)
	state := stCode
	var quote byte
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch state {
		case stCode:
			switch {
			case c == '/' && i+1 < len(src) && src[i+1] == '/':
				mask[i] = commentRegion
				mask[i+1] = commentRegion
				i++
				state = stLine
			case c == '/' && i+1 < len(src) && src[i+1] == '*':
				mask[i] = commentRegion
				mask[i+1] = commentRegion
				i++
				state = stBlock
			case c == '\'' || c == '"' || c == '`':
				// Opening quote is code-level (the import keyword
				// match needs to find it via regex), but the
				// body that follows is string-region.
				mask[i] = codeRegion
				quote = c
				state = stStr
			default:
				mask[i] = codeRegion
			}
		case stStr:
			switch {
			case c == '\\' && i+1 < len(src):
				mask[i] = stringRegion
				mask[i+1] = stringRegion
				i++
			case c == quote:
				mask[i] = codeRegion // closing quote rejoins code
				state = stCode
			default:
				mask[i] = stringRegion
			}
		case stLine:
			// The LF that terminates a // comment is NOT part of
			// the comment in JS lexer rules — the comment ends
			// AT the LF. Painting it codeRegion matters because
			// our import regex consumes a leading whitespace char
			// before `import`; if that whitespace happens to be
			// the terminating LF, dropping it as comment would
			// filter out an otherwise-valid import on the next
			// line.
			if c == '\n' {
				mask[i] = codeRegion
				state = stCode
			} else {
				mask[i] = commentRegion
			}
		case stBlock:
			mask[i] = commentRegion
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				mask[i+1] = commentRegion
				i++
				state = stCode
			}
		}
	}
	return mask
}
