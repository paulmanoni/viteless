package bundler_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulmanoni/viteless/internal/bundler"
)

// TestQuerySuffix_Raw: `./shader.glsl?raw` should inline the
// file's text as the default export. Used widely for shaders,
// SQL, markdown templates — anything you want as a string at
// runtime without a fetch.
func TestQuerySuffix_Raw(t *testing.T) {
	tmp := t.TempDir()
	const shaderSrc = "precision mediump float;\nvoid main() { gl_FragColor = vec4(1.0); }"
	mustWrite7(t, filepath.Join(tmp, "shader.glsl"), shaderSrc)
	mustWrite7(t, filepath.Join(tmp, "entry.ts"), `
		import src from "./shader.glsl?raw";
		console.log(src);
	`)
	outDir := filepath.Join(tmp, "out")
	b := bundler.New()
	b.AddPlugin(bundler.NewQuerySuffixPlugin())
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
	bundle := mustReadOutput(t, outDir, "entry.js")
	if !strings.Contains(bundle, "precision mediump float") {
		t.Errorf("expected raw shader source inlined, got:\n%s", bundle)
	}
}

// TestQuerySuffix_URL: `./image.png?url` should emit the file
// via the file loader and export the public URL — same as the
// default behavior for image extensions, but explicit + works
// for any extension.
func TestQuerySuffix_URL(t *testing.T) {
	tmp := t.TempDir()
	// Tiny valid PNG.
	pngBytes := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
		0x89, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E,
		0x44, 0xAE, 0x42, 0x60, 0x82,
	}
	mustWrite7(t, filepath.Join(tmp, "flag.png"), string(pngBytes))
	mustWrite7(t, filepath.Join(tmp, "entry.ts"), `
		import url from "./flag.png?url";
		document.body.dataset.flag = url;
	`)
	outDir := filepath.Join(tmp, "out")
	b := bundler.New()
	b.AddPlugin(bundler.NewQuerySuffixPlugin())
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
	// File loader should have emitted a hashed PNG alongside.
	entries, _ := os.ReadDir(outDir)
	var foundPNG bool
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "flag-") && strings.HasSuffix(e.Name(), ".png") {
			foundPNG = true
		}
	}
	if !foundPNG {
		t.Error("expected hashed flag-*.png emitted by file loader")
	}
}

// TestQuerySuffix_Inline: `./icon.svg?inline` should base64-
// encode the file + return as a data: URL.
func TestQuerySuffix_Inline(t *testing.T) {
	tmp := t.TempDir()
	mustWrite7(t, filepath.Join(tmp, "icon.svg"), `<svg xmlns="http://www.w3.org/2000/svg"/>`)
	mustWrite7(t, filepath.Join(tmp, "entry.ts"), `
		import dataURL from "./icon.svg?inline";
		document.body.dataset.icon = dataURL;
	`)
	outDir := filepath.Join(tmp, "out")
	b := bundler.New()
	b.AddPlugin(bundler.NewQuerySuffixPlugin())
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
	bundle := mustReadOutput(t, outDir, "entry.js")
	if !strings.Contains(bundle, "data:image/svg") {
		t.Errorf("expected data:image/svg* URL inlined, got:\n%s", bundle)
	}
	if !strings.Contains(bundle, "base64,") {
		t.Errorf("expected base64 encoding marker, got:\n%s", bundle)
	}
}

// TestQuerySuffix_SameFileDistinctSuffixes: importing the same
// file with two different suffixes should produce two distinct
// modules — esbuild's (path, namespace) keying separates them.
// Real-world: you might want the raw text AND a URL for the
// same asset.
func TestQuerySuffix_SameFileDistinctSuffixes(t *testing.T) {
	tmp := t.TempDir()
	mustWrite7(t, filepath.Join(tmp, "tmpl.html"), `<h1>Hello</h1>`)
	mustWrite7(t, filepath.Join(tmp, "entry.ts"), `
		import html from "./tmpl.html?raw";
		import url  from "./tmpl.html?url";
		document.body.innerHTML = html;
		fetch(url).then(r => r.text());
	`)
	outDir := filepath.Join(tmp, "out")
	b := bundler.New()
	b.AddPlugin(bundler.NewQuerySuffixPlugin())
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
	bundle := mustReadOutput(t, outDir, "entry.js")
	// Raw content should be inlined.
	if !strings.Contains(bundle, "Hello") {
		t.Errorf("expected raw HTML inlined, got:\n%s", bundle)
	}
	// File loader output should also exist for the ?url import.
	entries, _ := os.ReadDir(outDir)
	var foundHTML bool
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "tmpl-") && strings.HasSuffix(e.Name(), ".html") {
			foundHTML = true
		}
	}
	if !foundHTML {
		t.Error("expected hashed tmpl-*.html emitted for the ?url import")
	}
}

// TestQuerySuffix_NoSuffixUnchanged: imports without a
// recognized suffix should pass through. Guards against the
// plugin accidentally claiming all imports.
func TestQuerySuffix_NoSuffixUnchanged(t *testing.T) {
	tmp := t.TempDir()
	mustWrite7(t, filepath.Join(tmp, "lib.ts"), `export const greeting = "hello";`)
	mustWrite7(t, filepath.Join(tmp, "entry.ts"), `
		import { greeting } from "./lib";
		console.log(greeting);
	`)
	outDir := filepath.Join(tmp, "out")
	b := bundler.New()
	b.AddPlugin(bundler.NewQuerySuffixPlugin())
	res, err := b.Build(bundler.Options{
		Entries: []string{filepath.Join(tmp, "entry.ts")},
		OutDir:  outDir,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("plain imports must NOT be claimed by query plugin; errors: %v", res.Errors)
	}
}

func mustWrite7(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
