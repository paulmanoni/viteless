package fetcher

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/paulmanoni/viteless/internal/store"
)

// newTestStore mirrors the store package's test helper — t.TempDir
// so concurrent test runs don't share cache state.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return s
}

// mockRegistry returns an httptest server that serves a tiny fake
// registry. Routes:
//
//	GET /vue           → 302 /vue@3.4.21
//	GET /vue@3.4.21    → 200 "import 'shared'; export default x"
//	GET /shared        → 302 /shared@1.0.0
//	GET /shared@1.0.0  → 200 "export const x = 1"
//	GET /react         → 200 "export default React" (no version pin)
//	GET /styles.css    → 200 "body { color: red }" (CSS — no imports)
//
// Test cases tweak this set as needed.
func mockRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	// Direct routes — handle exact matches so /vue doesn't accidentally
	// match /vue@3.4.21 via prefix matching.
	mux.HandleFunc("/vue", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/vue@3.4.21", http.StatusFound)
	})
	mux.HandleFunc("/vue@3.4.21", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("ETag", `"vue-etag"`)
		_, _ = w.Write([]byte("import 'shared';\nexport default 42;\n"))
	})
	mux.HandleFunc("/shared", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/shared@1.0.0", http.StatusFound)
	})
	mux.HandleFunc("/shared@1.0.0", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte("export const x = 1;\n"))
	})
	mux.HandleFunc("/react", func(w http.ResponseWriter, r *http.Request) {
		// No redirect — registry doesn't version-pin this one. The
		// fetcher records empty Version in the lockfile.
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte("export default {};\n"))
	})
	mux.HandleFunc("/styles.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		_, _ = w.Write([]byte("body{color:red}"))
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetcher_FetchSimplePackage(t *testing.T) {
	srv := mockRegistry(t)
	f := New(newTestStore(t), srv.URL)

	res, err := f.Fetch(context.Background(), "react")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Root.Spec != "react" {
		t.Errorf("Spec = %q, want react", res.Root.Spec)
	}
	if !strings.HasPrefix(res.Root.Integrity, "sha256-") {
		t.Errorf("Integrity = %q, want sha256- prefix", res.Root.Integrity)
	}
	if res.Root.Resolved != srv.URL+"/react" {
		t.Errorf("Resolved = %q", res.Root.Resolved)
	}
	if len(res.Transitive) != 0 {
		t.Errorf("Transitive should be empty for react, got %v", keysOfGeneric(res.Transitive))
	}
}

func TestFetcher_FollowsRedirectToVersionPin(t *testing.T) {
	srv := mockRegistry(t)
	f := New(newTestStore(t), srv.URL)

	res, err := f.Fetch(context.Background(), "vue")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Root.Version != "3.4.21" {
		t.Errorf("Version = %q, want 3.4.21 (redirect-pin)", res.Root.Version)
	}
	if res.Root.Resolved != srv.URL+"/vue@3.4.21" {
		t.Errorf("Resolved = %q, want %s/vue@3.4.21", res.Root.Resolved, srv.URL)
	}
}

func TestFetcher_RecursesIntoImports(t *testing.T) {
	srv := mockRegistry(t)
	f := New(newTestStore(t), srv.URL)

	res, err := f.Fetch(context.Background(), "vue")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// vue@3.4.21 imports 'shared' → registry redirects to shared@1.0.0
	if _, ok := res.Transitive["shared@1.0.0"]; !ok {
		t.Errorf("Transitive missing shared@1.0.0; got keys %v", keysOfGeneric(res.Transitive))
	}
	if len(res.Root.Deps) != 1 || res.Root.Deps[0] != "shared@1.0.0" {
		t.Errorf("Root.Deps = %v, want [shared@1.0.0]", res.Root.Deps)
	}
}

func TestFetcher_CachedBlobsNotRefetched(t *testing.T) {
	// Drive two Fetches against the same registry but count HTTP
	// requests via a counting transport. Second Fetch should be
	// served from the store with zero extra GETs for vue@3.4.21
	// and shared@1.0.0 (we still HEAD-resolve the bare /vue URL
	// every time, since the version pin may have moved).
	srv := mockRegistry(t)
	counted := &countingTransport{base: http.DefaultTransport}
	st := newTestStore(t)
	f := New(st, srv.URL)
	f.HTTP = &http.Client{Transport: counted}

	if _, err := f.Fetch(context.Background(), "vue"); err != nil {
		t.Fatalf("Fetch 1: %v", err)
	}
	firstRound := counted.count

	if _, err := f.Fetch(context.Background(), "vue"); err != nil {
		t.Fatalf("Fetch 2: %v", err)
	}
	secondRound := counted.count - firstRound

	// We always re-resolve the bare URLs to pick up registry
	// version drift, but the resolved-URL bodies should NOT be
	// re-downloaded. With redirect-following the bare URL HEAD
	// counts as 2 requests (the bare + the resolved); but those
	// only fetch headers, not body bytes. For test simplicity we
	// just assert second round < first round.
	if secondRound >= firstRound {
		t.Errorf("second Fetch did %d requests vs first's %d — cache not effective",
			secondRound, firstRound)
	}
}

func TestFetcher_HTTPErrorSurfacedTyped(t *testing.T) {
	srv := mockRegistry(t)
	f := New(newTestStore(t), srv.URL)

	_, err := f.Fetch(context.Background(), "missing")
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("err type = %T, want *HTTPError", err)
	}
	if he.Status != 404 {
		t.Errorf("Status = %d, want 404", he.Status)
	}
	if !strings.Contains(he.URL, "/missing") {
		t.Errorf("URL = %q", he.URL)
	}
}

func TestFetcher_CSSContentNotScannedForImports(t *testing.T) {
	srv := mockRegistry(t)
	f := New(newTestStore(t), srv.URL)

	res, err := f.Fetch(context.Background(), "styles.css")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// Even if our regex would (incorrectly) find a match in CSS,
	// the JS-content gate stops the recursion. Transitive empty.
	if len(res.Transitive) != 0 {
		t.Errorf("CSS Fetch leaked transitive imports: %v", keysOfGeneric(res.Transitive))
	}
	if !strings.Contains(res.Root.ContentType, "text/css") {
		t.Errorf("ContentType = %q, want text/css", res.Root.ContentType)
	}
}

func TestExtractVersionFromURL(t *testing.T) {
	cases := []struct{ url, want string }{
		{"https://esm.sh/vue@3.4.21", "3.4.21"},
		{"https://esm.sh/@vue/runtime-dom@3.4.21", "3.4.21"},
		{"https://esm.sh/vue", ""},
		{"not a url", ""},
	}
	for _, tc := range cases {
		got := extractVersionFromURL(tc.url)
		if got != tc.want {
			t.Errorf("extractVersionFromURL(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestResolveAgainst(t *testing.T) {
	cases := []struct {
		parent, imp, want string
	}{
		{"https://esm.sh/vue@3.4.21", "./shared.js", "https://esm.sh/shared.js"},
		{"https://esm.sh/vue@3.4.21", "/v135/x.js", "https://esm.sh/v135/x.js"},
		{"https://esm.sh/vue@3.4.21", "lodash", "lodash"}, // bare spec — unchanged
		{"https://esm.sh/vue@3.4.21", "https://esm.sh/other", "https://esm.sh/other"},
	}
	for _, tc := range cases {
		got, err := resolveAgainst(tc.parent, tc.imp)
		if err != nil {
			t.Fatalf("resolveAgainst(%q,%q): %v", tc.parent, tc.imp, err)
		}
		if got != tc.want {
			t.Errorf("resolveAgainst(%q,%q) = %q, want %q",
				tc.parent, tc.imp, got, tc.want)
		}
	}
}

func TestFetcher_ExternalQueryAppliedToBareSpecs(t *testing.T) {
	var (
		observed []string
		baseURL  string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = append(observed, r.URL.String())
		w.Header().Set("Content-Type", "application/javascript")
		// Body recurses into an absolute-URL sibling so we also
		// exercise the recursion-path branch — its request URL
		// should NOT carry external=.
		if r.URL.Path == "/vue-flow" {
			_, _ = w.Write([]byte(`import "` + baseURL + `/internal/dep.mjs";` + "\n"))
			return
		}
		if r.URL.Path == "/internal/dep.mjs" {
			_, _ = w.Write([]byte("export default 1;\n"))
			return
		}
		_, _ = w.Write([]byte("export default 1;\n"))
	}))
	defer srv.Close()
	baseURL = srv.URL

	f := New(newTestStore(t), srv.URL)
	f.External = []string{"vue", "react"}

	if _, err := f.Fetch(context.Background(), "vue-flow"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// The bare-spec request must carry the external clause.
	if len(observed) == 0 || !strings.Contains(observed[0], "external=vue,react") {
		t.Fatalf("bare-spec request missing external query, got %q", observed)
	}
	// The recursion request (absolute URL) must NOT carry external.
	var sawInternal bool
	for _, u := range observed {
		if strings.Contains(u, "/internal/dep.mjs") {
			sawInternal = true
			if strings.Contains(u, "external=") {
				t.Errorf("absolute-URL recursion picked up external query: %s", u)
			}
		}
	}
	if !sawInternal {
		t.Errorf("recursion didn't fire — observed = %v", observed)
	}
}

func TestFetcher_ExternalComposesWithURLQuery(t *testing.T) {
	var observed string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte("export default 1;\n"))
	}))
	defer srv.Close()

	f := New(newTestStore(t), srv.URL)
	f.URLQuery = "target=es2015"
	f.External = []string{"vue"}

	if _, err := f.Fetch(context.Background(), "any-pkg"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// Order matters — URLQuery is composed first so it stays on the
	// left of the "&" join. The exact full match keeps us honest
	// about the join character (spec uses & not ;).
	if observed != "target=es2015&external=vue" {
		t.Errorf("query = %q, want %q", observed, "target=es2015&external=vue")
	}
}

