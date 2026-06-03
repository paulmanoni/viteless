package vue

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// loadFakeAdapter reads the minimal stand-in adapter bundle used by the
// pool tests so the Go ↔ QuickJS plumbing can be exercised without
// pulling in the real @vue/compiler-sfc bytes.
func loadFakeAdapter(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "fake-adapter.js"))
	if err != nil {
		t.Fatalf("loadFakeAdapter: %v", err)
	}
	return b
}

// fakeFactory builds compilers from the fake adapter for pool tests. It
// runs the WASM QuickJS backend (the only backend; the CGo one is gone).
func fakeFactory(t *testing.T) func() (SFCCompiler, error) {
	return func() (SFCCompiler, error) { return NewQJSCompiler(loadFakeAdapter(t), "fake-1.0.0") }
}

func TestNewPool_NilFactoryErrors(t *testing.T) {
	if _, err := NewPool(nil, 4); err == nil {
		t.Fatal("expected error on nil factory")
	}
}

func TestNewPool_FactoryErrorPropagates(t *testing.T) {
	boom := func() (SFCCompiler, error) { return nil, fmt.Errorf("boom") }
	if _, err := NewPool(boom, 4); err == nil {
		t.Fatal("expected error when factory fails")
	}
}

func TestNewPool_SizeClampedToOne(t *testing.T) {
	for _, size := range []int{0, -3} {
		p, err := NewPool(fakeFactory(t), size)
		if err != nil {
			t.Fatalf("NewPool(size=%d): %v", size, err)
		}
		if p.Size() != 1 {
			t.Errorf("size=%d clamped to %d, want 1", size, p.Size())
		}
		p.Close()
	}
}

func TestPool_SizeMatchesRequest(t *testing.T) {
	p, err := NewPool(fakeFactory(t), 4)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()
	if p.Size() != 4 {
		t.Errorf("Size() = %d, want 4", p.Size())
	}
}

func TestPool_CompileRoundTrip(t *testing.T) {
	p, err := NewPool(fakeFactory(t), 3)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()
	res, err := p.Compile(`<template><h1>Hi</h1></template>`, "Card.vue")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !strings.Contains(res.Code, "export default") {
		t.Errorf("Code missing export; got: %s", res.Code)
	}
	if !strings.Contains(res.Code, "Card.vue") {
		t.Errorf("filename not threaded through; got: %s", res.Code)
	}
}

// TestPool_ConcurrentCompilesDistinctFiles fires far more goroutines
// than the pool size at distinct sources and checks every result
// comes back correct — exercising the checkout/return free-list under
// contention without panics, deadlocks, or cross-talk between
// compilers.
func TestPool_ConcurrentCompilesDistinctFiles(t *testing.T) {
	p, err := NewPool(fakeFactory(t), 4)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()

	const N = 64
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)
	for i := range N {
		go func() {
			defer wg.Done()
			file := fmt.Sprintf("View%d.vue", i)
			res, cerr := p.Compile(`<template><p>x</p></template>`, file)
			if cerr != nil {
				errs <- fmt.Errorf("%s: %w", file, cerr)
				return
			}
			// The fake adapter echoes the filename into the output,
			// so a mismatch would prove a result got crossed between
			// goroutines.
			if !strings.Contains(res.Code, file) {
				errs <- fmt.Errorf("%s: result missing own filename: %s", file, res.Code)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

func TestPool_CloseIdempotent(t *testing.T) {
	p, err := NewPool(fakeFactory(t), 2)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	p.Close()
	p.Close() // must not panic or block
}

func TestPool_CompileAfterCloseErrors(t *testing.T) {
	p, err := NewPool(fakeFactory(t), 2)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	p.Close()
	if _, err := p.Compile(`<template><p>x</p></template>`, "Late.vue"); err == nil {
		t.Fatal("expected error compiling after Close")
	}
}
