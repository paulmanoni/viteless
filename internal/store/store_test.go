package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// newTestStore returns a store rooted under t.TempDir() — disposed
// when the test ends, so concurrent test runs don't share cache
// state.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// hashOf is the helper the tests use to compute the integrity hash
// they pass into Put; mirrors store.sha256Hex but kept separate so
// a regression in the production helper would still trigger a test
// failure rather than silently agreeing with itself.
func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestStore_New_CreatesRootStructure(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, sub := range []string{"cas", "url", "lock"} {
		if _, err := os.Stat(filepath.Join(s.Root(), sub)); err != nil {
			t.Errorf("expected %s subdir, got error: %v", sub, err)
		}
	}
}

func TestStore_New_EmptyRootIsError(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Error("expected error for empty root")
	}
}

func TestStore_PutGet_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	body := []byte(`export const x = 42;`)
	hash := hashOf(body)

	path, err := s.Put("https://esm.sh/vue@3.4.21", bytes.NewReader(body), hash, Metadata{
		ResolvedURL: "https://esm.sh/vue@3.4.21",
		ContentType: "application/javascript",
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("blob bytes mismatch")
	}

	gotPath, meta, err := s.Get("https://esm.sh/vue@3.4.21")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotPath != path {
		t.Errorf("Get path = %s, want %s", gotPath, path)
	}
	if meta.ContentSHA256 != hash {
		t.Errorf("meta hash = %s, want %s", meta.ContentSHA256, hash)
	}
	if meta.ResolvedURL != "https://esm.sh/vue@3.4.21" {
		t.Errorf("resolved url = %q, want %q", meta.ResolvedURL, "https://esm.sh/vue@3.4.21")
	}
	if meta.URL != "https://esm.sh/vue@3.4.21" {
		t.Errorf("url echoed back = %q", meta.URL)
	}
}

func TestStore_Get_NotCachedReturnsSentinel(t *testing.T) {
	s := newTestStore(t)
	_, _, err := s.Get("https://esm.sh/never-fetched")
	if !errors.Is(err, ErrNotCached) {
		t.Errorf("err = %v, want ErrNotCached", err)
	}
}

func TestStore_Get_MissingBlobBehavesAsNotCached(t *testing.T) {
	// Simulate a torn cache: metadata exists but the blob is gone
	// (manual disk tampering, an interrupted gc, etc.). Get must
	// degrade to ErrNotCached so the fetcher re-fills.
	s := newTestStore(t)
	body := []byte(`x`)
	hash := hashOf(body)
	_, err := s.Put("https://x", bytes.NewReader(body), hash, Metadata{})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Surgically remove the blob.
	blob := s.casPath(hash)
	if err := os.Remove(blob); err != nil {
		t.Fatalf("remove blob: %v", err)
	}
	_, _, err = s.Get("https://x")
	if !errors.Is(err, ErrNotCached) {
		t.Errorf("torn cache Get err = %v, want ErrNotCached", err)
	}
}

func TestStore_Put_IntegrityMismatchAborts(t *testing.T) {
	s := newTestStore(t)
	body := []byte(`real bytes`)
	bogusHash := strings.Repeat("0", 64)

	_, err := s.Put("https://x", bytes.NewReader(body), bogusHash, Metadata{})
	var ie *IntegrityError
	if !errors.As(err, &ie) {
		t.Fatalf("err type = %T, want *IntegrityError", err)
	}
	if ie.Expected != bogusHash {
		t.Errorf("ie.Expected = %s", ie.Expected)
	}
	if ie.Got == "" {
		t.Errorf("ie.Got should carry the actual hash")
	}
	// No blob should exist after the rejected Put.
	if _, _, err := s.Get("https://x"); !errors.Is(err, ErrNotCached) {
		t.Errorf("post-mismatch Get should be ErrNotCached, got %v", err)
	}
}

func TestStore_Put_EmptyExpectedHashSkipsVerification(t *testing.T) {
	// When the caller (e.g. first-time fetch with no lockfile entry)
	// doesn't know the hash yet, an empty expectedHash bypasses the
	// integrity check. The computed hash still lands in metadata so
	// the lockfile can pin it after the write.
	s := newTestStore(t)
	body := []byte(`first fetch`)
	path, err := s.Put("https://x", bytes.NewReader(body), "", Metadata{})
	if err != nil {
		t.Fatalf("Put with empty hash: %v", err)
	}
	if path == "" {
		t.Error("Put returned empty path")
	}
	_, meta, err := s.Get("https://x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if meta.ContentSHA256 != hashOf(body) {
		t.Errorf("auto-computed hash mismatch")
	}
}

