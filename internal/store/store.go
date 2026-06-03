// Package store implements a content-addressed disk cache for ESM
// files fetched from a registry (esm.sh by default). One cache is
// shared across every nexus project on the machine, so two projects
// depending on "vue@3.4.21" pay one download + one set of bytes on
// disk.
//
// Layout:
//
//	<root>/cas/<aa>/<bbbb...>          # content blob, named after sha256
//	<root>/url/<urlhash>               # url → blob mapping (a tiny file
//	                                     containing the content sha256)
//	<root>/url/<urlhash>.meta          # JSON metadata for the url
//
// The two-level shard on the cas key keeps any one directory under a
// few thousand entries even with tens of thousands of cached blobs;
// ext4/HFS+/APFS all handle that gracefully.
//
// Writes are atomic via temp-file-then-rename, so a torn write from a
// concurrent process never produces a partial blob. Concurrent
// readers/writers across processes are serialized per-key by an
// flock on a sentinel file (see lock.go).
//
// The store is intentionally not a generic kv store: it indexes by
// URL because that's what the fetcher resolves, and by content hash
// because that's what the lockfile pins. Callers Get by URL, Put by
// URL + content + hash.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Store is a handle to a disk cache rooted at a directory.
//
// All operations are safe for concurrent use within one process; the
// flock-based protocol additionally serializes writes across separate
// nexus invocations on the same machine. The struct itself carries no
// state besides the root path — open as many handles as you like.
type Store struct {
	root string
}

// Metadata is the small JSON sidecar persisted alongside the
// url → content-hash mapping. Carries everything the fetcher might
// want on the next lookup without re-reading the blob itself
// (Content-Type for dispatch, ETag for conditional refresh, the
// resolved URL after redirect-pinning, fetch time for staleness
// heuristics).
type Metadata struct {
	// URL is the original (pre-redirect) URL the caller asked for.
	// Stored explicitly because the hash is over URL bytes, and a
	// URL collision would shadow this entry — we want to detect it.
	URL string `json:"url"`

	// ResolvedURL is the post-redirect URL the bytes came from. For
	// esm.sh this is what version-pins a bare spec — "vue" redirects
	// to "vue@3.4.21" and that's what the lockfile records.
	ResolvedURL string `json:"resolved_url,omitempty"`

	// ContentSHA256 is the hex-encoded sha256 of the blob bytes.
	// Same value as the on-disk blob's directory key — duplicated
	// here so a metadata-only read tells you the blob path without
	// a second indirection.
	ContentSHA256 string `json:"content_sha256"`

	// ContentType is the Content-Type header from the fetch. Used
	// by the resolver to dispatch (.css vs .js vs .map etc).
	ContentType string `json:"content_type,omitempty"`

	// ETag is the server-supplied ETag if any, for conditional
	// refresh on future fetches.
	ETag string `json:"etag,omitempty"`
}

// New opens a Store rooted at root, creating the directory tree if
// it doesn't exist. The typical caller passes ~/.nexus/cache (see
// DefaultRoot below); tests pass a t.TempDir() to stay isolated.
//
// The directories are created with 0o755 so a project shared
// between users on the same machine (rare but legitimate) can still
// read entries another user wrote.
func New(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("store: root is empty")
	}
	for _, sub := range []string{"cas", "url", "lock"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			return nil, fmt.Errorf("store: mkdir %s: %w", sub, err)
		}
	}
	return &Store{root: root}, nil
}

// Root returns the cache root directory. Useful in error messages,
// dashboards, and the `nexus gc` command which walks the tree.
func (s *Store) Root() string { return s.root }

// DefaultRoot returns the shared on-disk cache root. It honors
// $VITELESS_CACHE when set, else ~/.viteless/cache, falling back to a
// temp dir when the home directory isn't resolvable (CI containers,
// etc.) — the cache still works, it just won't survive the process.
func DefaultRoot() string {
	if env := os.Getenv("VITELESS_CACHE"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "viteless-cache")
	}
	return filepath.Join(home, ".viteless", "cache")
}

