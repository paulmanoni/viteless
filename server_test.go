package viteless

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeHost is an in-memory Host: a module map + trivial transform + a
// resolver that maps bare specs to /@id/ URLs. No esbuild, no network.
type fakeHost struct {
	mods map[string]struct {
		src  string
		kind string
	}
	failTransform bool
}

func (h *fakeHost) LoadModule(p string) ([]byte, string, bool) {
	m, ok := h.mods[p]
	if !ok {
		return nil, "", false
	}
	return []byte(m.src), m.kind, true
}

// Transform here is a passthrough (the fake "source" is already JS),
// unless failTransform is set — then it errors so the overlay path runs.
func (h *fakeHost) Transform(p string, src []byte) ([]byte, error) {
	if h.failTransform {
		return nil, errDummyTransform
	}
	return src, nil
}

var errDummyTransform = errTransform("boom")

type errTransform string

func (e errTransform) Error() string { return string(e) }

func (h *fakeHost) ResolveImport(spec string, kind SpecKind, importer string) string {
	switch kind {
	case SpecBare:
		return "/@id/" + spec
	case SpecRelative:
		return "/src" + spec[1:] // "./Foo.js" -> "/src/Foo.js"
	}
	return ""
}

func newTestServer() *httptest.Server {
	h := &fakeHost{mods: map[string]struct {
		src  string
		kind string
	}{
		"/src/main.js": {src: `import { h } from "vue"; import App from "./App.js"; console.log(h, App);`, kind: "js"},
		"/src/App.js":  {src: `export default { name: "App" };`, kind: "js"},
		"/src/x.css":   {src: `.a{color:red}`, kind: "css"},
		"/logo.png":    {src: ``, kind: "asset"},
	}}
	return httptest.NewServer(NewServer(h).Handler())
}

// newTestHost returns the same in-memory host used by newTestServer, plus
// an index.html (kind "html") and a non-empty asset so the raw/url and
// proxy tests have something to serve.
func newTestHost() *fakeHost {
	return &fakeHost{mods: map[string]struct {
		src  string
		kind string
	}{
		"/src/main.js": {src: `import { h } from "vue"; import App from "./App.js"; console.log(h, App);`, kind: "js"},
		"/src/App.js":  {src: `export default { name: "App" };`, kind: "js"},
		"/src/x.css":   {src: `.a{color:red}`, kind: "css"},
		"/logo.png":    {src: "PNGBYTES", kind: "asset"},
		"/index.html":  {src: "<html><head></head><body><script type=\"module\" src=\"/src/main.js\"></script></body></html>", kind: "html"},
	}}
}