func TestStore_Put_TwoURLsSameBytesShareBlob(t *testing.T) {
	// Critical correctness property: two cached URLs for the same
	// content (e.g. /vue and /vue@3.4.21 redirect-resolved to the
	// same bytes) point to one blob on disk. Saves space and makes
	// gc reachability analysis tractable.
	s := newTestStore(t)
	body := []byte(`shared content`)
	hash := hashOf(body)
	p1, err := s.Put("https://esm.sh/vue", bytes.NewReader(body), hash, Metadata{})
	if err != nil {
		t.Fatalf("Put 1: %v", err)
	}
	p2, err := s.Put("https://esm.sh/vue@3.4.21", bytes.NewReader(body), hash, Metadata{})
	if err != nil {
		t.Fatalf("Put 2: %v", err)
	}
	if p1 != p2 {
		t.Errorf("same content → different blob paths: %s vs %s", p1, p2)
	}
}

func TestStore_Has_TrueIffBlobExists(t *testing.T) {
	s := newTestStore(t)
	body := []byte(`b`)
	hash := hashOf(body)
	if s.Has(hash) {
		t.Error("Has returned true before Put")
	}
	if _, err := s.Put("https://x", bytes.NewReader(body), hash, Metadata{}); err != nil {
		t.Fatal(err)
	}
	if !s.Has(hash) {
		t.Error("Has returned false after Put")
	}
}

func TestStore_Delete_RemovesMetaKeepsBlob(t *testing.T) {
	// Delete only drops the url → blob index; the blob may still be
	// referenced by another url. gc is what reclaims orphan blobs.
	s := newTestStore(t)
	body := []byte(`d`)
	hash := hashOf(body)
	if _, err := s.Put("https://x", bytes.NewReader(body), hash, Metadata{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("https://x"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := s.Get("https://x"); !errors.Is(err, ErrNotCached) {
		t.Errorf("post-Delete Get err = %v, want ErrNotCached", err)
	}
	if !s.Has(hash) {
		t.Error("blob should survive Delete (gc reclaims it)")
	}
}

func TestStore_EachURL_VisitsAllStoredMetas(t *testing.T) {
	s := newTestStore(t)
	urls := []string{
		"https://esm.sh/vue",
		"https://esm.sh/react",
		"https://esm.sh/lodash",
	}
	for _, u := range urls {
		body := []byte(u) // distinct bytes per url
		if _, err := s.Put(u, bytes.NewReader(body), hashOf(body), Metadata{ResolvedURL: u}); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	err := s.EachURL(func(meta Metadata) error {
		seen[meta.URL] = true
		return nil
	})
	if err != nil {
		t.Fatalf("EachURL: %v", err)
	}
	for _, u := range urls {
		if !seen[u] {
			t.Errorf("EachURL missed %s", u)
		}
	}
}

func TestStore_EachBlob_VisitsAllBlobs(t *testing.T) {
	s := newTestStore(t)
	bodies := [][]byte{[]byte(`a`), []byte(`bb`), []byte(`ccc`)}
	want := map[string]bool{}
	for i, body := range bodies {
		want[hashOf(body)] = true
		if _, err := s.Put("https://x/"+string(rune('a'+i)), bytes.NewReader(body), hashOf(body), Metadata{}); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	err := s.EachBlob(func(hash, _ string) error {
		seen[hash] = true
		return nil
	})
	if err != nil {
		t.Fatalf("EachBlob: %v", err)
	}
	for h := range want {
		if !seen[h] {
			t.Errorf("EachBlob missed %s", h[:12])
		}
	}
}

func TestStore_ConcurrentPuts_AreSerialized(t *testing.T) {
	// Stress the locking: 32 goroutines all try to Put the same URL
	// with the same content. Exactly one set of bytes should land,
	// no torn writes, no IntegrityError surprise.
	s := newTestStore(t)
	body := []byte(`concurrent`)
	hash := hashOf(body)

	const N = 32
	var wg sync.WaitGroup
	wg.Add(N)
	errCh := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			if _, err := s.Put("https://x", bytes.NewReader(body), hash, Metadata{}); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent Put: %v", err)
	}

	// Final state: one blob, one metadata, consistent Get.
	_, meta, err := s.Get("https://x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if meta.ContentSHA256 != hash {
		t.Errorf("final meta hash mismatch")
	}
}
