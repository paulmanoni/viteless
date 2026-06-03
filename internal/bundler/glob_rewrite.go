package bundler

import (
	"fmt"
	"strings"
)

// RewriteGlobCalls is the exported form of rewriteGlobCalls,
// so downstream plugins (e.g. the Vue SFC compiler) can run the
// glob transform on their generated JS before handing it back
// to esbuild. esbuild doesn't re-run OnLoad on a plugin's
// returned Contents, so each per-language compiler that wants
// glob support inside its source has to call us explicitly.
//
// baseDir is the directory glob patterns are resolved against —
// usually filepath.Dir of the source file being compiled.
func RewriteGlobCalls(src, baseDir string) (string, int, error) {
	return rewriteGlobCalls(src, baseDir)
}

// rewriteGlobCalls replaces every `import.meta.glob(...)` call
// in src with an object literal expanding the glob against
// baseDir. Returns the rewritten source and the number of
// replacements made (0 means the source had no glob calls and
// should be returned as-is by the caller).
//
// Supported call shapes:
//
//	import.meta.glob('./pages/*.vue')
//	import.meta.glob("./pages/*.vue")
//	import.meta.glob([\`./a/*.ts\`, './b/*.ts'])
//	import.meta.glob('./pages/*.vue', { eager: false })
//
// Eager mode (`{ eager: true }`) is detected and returns a
// syntax error pointing at the call site — supporting it would
// require static-import emission which we don't have yet.
//
// Limitations operators should know about:
//   - Patterns must be string LITERALS, not variables
//     (`const p = './*'; glob(p)` won't work — pattern must
//     be evaluable at compile time)
//   - Object alternation `{a,b}` in patterns isn't supported
//   - eager: true isn't supported (issue future work)
func rewriteGlobCalls(src, baseDir string) (string, int, error) {
	calls := findGlobCalls(src)
	if len(calls) == 0 {
		return src, 0, nil
	}
	var b strings.Builder
	b.Grow(len(src))
	cursor := 0
	for _, c := range calls {
		b.WriteString(src[cursor:c.start])
		replacement, err := generateGlobReplacement(c.args, baseDir)
		if err != nil {
			return "", 0, fmt.Errorf("import.meta.glob at offset %d: %w", c.start, err)
		}
		b.WriteString(replacement)
		cursor = c.end
	}
	b.WriteString(src[cursor:])
	return b.String(), len(calls), nil
}

// globCall locates one import.meta.glob(...) invocation.
// [start, end) is the byte range to replace; args is the raw
// content BETWEEN the parens (no parens included).
type globCall struct {
	start int
	end   int
	args  string
}

// findGlobCalls scans src for `import.meta.glob` followed by
// optional whitespace and an opening paren. The scan is token-
// aware — string literals (', ", `), template literals with
// `${...}` interpolations, and // + /* */ comments are skipped
// over without matching their contents. So a docstring like
// `"see import.meta.glob() docs"` doesn't trigger a false
// rewrite that would mangle the JSDoc.
//
// For each genuine match, walks forward through nested parens
// + string literals to find the matching closing paren.
func findGlobCalls(src string) []globCall {
	const needle = "import.meta.glob"
	var calls []globCall
	i := 0
	for i < len(src) {
		c := src[i]
		// Skip context where the needle is part of source text
		// rather than a real call: string + template literals,
		// and JS comments.
		switch c {
		case '\'', '"':
			i = skipQuoted(src, i)
			continue
		case '`':
			i = skipTemplate(src, i)
			continue
		case '/':
			if i+1 < len(src) && src[i+1] == '/' {
				// Line comment runs to end of line.
				for i < len(src) && src[i] != '\n' {
					i++
				}
				continue
			}
			if i+1 < len(src) && src[i+1] == '*' {
				// Block comment runs to */.
				i += 2
				for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
					i++
				}
				i += 2
				continue
			}
		}
		// Check whether needle starts at i.
		if i+len(needle) > len(src) || src[i:i+len(needle)] != needle {
			i++
			continue
		}
		start := i
		j := start + len(needle)
		for j < len(src) && isSpace(src[j]) {
			j++
		}
		if j >= len(src) || src[j] != '(' {
			i = start + len(needle)
			continue
		}
		end := findMatchingParen(src, j)
		if end < 0 {
			i = j + 1
			continue
		}
		calls = append(calls, globCall{
			start: start,
			end:   end + 1, // include the `)`
			args:  src[j+1 : end],
		})
		i = end + 1
	}
	return calls
}

