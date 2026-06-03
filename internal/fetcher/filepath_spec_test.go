package fetcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/paulmanoni/viteless/internal/store"
)

// TestIsFilePathSpec covers the boundary between "bare package
// reference" (which gets ?external= + PinnedVersions treatment)
// and "package + file-path" (which passes through verbatim).
// The distinction matters because CSS / font / JSON assets can't
// take the JS-shaped query params and we'd rather route them
// through a single clean code path.
func TestIsFilePathSpec(t *testing.T) {
	cases := []struct {
		spec string
		want bool
	}{
		// Bare packages — no file path.
		{"vue", false},
		{"vue@3.5.26", false},
		{"@vue/runtime-dom", false},
		{"@vue/runtime-dom@3.5.26", false},
		{"@mdi/font", false},
		// File-path specs — should pass through to the registry.
		{"vuetify/styles", true},
		{"vue@3.5.26/dist/vue.esm-browser.js", true},
		{"@mdi/font/css/materialdesignicons.min.css", true},
		{"@mdi/font@7.4.47/css/materialdesignicons.min.css", true},
		{"@apollo/client/core/index.js", true},
		// Edge cases.
		{"", false},
		{"@", false},
		{"@scope", false},
	}
	for _, c := range cases {
		got := isFilePathSpec(c.spec)
		if got != c.want {
			t.Errorf("isFilePathSpec(%q) = %v, want %v", c.spec, got, c.want)
		}
	}
}

// TestSplitSpecPath verifies the lockfile-shape extraction:
// file-path specs need (pkg-name, version, path) carved out so
// the lockfile records each entry with a useful Spec field
// (not the full URL-style mash that parseSpec would produce).
func TestSplitSpecPath(t *testing.T) {
	cases := []struct {
		spec    string
		name    string
		version string
		path    string
		ok      bool
	}{
		{"vuetify/styles", "vuetify", "", "styles", true},
		{"vue@3.5.26/dist/vue.esm-browser.js", "vue", "3.5.26", "dist/vue.esm-browser.js", true},
		{"@mdi/font/css/materialdesignicons.min.css", "@mdi/font", "", "css/materialdesignicons.min.css", true},
		{"@mdi/font@7.4.47/css/materialdesignicons.min.css", "@mdi/font", "7.4.47", "css/materialdesignicons.min.css", true},
		// Non-path specs → ok=false; caller falls back to parseSpec.
		{"vue", "", "", "", false},
		{"vue@3.5.26", "", "", "", false},
		{"@vue/runtime-dom@3.5.26", "", "", "", false},
	}
	for _, c := range cases {
		name, version, path, ok := splitSpecPath(c.spec)
		if ok != c.ok || name != c.name || version != c.version || path != c.path {
			t.Errorf("splitSpecPath(%q) = (%q, %q, %q, %v); want (%q, %q, %q, %v)",
				c.spec, name, version, path, ok, c.name, c.version, c.path, c.ok)
		}
	}
}

// TestSpecToURL_FilePathSkipsExternalQuery is the integration
// promise: a file-path spec composes a clean registry URL with
// NO ?external= or ?target= junk that esm.sh would 404 on for
// direct file access.
func TestSpecToURL_FilePathSkipsExternalQuery(t *testing.T) {
	f := &Fetcher{
		Registry: "https://esm.sh",
		External: []string{"vue", "react", "react-dom"},
		URLQuery: "target=es2022",
	}

	cases := []struct {
		spec string
		want string
	}{
		{
			"@mdi/font/css/materialdesignicons.min.css",
			"https://esm.sh/@mdi/font/css/materialdesignicons.min.css",
		},
		{
			"@mdi/font@7.4.47/css/materialdesignicons.min.css",
			"https://esm.sh/@mdi/font@7.4.47/css/materialdesignicons.min.css",
		},
		{
			"vuetify/styles",
			"https://esm.sh/vuetify/styles",
		},
		// Bare specs should STILL get the query suffix (sanity
		// check that we didn't break the existing path).
		{
			"vue",
			"https://esm.sh/vue?target=es2022&external=vue,react,react-dom",
		},
	}
	for _, c := range cases {
		got, err := f.specToURL(c.spec)
		if err != nil {
			t.Fatalf("specToURL(%q): %v", c.spec, err)
		}
		if got != c.want {
			t.Errorf("specToURL(%q):\n  got  %s\n  want %s", c.spec, got, c.want)
		}
	}
}

// TestFetch_FilePathSpecEndToEnd drives a CSS file fetch all
// the way through to a lockfile.Package + cache entry. Verifies
// that the integration knit-up (specToURL → fetchOne →
// splitSpecPath → store.Put → Result.Root) lands a sensible
// shape the resolver can later import as a stylesheet.
func TestFetch_FilePathSpecEndToEnd(t *testing.T) {
	const cssBody = `
		.test-class { color: red; }
		@import url("./fonts.css");
	`
	mux := http.NewServeMux()
	mux.HandleFunc("/@mdi/font@7.4.47/css/materialdesignicons.min.css", func(w http.ResponseWriter, r *http.Request) {
		// Esm.sh would NOT carry external= here; a file path with
		// query params would either 404 or be ignored. Test that
		// the fetcher composed a clean URL.
		if r.URL.RawQuery != "" {
			t.Errorf("file-path spec should not carry query params, got %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Write([]byte(cssBody))
	})
	mux.HandleFunc("/@mdi/font@7.4.47/css/fonts.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Write([]byte("/* fonts */"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &Fetcher{
		Registry: srv.URL,
		Store:    st,
		HTTP:     srv.Client(),
		External: []string{"vue", "react", "react-dom"}, // would corrupt the URL if applied
	}

	res, err := f.Fetch(context.Background(), "@mdi/font@7.4.47/css/materialdesignicons.min.css")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Root.Spec != "@mdi/font@7.4.47/css/materialdesignicons.min.css" {
		t.Errorf("Root.Spec: got %q, want full file-path spec", res.Root.Spec)
	}
	if !strings.Contains(res.Root.ContentType, "css") {
		t.Errorf("Root.ContentType should mention css, got %q", res.Root.ContentType)
	}
	if !strings.HasPrefix(res.Root.Integrity, "sha256-") {
		t.Errorf("Root.Integrity missing sha256- prefix: %q", res.Root.Integrity)
	}
	// CSS recursion should have followed the @import into fonts.css.
	if len(res.Transitive) != 1 {
		t.Errorf("expected 1 transitive (fonts.css), got %d: %v", len(res.Transitive), res.Transitive)
	}
}
