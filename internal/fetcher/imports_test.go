package fetcher

import (
	"reflect"
	"testing"
)

func TestExtractImports_CommonShapes(t *testing.T) {
	src := `
import 'side-effect-only';
import vue from 'vue';
import { ref, computed } from 'vue';
import * as React from 'react';
import x, { y } from 'lodash';
export { z } from './local';
export * from 'something';
const lazy = () => import('chunky');
`
	want := []string{
		"side-effect-only",
		"vue",
		"react",
		"lodash",
		"./local",
		"something",
		"chunky",
	}
	got := ExtractImports(src)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractImports_DedupRepeatedSpec(t *testing.T) {
	src := `import a from 'vue'; import { b } from 'vue'; import('vue');`
	got := ExtractImports(src)
	if !reflect.DeepEqual(got, []string{"vue"}) {
		t.Errorf("got %v, want [vue]", got)
	}
}

func TestExtractImports_IgnoresStringLiterals(t *testing.T) {
	// A literal containing the word "from" must NOT trigger an
	// import match. Common case: a log message.
	src := `
const msg = "hello from 'fake-import'";
import real from 'real-vue';
`
	got := ExtractImports(src)
	if !reflect.DeepEqual(got, []string{"real-vue"}) {
		t.Errorf("got %v, want [real-vue]", got)
	}
}

func TestExtractImports_IgnoresLineComment(t *testing.T) {
	src := `
// import legacy from 'old-vue';
import real from 'new-vue';
`
	got := ExtractImports(src)
	if !reflect.DeepEqual(got, []string{"new-vue"}) {
		t.Errorf("got %v, want [new-vue]", got)
	}
}

func TestExtractImports_IgnoresBlockComment(t *testing.T) {
	src := `
/*
 * import old from 'commented';
 */
import real from 'new';
`
	got := ExtractImports(src)
	if !reflect.DeepEqual(got, []string{"new"}) {
		t.Errorf("got %v, want [new]", got)
	}
}

func TestExtractImports_HonorsBackslashEscapesInStrings(t *testing.T) {
	// The string contains \" — without escape handling, the parser
	// would think the string ended early and start scanning the
	// remainder as code, false-matching the trailing import.
	src := `const s = "before \" import x from 'fake'"; import real from 'real';`
	got := ExtractImports(src)
	if !reflect.DeepEqual(got, []string{"real"}) {
		t.Errorf("got %v, want [real]", got)
	}
}

func TestExtractImports_BacktickTemplateIsOpaque(t *testing.T) {
	// Template literals are treated as opaque strings; nothing
	// inside them can be an import.
	src := "const tpl = `from 'template-fake'`; import real from 'real';"
	got := ExtractImports(src)
	if !reflect.DeepEqual(got, []string{"real"}) {
		t.Errorf("got %v, want [real]", got)
	}
}

func TestExtractImports_EmptyInput(t *testing.T) {
	if got := ExtractImports(""); len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestBuildMask_TagsRegionsCorrectly(t *testing.T) {
	// Sanity check the mask: code bytes are 0, string-body bytes
	// are 's', comment bytes are 'c'. Quotes themselves are code
	// (so the regex anchor finds them).
	src := `a "bc" //x` + "\n"
	mask := buildMask(src)
	want := []byte{
		codeRegion,    // 'a'
		codeRegion,    // ' '
		codeRegion,    // '"'  opening quote
		stringRegion,  // 'b'
		stringRegion,  // 'c'
		codeRegion,    // '"'  closing quote
		codeRegion,    // ' '
		commentRegion, // '/'
		commentRegion, // '/'
		commentRegion, // 'x'
		codeRegion,    // '\n' (LF terminates the line comment, not part of it)
	}
	for i, w := range want {
		if mask[i] != w {
			t.Errorf("mask[%d] = %d (%q), want %d", i, mask[i], src[i], w)
		}
	}
}
