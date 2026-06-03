package viteless

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// copyTree recursively copies src into dst (files + dirs).
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatalf("copyTree: %v", err)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDev_NodeModules verifies the auto node_modules path: a project with a
// node_modules dir resolves bare imports through the optimizer (/@nm/),
// fully offline, and handles a CJS package via esbuild interop.
func TestDev_NodeModules(t *testing.T) {
	app := filepath.Join(t.TempDir(), "app")
	writeFile(t, filepath.Join(app, "index.html"),
		`<html><head></head><body><div id="app"></div><script type="module" src="/src/main.ts"></script></body></html>`)
	writeFile(t, filepath.Join(app, "src", "main.ts"),
		`import pad from "leftpad";`+"\n"+`document.getElementById("app").textContent = pad("hi");`)
	writeFile(t, filepath.Join(app, "package.json"),
		`{"name":"nm-app","private":true,"dependencies":{"leftpad":"1.0.0"}}`)
	// A CJS package (default export via module.exports) to exercise interop.
	writeFile(t, filepath.Join(app, "node_modules", "leftpad", "package.json"),
		`{"name":"leftpad","version":"1.0.0","main":"index.js"}`)
	writeFile(t, filepath.Join(app, "node_modules", "leftpad", "index.js"),
		`module.exports = function leftpad(s){ return "    " + s; };`)

	d, err := Dev(DevConfig{Root: app, Addr: "127.0.0.1:0", CacheRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("Dev: %v", err)
	}
	defer d.Close()
	base := strings.TrimSuffix(d.URL(), "/")

	// main.ts's bare "leftpad" import is rewritten to the optimizer URL.
	_, body := hostGet(t, base, "/src/main.ts")
	if !strings.Contains(body, `from "/@nm/leftpad.js"`) {
		t.Fatalf("bare import not routed to optimizer:\n%s", body)
	}
	// The optimized bundle serves and contains the package's code (no network).
	code, nm := hostGet(t, base, "/@nm/leftpad.js")
	if code != 200 {
		t.Fatalf("/@nm/leftpad.js status %d", code)
	}
	if !strings.Contains(nm, "leftpad") {
		t.Errorf("optimized bundle missing package code:\n%s", nm[:min(len(nm), 300)])
	}
}

func TestDev_ServesShellProxiesAndHMR(t *testing.T) {
	// Work on a copy so the watcher's touch doesn't mutate testdata.
	app := filepath.Join(t.TempDir(), "app")
	copyTree(t, filepath.Join("testdata", "ts-app"), app)

	// Stub backend the dev server proxies unmatched requests to.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ping" {
			io.WriteString(w, "BACKEND_PONG")
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer backend.Close()

	d, err := Dev(DevConfig{
		Root:         app,
		Addr:         "127.0.0.1:0", // random free port
		ProxyTarget:  backend.URL,
		CacheRoot:    t.TempDir(),
		LockfilePath: filepath.Join(t.TempDir(), "nexus.lock"),
	})
	if err != nil {
		t.Fatalf("Dev: %v", err)
	}
	defer d.Close()

	// 1) Shell served with the HMR client injected.
	code, body := hostGet(t, strings.TrimSuffix(d.URL(), "/"), "/")
	if code != 200 {
		t.Fatalf("GET / status %d", code)
	}
	if !strings.Contains(body, "/@viteless/client.js") {
		t.Errorf("shell missing HMR client:\n%s", body)
	}

	// 2) Unmatched request proxies to the backend.
	if code, body := hostGet(t, strings.TrimSuffix(d.URL(), "/"), "/api/ping"); code != 200 || body != "BACKEND_PONG" {
		t.Errorf("proxy: got %d %q, want 200 BACKEND_PONG", code, body)
	}

	// 3) Editing a source file pushes an HMR update over SSE.
	frames := make(chan string, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		req, _ := http.NewRequestWithContext(ctx, "GET", d.URL()+"@viteless/hmr", nil)
		resp, rerr := http.DefaultClient.Do(req)
		if rerr != nil {
			return
		}
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "data:") {
				frames <- line
			}
		}
	}()

	time.Sleep(150 * time.Millisecond) // let the SSE client connect
	if err := os.WriteFile(filepath.Join(app, "src", "main.ts"), []byte("export const touched = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(4 * time.Second)
	for {
		select {
		case line := <-frames:
			if strings.Contains(line, `"type":"update"`) && strings.Contains(line, "/src/main.ts") {
				return // success
			}
		case <-deadline:
			t.Fatal("timed out waiting for HMR update frame for /src/main.ts")
		}
	}
}
