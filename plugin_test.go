package viteless

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPlugin_TailwindDev verifies the built-in Tailwind plugin expands
// directives in the dev pipeline (previously build-only). Skips when the
// standalone tailwindcss binary isn't installed.
func TestPlugin_TailwindDev(t *testing.T) {
	if _, err := exec.LookPath("tailwindcss"); err != nil {
		t.Skip("tailwindcss CLI not on PATH")
	}
	app := filepath.Join(t.TempDir(), "app")
	writeFile(t, filepath.Join(app, "index.html"),
		`<html><head></head><body><div class="text-red-500 p-4">hi</div><script type="module" src="/src/main.ts"></script></body></html>`)
	writeFile(t, filepath.Join(app, "src", "main.ts"), `import "./style.css";`)
	writeFile(t, filepath.Join(app, "src", "style.css"), `@import "tailwindcss";`)

	d, err := Dev(DevConfig{Root: app, Addr: "127.0.0.1:0", CacheRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("Dev: %v", err)
	}
	defer d.Close()
	_, css := hostGet(t, strings.TrimSuffix(d.URL(), "/"), "/src/style.css")
	if !strings.Contains(css, "text-red-500") {
		t.Errorf("Tailwind did not expand in dev (no .text-red-500 utility):\n%.300s", css)
	}
}

// markerPlugin replaces __MARK__ with MARKED in any module's code, and
// injects a meta tag into the HTML shell — a minimal Plugin exercising the
// transform + transformIndexHtml hooks in both dev and build.
type markerPlugin struct{}

func (markerPlugin) Name() string { return "marker" }

func (markerPlugin) Transform(code, id string) (string, bool, error) {
	if strings.Contains(code, "__MARK__") {
		return strings.ReplaceAll(code, "__MARK__", "MARKED"), true, nil
	}
	return code, false, nil
}

func (markerPlugin) TransformIndexHtml(html string) (string, error) {
	return strings.Replace(html, "</head>", `<meta name="by" content="marker"></head>`, 1), nil
}

func markerApp(t *testing.T) string {
	t.Helper()
	app := filepath.Join(t.TempDir(), "app")
	writeFile(t, filepath.Join(app, "index.html"),
		`<html><head></head><body><div id="app"></div><script type="module" src="/src/main.ts"></script></body></html>`)
	writeFile(t, filepath.Join(app, "src", "main.ts"),
		`export const tag = "__MARK__";`+"\n"+`document.title = tag;`)
	return app
}

func TestPlugin_TransformAndHtml_Dev(t *testing.T) {
	app := markerApp(t)
	d, err := Dev(DevConfig{Root: app, Addr: "127.0.0.1:0", CacheRoot: t.TempDir(), Plugins: []Plugin{markerPlugin{}}})
	if err != nil {
		t.Fatalf("Dev: %v", err)
	}
	defer d.Close()
	base := strings.TrimSuffix(d.URL(), "/")

	_, mod := hostGet(t, base, "/src/main.ts")
	if !strings.Contains(mod, "MARKED") || strings.Contains(mod, "__MARK__") {
		t.Errorf("transform hook did not run in dev:\n%s", mod)
	}
	_, html := hostGet(t, base, "/")
	if !strings.Contains(html, `content="marker"`) {
		t.Errorf("transformIndexHtml hook did not run in dev:\n%s", html)
	}
}

func TestPlugin_Transform_Build(t *testing.T) {
	app := markerApp(t)
	out := filepath.Join(t.TempDir(), "dist")
	res, err := Build(BuildConfig{
		Root:         app,
		OutDir:       out,
		CacheRoot:    t.TempDir(),
		LockfilePath: filepath.Join(t.TempDir(), "nexus.lock"),
		Minify:       boolPtr(false),
		Splitting:    boolPtr(false),
		Plugins:      []Plugin{markerPlugin{}},
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
	if !strings.Contains(string(js), "MARKED") || strings.Contains(string(js), "__MARK__") {
		t.Errorf("transform hook did not run in build:\n%s", js)
	}
	idx, err := os.ReadFile(filepath.Join(out, "index.html"))
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	if !strings.Contains(string(idx), `content="marker"`) {
		t.Errorf("transformIndexHtml hook did not run in build:\n%s", idx)
	}
}

// enforcePlugin records call order to verify pre/normal/post sequencing.
type orderPlugin struct {
	name    string
	enforce string
	log     *[]string
}

func (p orderPlugin) Name() string     { return p.name }
func (p orderPlugin) Enforce() string  { return p.enforce }
func (p orderPlugin) Transform(code, id string) (string, bool, error) {
	*p.log = append(*p.log, p.name)
	return code, false, nil
}

func TestPlugin_EnforceOrdering(t *testing.T) {
	var log []string
	c := newPluginContainer([]Plugin{
		orderPlugin{"post1", "post", &log},
		orderPlugin{"normal1", "", &log},
		orderPlugin{"pre1", "pre", &log},
		orderPlugin{"normal2", "", &log},
	})
	if _, err := c.transform("code", "x.ts"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(log, ",")
	want := "pre1,normal1,normal2,post1"
	if got != want {
		t.Errorf("order = %q, want %q", got, want)
	}
}
