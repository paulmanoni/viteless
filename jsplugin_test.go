package viteless

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// jsPluginConfig is a viteless.config.ts with an INLINE JavaScript plugin
// (no imports, so it bundles offline) exercising the transform +
// transformIndexHtml hooks through the Node sidecar.
const jsPluginConfig = `export default {
  plugins: [
    {
      name: 'inline-upper',
      transform(code, id) {
        if (id.endsWith('.ts')) return code.replace('PLACEHOLDER', 'REPLACED_BY_JS');
        return null;
      },
      transformIndexHtml(html) {
        return html.replace('</head>', '<meta name="js-plugin" content="ran"></head>');
      },
    },
  ],
}
`

func jsPluginApp(t *testing.T) string {
	t.Helper()
	app := filepath.Join(t.TempDir(), "app")
	writeFile(t, filepath.Join(app, "index.html"),
		`<html><head></head><body><div id="app"></div><script type="module" src="/src/main.ts"></script></body></html>`)
	writeFile(t, filepath.Join(app, "src", "main.ts"), `export const tag = "PLACEHOLDER";`)
	writeFile(t, filepath.Join(app, "viteless.config.ts"), jsPluginConfig)
	return app
}

func TestJSPlugin_Dev(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH (JS plugins require Node)")
	}
	app := jsPluginApp(t)
	d, err := Dev(DevConfig{Root: app, Addr: "127.0.0.1:0", CacheRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("Dev: %v", err)
	}
	defer d.Close()
	base := strings.TrimSuffix(d.URL(), "/")

	_, mod := hostGet(t, base, "/src/main.ts")
	if !strings.Contains(mod, "REPLACED_BY_JS") || strings.Contains(mod, "PLACEHOLDER") {
		t.Errorf("JS plugin transform hook did not run in dev:\n%s", mod)
	}
	_, html := hostGet(t, base, "/")
	if !strings.Contains(html, `content="ran"`) {
		t.Errorf("JS plugin transformIndexHtml hook did not run in dev:\n%s", html)
	}
}

func TestJSPlugin_Build(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH (JS plugins require Node)")
	}
	app := jsPluginApp(t)
	out := filepath.Join(t.TempDir(), "dist")
	res, err := Build(BuildConfig{
		Root:         app,
		OutDir:       out,
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
	js, err := os.ReadFile(filepath.Join(out, "main.js"))
	if err != nil {
		t.Fatalf("read main.js: %v", err)
	}
	if !strings.Contains(string(js), "REPLACED_BY_JS") {
		t.Errorf("JS plugin transform hook did not run in build:\n%s", js)
	}
	idx, err := os.ReadFile(filepath.Join(out, "index.html"))
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	if !strings.Contains(string(idx), `content="ran"`) {
		t.Errorf("JS plugin transformIndexHtml hook did not run in build:\n%s", idx)
	}
}
