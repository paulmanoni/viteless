package bundler_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/paulmanoni/viteless/internal/bundler"
)

// TestTailwind_PlainCSSPassesThrough: the no-op contract.
// CSS files WITHOUT Tailwind directives must NOT touch the
// tailwindcss CLI — esbuild's native loader takes over and
// the bundle builds even when tailwindcss is missing.
//
// Critical: most projects don't use Tailwind. Registering the
// plugin must not penalize them.
func TestTailwind_PlainCSSPassesThrough(t *testing.T) {
	t.Setenv("PATH", "") // force tailwindcss "not found"

	tmp := t.TempDir()
	mustWrite5(t, filepath.Join(tmp, "styles.css"), `
		.btn { color: red; }
		.card { background: blue; }
	`)
	mustWrite5(t, filepath.Join(tmp, "entry.ts"), `
		import "./styles.css";
	`)

	b := bundler.New()
	b.AddPlugin(bundler.NewTailwindPlugin())
	res, err := b.Build(bundler.Options{
		Entries: []string{filepath.Join(tmp, "entry.ts")},
		OutDir:  filepath.Join(tmp, "out"),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("plain CSS must NOT require tailwindcss; got errors: %v", res.Errors)
	}
}

// TestTailwind_DirectivesWithoutCLIErrorActionable: when the
// CSS DOES contain @tailwind but tailwindcss isn't on PATH,
// the operator must see a clear "install tailwindcss" error
// naming the directives in their file — not esbuild's
// confusing "no loader configured" or worse, a silent build
// that ships @tailwind directives the browser ignores.
func TestTailwind_DirectivesWithoutCLIErrorActionable(t *testing.T) {
	t.Setenv("PATH", "")

	tmp := t.TempDir()
	mustWrite5(t, filepath.Join(tmp, "styles.css"), `
		@tailwind base;
		@tailwind components;
		@tailwind utilities;
	`)
	mustWrite5(t, filepath.Join(tmp, "entry.ts"), `
		import "./styles.css";
	`)

	b := bundler.New()
	b.AddPlugin(bundler.NewTailwindPlugin())
	res, err := b.Build(bundler.Options{
		Entries: []string{filepath.Join(tmp, "entry.ts")},
		OutDir:  filepath.Join(tmp, "out"),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) == 0 {
		t.Fatal("expected an error when @tailwind directives present + tailwindcss missing")
	}
	msg := res.Errors[0].Text
	if !strings.Contains(msg, "tailwindcss") {
		t.Errorf("error should name tailwindcss, got: %q", msg)
	}
	if !strings.Contains(msg, "@tailwind") && !strings.Contains(msg, "@apply") {
		t.Errorf("error should explain which directives triggered the lookup, got: %q", msg)
	}
}

// TestTailwind_CompilesViaSystemCLI: when tailwindcss IS on
// PATH, @tailwind directives must be processed + the resulting
// utility CSS must end up in the bundled output.
//
// Skipped when tailwindcss isn't available — the compile path
// needs the actual binary. Run locally after `brew install
// tailwindcss` to exercise the happy path.
func TestTailwind_CompilesViaSystemCLI(t *testing.T) {
	if !bundler.TailwindAvailable() {
		t.Skip("tailwindcss CLI not on PATH — `brew install tailwindcss` or skip this test")
	}
	if runtime.GOOS == "windows" {
		t.Skip("path quoting is finicky on windows; skip until needed")
	}
	tmp := t.TempDir()
	// Tailwind v4 entry: a CSS file that imports tailwindcss
	// + declares which source files to scan via @source. v4
	// dropped the JS config in favor of CSS-side configuration.
	// The plugin should detect the @import "tailwindcss"
	// marker and route through the CLI.
	mustWrite5(t, filepath.Join(tmp, "template.html"), `
		<div class="text-red-500 p-4">hello</div>
	`)
	mustWrite5(t, filepath.Join(tmp, "styles.css"), `
		@import "tailwindcss";
		@source "./template.html";
	`)
	mustWrite5(t, filepath.Join(tmp, "entry.ts"), `
		import "./styles.css";
	`)
	outDir := filepath.Join(tmp, "out")

	b := bundler.New()
	b.AddPlugin(bundler.NewTailwindPlugin())
	res, err := b.Build(bundler.Options{
		Entries: []string{filepath.Join(tmp, "entry.ts")},
		OutDir:  outDir,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("expected zero errors, got: %v", res.Errors)
	}
	cssBytes, err := os.ReadFile(filepath.Join(outDir, "entry.css"))
	if err != nil {
		t.Fatalf("read CSS output: %v", err)
	}
	css := string(cssBytes)
	// The template used `text-red-500` and `p-4` — both
	// utility classes Tailwind should have emitted.
	if !strings.Contains(css, "text-red-500") && !strings.Contains(css, "color:") {
		t.Errorf("compiled CSS missing tailwind output; got:\n%s", css)
	}
}

func mustWrite5(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
