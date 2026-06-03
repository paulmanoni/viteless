package vue

import (
	"fmt"
	"sync"
)

// Pool is a fixed-size set of SFCCompilers that compiles .vue files
// concurrently. A single compiler serializes every Compile call (the
// CGo *Compiler on its one QuickJS worker thread; the WASM
// *QJSCompiler on a mutex), so a Vue-heavy build — where esbuild fans
// OnLoad callbacks across all its worker goroutines — bottlenecks on
// that one interpreter. A Pool removes the bottleneck by holding N
// independent compilers and handing each incoming Compile to a free
// one, bounding concurrency at the pool size.
//
// Pool is backend-agnostic: NewPool takes a factory, so it pools
// either the CGo or the WASM backend. Pool itself satisfies
// SFCCompiler, so Plugin accepts it directly.
//
// Memory cost scales linearly: each compiler parses the ~850 KB
// @vue/compiler-sfc bundle into its own runtime, so a pool of N holds
// N copies. Callers pick N as a CPU/memory trade-off — see the CLI's
// sizing helper.
type Pool struct {
	compilers []SFCCompiler
	// idle is a buffered channel used as a free-list. It's pre-filled
	// with every compiler at construction; Compile takes one and
	// returns it when done. Capacity equals the compiler count, so the
	// return send never blocks. We deliberately never close it — after
	// Close the buffered compilers are still drainable, and Compile on
	// a closed compiler returns a clean error rather than panicking on
	// a send-to-closed-channel.
	idle chan SFCCompiler
	// closed is closed by Close before the underlying compilers are
	// torn down. Compile checks it first so a call after Close returns
	// a clean error instead of pulling a now-dead compiler off the
	// free-list.
	closed    chan struct{}
	closeOnce sync.Once
}

// NewPool builds size compilers via factory and returns a Pool that
// load-balances across them. size < 1 is clamped to 1, making a Pool
// behave exactly like a single compiler.
//
// Compilers are constructed concurrently: each factory call blocks
// (~100 ms parsing the bundle for the CGo backend; a one-time WASM
// module compile for the WASM backend), so building them in parallel
// keeps pool startup at roughly one compiler's cost instead of N times
// it. If any compiler fails to build, every already-built one is
// closed and the first error is returned — a half-built pool is never
// handed back.
func NewPool(factory func() (SFCCompiler, error), size int) (*Pool, error) {
	if factory == nil {
		return nil, fmt.Errorf("vue: NewPool requires a factory")
	}
	if size < 1 {
		size = 1
	}

	p := &Pool{
		idle:   make(chan SFCCompiler, size),
		closed: make(chan struct{}),
	}

	type built struct {
		c   SFCCompiler
		err error
	}
	results := make(chan built, size)
	for i := 0; i < size; i++ {
		go func() {
			c, err := factory()
			results <- built{c: c, err: err}
		}()
	}

	var firstErr error
	for i := 0; i < size; i++ {
		r := <-results
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		p.compilers = append(p.compilers, r.c)
		p.idle <- r.c
	}
	if firstErr != nil {
		p.Close() // tear down any that did come up
		return nil, fmt.Errorf("vue: build compiler pool: %w", firstErr)
	}
	return p, nil
}

// Compile checks out a free compiler, runs the SFC compile on it, and
// returns it to the pool. Blocks while every compiler is busy, which
// is the intended backpressure: in-flight compiles never exceed the
// pool size. Safe for concurrent use from esbuild's OnLoad fan-out.
func (p *Pool) Compile(source, filename string) (CompileResult, error) {
	// Non-blocking shutdown check first. Both p.closed and p.idle can
	// be ready at once after Close (idle stays buffered), and a plain
	// select would pick a dead compiler half the time — so we short-
	// circuit on closed before ever touching the free-list.
	select {
	case <-p.closed:
		return CompileResult{}, fmt.Errorf("vue: Compile on closed pool")
	default:
	}
	c := <-p.idle
	defer func() { p.idle <- c }()
	return c.Compile(source, filename)
}

// Close shuts down every compiler in the pool and releases their
// underlying runtimes. Idempotent. Callers MUST call it when done.
func (p *Pool) Close() {
	p.closeOnce.Do(func() {
		close(p.closed) // signal Compile before tearing compilers down
		for _, c := range p.compilers {
			c.Close()
		}
	})
}

// Size reports how many compilers the pool holds — i.e. its maximum
// compile concurrency.
func (p *Pool) Size() int { return len(p.compilers) }
