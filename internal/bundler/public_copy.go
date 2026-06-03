package bundler

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// CopyPublicDir copies every file under srcDir into dstDir
// verbatim — no content hashing, no rewriting, no minification.
// The convention every Vite/CRA/Next.js project uses: files in
// `public/` get served at the site root with their original
// names, so `<link rel="icon" href="/favicon.ico">` works
// against `public/favicon.ico` without any import.
//
// Distinction from the asset loader: the file loader rewrites
// imports into hashed copies (so `import flag from "./flag.png"`
// becomes `flag-A1B2C3.png` in both the bundle reference + the
// emitted file). That's right for code-referenced assets. But
// HTML-referenced assets (favicons, manifest.json, robots.txt,
// PWA icons referenced from manifest.json) need stable paths the
// HTML can hard-code — they're NOT imported by JS.
//
// Returns the count of files copied; useful for the "copied N
// files" notice in dev/build output. Missing srcDir is not an
// error (most projects don't have a public/ at all); the count
// is just 0 in that case.
//
// Each file is read into memory once and written out atomically
// (write to tmp + rename); for typical public/ contents (a few
// PNG icons + manifest.json, total under 1MB) this is
// negligible. Sub-directories are recursed into and preserved.
func CopyPublicDir(srcDir, dstDir string) (int, error) {
	info, err := os.Stat(srcDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("stat %s: %w", srcDir, err)
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("public path %s is not a directory", srcDir)
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return 0, fmt.Errorf("mkdir %s: %w", dstDir, err)
	}

	var copied int
	err = filepath.WalkDir(srcDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return fmt.Errorf("rel %s: %w", path, err)
		}
		dst := filepath.Join(dstDir, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		// Skip non-regular files (symlinks, devices). public/
		// dirs should be plain assets; non-regular entries
		// likely indicate something the operator didn't intend
		// to ship and silently copying them would surprise.
		if !d.Type().IsRegular() {
			return nil
		}
		if err := copyFile(path, dst); err != nil {
			return err
		}
		copied++
		return nil
	})
	return copied, err
}

// copyFile copies the bytes from src to dst, atomic via temp +
// rename. Preserves file mode from src so executable scripts in
// public/ (rare but possible) stay executable. Existing dst
// files are overwritten — public/ is the source of truth.
func copyFile(src, dst string) (retErr error) {
	in, err := os.Open(src) // #nosec G304 -- operator-supplied public path
	if err != nil {
		return fmt.Errorf("open src %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat src %s: %w", src, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".nexus-public-*")
	if err != nil {
		return fmt.Errorf("create tmp in %s: %w", filepath.Dir(dst), err)
	}
	tmpPath := tmp.Name()
	defer func() {
		// On any error, clean up the temp. On success the
		// rename below moved it.
		if retErr != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copy %s → %s: %w", src, dst, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tmp %s: %w", tmpPath, err)
	}
	if err := os.Chmod(tmpPath, info.Mode().Perm()); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return fmt.Errorf("rename %s → %s: %w", tmpPath, dst, err)
	}
	return nil
}
