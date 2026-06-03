package viteless

import (
	"strings"
	"testing"
	"time"

	"github.com/paulmanoni/viteless/internal/lockfile"
	"github.com/paulmanoni/viteless/internal/resolver"
	"github.com/paulmanoni/viteless/internal/store"
)

func TestPkgKey(t *testing.T) {
	cases := map[string]string{
		"https://esm.sh/vue@3.5.34/es2022/vue.mjs":                         "vue@3.5.34",
		"https://esm.sh/@vue/runtime-core@3.5.34/es2022/runtime-core.mjs":  "@vue/runtime-core@3.5.34",
		"https://esm.sh/vuetify@3.11.7/X-ZX.../es2022/components.mjs":      "vuetify@3.11.7",
		"https://esm.sh/pinia@3.0.4?external=vue,react,react-dom":          "pinia@3.0.4", // query must not leak
		"https://esm.sh/@vueup/vue-quill@1.2.0?external=vue&target=es2022": "@vueup/vue-quill@1.2.0",
		"/src/main.ts": "", // not a package URL
		"":             "",
	}
	for url, want := range cases {
		if got := pkgKey(url); got != want {
			t.Errorf("pkgKey(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestPrebundleEligible(t *testing.T) {
	// The whole Vue family must be excluded (single-instance anchor);
	// everything else is eligible.
	excluded := []string{
		"https://esm.sh/vue@3.5.34/es2022/vue.development.mjs",
		"https://esm.sh/@vue/runtime-core@3.5.34/es2022/runtime-core.mjs",
		"https://esm.sh/@vue/reactivity@3.5.34/es2022/reactivity.mjs",
	}
	for _, u := range excluded {
		if prebundleEligible(u) {
			t.Errorf("prebundleEligible(%q) = true, want false (Vue family must stay per-module)", u)
		}
	}
	eligible := []string{
		"https://esm.sh/vuetify@3.11.7/es2022/vuetify.mjs",
		"https://esm.sh/pinia@3.0.4?external=vue,react,react-dom",
		"https://esm.sh/@apollo/client@3.14.0/core/index.mjs",
	}
	for _, u := range eligible {
		if !prebundleEligible(u) {
			t.Errorf("prebundleEligible(%q) = false, want true", u)
		}
	}
	// Non-package URLs are never eligible.
	if prebundleEligible("/src/main.ts") {
		t.Error("non-package URL should not be eligible")
	}
}

func TestToPrebundleURLSafe(t *testing.T) {
	h := NewDefaultHost(HostConfig{Root: t.TempDir(), Prebundle: true})
	// A query-bearing entry URL must produce a served path with NO query
	// chars (they'd split at the server and break the /@pre/ routing).
	entryURL := "https://esm.sh/pinia@3.0.4?external=vue,react-dom"
	sp := h.toPrebundle(entryURL)
	for _, bad := range []string{"?", "&", " "} {
		if containsStr(sp, bad) {
			t.Errorf("prebundle path %q contains %q — would break routing", sp, bad)
		}
	}
	// Shape: /@pre/<pkg@ver>/e-<hash>.js
	if !strings.HasPrefix(sp, PrebundlePrefix+"pinia@3.0.4/e-") || !strings.HasSuffix(sp, ".js") {
		t.Errorf("unexpected prebundle path shape: %q", sp)
	}
	// toPrebundle must register the entry under its package, so loadPrebundle
	// can split pkg/base back out and the package build covers it.
	pkg := "pinia@3.0.4"
	h.pre.mu.Lock()
	registered := h.pre.entries[pkg][entryURL]
	h.pre.mu.Unlock()
	if !registered {
		t.Errorf("toPrebundle did not register entry %q under %q", entryURL, pkg)
	}
}

func TestPrebundleDiskPersistence(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := NewDefaultHost(HostConfig{
		Root:      t.TempDir(),
		Resolver:  resolver.Options{Lockfile: lockfile.New(), Store: st},
		Prebundle: true,
		Mode:      "development",
	})
	p := h.pre
	pkg, sig := "vuetify@3.11.7", "https://esm.sh/vuetify@3.11.7/x.mjs"

	// Nothing cached yet.
	if got := p.loadFromDisk(pkg, sig); got != nil {
		t.Fatal("expected disk miss before any save")
	}
	// Save a build, then it must round-trip from disk byte-for-byte.
	want := &pkgBuild{
		sig: sig,
		files: map[string]string{
			"e-abc123.js": "export const a = 1;\nimport \"/@pre/vuetify@3.11.7/chunk-X.js\";",
			"chunk-X.js":  "export const shared = 2;",
		},
	}
	p.saveToDisk(pkg, want)
	got := p.loadFromDisk(pkg, sig)
	if got == nil {
		t.Fatal("expected disk hit after save")
	}
	if len(got.files) != len(want.files) {
		t.Fatalf("file count: got %d want %d", len(got.files), len(want.files))
	}
	for k, v := range want.files {
		if got.files[k] != v {
			t.Errorf("file %q: got %q want %q", k, got.files[k], v)
		}
	}
	// A different defines signature must MISS (cache keyed on defines).
	h2 := NewDefaultHost(HostConfig{
		Root:      t.TempDir(),
		Resolver:  resolver.Options{Lockfile: lockfile.New(), Store: st},
		Prebundle: true,
		Mode:      "production", // different defines → different key
	})
	if got := h2.pre.loadFromDisk(pkg, sig); got != nil {
		t.Error("expected disk miss when defines differ (key must include defines)")
	}
	// A different entry-set signature must also MISS.
	if got := p.loadFromDisk(pkg, "different-sig"); got != nil {
		t.Error("expected disk miss when entry-set sig differs")
	}
}

func TestPrebundleGC(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := NewDefaultHost(HostConfig{
		Root:      t.TempDir(),
		Resolver:  resolver.Options{Lockfile: lockfile.New(), Store: st},
		Prebundle: true,
	})
	p := h.pre

	// Pin the clock; save two builds.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	timeNow = func() time.Time { return base }
	defer func() { timeNow = time.Now }()

	mk := func(sig string) {
		p.saveToDisk("pkg@1.0.0", &pkgBuild{sig: sig, files: map[string]string{"e-x.js": "export const x=1;"}})
	}
	mk("keep")  // will be refreshed (in use)
	mk("stale") // will be left to age out

	// Age everything by backdating: GC measures from dir mtime.
	// "keep" gets refreshed via loadFromDisk; "stale" does not.
	timeNow = func() time.Time { return base.Add(prebundleTTL + time.Hour) }
	if got := p.loadFromDisk("pkg@1.0.0", "keep"); got == nil {
		t.Fatal("expected hit for keep")
	} // refreshes keep's mtime to now

	removed := p.gcDisk()
	if removed != 1 {
		t.Errorf("gcDisk removed %d, want 1 (only the stale, unused dir)", removed)
	}
	if p.loadFromDisk("pkg@1.0.0", "keep") == nil {
		t.Error("recently-used 'keep' build was wrongly swept")
	}
	if p.loadFromDisk("pkg@1.0.0", "stale") != nil {
		t.Error("stale build should have been swept")
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