// Get returns the disk path of the blob cached for url, plus the
// stored metadata. Returns (_, _, ErrNotCached) when the url has no
// entry yet — the caller (typically the fetcher) treats that as
// "go fetch from the registry".
//
// Get takes a shared lock for the duration of the read so a
// concurrent Put for the same url can't observe an intermediate
// state. Reads with no concurrent writers are effectively free —
// flock is a syscall, not a disk hit.
func (s *Store) Get(url string) (path string, meta Metadata, err error) {
	urlKey := hashURL(url)
	unlock, err := s.lockShared(urlKey)
	if err != nil {
		return "", Metadata{}, fmt.Errorf("store: lock %s: %w", url, err)
	}
	defer unlock()

	metaPath := s.metaPath(urlKey)
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", Metadata{}, ErrNotCached
		}
		return "", Metadata{}, fmt.Errorf("store: read meta %s: %w", url, err)
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return "", Metadata{}, fmt.Errorf("store: parse meta %s: %w", url, err)
	}
	blob := s.casPath(meta.ContentSHA256)
	if _, err := os.Stat(blob); err != nil {
		// Metadata pointing at a missing blob means a previous
		// half-completed Put or external tampering. Treat as
		// uncached so the fetcher refreshes.
		return "", Metadata{}, ErrNotCached
	}
	return blob, meta, nil
}

// Has returns true when the store has a blob with the given content
// hash. Used by `nexus install` to decide whether to fetch each
// lockfile entry — a project install against a warm shared cache
// should be a no-op for already-cached blobs.
func (s *Store) Has(contentSHA256 string) bool {
	if contentSHA256 == "" {
		return false
	}
	_, err := os.Stat(s.casPath(contentSHA256))
	return err == nil
}

// Put writes content under url + content-hash, creating both the
// content-addressed blob and the url → hash mapping atomically.
//
// hash is the caller-supplied expected sha256. If it doesn't match
// what we compute over content, Put fails with ErrIntegrityMismatch
// without writing anything to disk — defense against a registry
// serving different bytes than the lockfile pinned.
//
// Atomicity model:
//  1. Compute hash; if caller's expected hash diverges, abort.
//  2. Write blob to <cas>/<aa>/<bbbb...>.tmp, then rename in-place.
//  3. Write meta to <url>/<urlhash>.meta.tmp, then rename in-place.
//
// A crash between (2) and (3) leaves an orphan blob, which is
// harmless — `nexus gc` sweeps blobs no metadata references. A crash
// during (2) or (3) leaves a .tmp file the next Put or `nexus gc`
// removes.
func (s *Store) Put(url string, content io.Reader, expectedHash string, meta Metadata) (path string, err error) {
	urlKey := hashURL(url)
	unlock, err := s.lockExclusive(urlKey)
	if err != nil {
		return "", fmt.Errorf("store: lock %s: %w", url, err)
	}
	defer unlock()

	// Buffer content while hashing so we don't need to keep the
	// source around for a second pass. For typical ESM files
	// (single KBs to a few hundred KB) this is fine; if we ever
	// cache multi-MB CSS sprites we'll stream + rewind via a temp
	// file. For now: simplicity wins.
	buf, err := io.ReadAll(content)
	if err != nil {
		return "", fmt.Errorf("store: read content: %w", err)
	}
	gotHash := sha256Hex(buf)
	if expectedHash != "" && gotHash != expectedHash {
		return "", &IntegrityError{URL: url, Expected: expectedHash, Got: gotHash}
	}
	meta.ContentSHA256 = gotHash
	if meta.URL == "" {
		meta.URL = url
	}

	blobPath, err := s.writeBlobAtomic(gotHash, buf)
	if err != nil {
		return "", err
	}
	if err := s.writeMetaAtomic(urlKey, meta); err != nil {
		return "", err
	}
	return blobPath, nil
}

// Delete drops the url → blob mapping but does NOT delete the blob
// itself; another url entry may reference the same content hash
// (e.g. an unversioned URL and its resolved-version URL pointing at
// the same bytes). `nexus gc` collects orphan blobs separately.
func (s *Store) Delete(url string) error {
	urlKey := hashURL(url)
	unlock, err := s.lockExclusive(urlKey)
	if err != nil {
		return fmt.Errorf("store: lock %s: %w", url, err)
	}
	defer unlock()
	metaPath := s.metaPath(urlKey)
	if err := os.Remove(metaPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("store: remove meta %s: %w", url, err)
	}
	return nil
}

