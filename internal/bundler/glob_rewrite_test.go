package bundler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRewriteGlobCalls_BasicLazy covers the Vue Router auto-
// discovery pattern: `import.meta.glob('./pages/*.vue')` should
// rewrite to a plain object literal mapping each matched path
// to a dynamic import.
func TestRewriteGlobCalls_BasicLazy(t *testing.T) {
	tmp := t.TempDir()
	mkdirP2(t, filepath.Join(tmp, "pages"))
	writeFile2(t, filepath.Join(tmp, "pages", "Home.vue"), "<template>home</template>")
	writeFile2(t, filepath.Join(tmp, "pages", "About.vue"), "<template>about</template>")

	src := `const modules = import.meta.glob('./pages/*.vue');`
	got, n, err := rewriteGlobCalls(src, tmp)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 replacement, got %d", n)
	}
	for _, want := range []string{
		`"./pages/About.vue":()=>import("./pages/About.vue")`,
		`"./pages/Home.vue":()=>import("./pages/Home.vue")`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
}

// TestRewriteGlobCalls_RecursivePattern: `**/*.vue` should walk
// nested directories.
func TestRewriteGlobCalls_RecursivePattern(t *testing.T) {
	tmp := t.TempDir()
	mkdirP2(t, filepath.Join(tmp, "pages", "admin"))
	writeFile2(t, filepath.Join(tmp, "pages", "Home.vue"), "x")
	writeFile2(t, filepath.Join(tmp, "pages", "admin", "Users.vue"), "x")

	src := `const m = import.meta.glob('./pages/**/*.vue');`
	got, _, err := rewriteGlobCalls(src, tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"./pages/Home.vue"`,
		`"./pages/admin/Users.vue"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// TestRewriteGlobCalls_ArrayOfPatterns: glob accepts an array
// of patterns; results merge (de-duped, sorted).
func TestRewriteGlobCalls_ArrayOfPatterns(t *testing.T) {
	tmp := t.TempDir()
	mkdirP2(t, filepath.Join(tmp, "a"))
	mkdirP2(t, filepath.Join(tmp, "b"))
	writeFile2(t, filepath.Join(tmp, "a", "x.ts"), "x")
	writeFile2(t, filepath.Join(tmp, "b", "y.ts"), "y")

	src := `import.meta.glob(['./a/*.ts', './b/*.ts'])`
	got, _, err := rewriteGlobCalls(src, tmp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"./a/x.ts"`) || !strings.Contains(got, `"./b/y.ts"`) {
		t.Errorf("expected both array results in output:\n%s", got)
	}
}

// TestRewriteGlobCalls_NoCallsLeavesSourceAlone: when the
// source has no import.meta.glob calls, return identical
// source + zero replacements. Avoids wasted allocations in
// the no-op path.
func TestRewriteGlobCalls_NoCallsLeavesSourceAlone(t *testing.T) {
	src := `const foo = "hello"; console.log(foo);`
	got, n, err := rewriteGlobCalls(src, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("expected 0 replacements, got %d", n)
	}
	if got != src {
		t.Errorf("source must be unchanged when no calls present")
	}
}

// TestRewriteGlobCalls_EagerErrors: eager: true is documented
// as unsupported; the rewrite should fail with a clear message
// instead of silently producing wrong output.
func TestRewriteGlobCalls_EagerErrors(t *testing.T) {
	src := `import.meta.glob('./pages/*.vue', { eager: true })`
	_, _, err := rewriteGlobCalls(src, t.TempDir())
	if err == nil {
		t.Fatal("expected error for eager: true")
	}
	if !strings.Contains(err.Error(), "eager") {
		t.Errorf("error should mention eager, got: %v", err)
	}
}

// TestRewriteGlobCalls_LeavesStringLiteralsAlone: a call inside
// a string ("the function import.meta.glob() does X") must NOT
// be rewritten — that'd be source-corrupting nonsense. The
// scanner skips quoted regions so the docstring round-trips
// unchanged.
func TestRewriteGlobCalls_LeavesStringLiteralsAlone(t *testing.T) {
	src := `const docs = "see import.meta.glob() docs for details";`
	got, n, err := rewriteGlobCalls(src, t.TempDir())
	if err != nil {
		t.Fatalf("rewrite errored unexpectedly: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 rewrites for needle inside a string; got %d", n)
	}
	if got != src {
		t.Errorf("source mutated:\nwant: %s\ngot:  %s", src, got)
	}
}

// TestRewriteGlobCalls_LeavesCommentsAlone: same protection
// for // line and /* block */ comments — a JSDoc explaining
// what import.meta.glob does shouldn't trigger a rewrite.
func TestRewriteGlobCalls_LeavesCommentsAlone(t *testing.T) {
	cases := []string{
		`// see import.meta.glob('./pages/*.vue') for example`,
		`/* import.meta.glob('./pages/*.vue') — explained below */`,
	}
	for _, src := range cases {
		got, n, err := rewriteGlobCalls(src, t.TempDir())
		if err != nil {
			t.Errorf("error rewriting %q: %v", src, err)
		}
		if n != 0 {
			t.Errorf("rewrote inside a comment: %q → %d rewrites", src, n)
		}
		if got != src {
			t.Errorf("source mutated:\nwant: %s\ngot:  %s", src, got)
		}
	}
}

// TestRewriteGlobCalls_PreservesSurroundingSource: the rewrite
// keeps everything before/after the call intact — only the
// call expression itself changes.
func TestRewriteGlobCalls_PreservesSurroundingSource(t *testing.T) {
	tmp := t.TempDir()
	mkdirP2(t, filepath.Join(tmp, "pages"))
	writeFile2(t, filepath.Join(tmp, "pages", "Home.vue"), "x")

	src := "// before\nconst routes = import.meta.glob('./pages/*.vue');\n// after\n"
	got, _, err := rewriteGlobCalls(src, tmp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "// before\n") {
		t.Errorf("leading content stripped:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n// after\n") {
		t.Errorf("trailing content stripped:\n%s", got)
	}
}

// TestRewriteGlobCalls_MultipleCalls: multiple calls in the
// same file all get rewritten.
func TestRewriteGlobCalls_MultipleCalls(t *testing.T) {
	tmp := t.TempDir()
	mkdirP2(t, filepath.Join(tmp, "a"))
	mkdirP2(t, filepath.Join(tmp, "b"))
	writeFile2(t, filepath.Join(tmp, "a", "x.ts"), "x")
	writeFile2(t, filepath.Join(tmp, "b", "y.ts"), "y")

	src := `
		const aMods = import.meta.glob('./a/*.ts');
		const bMods = import.meta.glob('./b/*.ts');
	`
	got, n, err := rewriteGlobCalls(src, tmp)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("expected 2 replacements, got %d", n)
	}
	if !strings.Contains(got, `"./a/x.ts"`) || !strings.Contains(got, `"./b/y.ts"`) {
		t.Errorf("not all calls rewritten:\n%s", got)
	}
}

func mkdirP2(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func writeFile2(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
