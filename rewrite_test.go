package viteless

import "testing"

// fakeResolve turns specs into predictable URLs so tests assert the
// rewrite shape without a store/network.
func fakeResolve(spec string, kind SpecKind, importer string) string {
	switch kind {
	case SpecBare:
		return "/@id/" + spec
	case SpecRelative:
		return "/resolved" + spec[1:] // "./Foo.vue" -> "/resolved/Foo.vue"
	case SpecAbsolute:
		if spec == "/skip" {
			return "" // resolver declines → leave untouched
		}
		return "/@abs/" + spec
	}
	return ""
}

func rw(src string) string {
	return RewriteImports(src, "/src/x.js", ClassifySpec, fakeResolve)
}

func TestRewrite_StaticNamedImport(t *testing.T) {
	in := `import { openBlock as _o, toValue as _tv } from "vue"`
	want := `import { openBlock as _o, toValue as _tv } from "/@id/vue"`
	if got := rw(in); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestRewrite_DefaultImport(t *testing.T) {
	in := `import Foo from "./Foo.vue"`
	want := `import Foo from "/resolved/Foo.vue"`
	if got := rw(in); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestRewrite_SideEffectImport(t *testing.T) {
	in := `import "./styles.css"`
	want := `import "/resolved/styles.css"`
	if got := rw(in); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestRewrite_ReexportFrom(t *testing.T) {
	in := `export { ref, computed } from "vue"`
	want := `export { ref, computed } from "/@id/vue"`
	if got := rw(in); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestRewrite_ExportStarFrom(t *testing.T) {
	in := `export * from "@vue/runtime-core"`
	want := `export * from "/@id/@vue/runtime-core"`
	if got := rw(in); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestRewrite_DynamicImport(t *testing.T) {
	in := `const m = await import("./views/Settings.vue")`
	want := `const m = await import("/resolved/views/Settings.vue")`
	if got := rw(in); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestRewrite_DynamicImportSingleQuote(t *testing.T) {
	in := `() => import('vue-router')`
	want := `() => import('/@id/vue-router')`
	if got := rw(in); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestRewrite_AbsoluteURLSibling(t *testing.T) {
	in := `import { warn } from "https://esm.sh/@vue/runtime-core@3.5.34/x.mjs"`
	want := `import { warn } from "/@abs/https://esm.sh/@vue/runtime-core@3.5.34/x.mjs"`
	if got := rw(in); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestRewrite_ResolverDeclines_LeavesUntouched(t *testing.T) {
	in := `import "/skip"`
	if got := rw(in); got != in {
		t.Errorf("declined resolve should leave spec untouched; got %q", got)
	}
}

func TestRewrite_IgnoresImportInLineComment(t *testing.T) {
	in := "// import Foo from \"vue\"\nconst x = 1"
	if got := rw(in); got != in {
		t.Errorf("line comment must not be rewritten; got %q", got)
	}
}

func TestRewrite_IgnoresImportInBlockComment(t *testing.T) {
	in := "/* import Foo from \"vue\" */\nconst x = 1"
	if got := rw(in); got != in {
		t.Errorf("block comment must not be rewritten; got %q", got)
	}
}

func TestRewrite_IgnoresImportInString(t *testing.T) {
	in := "const s = \"import Foo from 'vue'\";"
	if got := rw(in); got != in {
		t.Errorf("string literal must not be rewritten; got %q", got)
	}
}

func TestRewrite_DoesNotMatchImportSubstringIdentifier(t *testing.T) {
	in := `const important = fromCache; let reimport = 1;`
	if got := rw(in); got != in {
		t.Errorf("identifier containing 'import' must not match; got %q", got)
	}
}

func TestRewrite_MultipleImportsOneFile(t *testing.T) {
	in := `import { h } from "vue"
import App from "./App.vue"
const lazy = () => import("./Lazy.vue")
export { useThing } from "@scope/pkg"`
	want := `import { h } from "/@id/vue"
import App from "/resolved/App.vue"
const lazy = () => import("/resolved/Lazy.vue")
export { useThing } from "/@id/@scope/pkg"`
	if got := rw(in); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestClassifySpec(t *testing.T) {
	cases := map[string]SpecKind{
		"vue":                SpecBare,
		"@vue/runtime-core":  SpecBare,
		"./Foo.vue":          SpecRelative,
		"../bar":             SpecRelative,
		"/abs/x":             SpecAbsolute,
		"https://esm.sh/vue": SpecAbsolute,
	}
	for spec, want := range cases {
		if got := ClassifySpec(spec); got != want {
			t.Errorf("ClassifySpec(%q)=%v want %v", spec, got, want)
		}
	}
}
