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
}

func (h *fakeHost) LoadModule(p string) ([]byte, string, bool) {
	m, ok := h.mods[p]
	if !ok {
		return nil, "", false
	}
	return []byte(m.src), m.kind, true
}

// Transform here is a passthrough (the fake "source" is already JS).
func (h *fakeHost) Transform(p string, src []byte) ([]byte, error) { return src, nil }

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
	if !strings.Contains(body, `export default "/logo.png"`) {
		t.Errorf("asset not served as URL export:\n%s", body)
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
