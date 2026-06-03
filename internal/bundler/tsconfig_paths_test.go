package bundler_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulmanoni/viteless/internal/bundler"
)

// TestBundler_TSConfigPathsResolveAlias is the headline test:
// `import "@/utils/foo"` in a .ts entry should resolve to
// `src/utils/foo.ts` when tsconfig.json maps `"@/*"` →
// `"./src/*"`. The framework's job is just to plumb the
// tsconfig path through — esbuild handles the actual alias
// resolution.
func TestBundler_TSConfigPathsResolveAlias(t *testing.T) {
	tmp := t.TempDir()
	// Source tree: src/utils/foo.ts + an entry that imports it via "@/".
	mustMkdirAll(t, filepath.Join(tmp, "src", "utils"))
	mustWrite(t, filepath.Join(tmp, "src", "utils", "foo.ts"), `
		export const greeting = "hello";
	`)
	mustWrite(t, filepath.Join(tmp, "entry.ts"), `
		import { greeting } from "@/utils/foo";
		console.log(greeting);
	`)
	mustWrite(t, filepath.Join(tmp, "tsconfig.json"), `{
		"compilerOptions": {
			"baseUrl": ".",
			"paths": { "@/*": ["./src/*"] }
		}
	}`)

	outDir := filepath.Join(tmp, "out")
	b := bundler.New()
	res, err := b.Build(bundler.Options{
		Entries:  []string{filepath.Join(tmp, "entry.ts")},
		OutDir:   outDir,
		TSConfig: filepath.Join(tmp, "tsconfig.json"),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("expected zero build errors, got: %v", res.Errors)
	}
	bundle := mustReadOutput(t, outDir, "entry.js")
	if !strings.Contains(bundle, "hello") {
		t.Errorf("output bundle should inline the foo.ts greeting, got:\n%s", bundle)
	}
}

// TestBundler_NoTSConfigStillBundlesRelativeImports: when
// TSConfig is empty, plain relative imports must still work.
// Guards against accidentally requiring a tsconfig for the
// default code path.
func TestBundler_NoTSConfigStillBundlesRelativeImports(t *testing.T) {
	tmp := t.TempDir()
	mustWrite(t, filepath.Join(tmp, "foo.ts"), `export const x = 42;`)
	mustWrite(t, filepath.Join(tmp, "entry.ts"), `
		import { x } from "./foo";
		console.log(x);
	`)
	outDir := filepath.Join(tmp, "out")
	b := bundler.New()
	res, err := b.Build(bundler.Options{
		Entries: []string{filepath.Join(tmp, "entry.ts")},
		OutDir:  outDir,
		// TSConfig deliberately empty.
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("expected zero build errors, got: %v", res.Errors)
	}
}

// TestBundler_TSConfigAliasNotResolvedWithoutConfig: the inverse
// — the SAME `@/utils/foo` import that worked with tsconfig
// should FAIL when TSConfig is not provided. Guards that the
// tsconfig plumbing is actually load-bearing for the alias
// resolution (not just decorative).
func TestBundler_TSConfigAliasNotResolvedWithoutConfig(t *testing.T) {
	tmp := t.TempDir()
	mustMkdirAll(t, filepath.Join(tmp, "src", "utils"))
	mustWrite(t, filepath.Join(tmp, "src", "utils", "foo.ts"), `export const x = 42;`)
	mustWrite(t, filepath.Join(tmp, "entry.ts"), `
		import { x } from "@/utils/foo";
		console.log(x);
	`)
	// tsconfig.json deliberately absent.

	outDir := filepath.Join(tmp, "out")
	b := bundler.New()
	res, err := b.Build(bundler.Options{
		Entries: []string{filepath.Join(tmp, "entry.ts")},
		OutDir:  outDir,
	})
	if err != nil {
		t.Fatalf("Build invocation failed unexpectedly: %v", err)
	}
	if len(res.Errors) == 0 {
		t.Fatal("expected build to fail without tsconfig — @/ alias should be unresolved")
	}
}

// --- helpers ---

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustReadOutput(t *testing.T, outDir, name string) string {
	t.Helper()
	p := filepath.Join(outDir, name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}
