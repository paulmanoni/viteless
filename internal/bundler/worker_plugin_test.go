package bundler_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulmanoni/viteless/internal/bundler"
)

// TestWorker_BasicSuffix: the headline case. `import W from
// './heavy.ts?worker'` should:
//  1. Emit a separate worker bundle into outDir (heavy.worker-HASH.js)
//  2. Generate a Worker subclass in the main bundle pointing at it
//  3. The main bundle's `new W()` should construct the worker URL
func TestWorker_BasicSuffix(t *testing.T) {
	tmp := t.TempDir()
	mustWrite8(t, filepath.Join(tmp, "heavy.ts"), `
		self.onmessage = (e) => {
			const n = e.data;
			let sum = 0;
			for (let i = 0; i < n; i++) sum += i;
			postMessage(sum);
		};
	`)
	mustWrite8(t, filepath.Join(tmp, "entry.ts"), `
		import HeavyWorker from "./heavy.ts?worker";
		const w = new HeavyWorker();
		w.postMessage(1000);
		w.onmessage = (e) => console.log("sum:", e.data);
	`)
	outDir := filepath.Join(tmp, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	b := bundler.New()
	b.AddPlugin(bundler.NewWorkerPlugin(bundler.WorkerPluginOptions{
		OutDir:     outDir,
		PublicPath: "/",
	}))
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
	// Worker bundle should exist alongside the main bundle.
	entries, _ := os.ReadDir(outDir)
	var workerFile string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "heavy.worker-") && strings.HasSuffix(e.Name(), ".js") {
			workerFile = e.Name()
		}
	}
	if workerFile == "" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected heavy.worker-*.js in outDir, got: %v", names)
	}
	// Main bundle should reference the worker URL.
	main := mustReadOutput(t, outDir, "entry.js")
	if !strings.Contains(main, workerFile) {
		t.Errorf("main bundle should reference %q, got:\n%s", workerFile, main)
	}
	// Main bundle should also include the Worker subclass scaffolding.
	if !strings.Contains(main, "extends Worker") {
		t.Errorf("main bundle should declare a Worker subclass, got:\n%s", main)
	}
	// Worker bundle itself should contain the worker source body.
	workerJS, err := os.ReadFile(filepath.Join(outDir, workerFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(workerJS), "postMessage") {
		t.Errorf("worker bundle missing source body, got:\n%s", workerJS)
	}
}

// TestWorker_RespectsPublicPath: when PublicPath is /admin/,
// the runtime should construct the worker URL with that prefix
// so it resolves under sub-path deployments.
func TestWorker_RespectsPublicPath(t *testing.T) {
	tmp := t.TempDir()
	mustWrite8(t, filepath.Join(tmp, "w.ts"), `self.onmessage = (e) => postMessage(e.data);`)
	mustWrite8(t, filepath.Join(tmp, "entry.ts"), `
		import W from "./w.ts?worker";
		new W();
	`)
	outDir := filepath.Join(tmp, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	b := bundler.New()
	b.AddPlugin(bundler.NewWorkerPlugin(bundler.WorkerPluginOptions{
		OutDir:     outDir,
		PublicPath: "/admin/",
	}))
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
	main := mustReadOutput(t, outDir, "entry.js")
	if !strings.Contains(main, "/admin/w.worker-") {
		t.Errorf("expected /admin/-prefixed worker URL, got:\n%s", main)
	}
}

// TestWorker_RejectsBareSpecs: `?worker` on a bare spec (no
// leading ./) should error with an actionable message rather
// than guessing at npm package resolution.
func TestWorker_RejectsBareSpecs(t *testing.T) {
	tmp := t.TempDir()
	mustWrite8(t, filepath.Join(tmp, "entry.ts"), `
		import W from "some-package?worker";
		new W();
	`)
	outDir := filepath.Join(tmp, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	b := bundler.New()
	b.AddPlugin(bundler.NewWorkerPlugin(bundler.WorkerPluginOptions{
		OutDir:     outDir,
		PublicPath: "/",
	}))
	res, err := b.Build(bundler.Options{
		Entries: []string{filepath.Join(tmp, "entry.ts")},
		OutDir:  outDir,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) == 0 {
		t.Fatal("expected error for bare-spec ?worker import")
	}
	if !strings.Contains(res.Errors[0].Text, "relative path") {
		t.Errorf("error should mention relative-path requirement, got: %q", res.Errors[0].Text)
	}
}

// TestWorker_DedupesIdenticalImports: importing the same
// worker file from two places should emit ONE bundle, not two.
// The plugin's instance-level cache keys by source path.
func TestWorker_DedupesIdenticalImports(t *testing.T) {
	tmp := t.TempDir()
	mustWrite8(t, filepath.Join(tmp, "w.ts"), `self.onmessage = (e) => postMessage(e.data);`)
	mustWrite8(t, filepath.Join(tmp, "a.ts"), `
		import W from "./w.ts?worker";
		export const a = new W();
	`)
	mustWrite8(t, filepath.Join(tmp, "b.ts"), `
		import W from "./w.ts?worker";
		export const b = new W();
	`)
	mustWrite8(t, filepath.Join(tmp, "entry.ts"), `
		import { a } from "./a";
		import { b } from "./b";
		console.log(a, b);
	`)
	outDir := filepath.Join(tmp, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	b := bundler.New()
	b.AddPlugin(bundler.NewWorkerPlugin(bundler.WorkerPluginOptions{
		OutDir:     outDir,
		PublicPath: "/",
	}))
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
	// Count worker bundles.
	entries, _ := os.ReadDir(outDir)
	var workerCount int
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "w.worker-") && strings.HasSuffix(e.Name(), ".js") {
			workerCount++
		}
	}
	if workerCount != 1 {
		t.Errorf("expected 1 worker bundle (dedup); got %d", workerCount)
	}
}

func mustWrite8(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
