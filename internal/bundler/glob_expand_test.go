package bundler

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestExpandGlob covers the patterns Vue-Router auto-discovery
// + similar import.meta.glob usages routinely hit.
func TestExpandGlob(t *testing.T) {
	tmp := t.TempDir()
	tree := []string{
		"pages/Home.vue",
		"pages/About.vue",
		"pages/admin/Dashboard.vue",
		"pages/admin/Users.vue",
		"pages/admin/nested/Reports.vue",
		"components/Button.vue",
		"utils/foo.ts",
		"utils/bar.ts",
		"utils/bar.test.ts",
		"types.d.ts",
	}
	for _, f := range tree {
		full := filepath.Join(tmp, f)
		mkdirP(t, filepath.Dir(full))
		writeFile(t, full, "x")
	}

	cases := []struct {
		name    string
		pattern string
		want    []string
	}{
		{
			"flat-vue",
			"./pages/*.vue",
			[]string{"./pages/About.vue", "./pages/Home.vue"},
		},
		{
			"recursive-vue",
			"./pages/**/*.vue",
			[]string{
				"./pages/About.vue",
				"./pages/Home.vue",
				"./pages/admin/Dashboard.vue",
				"./pages/admin/Users.vue",
				"./pages/admin/nested/Reports.vue",
			},
		},
		{
			"non-recursive-flat-stays-flat",
			"./components/*.vue",
			[]string{"./components/Button.vue"},
		},
		{
			"ts-only",
			"./utils/*.ts",
			[]string{
				"./utils/bar.test.ts",
				"./utils/bar.ts",
				"./utils/foo.ts",
			},
		},
		{
			"missing-dir-is-empty-not-error",
			"./nonexistent/*.vue",
			nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := expandGlob(tmp, c.pattern)
			if err != nil {
				t.Fatalf("expandGlob: %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v\nwant %v", got, c.want)
			}
		})
	}
}

// TestGlobToRegex spot-checks the pattern → regex translation.
// We don't need to be exhaustive — the integration test above
// already covers the matching end-to-end. Just verify the
// translation handles the special chars correctly.
func TestGlobToRegex(t *testing.T) {
	cases := []struct {
		pattern string
		input   string
		match   bool
	}{
		// * matches one segment, no slashes.
		{"*.vue", "Home.vue", true},
		{"*.vue", "admin/Home.vue", false},
		{"pages/*.vue", "pages/Home.vue", true},
		{"pages/*.vue", "pages/admin/Home.vue", false},
		// ** matches multiple segments.
		{"pages/**/*.vue", "pages/Home.vue", true},
		{"pages/**/*.vue", "pages/admin/Home.vue", true},
		{"pages/**/*.vue", "pages/admin/deep/Home.vue", true},
		{"pages/**/*.vue", "other/Home.vue", false},
		// Dot literal.
		{"foo.ts", "foo.ts", true},
		{"foo.ts", "fooXts", false},
	}
	for _, c := range cases {
		re, err := globToRegex(c.pattern)
		if err != nil {
			t.Fatalf("globToRegex(%q): %v", c.pattern, err)
		}
		got := re.MatchString(c.input)
		if got != c.match {
			t.Errorf("globToRegex(%q).MatchString(%q) = %v, want %v",
				c.pattern, c.input, got, c.match)
		}
	}
}

func mkdirP(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if !strings.Contains(path, string(os.PathSeparator)) {
		// nothing to mkdir
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
