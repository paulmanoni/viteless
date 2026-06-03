package store

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// lockShared and lockExclusive serialize concurrent Get/Put calls
// across processes (one user runs `nexus install` while another
// runs `nexus add` in a sibling project sharing the same
// ~/.nexus/cache).
//
// We use a per-key sentinel file under <root>/lock/<urlhash>.lock
// rather than one global lock so unrelated URLs don't block each
// other. flock is advisory but every nexus process plays by the
// same rules, so the cooperation is enough.
//
// Both helpers return an unlock func the caller must defer. The
// returned func is safe to call multiple times — the second call
// is a no-op.

func (s *Store) lockPath(urlKey string) string {
	return filepath.Join(s.root, "lock", urlKey+".lock")
}

func (s *Store) lockShared(urlKey string) (unlock func(), err error) {
	return s.lock(urlKey, unix.LOCK_SH)
}

func (s *Store) lockExclusive(urlKey string) (unlock func(), err error) {
	return s.lock(urlKey, unix.LOCK_EX)
}

// lock opens (creating if needed) the per-key sentinel file and
// applies the requested flock mode. Caller's unlock func releases
// the lock and closes the file.
//
// Why a file we keep open vs a path we re-stat: flock locks are
// associated with file descriptors, not paths. Closing the file
// drops the lock, which is exactly the cleanup we want; the file
// itself is harmless to leave on disk between sessions and is
// reused by every future call for the same URL.
func (s *Store) lock(urlKey string, mode int) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(s.lockPath(urlKey)), 0o755); err != nil {
		return nil, fmt.Errorf("store: mkdir lock dir: %w", err)
	}
	f, err := os.OpenFile(s.lockPath(urlKey), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("store: open lock: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), mode); err != nil {
		f.Close()
		return nil, fmt.Errorf("store: flock: %w", err)
	}
	var released bool
	return func() {
		if released {
			return
		}
		released = true
		// Best-effort release; the kernel drops the lock anyway
		// when the fd closes, so a Flock(LOCK_UN) error here is
		// not worth surfacing.
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}
