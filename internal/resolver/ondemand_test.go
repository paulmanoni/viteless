package resolver

import (
	"errors"
	"strings"
	"testing"

	"github.com/evanw/esbuild/pkg/api"

	"github.com/paulmanoni/viteless/internal/lockfile"
	"github.com/paulmanoni/viteless/internal/store"
)

// TestResolver_SubpathCacheMiss_TriggersFetchOnDemand: when an
// import like `@apollo/client/core` resolves to a sub-path URL
// that isn't in the cache, the FetchOnDemand hook should fire
// and on success the resolver returns a successful resolve.
func TestResolver_SubpathCacheMiss_TriggersFetchOnDemand(t *testing.T) {
	tmp := t.TempDir()
	st, err := store.New(tmp)
	if err != nil {
		t.Fatal(err)
	}
	// Lockfile has the root @apollo/client pinned but NOT the
	// /core sub-path — same shape as a real `nexus install`
	// against a project that only references the root.
	lf := lockfile.New()
	lf.Add(lockfile.Package{
		Spec:     "@apollo/client",
		Version:  "3.14.0",
		Resolved: "https://esm.sh/@apollo/client@3.14.0",
	})

	// Track which URL the hook was asked to fetch + populate
	// the cache when called so the resolver's retry succeeds.
	var fetched string
	hook := func(u string) (string, error) {
		fetched = u
		_, err := st.Put(u, strings.NewReader("export const x = 1;"), "",
			store.Metadata{URL: u, ResolvedURL: u, ContentType: "application/javascript"})
		return u, err
	}

	res, err := resolveOne(Options{Lockfile: lf, Store: st, FetchOnDemand: hook},
		api.OnResolveArgs{Path: "@apollo/client/core"})
	if err != nil {
		t.Fatalf("resolveOne: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("expected no errors, got: %v", res.Errors)
	}
	if !strings.HasSuffix(fetched, "/core") {
		t.Errorf("FetchOnDemand should have been called with the sub-path URL, got %q", fetched)
	}
	if !strings.HasSuffix(res.Path, "/core") {
		t.Errorf("Resolver should return the fetched URL, got %q", res.Path)
	}
	if res.Namespace != Namespace {
		t.Errorf("expected our namespace, got %q", res.Namespace)
	}
}

// TestResolver_SubpathCacheMiss_NoHookPreservesOldError: when
// FetchOnDemand is nil (offline-only / pre-v0.88 behavior), the
// resolver should surface the original "no cached blob" error so
// operators get clear actionable output.
func TestResolver_SubpathCacheMiss_NoHookPreservesOldError(t *testing.T) {
	tmp := t.TempDir()
	st, err := store.New(tmp)
	if err != nil {
		t.Fatal(err)
	}
	lf := lockfile.New()
	lf.Add(lockfile.Package{
		Spec:     "@apollo/client",
		Version:  "3.14.0",
		Resolved: "https://esm.sh/@apollo/client@3.14.0",
	})

	res, err := resolveOne(Options{Lockfile: lf, Store: st}, // no FetchOnDemand
		api.OnResolveArgs{Path: "@apollo/client/core"})
	if err != nil {
		t.Fatalf("resolveOne: %v", err)
	}
	if len(res.Errors) == 0 {
		t.Fatal("expected 'no cached blob' error when FetchOnDemand is nil")
	}
	if !strings.Contains(res.Errors[0].Text, "no cached blob exists") {
		t.Errorf("expected actionable error mentioning the cache, got: %q", res.Errors[0].Text)
	}
}

// TestResolver_SubpathHookCanonicalizesURL: the hook may return
// a DIFFERENT URL than the resolver asked for — esm.sh
// 301-redirects shapes like `*.css.mjs` → `*.css`, so the
// fetcher stores under the post-redirect URL. The resolver must
// use the hook's return value (not the original asked-for URL)
// when retrying the cache lookup.
//
// Without this fix vuetify's per-component CSS files
// (VSlider.css.mjs, VTextField.css.mjs, etc.) cache-miss after
// successful on-demand fetch, surfacing a confusing "no cached
// blob" error even though the bytes ARE on disk under a
// neighboring key.
func TestResolver_SubpathHookCanonicalizesURL(t *testing.T) {
	tmp := t.TempDir()
	st, err := store.New(tmp)
	if err != nil {
		t.Fatal(err)
	}
	lf := lockfile.New()
	lf.Add(lockfile.Package{
		Spec:     "vuetify",
		Version:  "3.11.7",
		Resolved: "https://esm.sh/vuetify@3.11.7",
	})

	// Simulate the .css.mjs → .css redirect: the hook gets
	// asked for the .css.mjs URL, fetches the redirect target
	// (.css), and returns THAT as the cache key.
	const askedURL = "https://esm.sh/vuetify@3.11.7/lib/foo.css.mjs"
	const canonicalURL = "https://esm.sh/vuetify@3.11.7/lib/foo.css"
	hook := func(u string) (string, error) {
		if u != askedURL {
			t.Errorf("hook asked for %q, expected %q", u, askedURL)
		}
		// Write to the cache under the CANONICAL URL — the
		// post-redirect key.
		_, err := st.Put(canonicalURL, strings.NewReader(".foo { }"), "",
			store.Metadata{URL: u, ResolvedURL: canonicalURL, ContentType: "text/css"})
		return canonicalURL, err
	}

	res, err := resolveOne(Options{Lockfile: lf, Store: st, FetchOnDemand: hook},
		api.OnResolveArgs{Path: "vuetify/lib/foo.css.mjs"})
	if err != nil {
		t.Fatalf("resolveOne: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("expected no errors, got: %v", res.Errors)
	}
	if res.Path != canonicalURL {
		t.Errorf("resolver should use the canonical URL as Path, got %q, want %q", res.Path, canonicalURL)
	}
}

// TestResolver_SubpathHookFailure_PropagatesError: if the
// FetchOnDemand hook returns an error (network failure, 404,
// etc.), the resolver should fall through to the same "no
// cached blob" error rather than surface the fetch error
// directly — the operator needs a single consistent message.
func TestResolver_SubpathHookFailure_PropagatesError(t *testing.T) {
	tmp := t.TempDir()
	st, err := store.New(tmp)
	if err != nil {
		t.Fatal(err)
	}
	lf := lockfile.New()
	lf.Add(lockfile.Package{
		Spec:     "@apollo/client",
		Version:  "3.14.0",
		Resolved: "https://esm.sh/@apollo/client@3.14.0",
	})
	failing := func(u string) (string, error) {
		return "", errors.New("network borked")
	}

	res, err := resolveOne(Options{Lockfile: lf, Store: st, FetchOnDemand: failing},
		api.OnResolveArgs{Path: "@apollo/client/core"})
	if err != nil {
		t.Fatalf("resolveOne: %v", err)
	}
	if len(res.Errors) == 0 {
		t.Fatal("expected 'no cached blob' error when hook fails")
	}
}

// TestLooksLikePackageImport: regression for the v0.88.2 bug
// where on-demand fetch fired for tsconfig-paths aliases like
// `@/composables/foo`, hitting esm.sh with 400 + cluttering
// operator output. Only REAL package shapes should be subject
// to on-demand fetch.
func TestLooksLikePackageImport(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// Tsconfig aliases + tilde imports + node-imports —
		// NEVER on esm.sh.
		{"@/foo", false},
		{"@/", false},
		{"@/composables/useFoo", false},
		{"~/foo/bar", false},
		{"#internal", false},
		{"", false},
		{"@", false},
		// Real package shapes.
		{"vue", true},
		{"vue-router", true},
		{"@vue/runtime-dom", true},
		{"@apollo/client/core", true},
		{"@mdi/font/css/foo.css", true},
	}
	for _, c := range cases {
		got := looksLikePackageImport(c.in)
		if got != c.want {
			t.Errorf("looksLikePackageImport(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
