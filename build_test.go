package viteless

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// baseBuildCfg points Build at a fixture but routes every write (dist,
// cache, lockfile) into temp dirs so the test never mutates testdata.
func baseBuildCfg(t *testing.T, fixture string) BuildConfig {
	t.Helper()
	return BuildConfig{
		Root:         filepath.Join("testdata", fixture),
		OutDir:       filepath.Join(t.TempDir(), "dist"),
		CacheRoot:    t.TempDir(),
		LockfilePath: filepath.Join(t.TempDir(), "nexus.lock"),
	}
}

func boolPtr(b bool) *bool { return &b }

func TestBuild_VanillaTS(t *testing.T) {
	cfg := baseBuildCfg(t, "ts-app")
	// Readable, single-file output so assertions are stable.
	cfg.Minify = boolPtr(false)
	cfg.Splitting = boolPtr(false)
	res, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("build errors: %v", res.Errors)
	}

	js := filepath.Join(cfg.OutDir, "main.js")
	jsBytes, err := os.ReadFile(js)
	if err != nil {
		t.Fatalf("read main.js: %v", err)
	}
	jsSrc := string(jsBytes)
	if !strings.Contains(jsSrc, "hello, ") {
		t.Errorf("bundle missing util output:\n%s", jsSrc)
	}
	if strings.Contains(jsSrc, ": string") {
		t.Errorf("TS annotations not stripped:\n%s", jsSrc)
	}

	// CSS imported by JS is extracted to main.css.
	cssBytes, err := os.ReadFile(filepath.Join(cfg.OutDir, "main.css"))
	if err != nil {
		t.Fatalf("read main.css: %v", err)
	}
	if !strings.Contains(string(cssBytes), "rebeccapurple") {
		t.Errorf("css not extracted:\n%s", cssBytes)
	}

	// index.html references the built entry + stylesheet.
	idxBytes, err := os.ReadFile(filepath.Join(cfg.OutDir, "index.html"))
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	idx := string(idxBytes)
	if !strings.Contains(idx, `src="/main.js"`) {
		t.Errorf("index.html does not reference /main.js:\n%s", idx)
	}
	if !strings.Contains(idx, `href="/main.css"`) {
		t.Errorf("index.html does not link /main.css:\n%s", idx)
	}
}

// networkish reports whether a build error looks like a registry/network
// failure rather than a code error, so the React test can skip offline.
func networkish(errs []string) bool {
	for _, e := range errs {
		le := strings.ToLower(e)
		for _, sub := range []string{"could not resolve", "no cached blob", "fetch", "lookup", "dial", "connection", "timeout", "esm.sh", "no such host", "tls"} {
			if strings.Contains(le, sub) {
				return true
			}
		}
	}
	return false
}

func TestBuild_React(t *testing.T) {
	cfg := baseBuildCfg(t, "react-app")
	res, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) != 0 {
		if networkish(res.Errors) {
			t.Skipf("react build needs the esm.sh registry (offline?): %v", res.Errors)
		}
		t.Fatalf("build errors: %v", res.Errors)
	}

	jsBytes, err := os.ReadFile(filepath.Join(cfg.OutDir, "main.js"))
	if err != nil {
		t.Fatalf("read main.js: %v", err)
	}
	js := string(jsBytes)
	// JSX compiled away (no raw <h1> tag) and the React runtime bundled in.
	if strings.Contains(js, "<h1") {
		t.Errorf("JSX not transformed:\n%s", js[:min(len(js), 400)])
	}
	if !strings.Contains(js, "hello from react") {
		t.Errorf("app text missing from bundle")
	}
}

func TestBuild_Vue(t *testing.T) {
	cfg := baseBuildCfg(t, "vue-app")
	res, err := Build(cfg)
	if err != nil {
		// Bootstrap failures (fetching @vue/compiler-sfc) surface as a
		// Go error; treat registry/network issues as skippable.
		if networkish([]string{err.Error()}) {
			t.Skipf("vue build needs the registry (offline?): %v", err)
		}
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) != 0 {
		if networkish(res.Errors) {
			t.Skipf("vue build needs the registry (offline?): %v", res.Errors)
		}
		t.Fatalf("build errors: %v", res.Errors)
	}

	jsBytes, err := os.ReadFile(filepath.Join(cfg.OutDir, "main.js"))
	if err != nil {
		t.Fatalf("read main.js: %v", err)
	}
	js := string(jsBytes)
	// The SFC compiled: the reactive message + a render function landed
	// in the bundle, and the raw <template> block did not.
	if !strings.Contains(js, "hello from vue") {
		t.Errorf("compiled SFC text missing from bundle")
	}
	if strings.Contains(js, "<template>") {
		t.Errorf("raw <template> leaked into bundle (SFC not compiled)")
	}
}