// findMatchingParen returns the index of the `)` matching the
// `(` at openIdx, respecting nested parens + string literals
// + template literals (with their `${...}` expressions).
// Returns -1 on EOF / unclosed.
func findMatchingParen(src string, openIdx int) int {
	depth := 1
	i := openIdx + 1
	for i < len(src) && depth > 0 {
		c := src[i]
		switch c {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		case '\'', '"':
			// Skip simple string literal. Honor backslash
			// escapes.
			i = skipQuoted(src, i)
			continue
		case '`':
			// Template literal — handle ${...} bracket nesting.
			i = skipTemplate(src, i)
			continue
		case '/':
			// Could be a comment. Glob calls rarely have
			// comments in them but be defensive.
			if i+1 < len(src) {
				if src[i+1] == '/' {
					// Line comment — skip to end of line.
					i += 2
					for i < len(src) && src[i] != '\n' {
						i++
					}
					continue
				}
				if src[i+1] == '*' {
					i += 2
					for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
						i++
					}
					i += 2
					continue
				}
			}
		}
		i++
	}
	return -1
}

// skipQuoted returns the index AFTER the matching close quote
// for the string literal that starts at src[startIdx]. Honors
// `\` escapes.
func skipQuoted(src string, startIdx int) int {
	quote := src[startIdx]
	i := startIdx + 1
	for i < len(src) {
		if src[i] == '\\' {
			i += 2
			continue
		}
		if src[i] == quote {
			return i + 1
		}
		i++
	}
	return len(src)
}

