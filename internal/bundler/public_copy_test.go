package bundler_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/paulmanoni/viteless/internal/bundler"
)

// TestCopyPublicDir_RoundTripVerbatim: the headline contract —
// files copy with original names + bytes, ready to be served at
// the same paths the HTML <link>/<meta> tags reference.
func TestCopyPublicDir_RoundTripVerbatim(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "public")
	dst := filepath.Join(tmp, "out")

	// PWA icon, plus a manifest.json that references it by name
	// (so the copy MUST preserve the original filename, not
	// content-hash it).
	mustMkdirAll2(t, src)
	mustWritePNG(t, filepath.Join(src, "arm-192.png"), []byte("fake-png-bytes"))
	mustWritePNG(t, filepath.Join(src, "manifest.json"), []byte(`{"icons":[{"src":"/arm-192.png","sizes":"192x192"}]}`))

	n, err := bundler.CopyPublicDir(src, dst)
	if err != nil {
		t.Fatalf("CopyPublicDir: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 files copied, got %d", n)
	}
	// arm-192.png MUST land at /arm-192.png — no hash mangling.
	if _, err := os.Stat(filepath.Join(dst, "arm-192.png")); err != nil {
		t.Errorf("arm-192.png missing from output: %v", err)
	}
	// Bytes preserved.
	gotBytes, err := os.ReadFile(filepath.Join(dst, "arm-192.png"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotBytes) != "fake-png-bytes" {
		t.Errorf("PNG bytes corrupted in copy: got %q", string(gotBytes))
	}
}

// TestCopyPublicDir_PreservesSubdirs: nested public/ structure
// like /public/icons/foo.png → /out/icons/foo.png. Vite +
// CRA both preserve sub-paths verbatim.
func TestCopyPublicDir_PreservesSubdirs(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "public")
	dst := filepath.Join(tmp, "out")

	mustMkdirAll2(t, filepath.Join(src, "icons", "deep"))
	mustWritePNG(t, filepath.Join(src, "icons", "deep", "foo.png"), []byte("deep"))
	mustWritePNG(t, filepath.Join(src, "favicon.ico"), []byte("ico"))

	n, err := bundler.CopyPublicDir(src, dst)
	if err != nil {
		t.Fatalf("CopyPublicDir: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 files, got %d", n)
	}
	if _, err := os.Stat(filepath.Join(dst, "icons", "deep", "foo.png")); err != nil {
		t.Errorf("nested file not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "favicon.ico")); err != nil {
		t.Errorf("favicon not copied: %v", err)
	}
}

// TestCopyPublicDir_NoSrcDirIsFine: most projects don't have a
// public/ at all. Missing srcDir must NOT error — returns (0,
// nil) so the CLI can call us unconditionally.
func TestCopyPublicDir_NoSrcDirIsFine(t *testing.T) {
	tmp := t.TempDir()
	dst := filepath.Join(tmp, "out")
	// No src created.
	n, err := bundler.CopyPublicDir(filepath.Join(tmp, "nope"), dst)
	if err != nil {
		t.Errorf("expected nil error for missing src, got: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 files for missing src, got %d", n)
	}
}

// TestCopyPublicDir_OverwritesExisting: a stale file in dst
// must be overwritten by the public/ copy. This matters for
// dev rebuilds — if the operator updates a favicon, the new
// bytes should replace the old.
func TestCopyPublicDir_OverwritesExisting(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "public")
	dst := filepath.Join(tmp, "out")

	mustMkdirAll2(t, src)
	mustMkdirAll2(t, dst)
	mustWritePNG(t, filepath.Join(dst, "favicon.ico"), []byte("OLD"))
	mustWritePNG(t, filepath.Join(src, "favicon.ico"), []byte("NEW"))

	if _, err := bundler.CopyPublicDir(src, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "favicon.ico"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW" {
		t.Errorf("dst favicon not overwritten: got %q", string(got))
	}
}

func mustMkdirAll2(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWritePNG(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