func get(t *testing.T, base, p string) (int, string) {
	t.Helper()
	resp, err := http.Get(base + p)
	if err != nil {
		t.Fatalf("GET %s: %v", p, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestServer_ServesModuleWithRewrittenImports(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()
	code, body := get(t, ts.URL, "/src/main.js")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if !strings.Contains(body, `from "/@id/vue"`) {
		t.Errorf("bare import not rewritten to URL:\n%s", body)
	}
	if !strings.Contains(body, `from "/src/App.js"`) {
		t.Errorf("relative import not rewritten:\n%s", body)
	}
}

func TestServer_ServesJSContentType(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/src/main.js")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("content-type = %q, want text/javascript", ct)
	}
}

func TestServer_CSSServedAsStyleInjectingModule(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()
	code, body := get(t, ts.URL, "/src/x.css")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if !strings.Contains(body, "createElement('style')") || !strings.Contains(body, "color:red") {
		t.Errorf("css not wrapped as style-injecting module:\n%s", body)
	}
	if !strings.Contains(body, "import.meta.hot") {
		t.Errorf("css module should self-accept for HMR:\n%s", body)
	}
}

func TestServer_AssetServedAsURLExport(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()
	code, body := get(t, ts.URL, "/logo.png")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	// Default import binds to the asset's ?url path (which yields raw bytes).
	if !strings.Contains(body, `export default "/logo.png?url"`) {
		t.Errorf("asset not served as ?url export:\n%s", body)
	}
}

func TestServer_AssetURLQueryServesRawBytes(t *testing.T) {
	ts := httptest.NewServer(NewServer(newTestHost()).Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/logo.png?url")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "PNGBYTES" {
		t.Errorf("?url should serve raw bytes, got %q", string(b))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
		t.Errorf("?url content-type = %q, want image/png", ct)
	}
}

func TestServer_AssetResourceFetchServesRawBytes(t *testing.T) {
	ts := httptest.NewServer(NewServer(newTestHost()).Handler())
	defer ts.Close()
	// A browser resource fetch (Sec-Fetch-Dest: image) must get raw bytes
	// even without ?url — a <img src> / <link href> / CSS url() load.
	req, _ := http.NewRequest("GET", ts.URL+"/logo.png", nil)
	req.Header.Set("Sec-Fetch-Dest", "image")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "PNGBYTES" {
		t.Errorf("resource fetch should serve raw bytes, got %q", string(b))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
		t.Errorf("content-type = %q, want image/png", ct)
	}
}

func TestServer_AssetModuleImportServesURLExport(t *testing.T) {
	ts := httptest.NewServer(NewServer(newTestHost()).Handler())
	defer ts.Close()
	// A JS module import (Sec-Fetch-Dest: script) gets the URL-export
	// module, not raw bytes.
	req, _ := http.NewRequest("GET", ts.URL+"/logo.png", nil)
	req.Header.Set("Sec-Fetch-Dest", "script")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `export default "/logo.png?url"`) {
		t.Errorf("module import should get URL export, got %q", string(b))
	}
}

func TestServer_RawQueryReturnsStringModule(t *testing.T) {
	ts := httptest.NewServer(NewServer(newTestHost()).Handler())
	defer ts.Close()
	code, body := get(t, ts.URL, "/src/x.css?raw")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if !strings.Contains(body, `export default ".a{color:red}"`) {
		t.Errorf("?raw should export source as string:\n%s", body)
	}
}

func TestServer_InjectsHotContextIntoJS(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()
	_, body := get(t, ts.URL, "/src/main.js")
	if !strings.Contains(body, `import.meta.hot = __viteless_hot("/src/main.js")`) {
		t.Errorf("js module missing hot-context preamble:\n%s", body)
	}
}

func TestServer_InjectsHotContextIntoCSS(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()
	_, body := get(t, ts.URL, "/src/x.css")
	if !strings.Contains(body, `import.meta.hot = __viteless_hot("/src/x.css")`) {
		t.Errorf("css module missing hot-context preamble:\n%s", body)
	}
}

func TestServer_ServesTransformedHTML(t *testing.T) {
	ts := httptest.NewServer(NewServer(newTestHost()).Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/index.html")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("html content-type = %q", ct)
	}
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	if !strings.Contains(body, `/@viteless/client.js`) {
		t.Errorf("html not injected with HMR client:\n%s", body)
	}
	// Client must precede the entry module so __viteless_hot is defined first.
	if strings.Index(body, "/@viteless/client.js") > strings.Index(body, "/src/main.js") {
		t.Errorf("client script must come before entry module:\n%s", body)
	}
}

func TestServer_TransformHTMLInjectsClient(t *testing.T) {
	s := NewServer(newTestHost())
	out := string(s.TransformHTML([]byte("<html><head></head><body></body></html>")))
	if !strings.Contains(out, `/@viteless/client.js`) {
		t.Errorf("TransformHTML did not inject client:\n%s", out)
	}
}

func TestServer_ProxyFallbackForUnservedPaths(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("API:" + r.URL.Path))
	}))
	defer backend.Close()
	ts := httptest.NewServer(NewServer(newTestHost(), WithProxy(backend.URL)).Handler())
	defer ts.Close()
	code, body := get(t, ts.URL, "/graphql")
	if code != 200 || body != "API:/graphql" {
		t.Errorf("unserved path should proxy to backend; got %d %q", code, body)
	}
}

