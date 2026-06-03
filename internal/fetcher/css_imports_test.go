package fetcher

import (
	"reflect"
	"testing"
)

func TestExtractCSSImports_FontFaceURLs(t *testing.T) {
	src := `
@font-face {
  font-family: 'Inter';
  src: url('./files/inter-cyrillic.woff2') format('woff2');
}
@font-face {
  font-family: 'Inter';
  src: url(./files/inter-latin.woff2) format('woff2');
}
`
	got := ExtractCSSImports(src)
	want := []string{
		"./files/inter-cyrillic.woff2",
		"./files/inter-latin.woff2",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractCSSImports_AtImportShapes(t *testing.T) {
	src := `
@import "./theme.css";
@import url("./reset.css");
@import url(./vendor.css);
.x { color: red; }
`
	got := ExtractCSSImports(src)
	// Both @import patterns AND the url() pattern can match the
	// same line — that's why dedup matters. theme.css doesn't
	// have url() so only @import matches; the others get matched
	// by both, dedup'd to one.
	want := []string{
		"./theme.css",
		"./reset.css",
		"./vendor.css",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractCSSImports_BackgroundImage(t *testing.T) {
	src := `.hero { background-image: url("/img/hero.webp"); }`
	got := ExtractCSSImports(src)
	if !reflect.DeepEqual(got, []string{"/img/hero.webp"}) {
		t.Errorf("got %v", got)
	}
}

func TestExtractCSSImports_SkipsDataAndFragment(t *testing.T) {
	src := `
.icon { background: url(data:image/svg+xml;base64,PHN2Zw==); }
.svg-ref { fill: url(#gradient-1); }
.real { background: url("./real.png"); }
`
	got := ExtractCSSImports(src)
	if !reflect.DeepEqual(got, []string{"./real.png"}) {
		t.Errorf("got %v, want [./real.png]", got)
	}
}

func TestExtractCSSImports_DedupSameURL(t *testing.T) {
	src := `
.a { background: url("./bg.png"); }
.b { background: url('./bg.png'); }
.c { background: url(./bg.png); }
`
	got := ExtractCSSImports(src)
	if !reflect.DeepEqual(got, []string{"./bg.png"}) {
		t.Errorf("got %v, want [./bg.png]", got)
	}
}

func TestExtractCSSImports_Empty(t *testing.T) {
	if got := ExtractCSSImports(""); len(got) != 0 {
		t.Errorf("empty input got %v", got)
	}
}
