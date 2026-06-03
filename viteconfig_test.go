package viteless

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestViteConfig_Dev verifies a project's vite.config.ts is read and applied
// in dev: the "@/" alias resolves to the source tree and server.proxy routes
// unmatched requests to the configured backend — with no DevConfig flags.
func TestViteConfig_Dev(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "FROM_VITE_PROXY")
	}))
	defer backend.Close()

	app := filepath.Join(t.TempDir(), "app")
	writeFile(t, filepath.Join(app, "index.html"),
		`<html><head></head><body><div id="app"></div><script type="module" src="/src/main.ts"></script></body></html>`)
	// main.ts imports via the "@/" alias defined in vite.config.ts.
	writeFile(t, filepath.Join(app, "src", "main.ts"),
		`import { hi } from "@/util";`+"\n"+`document.title = hi;`)
	writeFile(t, filepath.Join(app, "src", "util.ts"), `export const hi = "aliased";`)
	writeFile(t, filepath.Join(app, "vite.config.ts"), `import { defineConfig } from 'vite'
import { fileURLToPath, URL } from 'node:url'
export default defineConfig({
  resolve: { alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) } },
  server: { proxy: { '/api': '`+backend.URL+`' } },
})
`)

	// No Aliases, no ProxyTarget passed — both must come from vite.config.
	d, err := Dev(DevConfig{Root: app, Addr: "127.0.0.1:0", CacheRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("Dev: %v", err)
	}
	defer d.Close()
	base := strings.TrimSuffix(d.URL(), "/")

	// The "@/util" import resolves to /src/util.ts via the config's alias.
	_, mod := hostGet(t, base, "/src/main.ts")
	if !strings.Contains(mod, "/src/util.ts") {
		t.Errorf("vite.config @/ alias not applied in dev:\n%s", mod)
	}
	// An unmatched request proxies to the config's server.proxy target.
	if code, body := hostGet(t, base, "/api/x"); code != 200 || body != "FROM_VITE_PROXY" {
		t.Errorf("vite.config server.proxy not applied: got %d %q", code, body)
	}
}

// TestViteConfig_BuildBaseOutDir verifies base + build.outDir from a
// vite.config drive the production build's public path and output dir.
func TestViteConfig_BuildBaseOutDir(t *testing.T) {
	app := filepath.Join(t.TempDir(), "app")
	writeFile(t, filepath.Join(app, "index.html"),
		`<html><head></head><body><div id="app"></div><script type="module" src="/src/main.ts"></script></body></html>`)
	writeFile(t, filepath.Join(app, "src", "main.ts"), `document.title = "hi";`)
	writeFile(t, filepath.Join(app, "vite.config.ts"), `import { defineConfig } from 'vite'
export default defineConfig({ base: '/sub/', build: { outDir: 'public_html' } })
`)

	res, err := Build(BuildConfig{
		Root:         app,
		CacheRoot:    t.TempDir(),
		LockfilePath: filepath.Join(t.TempDir(), "nexus.lock"),
		Minify:       boolPtr(false),
		Splitting:    boolPtr(false),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("build errors: %v", res.Errors)
	}
	// build.outDir → output under app/public_html.
	wantOut := filepath.Join(app, "public_html")
	if res.OutDir != wantOut {
		t.Errorf("outDir = %q, want %q (from vite.config build.outDir)", res.OutDir, wantOut)
	}
	// base → index.html references /sub/main.js.
	idxBytes, rerr := os.ReadFile(filepath.Join(wantOut, "index.html"))
	if rerr != nil {
		t.Fatalf("read index.html: %v", rerr)
	}
	idx := string(idxBytes)
	if !strings.Contains(idx, `src="/sub/main.js"`) {
		t.Errorf("vite.config base not applied to index.html:\n%s", idx)
	}
}
