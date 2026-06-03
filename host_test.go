package viteless

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulmanoni/viteless/internal/lockfile"
	"github.com/paulmanoni/viteless/internal/resolver"
	"github.com/paulmanoni/viteless/internal/store"
)

// Host must satisfy the viteless contract.
var _ Host = (*DefaultHost)(nil)

// fixture builds a temp project (Root + src tree) and a warm store/lockfile
// with vue cached, then returns a running viteless server backed by the
// Host.
func fixture(t *testing.T) (*httptest.Server, *DefaultHost) {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", `<html><head></head><body><div id="app"></div><script type="module" src="/src/main.ts"></script></body></html>`)
	write("src/main.ts", `import { createApp } from "vue";`+"\n"+`import App from "./App.vue";`+"\n"+`import logo from "@/logo.png";`+"\n"+`const x: number = 1; console.log(createApp, App, logo, x);`)
	write("src/App.vue", "<template><div/></template>")
	write("src/logo.png", "PNGDATA")
	write("src/style.css", ".a{color:red}")
	write("src/theme.scss", "$w: 250px;\n.sidebar { width: $w; }")
	// Extensionless-import targets: a bare file (router.ts) and a
	// directory with an index (plugins/index.ts), to exercise Vite-style
	// extension + index resolution.
	write("src/router.ts", `export default {};`)
	write("src/plugins/index.ts", `export const p = 1;`)
	write("src/withexts.ts", `import r from "./router";`+"\n"+`import { p } from "./plugins";`+"\n"+`console.log(r, p);`)
	// public/ asset the HTML hard-codes (favicon convention).
	write("public/favicon.png", "FAVICON")

	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const vueURL = "https://esm.sh/vue@3.5.13/es2022/vue.mjs"
	if _, err := st.Put(vueURL, strings.NewReader(`export const createApp = () => {};`), "", store.Metadata{ContentType: "application/javascript"}); err != nil {
		t.Fatal(err)
	}
	lf := lockfile.New()
	lf.Add(lockfile.Package{Spec: "vue", Version: "3.5.13", Resolved: vueURL})

	h := NewDefaultHost(HostConfig{
		Root:     root,
		Resolver: resolver.Options{Lockfile: lf, Store: st},
		Compiler: nil, // no Vue compiler in headless test; .vue → overlay
		Aliases:  []Alias{{Prefix: "@/", Dir: filepath.Join(root, "src")}},
	})
	return httptest.NewServer(NewServer(h).Handler()), h
}