// EachBlob calls fn for every content-addressed blob in the cas
// tree, with the hex hash and the on-disk path. Used by `nexus gc`
// to enumerate candidates for collection. Walk order is unspecified
// — callers building a set should not assume sort order.
func (s *Store) EachBlob(fn func(hash, path string) error) error {
	root := filepath.Join(s.root, "cas")
	return filepath.Walk(root, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		// Reconstruct the hash from the two-level shard layout.
		// <root>/cas/<aa>/<bbbb...>  →  aa + bbbb...
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		parts := filepath.SplitList(rel)
		_ = parts
		// SplitList uses the OS PATH separator; use filepath split.
		dir, base := filepath.Split(rel)
		dir = filepath.Clean(dir)
		// Skip stray .tmp files left by an interrupted write.
		if filepath.Ext(base) == ".tmp" {
			return nil
		}
		hash := dir + base
		return fn(hash, p)
	})
}

// EachURL calls fn for every cached url → blob mapping, yielding the
// stored Metadata. `nexus gc` walks both EachURL (to mark referenced
// blobs) and EachBlob (to sweep unreferenced ones).
func (s *Store) EachURL(fn func(meta Metadata) error) error {
	root := filepath.Join(s.root, "url")
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".meta" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, e.Name()))
		if err != nil {
			return err
		}
		var meta Metadata
		if err := json.Unmarshal(raw, &meta); err != nil {
			return fmt.Errorf("store: parse meta %s: %w", e.Name(), err)
		}
		if err := fn(meta); err != nil {
			return err
		}
	}
	return nil
}

// --- internal layout helpers ---------------------------------------

func (s *Store) casPath(hash string) string {
	if len(hash) < 2 {
		// Defensive: pathological short hash. Real sha256 hex is
		// 64 chars; anything shorter is a programming error.
		return filepath.Join(s.root, "cas", "__bad__", hash)
	}
	return filepath.Join(s.root, "cas", hash[:2], hash[2:])
}

func (s *Store) metaPath(urlKey string) string {
	return filepath.Join(s.root, "url", urlKey+".meta")
}

// writeBlobAtomic does temp-file-then-rename in the same directory
// so the rename is guaranteed atomic by the OS (rename within a
// filesystem is one inode update).
func (s *Store) writeBlobAtomic(hash string, content []byte) (string, error) {
	dst := s.casPath(hash)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", fmt.Errorf("store: mkdir cas shard: %w", err)
	}
	// Already-present blob is a hit — return without rewriting. Two
	// concurrent Puts with the same hash converge on the same bytes
	// either way; skipping avoids needless IO and rename churn.
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("store: create temp blob: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("store: write temp blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("store: close temp blob: %w", err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("store: rename blob: %w", err)
	}
	return dst, nil
}

func (s *Store) writeMetaAtomic(urlKey string, meta Metadata) error {
	dst := s.metaPath(urlKey)
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal meta: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*.tmp")
	if err != nil {
		return fmt.Errorf("store: create temp meta: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("store: write temp meta: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("store: close temp meta: %w", err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("store: rename meta: %w", err)
	}
	return nil
}

// hashURL returns the hex-encoded sha256 of the URL bytes. Used as
// the filename for the url → blob mapping. URL collisions are
// astronomically improbable; we still store the original URL in the
// metadata so a hypothetical collision is detectable.
func hashURL(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:])
}

// sha256Hex returns the hex-encoded sha256 of content. Caller-facing
// integrity strings use this format (matching what npm and pnpm
// shrinkwrap encode in their lockfiles, modulo the "sha256-" prefix).
func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// --- errors --------------------------------------------------------

// ErrNotCached is returned by Get when the URL has no cache entry.
// Callers detect it with errors.Is(err, store.ErrNotCached).
var ErrNotCached = errors.New("store: not cached")

// IntegrityError is returned by Put when the caller-supplied
// expected hash doesn't match the sha256 of the bytes. Carries
// enough detail to diagnose: which URL, which hash we expected,
// which hash we got.
type IntegrityError struct {
	URL      string
	Expected string
	Got      string
}

func (e *IntegrityError) Error() string {
	return fmt.Sprintf("store: integrity mismatch for %s: expected sha256=%s, got sha256=%s",
		e.URL, e.Expected, e.Got)
}
