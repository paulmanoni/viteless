package resolver

import (
	"bytes"
	"strings"
	"testing"

	"github.com/evanw/esbuild/pkg/api"

	"github.com/paulmanoni/viteless/internal/lockfile"
	"github.com/paulmanoni/viteless/internal/store"
)

func TestLoaderFor(t *testing.T) {
	cases := []struct {
		name, ct, url string
		want          api.Loader
	}{
		// Content-Type wins when it's a known shape.
		{"js explicit", "application/javascript", "", api.LoaderJS},
		{"ts explicit", "application/typescript", "", api.LoaderJS},
		{"esm explicit", "application/ecmascript", "", api.LoaderJS},
		{"css explicit", "text/css; charset=utf-8", "", api.LoaderCSS},
		{"json explicit", "application/json", "", api.LoaderJSON},

		// Fonts via content-type — every variant esm.sh might emit.
		{"font woff2 ct", "font/woff2", "", api.LoaderFile},
		{"font woff ct", "font/woff", "", api.LoaderFile},
		{"font application/font-woff2", "application/font-woff2", "", api.LoaderFile},
		{"font eot ct", "application/vnd.ms-fontobject", "", api.LoaderFile},

		// Images via content-type.
		{"image png", "image/png", "", api.LoaderFile},
		{"image svg+xml", "image/svg+xml", "", api.LoaderFile},
		{"image webp", "image/webp", "", api.LoaderFile},

		// Content-Type missing / octet-stream → extension sniff.
		{"octet-stream woff2", "application/octet-stream", "https://esm.sh/fonts/inter.woff2", api.LoaderFile},
		{"octet-stream png", "application/octet-stream", "https://esm.sh/img/logo.png", api.LoaderFile},
		{"octet-stream js", "application/octet-stream", "https://esm.sh/code.js", api.LoaderJS},

		// Empty Content-Type, extension does all the work.
		{"empty ct woff2", "", "https://esm.sh/inter.woff2", api.LoaderFile},
		{"empty ct mjs", "", "https://esm.sh/foo.mjs", api.LoaderJS},
		{"empty ct css", "", "https://esm.sh/foo.css", api.LoaderCSS},

		// Query strings on URLs (esm.sh's ?target=es2015 etc.)
		// don't mask the real extension.
		{"woff2 with query", "", "https://esm.sh/inter.woff2?target=es2015", api.LoaderFile},

		// Genuinely unknown — fall through to JS default.
		{"empty both", "", "", api.LoaderJS},
		{"unknown ct + no ext", "application/x-weird", "https://esm.sh/foo", api.LoaderJS},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := loaderFor(tc.ct, tc.url)
			if got != tc.want {
				t.Errorf("loaderFor(%q,%q) = %v, want %v", tc.ct, tc.url, got, tc.want)
			}
		})
	}
}

func TestLoaderFromExtension_RejectsUnknown(t *testing.T) {
	if _, ok := loaderFromExtension("https://example.com/no-ext"); ok {
		t.Error("extensionless URL should return ok=false")
	}
	if _, ok := loaderFromExtension("https://example.com/foo.xyz"); ok {
		t.Error("unknown extension should return ok=false")
	}
}

func TestSplitSpec(t *testing.T) {
	cases := []struct{ in, wantSpec, wantSub string }{
		{"vue", "vue", ""},
		{"vue/dist/vue.esm.js", "vue", "dist/vue.esm.js"},
		{"@vue/runtime-dom", "@vue/runtime-dom", ""},
		{"@vue/runtime-dom/foo/bar.js", "@vue/runtime-dom", "foo/bar.js"},
		{"@scoped-no-name", "@scoped-no-name", ""}, // degenerate; pass through
	}
	for _, tc := range cases {
		s, sub := splitSpec(tc.in)
		if s != tc.wantSpec || sub != tc.wantSub {
			t.Errorf("splitSpec(%q) = (%q,%q), want (%q,%q)",
				tc.in, s, sub, tc.wantSpec, tc.wantSub)
		}
	}
}

func TestJoinSubpath(t *testing.T) {
	got := joinSubpath("https://esm.sh/vue@3.4.21", "dist/vue.esm.js")
	if got != "https://esm.sh/vue@3.4.21/dist/vue.esm.js" {
		t.Errorf("joinSubpath = %q", got)
	}
	got = joinSubpath("https://esm.sh/vue@3.4.21/", "/dist/x.js")
	if got != "https://esm.sh/vue@3.4.21/dist/x.js" {
		t.Errorf("trim-slash join = %q", got)
	}
}