func TestFetcher_CanonicalizesUnversionedURLViaXESMPath(t *testing.T) {
	// Mirrors esm.sh's 2025-era behavior: bare /vue returns 200
	// directly (no 302 to /vue@3.5.34) and discloses the resolved
	// version only via the X-ESM-Path response header. The fetcher
	// must rewrite the lockfile entry to the versioned URL or the
	// lockfile will silently drift on the next `nexus install`.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("X-ESM-Path", "/vue@3.5.34/es2022/vue.mjs")
		_, _ = w.Write([]byte("export default 1;\n"))
	}))
	defer srv.Close()

	f := New(newTestStore(t), srv.URL)
	f.External = []string{"vue"}

	res, err := f.Fetch(context.Background(), "vue")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Root.Version != "3.5.34" {
		t.Errorf("Version = %q, want 3.5.34 (extracted from canonicalized URL)", res.Root.Version)
	}
	wantURL := srv.URL + "/vue@3.5.34?external=vue"
	if res.Root.Resolved != wantURL {
		t.Errorf("Resolved = %q, want %q", res.Root.Resolved, wantURL)
	}
}

func TestFetcher_ScopedPackageCanonicalization(t *testing.T) {
	// Scoped packages have an extra path segment to skip — the
	// "@vue" scope itself isn't where the version lives, it's on
	// the next segment ("runtime-dom@3.5.34").
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("X-ESM-Path", "/@vue/runtime-dom@3.5.34/es2022/runtime-dom.mjs")
		_, _ = w.Write([]byte("export default 1;\n"))
	}))
	defer srv.Close()

	f := New(newTestStore(t), srv.URL)
	res, err := f.Fetch(context.Background(), "@vue/runtime-dom")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Root.Version != "3.5.34" {
		t.Errorf("Version = %q, want 3.5.34", res.Root.Version)
	}
	wantURL := srv.URL + "/@vue/runtime-dom@3.5.34"
	if res.Root.Resolved != wantURL {
		t.Errorf("Resolved = %q, want %q", res.Root.Resolved, wantURL)
	}
}

func TestFetcher_EmptyExternalLeavesURLUnchanged(t *testing.T) {
	var observed string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte("export default 1;\n"))
	}))
	defer srv.Close()

	f := New(newTestStore(t), srv.URL)
	// External left nil — must not synthesize an "external=" clause.
	if _, err := f.Fetch(context.Background(), "any-pkg"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if observed != "" {
		t.Errorf("query = %q, want empty (no External set)", observed)
	}
}

func TestIntegrityHex_RoundTrip(t *testing.T) {
	good := strings.Repeat("a", 64)
	if got := IntegrityHex("sha256-" + good); got != good {
		t.Errorf("good case = %q", got)
	}
	if got := IntegrityHex("sha256-not-hex"); got != "" {
		t.Errorf("bad case should return empty, got %q", got)
	}
	if got := IntegrityHex(""); got != "" {
		t.Errorf("empty case should return empty")
	}
}

// --- helpers --------------------------------------------------------

// countingTransport wraps an http.RoundTripper and counts requests.
// Used to assert cache effectiveness without parsing detailed log
// output.
type countingTransport struct {
	base  http.RoundTripper
	count int
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.count++
	return c.base.RoundTrip(r)
}

func keysOfGeneric[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
