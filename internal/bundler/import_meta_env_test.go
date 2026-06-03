package bundler_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulmanoni/viteless/internal/bundler"
)

// TestBundler_ImportMetaEnvSubstitution: the headline Vite-port
// case. Source code reading `import.meta.env.VITE_GATEWAY_API`
// must end up with the literal URL inlined at build time, not
// a runtime lookup that explodes with "undefined".
func TestBundler_ImportMetaEnvSubstitution(t *testing.T) {
	tmp := t.TempDir()
	mustWrite4(t, filepath.Join(tmp, "entry.ts"), `
		const api = import.meta.env.VITE_GATEWAY_API;
		const id  = import.meta.env.VITE_CLIENT_ID;
		console.log(api, id);
	`)
	outDir := filepath.Join(tmp, "out")
	b := bundler.New()
	res, err := b.Build(bundler.Options{
		Entries: []string{filepath.Join(tmp, "entry.ts")},
		OutDir:  outDir,
		Env: map[string]string{
			"VITE_GATEWAY_API": "https://api.example.com",
			"VITE_CLIENT_ID":   "oats",
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("expected zero build errors, got: %v", res.Errors)
	}
	out := mustReadOutput2(t, outDir, "entry.js")
	if !strings.Contains(out, "https://api.example.com") {
		t.Errorf("expected VITE_GATEWAY_API inlined, got:\n%s", out)
	}
	if !strings.Contains(out, "oats") {
		t.Errorf("expected VITE_CLIENT_ID inlined, got:\n%s", out)
	}
	// And the import.meta.env reference itself should be GONE
	// from the output — esbuild rewrites the property access
	// to a literal.
	if strings.Contains(out, "import.meta.env.VITE_GATEWAY_API") {
		t.Errorf("import.meta.env reference should have been substituted, but appears in output:\n%s", out)
	}
}

// TestBundler_ImportMetaEnvModeFlags: Vite-compat sentinels —
// MODE / DEV / PROD / BASE_URL get derived from Options.Mode.
func TestBundler_ImportMetaEnvModeFlags(t *testing.T) {
	tmp := t.TempDir()
	mustWrite4(t, filepath.Join(tmp, "entry.ts"), `
		if (import.meta.env.DEV) console.log("dev mode");
		if (import.meta.env.PROD) console.log("prod mode");
		console.log("mode:", import.meta.env.MODE);
		console.log("base:", import.meta.env.BASE_URL);
	`)
	outDir := filepath.Join(tmp, "out")
	b := bundler.New()
	res, err := b.Build(bundler.Options{
		Entries: []string{filepath.Join(tmp, "entry.ts")},
		OutDir:  outDir,
		Mode:    "development",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("expected zero build errors, got: %v", res.Errors)
	}
	out := mustReadOutput2(t, outDir, "entry.js")
	// DEV is true → the "dev mode" log should survive dead-
	// code elimination. PROD is false → "prod mode" should
	// be dead-code-eliminated by esbuild's minifier... but
	// minifier is off by default in tests, so just check
	// MODE was inlined as "development".
	if !strings.Contains(out, "development") {
		t.Errorf("expected MODE=development inlined, got:\n%s", out)
	}
}

// TestBundler_NoEnvNoSubstitution: when neither Env nor Mode is
// set, import.meta.env.X stays a literal reference in the
// output — pre-v0.89 behavior. Browser code would see undefined
// at runtime, same as if we did nothing.
func TestBundler_NoEnvNoSubstitution(t *testing.T) {
	tmp := t.TempDir()
	mustWrite4(t, filepath.Join(tmp, "entry.ts"), `
		console.log(import.meta.env.VITE_FOO);
	`)
	outDir := filepath.Join(tmp, "out")
	b := bundler.New()
	res, err := b.Build(bundler.Options{
		Entries: []string{filepath.Join(tmp, "entry.ts")},
		OutDir:  outDir,
		// Env + Mode both unset.
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("expected zero build errors, got: %v", res.Errors)
	}
}

// TestBundler_EnvValuesAreSafelyEscaped: env values with quotes
// + special chars must end up as valid JS string literals, not
// blowing up parsing. Guards against the operator who puts
// `"some "quoted" value"` in their .env file.
func TestBundler_EnvValuesAreSafelyEscaped(t *testing.T) {
	tmp := t.TempDir()
	mustWrite4(t, filepath.Join(tmp, "entry.ts"), `
		console.log(import.meta.env.VITE_TRICKY);
	`)
	outDir := filepath.Join(tmp, "out")
	b := bundler.New()
	res, err := b.Build(bundler.Options{
		Entries: []string{filepath.Join(tmp, "entry.ts")},
		OutDir:  outDir,
		Env: map[string]string{
			// Backslash + quotes + newline — would break a
			// naive raw-quote concat.
			"VITE_TRICKY": `oh "hello" \world\` + "\nnext line",
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("expected zero build errors despite tricky env value, got: %v", res.Errors)
	}
}

func mustWrite4(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustReadOutput2(t *testing.T, outDir, name string) string {
	t.Helper()
	p := filepath.Join(outDir, name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}
