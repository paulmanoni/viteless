package bundler_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulmanoni/viteless/internal/bundler"
)

// TestBundler_PublicPath_CustomPrefix: assets emitted via the
// file loader should respect a custom PublicPath. Operators
// deploying under /admin/ need /admin/foo-HASH.png references
// in the bundle, not /foo-HASH.png.
func TestBundler_PublicPath_CustomPrefix(t *testing.T) {
	tmp := t.TempDir()
	mustWrite6(t, filepath.Join(tmp, "flag.svg"), `<svg xmlns="http://www.w3.org/2000/svg"/>`)
	mustWrite6(t, filepath.Join(tmp, "entry.ts"), `
		import flag from "./flag.svg";
		document.body.dataset.flag = flag;
	`)
	outDir := filepath.Join(tmp, "out")
	b := bundler.New()
	res, err := b.Build(bundler.Options{
		Entries:    []string{filepath.Join(tmp, "entry.ts")},
		OutDir:     outDir,
		PublicPath: "/admin/",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("expected zero errors, got: %v", res.Errors)
	}
	bundle := mustReadOutput(t, outDir, "entry.js")
	if !strings.Contains(bundle, `"/admin/flag-`) {
		t.Errorf("expected /admin/flag-HASH.svg in bundle, got:\n%s", bundle)
	}
}

// TestBundler_PublicPath_DefaultStillRoot: when PublicPath is
// empty (the common case), assets stay root-absolute. Guards
// against accidental regression in the default behavior.
func TestBundler_PublicPath_DefaultStillRoot(t *testing.T) {
	tmp := t.TempDir()
	mustWrite6(t, filepath.Join(tmp, "icon.svg"), `<svg xmlns="http://www.w3.org/2000/svg"/>`)
	mustWrite6(t, filepath.Join(tmp, "entry.ts"), `
		import icon from "./icon.svg";
		console.log(icon);
	`)
	outDir := filepath.Join(tmp, "out")
	b := bundler.New()
	if _, err := b.Build(bundler.Options{
		Entries: []string{filepath.Join(tmp, "entry.ts")},
		OutDir:  outDir,
		// PublicPath omitted — should default to "/".
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	bundle := mustReadOutput(t, outDir, "entry.js")
	if !strings.Contains(bundle, `"/icon-`) {
		t.Errorf("expected /icon-HASH.svg default, got:\n%s", bundle)
	}
}

// TestBundler_PublicPath_FedToBaseURL: import.meta.env.BASE_URL
// should match the PublicPath operators set. Vite-port code
// often uses BASE_URL to build absolute fetch URLs; we must
// stay consistent with what the file loader emits.
func TestBundler_PublicPath_FedToBaseURL(t *testing.T) {
	tmp := t.TempDir()
	mustWrite6(t, filepath.Join(tmp, "entry.ts"), `
		console.log("base:", import.meta.env.BASE_URL);
	`)
	outDir := filepath.Join(tmp, "out")
	b := bundler.New()
	res, err := b.Build(bundler.Options{
		Entries:    []string{filepath.Join(tmp, "entry.ts")},
		OutDir:     outDir,
		Mode:       "production",
		PublicPath: "/v2/",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("expected zero errors, got: %v", res.Errors)
	}
	bundle := mustReadOutput(t, outDir, "entry.js")
	if !strings.Contains(bundle, `"/v2/"`) {
		t.Errorf("expected BASE_URL to be substituted with PublicPath, got:\n%s", bundle)
	}
}

// TestBundler_CSSModules_ScopedClassNames: importing a
// .module.css file should yield an object of HASHED class
// names, not the original short names. Plain .css files stay
// globally scoped (next test guards that).
func TestBundler_CSSModules_ScopedClassNames(t *testing.T) {
	tmp := t.TempDir()
	mustWrite6(t, filepath.Join(tmp, "Button.module.css"), `
		.primary { color: red; }
		.large { font-size: 2em; }
	`)
	mustWrite6(t, filepath.Join(tmp, "entry.ts"), `
		import styles from "./Button.module.css";
		document.body.dataset.cls = styles.primary;
	`)
	outDir := filepath.Join(tmp, "out")
	b := bundler.New()
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
	// The CSS output should contain hashed/suffixed class
	// names. esbuild's local-css loader stamps a content-hash
	// suffix (e.g. `.primary_<hash>`) so different modules'
	// `.primary` don't collide.
	css := mustReadOutput(t, outDir, "entry.css")
	if strings.Contains(css, ".primary {") {
		t.Errorf("CSS modules should rename .primary, but plain selector found:\n%s", css)
	}
	if !strings.Contains(css, "primary") {
		t.Errorf("CSS modules output should still mention 'primary' (with hash suffix):\n%s", css)
	}
}

// TestBundler_PlainCSSStaysGlobal: regression — `.css` files
// (no .module suffix) must keep their selectors un-renamed.
// Vuetify's `vuetify/styles` etc. break if every selector
// gets hashed.
func TestBundler_PlainCSSStaysGlobal(t *testing.T) {
	tmp := t.TempDir()
	mustWrite6(t, filepath.Join(tmp, "global.css"), `
		.my-button { color: blue; }
	`)
	mustWrite6(t, filepath.Join(tmp, "entry.ts"), `
		import "./global.css";
	`)
	outDir := filepath.Join(tmp, "out")
	b := bundler.New()
	if _, err := b.Build(bundler.Options{
		Entries: []string{filepath.Join(tmp, "entry.ts")},
		OutDir:  outDir,
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	css := mustReadOutput(t, outDir, "entry.css")
	if !strings.Contains(css, ".my-button") {
		t.Errorf("plain CSS must preserve original selectors, got:\n%s", css)
	}
}

func mustWrite6(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
