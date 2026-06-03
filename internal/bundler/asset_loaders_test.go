package bundler_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulmanoni/viteless/internal/bundler"
)

// TestBundler_PNGImportEmitsHashedFile: the headline asset case.
// `import flag from "./flag.png"` should:
//
//   1. NOT error with "No loader is configured for .png"
//   2. Emit flag-<hash>.png into the outDir
//   3. The bundle should contain a string referencing the
//      emitted file (so the browser can fetch it)
//
// This is the file-loader contract: assets become URLs, not
// inlined bytes.
func TestBundler_PNGImportEmitsHashedFile(t *testing.T) {
	tmp := t.TempDir()
	// A tiny valid PNG (1x1 black pixel). Content doesn't
	// matter for the loader; what matters is the .png extension.
	pngBytes := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
		0x89, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9C, 0x62, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
		0x42, 0x60, 0x82,
	}
	mustWrite2(t, filepath.Join(tmp, "flag.png"), string(pngBytes))
	mustWrite2(t, filepath.Join(tmp, "entry.ts"), `
		import flag from "./flag.png";
		document.body.style.backgroundImage = "url(" + flag + ")";
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
		t.Fatalf("expected zero build errors, got: %v", res.Errors)
	}
	// Hashed PNG should be in outDir alongside the JS bundle.
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	var foundPNG bool
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "flag-") && strings.HasSuffix(e.Name(), ".png") {
			foundPNG = true
		}
	}
	if !foundPNG {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected a hashed flag-*.png in outDir, got: %v", names)
	}
}

// TestBundler_AssetReferencesAreRootAbsolute: SPA-routing
// safety. With deep routes like /invigilator-access/foo, a
// relative asset URL ("flag-HASH.png") would resolve to
// /invigilator-access/flag-HASH.png and 404. PublicPath="/"
// makes file-loader output absolute (/flag-HASH.png) so the
// browser fetches from the root regardless of route.
func TestBundler_AssetReferencesAreRootAbsolute(t *testing.T) {
	tmp := t.TempDir()
	mustWrite2(t, filepath.Join(tmp, "flag.svg"), `<svg xmlns="http://www.w3.org/2000/svg"/>`)
	mustWrite2(t, filepath.Join(tmp, "entry.ts"), `
		import flag from "./flag.svg";
		document.body.dataset.flag = flag;
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
		t.Fatalf("expected zero build errors, got: %v", res.Errors)
	}
	bundle := mustReadOutput(t, outDir, "entry.js")
	// The import value should be a root-absolute URL.
	if !strings.Contains(bundle, `"/flag-`) {
		t.Errorf("expected root-absolute /flag-HASH.svg in bundle (regression: SPA deep routes would 404), got:\n%s", bundle)
	}
}

// TestBundler_SVGAndFontsAreFileLoaded: parallel cases —
// SVG + woff2 should both go through the file loader, no
// loader-configuration errors.
func TestBundler_SVGAndFontsAreFileLoaded(t *testing.T) {
	tmp := t.TempDir()
	mustWrite2(t, filepath.Join(tmp, "icon.svg"), `<svg xmlns="http://www.w3.org/2000/svg"/>`)
	mustWrite2(t, filepath.Join(tmp, "font.woff2"), "fake-font-bytes")
	mustWrite2(t, filepath.Join(tmp, "entry.ts"), `
		import icon from "./icon.svg";
		import font from "./font.woff2";
		console.log(icon, font);
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
		t.Fatalf("expected zero build errors, got: %v", res.Errors)
	}
}

// TestBundler_JSONImportInlines: JSON should be loaded as a JS
// value, not emitted as a separate file (esbuild's default for
// the `json` loader). Confirms our loader map doesn't break the
// inline-JSON pattern that's common in config-style imports.
func TestBundler_JSONImportInlines(t *testing.T) {
	tmp := t.TempDir()
	mustWrite2(t, filepath.Join(tmp, "config.json"), `{"theme": "dark", "v": 42}`)
	mustWrite2(t, filepath.Join(tmp, "entry.ts"), `
		import cfg from "./config.json";
		console.log(cfg.theme, cfg.v);
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
		t.Fatalf("expected zero build errors, got: %v", res.Errors)
	}
	js, err := os.ReadFile(filepath.Join(outDir, "entry.js"))
	if err != nil {
		t.Fatal(err)
	}
	// Theme + version should appear inlined as JS literals.
	if !strings.Contains(string(js), "dark") || !strings.Contains(string(js), "42") {
		t.Errorf("expected JSON to inline as JS values, got:\n%s", string(js))
	}
}

func mustWrite2(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
