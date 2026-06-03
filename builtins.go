package viteless

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// This file holds the tier-1 "native" built-in plugins — viteless's own
// implementations of capabilities a Vite project would otherwise pull in as
// JS plugins. The vite.config loader (P2) recognizes the corresponding
// plugin names (@tailwindcss/vite, @vitejs/plugin-vue, …) and substitutes
// these instead of executing the JS.

// tailwindPlugin runs Tailwind's standalone (Oxide) CLI on CSS that uses
// Tailwind directives — Node-free. It works in BOTH dev (via the container
// transform hook) and build, closing the gap where Tailwind previously only
// ran at build time.
type tailwindPlugin struct{ root string }

// TailwindPlugin returns the built-in Tailwind plugin. Add it to
// Dev/Build Config.Plugins, or let `nexus dev` add it automatically when
// Tailwind mode isn't Off.
func TailwindPlugin() Plugin { return &tailwindPlugin{} }

func (p *tailwindPlugin) Name() string { return "viteless:tailwind" }

func (p *tailwindPlugin) Config(c *ResolvedConfig) { p.root = c.Root }

func (p *tailwindPlugin) Transform(code, id string) (string, bool, error) {
	clean := id
	if i := strings.IndexAny(clean, "?#"); i >= 0 {
		clean = clean[:i]
	}
	if !strings.HasSuffix(strings.ToLower(clean), ".css") || !tailwindDirectives(code) {
		return code, false, nil
	}
	cli, err := exec.LookPath("tailwindcss")
	if err != nil {
		return "", false, fmt.Errorf("tailwindcss CLI not found on PATH (needed to process %s); install the standalone binary", id)
	}
	// Map the id to the CSS file on disk so the CLI scans the project
	// (config + content globs). In build the id is already a real absolute
	// path; in dev it's a served URL like "/src/style.css" — which looks
	// absolute but isn't a filesystem path — so resolve it under root.
	fp := clean
	if _, err := os.Stat(fp); err != nil {
		fp = filepath.Join(p.root, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
	}
	cmd := exec.Command(cli, "-i", fp, "-o", "-")
	if p.root != "" {
		cmd.Dir = p.root
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", false, fmt.Errorf("tailwindcss: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), true, nil
}

// tailwindDirectives reports whether CSS uses Tailwind directives that the
// CLI must expand.
func tailwindDirectives(css string) bool {
	return strings.Contains(css, "@tailwind") ||
		strings.Contains(css, `@import "tailwindcss"`) ||
		strings.Contains(css, "@import 'tailwindcss'") ||
		strings.Contains(css, "@apply")
}
