package bundler

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulmanoni/viteless/internal/lockfile"
	"github.com/paulmanoni/viteless/internal/resolver"
	"github.com/paulmanoni/viteless/internal/store"
)

func TestBuild_RequiresEntries(t *testing.T) {
	b := New()
	_, err := b.Build(Options{})
	if err == nil || !strings.Contains(err.Error(), "entry") {
		t.Errorf("err = %v, want 'entry required'", err)
	}
}

func TestBuild_LockfileWithoutStoreErrors(t *testing.T) {
	b := New()
	_, err := b.Build(Options{
		Entries:  []string{"x"},
		Lockfile: lockfile.New(),
	})
	if err == nil || !strings.Contains(err.Error(), "Store") {
		t.Errorf("err = %v, want 'Store missing' message", err)
	}
}

// TestBuild_EndToEnd_ResolverFromStore is the headline smoke test:
// a user-supplied entry file imports "vue" by bare spec, the
// resolver plugin reads the cached blob from the store, esbuild
// bundles them together, and the output references the bundled
// vue stub by name.
//
// This is the "minimum proof that the whole pipeline works"
// before any UI/CLI gets wired up.
func TestBuild_EndToEnd_ResolverFromStore(t *testing.T) {
	tmp := t.TempDir()

	// 1. Set up store with a tiny vue stub blob.
	s, err := store.New(filepath.Join(tmp, "cache"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	vueBody := []byte(`export default { name: "vue-stub" };`)
	if _, err := s.Put("https://esm.sh/vue@3.4.21", bytes.NewReader(vueBody), "",
		store.Metadata{
			URL:         "https://esm.sh/vue@3.4.21",
			ResolvedURL: "https://esm.sh/vue@3.4.21",
			ContentType: "application/javascript",
		}); err != nil {
		t.Fatalf("store.Put: %v", err)
	}

	// 2. Lockfile pins vue → that URL.
	lf := lockfile.New()
	lf.Add(lockfile.Package{
		Spec:     "vue",
		Version:  "3.4.21",
		Resolved: "https://esm.sh/vue@3.4.21",
	})

	// 3. User entry imports vue by bare spec.
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(srcDir, "app.js")
	if err := os.WriteFile(entry, []byte(
		`import Vue from "vue";
console.log("hello", Vue.name);
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// 4. Wire resolver into bundler and build.
	plugin, err := resolver.New(resolver.Options{Lockfile: lf, Store: s})
	if err != nil {
		t.Fatalf("resolver.New: %v", err)
	}
	b := New()
	b.AddPlugin(plugin)

	outDir := filepath.Join(tmp, "out")
	res, err := b.Build(Options{
		Entries:  []string{entry},
		OutDir:   outDir,
		Lockfile: lf,
		Store:    s,
		Minify:   false,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("build errors: %v", res.Errors)
	}

	// 5. Verify the bundle contains the vue stub inlined.
	out, err := os.ReadFile(filepath.Join(outDir, "app.js"))
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	bundle := string(out)
	if !strings.Contains(bundle, "vue-stub") {
		t.Errorf("bundle missing vue-stub literal; got:\n%s", bundle)
	}
	if !strings.Contains(bundle, "hello") {
		t.Errorf("bundle missing user code; got:\n%s", bundle)
	}
}

// TestBuild_EndToEnd_FontAssetEmittedAsSidecar exercises the
// font-asset loader path: a JS module imports a .woff2 by bare
// spec, the resolver dispatches LoaderFile, esbuild copies the
// bytes to outdir + rewrites the import to a URL string.
//
// This is the proof that binary asset imports work end-to-end
// without npm/vite. Same shape any @fontsource-* package
// produces; the dashboard's @fontsource-variable/inter follows
// this exact pattern.
func TestBuild_EndToEnd_FontAssetEmittedAsSidecar(t *testing.T) {
	tmp := t.TempDir()

	s, err := store.New(filepath.Join(tmp, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	// 8 bytes that look like a woff2 magic header. Real woff2
	// is hundreds of KB; for the test the resolver doesn't care
	// about content correctness, only that the bytes round-trip
	// through esbuild's LoaderFile path without corruption.
	fontBytes := []byte{0x77, 0x4F, 0x46, 0x32, 0x00, 0x01, 0x00, 0x00}
	const fontURL = "https://esm.sh/@fontsource-variable/inter@5/files/inter-latin-wght-normal.woff2"
	if _, err := s.Put(fontURL, bytes.NewReader(fontBytes), "",
		store.Metadata{
			URL:         fontURL,
			ResolvedURL: fontURL,
			ContentType: "font/woff2",
		}); err != nil {
		t.Fatalf("seed font blob: %v", err)
	}

	lf := lockfile.New()
	lf.Add(lockfile.Package{
		Spec:        "@fontsource-variable/inter",
		Version:     "5.0.0",
		Resolved:    fontURL,
		ContentType: "font/woff2",
	})

	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(srcDir, "app.js")
	if err := os.WriteFile(entry, []byte(
		`import fontURL from "@fontsource-variable/inter";
const el = document.createElement("link");
el.rel = "preload";
el.href = fontURL;
document.head.appendChild(el);
`), 0o644); err != nil {
		t.Fatal(err)
	}

	plugin, err := resolver.New(resolver.Options{Lockfile: lf, Store: s})
	if err != nil {
		t.Fatalf("resolver.New: %v", err)
	}
	b := New()
	b.AddPlugin(plugin)

	outDir := filepath.Join(tmp, "out")
	res, err := b.Build(Options{
		Entries:  []string{entry},
		OutDir:   outDir,
		Lockfile: lf,
		Store:    s,
		Minify:   false,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("build errors: %v", res.Errors)
	}

	// Bundle contains a URL reference, NOT the inlined bytes.
	jsBundle, err := os.ReadFile(filepath.Join(outDir, "app.js"))
	if err != nil {
		t.Fatalf("read js bundle: %v", err)
	}
	if !strings.Contains(string(jsBundle), ".woff2") {
		t.Errorf("bundle should reference the woff2 asset by URL, got:\n%s", jsBundle)
	}
	// The woff2 magic bytes must NOT be inlined into the JS bundle
	// — a sign the loader was JS instead of File and the binary
	// got round-tripped through string parsing.
	if bytes.Contains(jsBundle, []byte{0x77, 0x4F, 0x46, 0x32}) {
		t.Errorf("bundle contains raw woff2 magic — loader dispatch was wrong")
	}

	// Locate the emitted sidecar font asset under outDir. esbuild
	// names it after the original basename + a content hash, so we
	// can't predict the exact filename, but every entry under
	// outDir matching *.woff2 should have our magic bytes.
	matches, err := filepath.Glob(filepath.Join(outDir, "*.woff2"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected one sidecar .woff2 in %s, got %v", outDir, matches)
	}
	emitted, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(emitted, fontBytes) {
		t.Errorf("sidecar font bytes corrupted; want %v, got %v", fontBytes, emitted)
	}
}

func TestBuild_DefaultsApplied(t *testing.T) {
	tmp := t.TempDir()
	entry := filepath.Join(tmp, "noop.js")
	if err := os.WriteFile(entry, []byte(`export const x = 1;`), 0o644); err != nil {
		t.Fatal(err)
	}
	b := New()
	res, err := b.Build(Options{
		Entries: []string{entry},
		OutDir:  filepath.Join(tmp, "out"),
		Minify:  false,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("errors: %v", res.Errors)
	}
	// File should exist at the requested OutDir/<entry-basename>.
	if _, err := os.Stat(filepath.Join(tmp, "out", "noop.js")); err != nil {
		t.Errorf("expected output file: %v", err)
	}
}

// TestBuild_SplittingEmitsChunkForDynamicImport proves that with
// Splitting on, code behind a dynamic import() is hoisted into a
// separate chunk under chunks/ rather than inlined into the entry —
// the behavior that makes route-level lazy loading shrink the initial
// payload.
func TestBuild_SplittingEmitsChunkForDynamicImport(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "lazy.js"),
		[]byte(`export const heavy = "lazy-only-payload";`), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(tmp, "main.js")
	if err := os.WriteFile(entry,
		[]byte(`export async function go(){ return (await import("./lazy.js")).heavy; }`), 0o644); err != nil {
		t.Fatal(err)
	}

	b := New()
	outDir := filepath.Join(tmp, "out")
	res, err := b.Build(Options{
		Entries:   []string{entry},
		OutDir:    outDir,
		Minify:    false,
		Splitting: true,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("errors: %v", res.Errors)
	}

	// Entry must still land at its base name (the index rewriter
	// keys off this).
	mainJS, err := os.ReadFile(filepath.Join(outDir, "main.js"))
	if err != nil {
		t.Fatalf("read entry: %v", err)
	}
	// A chunk dir must exist and the lazy payload must live there,
	// not in the entry.
	chunkEntries, err := os.ReadDir(filepath.Join(outDir, "chunks"))
	if err != nil {
		t.Fatalf("expected chunks/ dir: %v", err)
	}
	if len(chunkEntries) == 0 {
		t.Fatal("chunks/ dir is empty; splitting produced no chunk")
	}
	if strings.Contains(string(mainJS), "lazy-only-payload") {
		t.Error("lazy payload was inlined into the entry; splitting did not hoist it")
	}
}

// TestBuild_NoSplittingKeepsSingleFile is the control: the same
// dynamic-import source with Splitting off must NOT create a chunks/
// directory (esbuild inlines or keeps a single output).
func TestBuild_NoSplittingKeepsSingleFile(t *testing.T) {
	tmp := t.TempDir()
	entry := filepath.Join(tmp, "main.js")
	if err := os.WriteFile(entry,
		[]byte(`export const x = 1;`), 0o644); err != nil {
		t.Fatal(err)
	}
	b := New()
	outDir := filepath.Join(tmp, "out")
	res, err := b.Build(Options{
		Entries: []string{entry},
		OutDir:  outDir,
		Minify:  false,
		// Splitting defaults to false.
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("errors: %v", res.Errors)
	}
	if _, err := os.Stat(filepath.Join(outDir, "chunks")); !os.IsNotExist(err) {
		t.Errorf("unexpected chunks/ dir with splitting off (err=%v)", err)
	}
}
