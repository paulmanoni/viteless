package fetcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/paulmanoni/viteless/internal/store"
)

// TestFetcher_CacheHitSkipsHEADForVersionedBareSpecs: the install
// flow's dominant case — `vue@3.5.34?external=...` already in
// the cache should make ZERO HTTP requests on the second Fetch.
// Vuetify install does ~1000 of these; the HEAD-per-blob was
// where minutes of wall time were being burned.
func TestFetcher_CacheHitSkipsHEADForVersionedBareSpecs(t *testing.T) {
	var requests atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/vue@3.5.34", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/javascript")
		w.Write([]byte("export const x = 1;"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st, _ := store.New(t.TempDir())
	f := &Fetcher{Registry: srv.URL, Store: st, HTTP: srv.Client(), Concurrency: 1}

	// Prime the cache.
	_, err := f.Fetch(context.Background(), "vue@3.5.34")
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() < 1 {
		t.Fatalf("first fetch should hit the server")
	}

	// Second fetch — fully cached, must skip HEAD entirely.
	requests.Store(0)
	_, err = f.Fetch(context.Background(), "vue@3.5.34")
	if err != nil {
		t.Fatal(err)
	}
	got := requests.Load()
	if got != 0 {
		t.Errorf("cached versioned-bare-spec fetch should make 0 HTTP requests, got %d (fast-path didn't fire for bare specs)", got)
	}
}

// TestFetcher_CSSFallbackForBarePackageThat404s: the @mdi/font
// case. esm.sh 404s on the bare spec because there's no JS to
// serve; the fetcher should fall back to fetching the package's
// package.json, find the `style` entry, and retry with the
// file-path spec.
func TestFetcher_CSSFallbackForBarePackageThat404s(t *testing.T) {
	mux := http.NewServeMux()
	// Bare-spec request → 404 (no JS to serve, like @mdi/font).
	mux.HandleFunc("/@mdi/font@7.4.47", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	// package.json → reveals the CSS entry via `style`.
	mux.HandleFunc("/@mdi/font@7.4.47/package.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"name": "@mdi/font",
			"version": "7.4.47",
			"style": "css/materialdesignicons.min.css"
		}`))
	})
	// The actual CSS file served via the file-path retry.
	mux.HandleFunc("/@mdi/font@7.4.47/css/materialdesignicons.min.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		w.Write([]byte(`.mdi { font-family: 'mdi'; }`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st, _ := store.New(t.TempDir())
	f := &Fetcher{Registry: srv.URL, Store: st, HTTP: srv.Client()}

	res, err := f.Fetch(context.Background(), "@mdi/font@7.4.47")
	if err != nil {
		t.Fatalf("expected fallback to succeed, got: %v", err)
	}
	// The discovered file-path spec is what landed in the lockfile.
	if !strings.Contains(res.Root.Spec, "css/materialdesignicons.min.css") {
		t.Errorf("Root.Spec should be the discovered file-path spec, got %q", res.Root.Spec)
	}
	if !strings.Contains(res.Root.ContentType, "css") {
		t.Errorf("Root.ContentType should be CSS, got %q", res.Root.ContentType)
	}
}

// TestFetcher_CSSFallbackPropagatesOriginal404WhenNoEntry: when
// the package's package.json has no `style` / `main`.css /
// exports[./style] field, the fallback finds nothing and we
// surface the ORIGINAL 404 (not a misleading "package.json
// parse failed" error). Operators get the actionable signal.
func TestFetcher_CSSFallbackPropagatesOriginal404WhenNoEntry(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/some-pkg@1.0.0", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/some-pkg@1.0.0/package.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// No style, no .css main, no exports./style — fallback should
		// find nothing and propagate the original 404.
		w.Write([]byte(`{"name": "some-pkg", "version": "1.0.0", "main": "index.js"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st, _ := store.New(t.TempDir())
	f := &Fetcher{Registry: srv.URL, Store: st, HTTP: srv.Client()}

	_, err := f.Fetch(context.Background(), "some-pkg@1.0.0")
	if err == nil {
		t.Fatal("expected 404 to propagate, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("expected 404 in error, got %v", err)
	}
}

// TestPickAssetEntry covers the package.json shape variants.
// Real-world packages use all four shapes; the parser needs to
// recognize each.
func TestPickAssetEntry(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			"top-level style",
			`{"style": "dist/styles.css"}`,
			"dist/styles.css",
		},
		{
			"exports./style as string",
			`{"exports": {"./style": "lib/main.css"}}`,
			"lib/main.css",
		},
		{
			"exports./style as conditional map",
			`{"exports": {"./style": {"default": "lib/main.css", "require": "lib/main.cjs.css"}}}`,
			"lib/main.css",
		},
		{
			"exports.[.].style",
			`{"exports": {".": {"style": "lib/main.css"}}}`,
			"lib/main.css",
		},
		{
			"main when it ends in .css",
			`{"main": "css/legacy.css"}`,
			"css/legacy.css",
		},
		{
			"main when it does NOT end in .css → no entry",
			`{"main": "index.js"}`,
			"",
		},
		{
			"empty package.json",
			`{}`,
			"",
		},
		{
			"style wins over main.css",
			`{"style": "a.css", "main": "b.css"}`,
			"a.css",
		},
	}
	for _, c := range cases {
		got := pickAssetEntry([]byte(c.body))
		if got != c.want {
			t.Errorf("pickAssetEntry(%s) = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestCanonicalSpecFromResolvedURL: the version-extractor for
// the fallback path. After esm.sh redirects `@mdi/font` →
// `@mdi/font@7.4.47?external=...` and THEN 404s, we need to
// extract `@mdi/font@7.4.47` from the post-redirect URL so we
// can compose the package.json URL.
func TestCanonicalSpecFromResolvedURL(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"https://esm.sh/@mdi/font@7.4.47?external=vue", "@mdi/font@7.4.47"},
		{"https://esm.sh/vue@3.5.34/es2022/vue.mjs", "vue@3.5.34"},
		{"https://esm.sh/@vue/runtime-dom@3.5.34", "@vue/runtime-dom@3.5.34"},
		{"https://esm.sh/something-without-version", ""},
		{"https://esm.sh/@scope/no-version", ""},
	}
	for _, c := range cases {
		got := canonicalSpecFromResolvedURL(c.url)
		if got != c.want {
			t.Errorf("canonicalSpecFromResolvedURL(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}
