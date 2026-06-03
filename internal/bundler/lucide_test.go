//go:build network

package bundler_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paulmanoni/viteless/internal/bundler"
	"github.com/paulmanoni/viteless/internal/fetcher"
	"github.com/paulmanoni/viteless/internal/lockfile"
	"github.com/paulmanoni/viteless/internal/resolver"
	"github.com/paulmanoni/viteless/internal/store"
)

// TestLucideVueNext_NamespaceImport_Bundles verifies that a real
// `import { Heart, Star, Bell } from "lucide-vue-next"` resolves
// + bundles end-to-end against esm.sh. This is the "icon library
// works" pin that the dashboard migration depends on.
//
// What this test does NOT prove:
//
//   - Tree-shaking. esm.sh's lucide-vue-next bundle has a
//     top-level Object.defineProperty(...) side-effect block
//     that creates a namespace object with all 1500+ icons as
//     getters. esbuild conservatively keeps the side effect,
//     which keeps every icon's defining var, which produces a
//     ~518 KB minified bundle regardless of how few icons the
//     consumer actually imports. This is a property of esm.sh's
//     output shape, not our pipeline.
//
//     Vite works around it by transforming bare-named imports
//     into deep imports (e.g. `import Heart from
//     "lucide-vue-next/dist/esm/icons/heart"`) at parse time.
//     We have the same workaround available — esm.sh serves
//     each /dist/esm/icons/<name>.js as a 1.5 KB stub — but
//     our v0.1 lockfile model collapses every sub-path of the
//     same <name>@<version> to one entry, which the resolver
//     can't disambiguate. A "sub-path entries in lockfile"
//     change unlocks per-icon imports; that's a v0.2 concern.
//
// What this test DOES prove:
//
//   - Cache footprint is sane (~8 URLs, ~700 KB) — the "800
//     icon blobs" fear was wrong. esm.sh consolidates lucide's
//     icon set into one pre-built file.
//   - Bundle succeeds with no errors (no missing transitives,
//     no resolver edge cases).
//   - User code's icon symbols (Heart, Star, Bell) survive to
//     the output, proving the namespace export reaches user
//     code through our pipeline.
//
// Bundle size IS asserted within a reasonable range, so a
// future regression that produces a 50-byte bundle (broken
// tree-shaking by accident) would surface here.
func TestLucideVueNext_NamespaceImport_Bundles(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}

	tmp := t.TempDir()
	s, err := store.New(filepath.Join(tmp, "cache"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	f := fetcher.New(s, "")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := f.Fetch(ctx, "lucide-vue-next@0.400.0")
	if err != nil {
		t.Fatalf("fetch lucide-vue-next: %v", err)
	}

	// Cache-footprint pin: low single-digit URLs, well under 1 MB.
	// Changes here flag a regression in fetcher recursion (e.g.
	// suddenly pulling per-icon files we shouldn't).
	var totalBytes int64
	urlCount := 0
	_ = s.EachURL(func(_ store.Metadata) error { urlCount++; return nil })
	_ = s.EachBlob(func(_, path string) error {
		if info, err := os.Stat(path); err == nil {
			totalBytes += info.Size()
		}
		return nil
	})
	if urlCount > 20 {
		t.Errorf("cache URL count = %d, expected < 20 — fetcher may have started over-recursing", urlCount)
	}
	if totalBytes > 2*1024*1024 {
		t.Errorf("cache bytes = %d, expected < 2 MB — esm.sh's lucide bundle is ~576 KB", totalBytes)
	}

	src := filepath.Join(tmp, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(src, "app.js")
	if err := os.WriteFile(entry, []byte(
		`import { Heart, Star, Bell } from "lucide-vue-next";
console.log(Heart, Star, Bell);
`), 0o644); err != nil {
		t.Fatal(err)
	}

	lf := lockfile.New()
	lf.Add(res.Root)
	for _, p := range res.Transitive {
		lf.Add(p)
	}
	plugin, err := resolver.New(resolver.Options{Lockfile: lf, Store: s})
	if err != nil {
		t.Fatalf("resolver.New: %v", err)
	}
	b := bundler.New()
	b.AddPlugin(plugin)
	r, err := b.Build(bundler.Options{
		Entries:  []string{entry},
		OutDir:   filepath.Join(tmp, "out"),
		Lockfile: lf,
		Store:    s,
		Minify:   true,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(r.Errors) > 0 {
		for _, e := range r.Errors {
			t.Errorf("build error: %s", e.Text)
		}
		t.FailNow()
	}

	out, err := os.ReadFile(filepath.Join(tmp, "out", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	bundle := string(out)
	for _, sym := range []string{"Heart", "Star", "Bell"} {
		if !strings.Contains(bundle, sym) {
			t.Errorf("bundle missing %s symbol", sym)
		}
	}
	// Sanity envelope on bundle size. Lower bound rules out
	// "broken — emitted 200 bytes". Upper bound rules out "esm.sh
	// changed shape and we now ship 5 MB".
	if len(out) < 200_000 {
		t.Errorf("bundle suspiciously small: %d bytes — namespace import should pull ~500 KB", len(out))
	}
	if len(out) > 2*1024*1024 {
		t.Errorf("bundle suspiciously large: %d bytes — esm.sh's lucide bundle is ~576 KB pre-minify", len(out))
	}
}
