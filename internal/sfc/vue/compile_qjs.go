// This file is the default, CGo-free SFC compiler backend. It runs
// the SAME @vue/compiler-sfc bundle (produced by Bootstrap) inside
// QuickJS-NG compiled to WebAssembly and driven by the Wazero
// runtime — pure Go, no C toolchain, cross-compilable. Plain
// `nexus build` uses it; the native CGo binding (compile.go) is the
// opt-in alternative under `-tags vue`.
//
// It satisfies SFCCompiler (result.go), so the esbuild Plugin and the
// Pool work against it unchanged. The headline difference from
// *Compiler: no cgo, no runtime.LockOSThread worker goroutine — Wazero
// has no C thread-affinity, so we just serialize calls on a mutex.

package vue

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/paulmanoni/qjs"
)

// QJSCompiler is the WASM-backed counterpart to *Compiler. One
// instance owns one QuickJS-NG context; Compile serializes on mu
// because a single context isn't safe for concurrent calls. For
// parallelism, wrap N of these in something like Pool (they satisfy
// the same interface) — Wazero compiles the embedded module once per
// process, so additional instances are cheap (~ms) after the first.
type QJSCompiler struct {
	version string

	mu     sync.Mutex
	rt     *qjs.Runtime
	ctx    *qjs.Context
	closed bool
}

// NewQJSCompiler builds a compiler from the same bundle bytes
// NewCompiler takes. It installs the host polyfills, evaluates the
// bundle (which must install globalThis.__nexus_compileSFC), and
// verifies that global is present.
func NewQJSCompiler(bundle []byte, version string) (*QJSCompiler, error) {
	if len(bundle) == 0 {
		return nil, errors.New("vue: empty compiler bundle")
	}
	rt, err := qjs.New()
	if err != nil {
		return nil, fmt.Errorf("vue: qjs runtime: %w", err)
	}
	c := &QJSCompiler{version: version, rt: rt, ctx: rt.Context()}

	// Polyfills BEFORE the bundle: esm.sh's unenv buffer polyfill
	// does globalThis.btoa.bind(globalThis) at top level, so btoa
	// must exist before the bundle evaluates.
	if err := c.installPolyfills(); err != nil {
		rt.Close()
		return nil, err
	}

	if _, err := c.ctx.Eval("nexus-vue-compiler.bundle.js", qjs.Code(string(bundle))); err != nil {
		rt.Close()
		return nil, fmt.Errorf("vue: load bundle: %w", err)
	}

	probe, err := c.ctx.Eval("nexus-vue-probe.js", qjs.Code(`typeof globalThis.__nexus_compileSFC`))
	if err != nil {
		rt.Close()
		return nil, fmt.Errorf("vue: probe adapter: %w", err)
	}
	ok := probe.String() == "function"
	probe.Free()
	if !ok {
		rt.Close()
		return nil, errors.New("vue: bundle did not expose globalThis.__nexus_compileSFC after load")
	}
	return c, nil
}

// installPolyfills mirrors the CGo backend's host globals: btoa/atob
// as Go functions plus a small prelude for process.env.NODE_ENV and
// the self/global aliases some bundles assume.
func (c *QJSCompiler) installPolyfills() error {
	c.ctx.SetFunc("btoa", func(this *qjs.This) (*qjs.Value, error) {
		args := this.Args()
		if len(args) == 0 {
			return this.Context().NewString(""), nil
		}
		return this.Context().NewString(base64.StdEncoding.EncodeToString([]byte(args[0].String()))), nil
	})
	c.ctx.SetFunc("atob", func(this *qjs.This) (*qjs.Value, error) {
		args := this.Args()
		if len(args) == 0 {
			return this.Context().NewString(""), nil
		}
		b, err := base64.StdEncoding.DecodeString(args[0].String())
		if err != nil {
			return nil, fmt.Errorf("atob: invalid base64: %w", err)
		}
		return this.Context().NewString(string(b)), nil
	})

	const prelude = `
		(function () {
			if (typeof globalThis.process === 'undefined') {
				globalThis.process = { env: { NODE_ENV: 'production' } };
			} else if (!globalThis.process.env) {
				globalThis.process.env = { NODE_ENV: 'production' };
			}
			if (typeof globalThis.self === 'undefined') {
				globalThis.self = globalThis;
			}
			if (typeof globalThis.global === 'undefined') {
				globalThis.global = globalThis;
			}
		})();
	`
	if _, err := c.ctx.Eval("nexus-vue-polyfills.js", qjs.Code(prelude)); err != nil {
		return fmt.Errorf("vue: polyfill prelude: %w", err)
	}
	return nil
}

// Compile runs the bundled @vue/compiler-sfc against source. Both
// args are inlined as JSON literals (valid JS expressions) so the
// call is a single Eval, and the result is JSON.stringify'd on the
// JS side then decoded on the Go side — identical boundary contract
// to the CGo backend's doCompile.
func (c *QJSCompiler) Compile(source, filename string) (CompileResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return CompileResult{}, errors.New("vue: Compile called after Close")
	}
	srcJSON, _ := json.Marshal(source)
	fileJSON, _ := json.Marshal(filename)
	expr := `JSON.stringify(__nexus_compileSFC(` + string(srcJSON) + `,` + string(fileJSON) + `))`
	v, err := c.ctx.Eval("nexus-vue-compile-call.js", qjs.Code(expr))
	if err != nil {
		return CompileResult{}, fmt.Errorf("vue: __nexus_compileSFC threw: %w", err)
	}
	defer v.Free()
	return decodeResult(v.String())
}

// Close releases the Wazero runtime. Idempotent.
func (c *QJSCompiler) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.rt.Close()
}

// Version returns the identifier passed to NewQJSCompiler.
func (c *QJSCompiler) Version() string { return c.version }
