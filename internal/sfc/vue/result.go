package vue

import (
	"encoding/json"
	"errors"
	"fmt"
)

// This file holds the backend-agnostic types + helpers shared by
// every SFC compiler implementation (the CGo QuickJS binding in
// compile.go and the CGo-free QuickJS-via-WASM backend in
// compile_qjs.go). It carries NO build tag so both backends — and a
// pure-Go build that has neither — can reference these symbols.

// SFCCompiler is the compile surface the esbuild Plugin and the Pool
// need. Every backend (*Compiler, *QJSCompiler) and the *Pool wrapper
// satisfy it, so callers pick the implementation — and the Pool can
// hold a homogeneous set of any backend — without anyone caring which
// it got. Close is part of the contract so the Pool can tear members
// down; Plugin simply never calls it.
type SFCCompiler interface {
	Compile(source, filename string) (CompileResult, error)
	Close()
}

// CompileResult is what a compiler returns: the synthesized JS module
// plus any diagnostics the compiler produced. A non-nil Errors slice
// means the result is partially valid — callers typically surface the
// errors and abort the build before reaching esbuild.
type CompileResult struct {
	Code   string
	Errors []CompileError
}

// CompileError describes a single diagnostic from the SFC compiler.
// Line/Column are 1-indexed if known, else zero.
type CompileError struct {
	Message string
	Line    int
	Column  int
}

// decodeResult parses the JSON the adapter returned. We use JSON
// rather than poking individual JS object fields via a binding's
// reflection because:
//
//  1. The boundary stays string-shaped — easier to reason about.
//  2. Fewer cross-boundary round trips per Compile (one JSONStringify
//     in JS, one Unmarshal in Go) compared to N property reads.
//  3. Errors in malformed adapter output surface as Unmarshal errors
//     with line numbers, which beats "field was unexpected type"
//     without context.
func decodeResult(jsonStr string) (CompileResult, error) {
	if jsonStr == "" || jsonStr == "undefined" || jsonStr == "null" {
		return CompileResult{}, errors.New("vue: __nexus_compileSFC returned null/undefined")
	}
	var raw struct {
		Code   string `json:"code"`
		Errors []struct {
			Message string `json:"message"`
			Line    int    `json:"line,omitempty"`
			Column  int    `json:"column,omitempty"`
		} `json:"errors,omitempty"`
	}
	if err := jsonUnmarshalString(jsonStr, &raw); err != nil {
		return CompileResult{}, fmt.Errorf("vue: parse adapter result: %w", err)
	}
	out := CompileResult{Code: raw.Code}
	for _, e := range raw.Errors {
		out.Errors = append(out.Errors, CompileError{
			Message: e.Message,
			Line:    e.Line,
			Column:  e.Column,
		})
	}
	return out, nil
}

// jsonUnmarshalString is the indirection point for decodeResult's
// JSON-decoding step. Wrapped so the import surface stays narrow in
// the backend files. Tests that want to feed a pre-canned result
// through decodeResult without going through a JS engine use the same
// entry point.
func jsonUnmarshalString(s string, v any) error {
	return json.Unmarshal([]byte(s), v)
}