func TestServer_SPAFallbackForDocumentNav(t *testing.T) {
	// Backend would answer with its own page — the dev server must NOT
	// reach it for an HTML navigation to a client-side route.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("BACKEND-INDEX"))
	}))
	defer backend.Close()
	ts := httptest.NewServer(NewServer(newTestHost(), WithProxy(backend.URL)).Handler())
	defer ts.Close()

	// Refresh of /login (no such file) as a document navigation → SPA shell.
	req, _ := http.NewRequest("GET", ts.URL+"/login", nil)
	req.Header.Set("Sec-Fetch-Dest", "document")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	if strings.Contains(body, "BACKEND-INDEX") {
		t.Errorf("document nav must serve SPA shell, not proxy to backend:\n%s", body)
	}
	if !strings.Contains(body, "/@viteless/client.js") || !strings.Contains(body, "/src/main.js") {
		t.Errorf("SPA fallback should serve transformed index.html:\n%s", body)
	}

	// A non-nav API call to the same kind of path still proxies.
	code, apiBody := get(t, ts.URL, "/graphql")
	if code != 200 || apiBody != "BACKEND-INDEX" {
		t.Errorf("API call should still proxy; got %d %q", code, apiBody)
	}
}

func TestServer_ProxyResolverResolvesLazily(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("API:" + r.URL.Path))
	}))
	defer backend.Close()

	// target is empty until "discovered" — mirrors a dev runner that
	// learns the backend addr only after the app boots.
	var target string
	ts := httptest.NewServer(NewServer(newTestHost(), WithProxyResolver(func() string { return target })).Handler())
	defer ts.Close()

	// Unknown target → 502, not a crash.
	if code, _ := get(t, ts.URL, "/graphql"); code != 502 {
		t.Errorf("unresolved target should 502; got %d", code)
	}
	// Once resolved, the same path proxies through — self-heals.
	target = backend.URL
	code, body := get(t, ts.URL, "/graphql")
	if code != 200 || body != "API:/graphql" {
		t.Errorf("after resolve, should proxy; got %d %q", code, body)
	}
}

func TestServer_TransformErrorRendersOverlay(t *testing.T) {
	h := newTestHost()
	h.mods["/src/bad.js"] = struct {
		src  string
		kind string
	}{src: "<<<broken", kind: "js"}
	h.failTransform = true
	ts := httptest.NewServer(NewServer(h).Handler())
	defer ts.Close()
	_, body := get(t, ts.URL, "/src/bad.js")
	if !strings.Contains(body, "__viteless_error") {
		t.Errorf("transform failure should emit overlay module:\n%s", body)
	}
}

func TestServer_404ForUnknownModule(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()
	if code, _ := get(t, ts.URL, "/nope.js"); code != 404 {
		t.Errorf("unknown module status = %d, want 404", code)
	}
}

func TestServer_ServesClientRuntime(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()
	code, body := get(t, ts.URL, "/@viteless/client.js")
	if code != 200 || !strings.Contains(body, "EventSource") {
		t.Errorf("client runtime missing or wrong (code %d):\n%s", code, body)
	}
}

func TestHMR_BroadcastReachesSSEClient(t *testing.T) {
	h := NewHMR()
	srv := httptest.NewServer(http.HandlerFunc(h.ServeSSE))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Stream everything the client receives into a channel, line by line,
	// so we don't race on a single Read landing exactly on the payload.
	lines := make(chan string, 8)
	go func() {
		buf := make([]byte, 512)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				lines <- string(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	// Drain the ": connected" preamble.
	select {
	case <-lines:
	case <-time.After(2 * time.Second):
		t.Fatal("no SSE preamble")
	}

	// Broadcast repeatedly (the client must be registered by now, but a
	// few retries make the test robust to scheduling) and assert the
	// payload arrives.
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		h.Broadcast(Update{Type: "update", Path: "/src/Foo.vue"})
		select {
		case got := <-lines:
			if strings.Contains(got, `"path":"/src/Foo.vue"`) {
				return // success
			}
		case <-tick.C:
		case <-deadline:
			t.Fatal("broadcast never reached SSE client")
		}
	}
}