func hostGet(t *testing.T, base, p string) (int, string) {
	t.Helper()
	resp, err := http.Get(base + p)
	if err != nil {
		t.Fatalf("GET %s: %v", p, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestHost_ServesHTMLWithClient(t *testing.T) {
	ts, _ := fixture(t)
	defer ts.Close()
	code, body := hostGet(t, ts.URL, "/")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if !strings.Contains(body, "/@viteless/client.js") {
		t.Errorf("index.html missing HMR client:\n%s", body)
	}
	if !strings.Contains(body, `src="/src/main.ts"`) {
		t.Errorf("entry script not preserved:\n%s", body)
	}
	// Vue esm-bundler feature flags must be defined before any module runs.
	if !strings.Contains(body, "__VUE_OPTIONS_API__") {
		t.Errorf("index.html missing Vue feature-flag script:\n%s", body)
	}
}

func TestHost_ServesPublicAsset(t *testing.T) {
	ts, _ := fixture(t)
	defer ts.Close()
	// <Root>/public/favicon.png is served at the site root (Vite
	// convention). A browser resource fetch (?url, what a <link href>
	// image load resolves to via viteless) returns the raw bytes.
	code, body := hostGet(t, ts.URL, "/favicon.png?url")
	if code != 200 || body != "FAVICON" {
		t.Errorf("public/ asset not served at root; got %d %q", code, body)
	}
}

func TestHost_RewritesBareImportToDepURL(t *testing.T) {
	ts, _ := fixture(t)
	defer ts.Close()
	_, body := hostGet(t, ts.URL, "/src/main.ts")
	if !strings.Contains(body, `from "/@dep/https/esm.sh/vue@3.5.13/es2022/vue.mjs"`) {
		t.Errorf("bare vue import not rewritten to dep URL:\n%s", body)
	}
}

func TestHost_RewritesRelativeImport(t *testing.T) {
	ts, _ := fixture(t)
	defer ts.Close()
	_, body := hostGet(t, ts.URL, "/src/main.ts")
	if !strings.Contains(body, `"/src/App.vue"`) {
		t.Errorf("relative ./App.vue not rewritten to served path:\n%s", body)
	}
}

func TestHost_RewritesAliasImport(t *testing.T) {
	ts, _ := fixture(t)
	defer ts.Close()
	_, body := hostGet(t, ts.URL, "/src/main.ts")
	// "@/logo.png" → /src/logo.png (asset → ?url export served separately)
	if !strings.Contains(body, `"/src/logo.png"`) {
		t.Errorf("@/ alias not rewritten:\n%s", body)
	}
}

func TestHost_StripsTypeScriptTypes(t *testing.T) {
	ts, _ := fixture(t)
	defer ts.Close()
	_, body := hostGet(t, ts.URL, "/src/main.ts")
	if strings.Contains(body, "const x: number") {
		t.Errorf("TS annotation not stripped:\n%s", body)
	}
}

func TestHost_ResolvesExtensionlessImports(t *testing.T) {
	ts, _ := fixture(t)
	defer ts.Close()
	_, body := hostGet(t, ts.URL, "/src/withexts.ts")
	// "./router" must resolve to router.ts; "./plugins" to plugins/index.ts.
	if !strings.Contains(body, `"/src/router.ts"`) {
		t.Errorf("extensionless ./router not resolved to router.ts:\n%s", body)
	}
	if !strings.Contains(body, `"/src/plugins/index.ts"`) {
		t.Errorf("extensionless ./plugins not resolved to plugins/index.ts:\n%s", body)
	}
	// And both resolved modules must actually serve.
	if code, _ := hostGet(t, ts.URL, "/src/router.ts"); code != 200 {
		t.Errorf("router.ts status %d", code)
	}
	if code, _ := hostGet(t, ts.URL, "/src/plugins/index.ts"); code != 200 {
		t.Errorf("plugins/index.ts status %d", code)
	}
}

func TestHost_ServesDepBlob(t *testing.T) {
	ts, h := fixture(t)
	defer ts.Close()
	// Prime the dep mapping the way ResolveImport would.
	sp := h.toDepPath("https://esm.sh/vue@3.5.13/es2022/vue.mjs")
	code, body := hostGet(t, ts.URL, sp)
	if code != 200 {
		t.Fatalf("dep blob status %d", code)
	}
	if !strings.Contains(body, "createApp") {
		t.Errorf("dep blob body wrong:\n%s", body)
	}
}

func TestHost_ServesAssetAsURLExport(t *testing.T) {
	ts, _ := fixture(t)
	defer ts.Close()
	code, body := hostGet(t, ts.URL, "/src/logo.png")
	if code != 200 {
		t.Fatalf("asset status %d", code)
	}
	if !strings.Contains(body, `export default "/src/logo.png?url"`) {
		t.Errorf("asset not served as ?url export:\n%s", body)
	}
}

func TestHost_ServesCSSAsStyleModule(t *testing.T) {
	ts, _ := fixture(t)
	defer ts.Close()
	code, body := hostGet(t, ts.URL, "/src/style.css")
	if code != 200 {
		t.Fatalf("css status %d", code)
	}
	if !strings.Contains(body, "createElement('style')") || !strings.Contains(body, "color:red") {
		t.Errorf("css not served as style module:\n%s", body)
	}
}

func TestHost_DepPathRoundTrip(t *testing.T) {
	h := NewDefaultHost(HostConfig{Root: t.TempDir()})
	for _, u := range []string{
		"https://esm.sh/vue@3.5.13/es2022/vue.mjs",
		"https://esm.sh/@vue/runtime-core@3.5.13/x.mjs?target=es2022",
	} {
		sp := h.toDepPath(u)
		got, ok := h.canonicalFor(sp)
		if !ok || got != u {
			t.Errorf("round-trip %q → %q → %q (ok=%v)", u, sp, got, ok)
		}
	}
}