// newPopulatedStore builds a store with vue@3.4.21's blob pre-
// placed under the expected URL. The blob is a tiny ESM stub —
// enough for esbuild to parse + bundle.
func newPopulatedStore(t *testing.T) (*store.Store, *lockfile.File) {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	body := []byte(`export default { name: "vue-stub" };`)
	if _, err := s.Put("https://esm.sh/vue@3.4.21", bytes.NewReader(body), "", store.Metadata{
		URL:         "https://esm.sh/vue@3.4.21",
		ResolvedURL: "https://esm.sh/vue@3.4.21",
		ContentType: "application/javascript",
	}); err != nil {
		t.Fatalf("store.Put: %v", err)
	}
	lf := lockfile.New()
	lf.Add(lockfile.Package{
		Spec:     "vue",
		Version:  "3.4.21",
		Resolved: "https://esm.sh/vue@3.4.21",
	})
	return s, lf
}

func TestResolveOne_BareSpecHitsStore(t *testing.T) {
	s, lf := newPopulatedStore(t)
	res, err := resolveOne(Options{Lockfile: lf, Store: s},
		api.OnResolveArgs{Path: "vue"})
	if err != nil {
		t.Fatalf("resolveOne: %v", err)
	}
	if res.Path == "" {
		t.Fatal("Path empty — resolver should have claimed the import")
	}
	if res.Namespace != Namespace {
		t.Errorf("Namespace = %q, want %q", res.Namespace, Namespace)
	}
}

func TestResolveOne_RelativeImportFallsThrough(t *testing.T) {
	s, lf := newPopulatedStore(t)
	res, _ := resolveOne(Options{Lockfile: lf, Store: s},
		api.OnResolveArgs{Path: "./foo"})
	if res.Path != "" {
		t.Errorf("relative should pass through, got Path=%q", res.Path)
	}
}

func TestResolveOne_AbsoluteImportFallsThrough(t *testing.T) {
	s, lf := newPopulatedStore(t)
	res, _ := resolveOne(Options{Lockfile: lf, Store: s},
		api.OnResolveArgs{Path: "/absolute/path"})
	if res.Path != "" {
		t.Errorf("absolute should pass through, got %q", res.Path)
	}
}

func TestResolveOne_UnknownSpecFallsThrough(t *testing.T) {
	// Spec not in lockfile → fall through (esbuild handles or
	// errors with its own message).
	s, lf := newPopulatedStore(t)
	res, err := resolveOne(Options{Lockfile: lf, Store: s},
		api.OnResolveArgs{Path: "react"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Path != "" {
		t.Errorf("unknown spec should pass through, got %q", res.Path)
	}
	if len(res.Errors) != 0 {
		t.Errorf("unknown spec should not generate errors, got %v", res.Errors)
	}
}

func TestResolveOne_AmbiguousSurfaceAsBuildError(t *testing.T) {
	// Two versions of the same spec → AmbiguousError → build msg.
	s, _ := newPopulatedStore(t)
	lf := lockfile.New()
	lf.Add(lockfile.Package{Spec: "vue", Version: "3.4.21", Resolved: "https://esm.sh/vue@3.4.21"})
	lf.Add(lockfile.Package{Spec: "vue", Version: "3.5.0", Resolved: "https://esm.sh/vue@3.5.0"})

	res, err := resolveOne(Options{Lockfile: lf, Store: s},
		api.OnResolveArgs{Path: "vue"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("expected 1 build error, got %d", len(res.Errors))
	}
	if !strings.Contains(res.Errors[0].Text, "multiple versions") {
		t.Errorf("error text = %q, expected to mention multiple versions", res.Errors[0].Text)
	}
}

func TestResolveOne_LockedButUncachedSurfaceAsBuildError(t *testing.T) {
	// Lockfile knows about the package but the store doesn't have
	// the blob — typical "fresh clone, ran build before install".
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lf := lockfile.New()
	lf.Add(lockfile.Package{Spec: "vue", Version: "3.4.21", Resolved: "https://esm.sh/vue@3.4.21"})

	res, err := resolveOne(Options{Lockfile: lf, Store: s},
		api.OnResolveArgs{Path: "vue"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("expected 1 build error, got %d", len(res.Errors))
	}
	if !strings.Contains(res.Errors[0].Text, "nexus install") {
		t.Errorf("error should suggest `nexus install`: %q", res.Errors[0].Text)
	}
}

func TestNew_ValidatesOptions(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("New should error on missing Lockfile")
	}
	if _, err := New(Options{Lockfile: lockfile.New()}); err == nil {
		t.Error("New should error on missing Store")
	}
}
