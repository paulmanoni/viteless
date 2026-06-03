package bundler

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// NewTailwindPlugin returns an esbuild plugin that pre-processes
// CSS files containing Tailwind directives (@tailwind, @apply)
// through the system `tailwindcss` CLI before handing the
// expanded CSS to esbuild's native bundler.
//
// Architecture mirrors sass_plugin: shell out to a standalone
// binary the operator installs via brew / direct download
// (Tailwind ships a Node-free standalone CLI starting with v3.4
// and the canonical v4). We deliberately don't bundle a Tailwind
// runtime into the nexus binary — Tailwind's standalone is
// ~30MB itself and most operators already have it (or can brew
// install it).
//
// Plain CSS files (no @tailwind / @apply) pass through to
// esbuild's native CSS handler unchanged, so this plugin is
// a no-op for non-Tailwind projects.
//
// When tailwindcss isn't on PATH the plugin surfaces a clear
// actionable error naming three install paths (brew, npm,
// direct download) instead of esbuild's default which would
// silently treat the @tailwind directives as literal CSS
// (browser ignores them — utility classes never render).
func NewTailwindPlugin() api.Plugin {
	return api.Plugin{
		Name: "nexus-tailwind",
		Setup: func(build api.PluginBuild) {
			// File-system CSS files (user code). Only intercept
			// the default namespace; esm.sh-served CSS goes
			// through the resolver plugin's namespace and never
			// contains Tailwind directives anyway.
			build.OnLoad(api.OnLoadOptions{
				Filter:    `\.css$`,
				Namespace: "file",
			}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
				return loadAndMaybeProcessCSS(args.Path)
			})
		},
	}
}

// loadAndMaybeProcessCSS reads the file at path. If the body
// contains Tailwind directives, pipes it through the CLI and
// returns the processed CSS. Otherwise returns a zero result
// so esbuild's native handler takes over (cheaper + preserves
// CSS-modules / @import behavior).
func loadAndMaybeProcessCSS(path string) (api.OnLoadResult, error) {
	body, err := os.ReadFile(path) // #nosec G304 -- esbuild-supplied CSS path
	if err != nil {
		return api.OnLoadResult{}, fmt.Errorf("nexus-tailwind: read %s: %w", path, err)
	}
	if !needsTailwind(body) {
		// Hand back zero — esbuild proceeds with its native
		// CSS loader. We're a no-op for plain CSS.
		return api.OnLoadResult{}, nil
	}
	return compileTailwind(path, body)
}

// needsTailwind reports whether body contains directives only
// the Tailwind CLI can handle. Recognized markers:
//
//	@tailwind base/components/utilities   ← Tailwind v3 entry
//	@apply <class>                        ← v3 + v4 utility composition
//	@import "tailwindcss"                 ← Tailwind v4 entry
//	@import 'tailwindcss'                 ← v4 with single-quoted import
//
// Other Tailwind-flavored directives (@layer, @screen,
// @variants, @config, @theme, @utility) also exist but the
// markers above cover the entry-point cases that ALWAYS need
// the CLI. Pure @layer / @theme inside an already-Tailwind
// pipeline gets handled by the CLI naturally.
//
// Cheap byte scan; no full CSS parse. False positives (e.g.
// the strings appear inside a comment) just route through the
// CLI unnecessarily — Tailwind handles non-Tailwind CSS as a
// passthrough, so the worst case is slower compile not broken
// output.
func needsTailwind(body []byte) bool {
	return bytes.Contains(body, []byte("@tailwind")) ||
		bytes.Contains(body, []byte("@apply")) ||
		bytes.Contains(body, []byte(`@import "tailwindcss"`)) ||
		bytes.Contains(body, []byte("@import 'tailwindcss'"))
}

// compileTailwind invokes the tailwindcss CLI with `-i <path>
// -o -` (input file, stdout output). The CLI does its own
// content scanning, @import resolution, and PostCSS pipeline
// (autoprefixer + nesting in v4), so we get a complete CSS
// document back.
//
// Returns an OnLoadResult with the processed CSS + LoaderCSS so
// esbuild's bundler picks it up. ResolveDir set to the input
// file's directory so any relative @import urls inside the
// processed output resolve correctly against the user's
// project layout.
//
// CLI lookup is done per-invocation (not cached at plugin
// construction) so adding tailwindcss mid-session via
// brew/install starts working without a CLI restart.
func compileTailwind(path string, body []byte) (api.OnLoadResult, error) {
	cliPath, err := exec.LookPath("tailwindcss")
	if err != nil {
		return api.OnLoadResult{
			Errors: []api.Message{{
				Text: fmt.Sprintf("nexus-tailwind: %q uses Tailwind directives (@tailwind / @apply) but the `tailwindcss` CLI is not on PATH", path),
				Notes: []api.Note{
					{Text: "Install standalone Tailwind: `brew install tailwindcss` (macOS) — Node-free binary, ~30MB"},
					{Text: "Or via npm: `npm install -g tailwindcss`"},
					{Text: "Or download from https://github.com/tailwindlabs/tailwindcss/releases"},
				},
			}},
		}, nil
	}
	// Use `-i <path> -o -` so the CLI handles @import resolution
	// from the input file's location + sees its own
	// tailwind.config.js relative to that input.
	cmd := exec.Command(cliPath, "-i", path, "-o", "-")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Run from the project root the CSS file lives under, so
	// tailwind.config.js resolves correctly (tailwind looks
	// upward from cwd for its config when --config isn't
	// passed).
	cmd.Dir = inferProjectRoot(path)
	if err := cmd.Run(); err != nil {
		return api.OnLoadResult{
			Errors: []api.Message{{
				Text: fmt.Sprintf("nexus-tailwind: compile %s failed", path),
				Notes: []api.Note{
					{Text: strings.TrimSpace(stderr.String())},
				},
			}},
		}, nil
	}
	processed := stdout.String()
	_ = body // already passed via -i path; body kept for future stdin mode
	return api.OnLoadResult{
		Contents:   &processed,
		Loader:     api.LoaderCSS,
		ResolveDir: filepath.Dir(path),
	}, nil
}

// inferProjectRoot walks up from path looking for a directory
// containing tailwind.config.* . Falls back to filepath.Dir(path)
// when no config is found (Tailwind defaults still work, just
// with no operator overrides).
//
// The walk stops at the first found directory or filesystem
// root, whichever comes first. Symlinks are not followed —
// configs in linked directories should be referenced via the
// actual project structure, not symlink target.
func inferProjectRoot(path string) string {
	dir := filepath.Dir(path)
	for {
		for _, name := range []string{
			"tailwind.config.js",
			"tailwind.config.cjs",
			"tailwind.config.mjs",
			"tailwind.config.ts",
		} {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached filesystem root without finding a config.
			break
		}
		dir = parent
	}
	return filepath.Dir(path)
}

// TailwindAvailable is the exported probe — CLI uses it to
// decide whether to print the "tailwind active" notice. Plugin
// is registered unconditionally; when tailwindcss isn't
// available the plugin only fires (and surfaces the install
// suggestion error) when a file actually contains @tailwind /
// @apply.
func TailwindAvailable() bool {
	_, err := exec.LookPath("tailwindcss")
	return err == nil
}