// skipTemplate returns the index AFTER the matching backtick
// for the template literal starting at src[startIdx]. Honors
// `${...}` expression nesting (which itself may contain
// strings + nested templates) by recursing into the brace
// matcher.
func skipTemplate(src string, startIdx int) int {
	i := startIdx + 1
	for i < len(src) {
		if src[i] == '\\' {
			i += 2
			continue
		}
		if src[i] == '`' {
			return i + 1
		}
		if src[i] == '$' && i+1 < len(src) && src[i+1] == '{' {
			// Skip ${...} matching balanced braces.
			depth := 1
			i += 2
			for i < len(src) && depth > 0 {
				switch src[i] {
				case '{':
					depth++
				case '}':
					depth--
				case '\'', '"':
					i = skipQuoted(src, i)
					continue
				case '`':
					i = skipTemplate(src, i)
					continue
				}
				i++
			}
			continue
		}
		i++
	}
	return len(src)
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// generateGlobReplacement parses the args string (the bytes
// between the call's parens) and returns the replacement
// expression. Args shapes:
//
//	"<pattern>"
//	[ "<pattern>", "<pattern>", ... ]
//	"<pattern>", <options>
//	[ ... ], <options>
//
// Options must be an object literal whose key/value layout we
// only need to recognize for `eager: true` rejection — every
// other option is silently ignored for v1 (operators relying
// on { import: 'default' } etc. will need to wait).
func generateGlobReplacement(args, baseDir string) (string, error) {
	patterns, options, err := parseGlobArgs(args)
	if err != nil {
		return "", err
	}
	if options.eager {
		return "", fmt.Errorf("eager: true is not yet supported by nexus; use the default lazy mode and call the returned import functions yourself")
	}
	// Expand each pattern + merge into a single map keyed by
	// resolved path. Duplicates across patterns are de-duped
	// (later patterns win, matching Vite's behavior).
	seen := map[string]bool{}
	var ordered []string
	for _, p := range patterns {
		matches, err := expandGlob(baseDir, p)
		if err != nil {
			return "", err
		}
		for _, m := range matches {
			if seen[m] {
				continue
			}
			seen[m] = true
			ordered = append(ordered, m)
		}
	}
	// Emit `{ "./path": () => import("./path"), ... }`.
	var b strings.Builder
	b.WriteString("{")
	for i, m := range ordered {
		if i > 0 {
			b.WriteString(",")
		}
		// Path comes from filesystem walk; safe to single-
		// quote without escaping (we don't expect quotes or
		// backslashes in valid module paths).
		fmt.Fprintf(&b, "%q:()=>import(%q)", m, m)
	}
	b.WriteString("}")
	return b.String(), nil
}

// globOptions captures the subset of import.meta.glob options
// we care about at compile time. Other keys are tolerated +
// ignored (operator's intent doesn't break the build).
type globOptions struct {
	eager bool
}

// parseGlobArgs splits the args string into a slice of patterns
// + the options struct. Adequate for the shapes operators
// actually write; intentionally lenient on whitespace.
func parseGlobArgs(args string) ([]string, globOptions, error) {
	args = strings.TrimSpace(args)
	if args == "" {
		return nil, globOptions{}, fmt.Errorf("import.meta.glob called with no arguments")
	}
	// Split into top-level comma-separated parts (respecting
	// brackets + braces).
	parts := splitTopLevelArgs(args)
	if len(parts) == 0 {
		return nil, globOptions{}, fmt.Errorf("import.meta.glob: failed to parse args %q", args)
	}
	patterns, err := parsePatternArg(parts[0])
	if err != nil {
		return nil, globOptions{}, err
	}
	var opts globOptions
	if len(parts) >= 2 {
		opts = parseOptionsArg(parts[1])
	}
	return patterns, opts, nil
}

// splitTopLevelArgs splits args by top-level commas, respecting
// [...] arrays, {...} objects, "..." / '...' / `...` strings.
func splitTopLevelArgs(args string) []string {
	var parts []string
	depthSquare := 0
	depthCurly := 0
	start := 0
	i := 0
	for i < len(args) {
		c := args[i]
		switch c {
		case '[':
			depthSquare++
		case ']':
			depthSquare--
		case '{':
			depthCurly++
		case '}':
			depthCurly--
		case '\'', '"':
			i = skipQuoted(args, i)
			continue
		case '`':
			i = skipTemplate(args, i)
			continue
		case ',':
			if depthSquare == 0 && depthCurly == 0 {
				parts = append(parts, strings.TrimSpace(args[start:i]))
				start = i + 1
			}
		}
		i++
	}
	if start < len(args) {
		parts = append(parts, strings.TrimSpace(args[start:]))
	}
	return parts
}

// parsePatternArg accepts either a single string literal or a
// bracketed array of string literals. Returns the unwrapped
// string values.
func parsePatternArg(arg string) ([]string, error) {
	arg = strings.TrimSpace(arg)
	if strings.HasPrefix(arg, "[") && strings.HasSuffix(arg, "]") {
		inner := arg[1 : len(arg)-1]
		var patterns []string
		for _, p := range splitTopLevelArgs(inner) {
			s, err := unquoteStringLiteral(p)
			if err != nil {
				return nil, err
			}
			patterns = append(patterns, s)
		}
		return patterns, nil
	}
	s, err := unquoteStringLiteral(arg)
	if err != nil {
		return nil, err
	}
	return []string{s}, nil
}

// unquoteStringLiteral strips matching surrounding single,
// double, or backtick quotes. Doesn't honor `${...}`
// interpolation inside backticks — patterns aren't expected
// to interpolate.
func unquoteStringLiteral(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return "", fmt.Errorf("expected string literal, got %q", s)
	}
	first, last := s[0], s[len(s)-1]
	if (first == '"' && last == '"') ||
		(first == '\'' && last == '\'') ||
		(first == '`' && last == '`') {
		return s[1 : len(s)-1], nil
	}
	return "", fmt.Errorf("expected string literal, got %q", s)
}

// parseOptionsArg recognizes `{ eager: true }` (with various
// whitespace). Other keys/values are tolerated as no-ops.
func parseOptionsArg(arg string) globOptions {
	// Cheap byte scan — looking specifically for "eager"
	// followed by colon + "true" with optional whitespace.
	idx := strings.Index(arg, "eager")
	if idx < 0 {
		return globOptions{}
	}
	rest := arg[idx+len("eager"):]
	// Skip ":" and whitespace.
	colon := strings.IndexByte(rest, ':')
	if colon < 0 {
		return globOptions{}
	}
	val := strings.TrimSpace(rest[colon+1:])
	if strings.HasPrefix(val, "true") {
		return globOptions{eager: true}
	}
	return globOptions{}
}
