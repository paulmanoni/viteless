package fetcher

import (
	"bytes"
	"io"
	"os"
)

// bytesReader is a one-line helper for the store.Put API which
// takes an io.Reader; we keep it package-local instead of
// importing bytes everywhere fetcher.go uses it.
func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// osReadFile is wrapped via the readFileImpl indirection in
// fetcher.go so tests can stub it if needed. Currently the real
// path: just os.ReadFile.
func osReadFile(path string) ([]byte, error) { return os.ReadFile(path) }
