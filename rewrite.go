// Package viteless is a zero-Node, native-ESM dev server for Go web
// frameworks — the dev-time architecture Vite uses, reimplemented in Go.
//
// Instead of bundling up front, each source file is served at its own URL,
// transformed on request, with its import specifiers rewritten to
// browser-resolvable URLs. The browser walks the import graph natively.
// Because every dependency (vue, etc.) resolves to ONE URL, all consumers
// share one module instance for free — which is what makes Vite-style HMR
// (re-import a module URL, swap it) work without bridges, stubs, or
// instance-sharing hacks.
//
// This file is the import REWRITER — the fiddly heart. It is a pure
// function (resolution is injected) so it is fully unit-testable without a
// store, network, or browser.
package viteless

import "strings"

// SpecKind classifies an import specifier so the caller's resolver can
// decide how to turn it into a URL.
type SpecKind int

const (
	// SpecBare is a package import: "vue", "@apollo/client/core".
	SpecBare SpecKind = iota
	// SpecRelative is "./x" or "../y".
	SpecRelative
	// SpecAbsolute is a root-absolute path "/x" or a full URL
	// "https://esm.sh/...". These appear inside served dependency blobs,
	// which reference siblings by absolute URL.
	SpecAbsolute
)

// ResolveFunc maps an import specifier (as written in the module) to the
// URL the browser should fetch. importerURL is the URL the current module
// is served at, so the resolver can resolve relative specs against it.
// Returning "" leaves the specifier untouched (e.g. data: URIs, or specs
// the resolver chooses not to handle).
type ResolveFunc func(spec string, kind SpecKind, importerURL string) string

// RewriteImports rewrites every import/export specifier in src to the URL
// returned by resolve. It handles:
//
//   - static:    import X from "spec" / import {a} from "spec" / import "spec"
//   - re-export: export {a} from "spec" / export * from "spec"
//   - dynamic:   import("spec") / import('spec')
//
// Only the string-literal specifier is replaced; statement structure is
// left intact, so the browser sees valid ESM with URLs in place of bare/
// relative specs. Occurrences of "import"/"from" inside comments, strings,
// or larger identifiers are not matched.
func RewriteImports(src, importerURL string, classify func(string) SpecKind, resolve ResolveFunc) string {
	var b strings.Builder
	b.Grow(len(src) + 64)

	i, n := 0, len(src)
	for i < n {
		// Skip comments and string/template literals so an "import"
		// inside them is never treated as code.
		if adv := skipNonCode(src, i); adv > i {
			b.WriteString(src[i:adv])
			i = adv
			continue
		}
		if spec, lo, hi, ok := matchSpecifierAt(src, i); ok {
			b.WriteString(src[i : lo+1]) // up to AND including the opening quote
			if rewritten := resolve(spec, classify(spec), importerURL); rewritten != "" {
				b.WriteString(rewritten)
			} else {
				b.WriteString(spec)
			}
			b.WriteByte(src[hi]) // closing quote
			i = hi + 1
			continue
		}
		b.WriteByte(src[i])
		i++
	}
	return b.String()
}

// ClassifySpec is the default specifier classifier.
func ClassifySpec(spec string) SpecKind {
	switch {
	case strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../"):
		return SpecRelative
	case strings.HasPrefix(spec, "/") || strings.Contains(spec, "://"):
		return SpecAbsolute
	default:
		return SpecBare
	}
}

// matchSpecifierAt checks whether a rewritable import/export specifier
// begins at position i. On success it returns the inner specifier text,
// lo = index of the opening quote, hi = index of the closing quote.
//
// Recognized (whitespace-flexible):
//
//	from "spec"     — covers `import … from "x"` and `export … from "x"`
//	import "spec"   — side-effect import
//	import("spec")  — dynamic import
func matchSpecifierAt(src string, i int) (spec string, lo, hi int, ok bool) {
	if hasWordAt(src, i, "from") {
		j := skipWS(src, i+4)
		if s, q, e, good := readStringLit(src, j); good {
			return s, q, e, true
		}
		return "", 0, 0, false
	}
	if hasWordAt(src, i, "import") {
		k := skipWS(src, i+6)
		if k < len(src) && src[k] == '(' { // dynamic import(
			k = skipWS(src, k+1)
			if s, q, e, good := readStringLit(src, k); good {
				return s, q, e, true
			}
			return "", 0, 0, false
		}
		// side-effect import "x" (next non-ws is a quote)
		if s, q, e, good := readStringLit(src, k); good {
			return s, q, e, true
		}
	}
	return "", 0, 0, false
}

// hasWordAt reports whether word sits at src[i:] as a whole token — the
// neighbouring chars must not be identifier characters.
func hasWordAt(src string, i int, word string) bool {
	if i+len(word) > len(src) || src[i:i+len(word)] != word {
		return false
	}
	if i > 0 && isIdentChar(src[i-1]) {
		return false
	}
	if after := i + len(word); after < len(src) && isIdentChar(src[after]) {
		return false
	}
	return true
}

// readStringLit reads a single- or double-quoted string literal at j (j
// must point at the quote). Returns inner text, opening-quote index,
// closing-quote index, ok. Templates are intentionally not matched —
// import specifiers are never template literals.
func readStringLit(src string, j int) (inner string, openQ, closeQ int, ok bool) {
	if j >= len(src) {
		return "", 0, 0, false
	}
	q := src[j]
	if q != '"' && q != '\'' {
		return "", 0, 0, false
	}
	for k := j + 1; k < len(src); k++ {
		switch src[k] {
		case '\\':
			k++ // skip escaped char
		case q:
			return src[j+1 : k], j, k, true
		case '\n':
			return "", 0, 0, false // unterminated on this line
		}
	}
	return "", 0, 0, false
}

// skipNonCode returns the index just past a comment or string/template
// literal beginning at i, or i if there is nothing to skip.
func skipNonCode(src string, i int) int {
	if i+1 < len(src) && src[i] == '/' {
		switch src[i+1] {
		case '/':
			k := i + 2
			for k < len(src) && src[k] != '\n' {
				k++
			}
			return k
		case '*':
			k := i + 2
			for k+1 < len(src) {
				if src[k] == '*' && src[k+1] == '/' {
					return k + 2
				}
				k++
			}
			return len(src)
		}
	}
	if c := src[i]; c == '"' || c == '\'' || c == '`' {
		for k := i + 1; k < len(src); k++ {
			if src[k] == '\\' {
				k++
				continue
			}
			if src[k] == c {
				return k + 1
			}
			if (c == '"' || c == '\'') && src[k] == '\n' {
				return i // not a real single-line literal; don't skip
			}
		}
		return len(src)
	}
	return i
}

func skipWS(src string, i int) int {
	for i < len(src) {
		switch src[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

func isIdentChar(c byte) bool {
	return c == '_' || c == '$' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
