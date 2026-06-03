package bundler

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// expandGlob returns every file matching pattern under baseDir,
// reported as a relative path with the same "./"-prefixed shape
// Vite + import.meta.glob users expect:
//
//	expandGlob("/abs/proj/src", "./pages/*.vue")
//	  → ["./pages/Home.vue", "./pages/About.vue", ...]
//
// Pattern syntax (subset of fast-glob, enough for the cases
// import.meta.glob users hit in practice):
//
//	*    matches any character except /
//	**   matches any character INCLUDING /
//	?    matches one character except /
//
// {a,b} alternation is NOT supported in v1 — operators wanting
// "match .vue OR .tsx" should use two patterns in an array.
//
// Patterns are anchored at baseDir + the leading literal
// path-prefix of pattern (everything before the first wildcard
// char). So `./pages/admin/*.vue` walks only `pages/admin/`,
// not the whole project.
//
// Returns paths sorted lexically — same-input-same-output for
// repeatable bundle hashes. Missing prefix dirs are not errors
// (returns empty); other I/O errors propagate.
func expandGlob(baseDir, pattern string) ([]string, error) {
	// Normalize the "./"" prefix away for internal handling;
	// we'll add it back on the output paths.
	clean := strings.TrimPrefix(pattern, "./")

	// Find the longest leading literal prefix (path segments
	// with no glob chars). Walk only from there — faster than
	// walking baseDir for every pattern.
	parts := strings.Split(clean, "/")
	var literalParts []string
	for _, p := range parts {
		if strings.ContainsAny(p, "*?{") {
			break
		}
		literalParts = append(literalParts, p)
	}
	walkRoot := baseDir
	if len(literalParts) > 0 {
		walkRoot = filepath.Join(baseDir, filepath.Join(literalParts...))
	}

	// Compile the full pattern (relative to baseDir) into a
	// regex that we match each visited file against.
	re, err := globToRegex(clean)
	if err != nil {
		return nil, fmt.Errorf("expandGlob: invalid pattern %q: %w", pattern, err)
	}

	var matches []string
	err = filepath.WalkDir(walkRoot, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			// Missing walkRoot is fine (pattern targets a
			// dir the operator hasn't created yet).
			if os.IsNotExist(werr) {
				return filepath.SkipDir
			}
			return werr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(baseDir, path)
		if err != nil {
			return err
		}
		// Use forward slashes for matching even on Windows —
		// the patterns are Vite-flavored.
		rel = filepath.ToSlash(rel)
		if re.MatchString(rel) {
			matches = append(matches, "./"+rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}

// globToRegex converts a Vite-style glob pattern (relative,
// uses ** for recursive) into a Go regular expression.
//
// The pattern is anchored end-to-end (^...$). Special regex
// chars in the pattern get escaped; the glob metacharacters
// (* ** ?) get translated.
//
// Translation rules:
//
//	**/  →  (.+/)?  (zero-or-more path segments + slash)
//	**   →  .*      (only at end of pattern; matches anything)
//	*    →  [^/]*   (one segment, no slashes)
//	?    →  [^/]    (one char, no slashes)
//
// All other chars get regex-escaped when needed.
func globToRegex(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	i := 0
	for i < len(pattern) {
		c := pattern[i]
		switch c {
		case '*':
			// Look ahead for **.
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				// `**/` consumes the slash too, matching
				// "any nested path with trailing /".
				if i+2 < len(pattern) && pattern[i+2] == '/' {
					b.WriteString("(.+/)?")
					i += 3
				} else {
					// `**` at end of pattern: match anything.
					b.WriteString(".*")
					i += 2
				}
			} else {
				b.WriteString("[^/]*")
				i++
			}
		case '?':
			b.WriteString("[^/]")
			i++
		case '.', '+', '(', ')', '|', '^', '$', '[', ']', '\\', '{', '}':
			b.WriteByte('\\')
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
