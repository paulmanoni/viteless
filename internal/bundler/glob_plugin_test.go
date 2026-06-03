package bundler_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulmanoni/viteless/internal/bundler"
)

// TestImportMetaGlobPlugin_EndToEnd is the headline integration
// test: a TS entry uses `import.meta.glob('./pages/*.ts')`,
// and the bundled output contains the matched module paths as
// dynamic-import lazy loaders.
func TestImportMetaGlobPlugin_EndToEnd(t *testing.T) {
	tmp := t.TempDir()
	mustMkdirAllG(t, filepath.Join(tmp, "pages"))
	mustWriteG(t, filepath.Join(tmp, "pages", "Home.ts"), `export const name = "home";`)
	mustWriteG(t, filepath.Join(tmp, "pages", "About.ts"), `export const name = "about";`)
	mustWriteG(t, filepath.Join(tmp, "entry.ts"), `
		const pages = import.meta.glob('./pages/*.ts');
		for (const [path, loader] of Object.entries(pages)) {
			loader().then(m => console.log(path, m.name));
		}
	`)
	outDir := filepath.Join(tmp, "out")

	b := bundler.New()
	b.AddPlugin(bundler.NewImportMetaGlobPlugin())
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
	// The output should include BOTH matched modules. esbuild's
	// dynamic-import handling means each becomes its own chunk
	// OR is inlined — either way the names "home" and "about"
	// from the source modules must show up in the build.
	files, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	var allContent strings.Builder
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		body, _ := os.ReadFile(filepath.Join(outDir, f.Name()))
		allContent.Write(body)
	}
	out := allContent.String()
	if !strings.Contains(out, "home") {
		t.Errorf("missing home page content in output:\n%s", out)
	}
	if !strings.Contains(out, "about") {
		t.Errorf("missing about page content in output:\n%s", out)
	}
}

// TestImportMetaGlobPlugin_NoGlobsPassThrough: files without
// glob calls must build without any modification. Guards
// against the plugin accidentally rewriting innocent code.
func TestImportMetaGlobPlugin_NoGlobsPassThrough(t *testing.T) {
	tmp := t.TempDir()
	mustWriteG(t, filepath.Join(tmp, "entry.ts"), `
		const greeting = "hello, world";
		console.log(greeting);
	`)
	outDir := filepath.Join(tmp, "out")

	b := bundler.New()
	b.AddPlugin(bundler.NewImportMetaGlobPlugin())
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
	out, err := os.ReadFile(filepath.Join(outDir, "entry.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "hello, world") {
		t.Errorf("plain string lost in build:\n%s", out)
	}
}

func mustMkdirAllG(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWriteG(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
