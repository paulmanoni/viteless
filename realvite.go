package viteless

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// This file implements the highest-fidelity tier: when the real `vite`
// package is installed in the project, viteless simply delegates to it.
// You get 100% Vite compatibility (every plugin, every feature) with viteless
// acting as a thin launcher. When Vite is absent, viteless uses its own
// zero-Node engine. Set VITELESS_ENGINE=1 to force the viteless engine even
// when Vite is installed.

// findViteBin returns the path to the project's installed Vite binary, or "".
func findViteBin(root string) string {
	name := "vite"
	if runtime.GOOS == "windows" {
		name = "vite.cmd"
	}
	p := filepath.Join(root, "node_modules", ".bin", name)
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		return p
	}
	return ""
}

// useRealVite reports whether viteless should delegate to an installed Vite.
func useRealVite(root string) bool {
	if os.Getenv("VITELESS_ENGINE") == "1" {
		return false // operator forced the viteless engine
	}
	return findViteBin(root) != ""
}

var viteLocalURLRE = regexp.MustCompile(`https?://[^\s/]+(?::\d+)?/?`)

// devWithRealVite starts the project's installed Vite dev server and wraps it
// in a DevServer (so callers get the same interface). viteless does nothing
// but launch + supervise — Vite reads its own vite.config and runs every
// plugin natively.
func devWithRealVite(cfg DevConfig) (*DevServer, error) {
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	bin := findViteBin(cfg.Root)
	args := []string{"--host", "127.0.0.1"}
	if port := portOfAddr(cfg.Addr); port != "" {
		args = append(args, "--port", port, "--strictPort")
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = cfg.Root
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr // surface Vite's own diagnostics
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start vite: %w", err)
	}

	d := &DevServer{serveErr: make(chan error, 1), proc: cmd}
	urlCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			fmt.Fprintln(os.Stderr, line) // forward Vite's banner/logs
			if strings.Contains(line, "Local:") {
				if u := viteLocalURLRE.FindString(stripANSI(line[strings.Index(line, "Local:")+len("Local:"):])); u != "" {
					select {
					case urlCh <- u:
					default:
					}
				}
			}
		}
	}()
	go func() { d.serveErr <- cmd.Wait() }()

	select {
	case u := <-urlCh:
		if !strings.HasSuffix(u, "/") {
			u += "/"
		}
		d.url = u
	case <-time.After(30 * time.Second):
		d.url = "http://127.0.0.1:5173/"
	}
	logf("viteless: vite is installed — delegating to it (%s)", d.url)
	return d, nil
}

// buildWithRealVite runs the project's installed `vite build`.
func buildWithRealVite(cfg BuildConfig) (BuildResult, error) {
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	bin := findViteBin(cfg.Root)
	cmd := exec.Command(bin, "build")
	cmd.Dir = cfg.Root
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	logf("viteless: vite is installed — delegating to `vite build`")
	if err := cmd.Run(); err != nil {
		return BuildResult{}, fmt.Errorf("vite build: %w", err)
	}
	out := cfg.OutDir
	if out == "" {
		out = filepath.Join(cfg.Root, "dist")
	}
	return BuildResult{OutDir: out}, nil
}

// portOfAddr extracts the port from a host:port (or :port) address.
func portOfAddr(addr string) string {
	if addr == "" {
		return ""
	}
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		p := addr[i+1:]
		if p != "" && p != "0" {
			return p
		}
	}
	return ""
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }
