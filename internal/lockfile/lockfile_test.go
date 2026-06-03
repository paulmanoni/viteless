package lockfile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNew_StartsEmptyAtCurrentVersion(t *testing.T) {
	f := New()
	if f.Version != CurrentVersion {
		t.Errorf("Version = %d, want %d", f.Version, CurrentVersion)
	}
	if len(f.Packages) != 0 {
		t.Errorf("Packages = %d, want 0", len(f.Packages))
	}
}

func TestKey_FormatsAndSplits(t *testing.T) {
	cases := []struct {
		spec, version, want string
	}{
		{"vue", "3.4.21", "vue@3.4.21"},
		{"@vue/runtime-dom", "3.4.21", "@vue/runtime-dom@3.4.21"},
		{"some-unversioned", "", "some-unversioned"},
	}
	for _, tc := range cases {
		got := Key(tc.spec, tc.version)
		if got != tc.want {
			t.Errorf("Key(%q,%q) = %q, want %q", tc.spec, tc.version, got, tc.want)
		}
		gotSpec, gotVer := SplitKey(got)
		if gotSpec != tc.spec || gotVer != tc.version {
			t.Errorf("SplitKey(%q) = (%q,%q), want (%q,%q)", got, gotSpec, gotVer, tc.spec, tc.version)
		}
	}
}

func TestSplitKey_ScopedPackageHonorsLastAt(t *testing.T) {
	// Critical: @vue/runtime-dom@3.4.21 splits at the SECOND @,
	// not the leading scope-marker @.
	spec, ver := SplitKey("@vue/runtime-dom@3.4.21")
	if spec != "@vue/runtime-dom" || ver != "3.4.21" {
		t.Errorf("scoped split = (%q,%q)", spec, ver)
	}
}

func TestAddRemove(t *testing.T) {
	f := New()
	f.Add(Package{Spec: "vue", Version: "3.4.21", Resolved: "https://esm.sh/vue@3.4.21"})
	if _, ok := f.Get("vue@3.4.21"); !ok {
		t.Error("Get after Add returned !ok")
	}
	if !f.Remove("vue@3.4.21") {
		t.Error("Remove returned false for present key")
	}
	if f.Remove("vue@3.4.21") {
		t.Error("Remove returned true for already-removed key")
	}
}

func TestResolve_ExactKey(t *testing.T) {
	f := New()
	want := Package{Spec: "vue", Version: "3.4.21", Resolved: "https://esm.sh/vue@3.4.21"}
	f.Add(want)
	got, err := f.Resolve("vue", "3.4.21")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Resolved != want.Resolved {
		t.Errorf("Resolve = %+v, want %+v", got, want)
	}
}

func TestResolve_BySpecOnly(t *testing.T) {
	// Caller didn't pin a version — single-version-per-spec
	// resolution returns the lone match.
	f := New()
	f.Add(Package{Spec: "vue", Version: "3.4.21"})
	got, err := f.Resolve("vue", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Version != "3.4.21" {
		t.Errorf("Version = %q", got.Version)
	}
}

func TestResolve_AmbiguousMultipleVersions(t *testing.T) {
	// v0.1 disallows nested resolution. Two versions of the same
	// spec → AmbiguousError naming every version found.
	f := New()
	f.Add(Package{Spec: "vue", Version: "3.4.21"})
	f.Add(Package{Spec: "vue", Version: "3.5.0"})
	_, err := f.Resolve("vue", "")
	var ae *AmbiguousError
	if !errors.As(err, &ae) {
		t.Fatalf("err type = %T, want *AmbiguousError", err)
	}
	if ae.Spec != "vue" {
		t.Errorf("ae.Spec = %q", ae.Spec)
	}
	if len(ae.Versions) != 2 || ae.Versions[0] != "3.4.21" || ae.Versions[1] != "3.5.0" {
		t.Errorf("ae.Versions = %v, want sorted [3.4.21, 3.5.0]", ae.Versions)
	}
}

func TestResolve_NotPresent(t *testing.T) {
	f := New()
	_, err := f.Resolve("never-added", "")
	if !errors.Is(err, ErrNotResolved) {
		t.Errorf("err = %v, want ErrNotResolved", err)
	}
}

func TestBytes_IsDeterministicAcrossMapIterations(t *testing.T) {
	// Build the same logical lockfile via two different insertion
	// orders. Bytes() must produce identical output regardless.
	build := func(specs []string) *File {
		f := New()
		for _, s := range specs {
			f.Add(Package{
				Spec:      s,
				Version:   "1.0.0",
				Resolved:  "https://esm.sh/" + s + "@1.0.0",
				Integrity: "sha256-deadbeef",
				Deps:      []string{"a@1.0.0", "b@1.0.0", "c@1.0.0"},
			})
		}
		return f
	}
	a, errA := build([]string{"vue", "react", "lodash"}).Bytes()
	b, errB := build([]string{"lodash", "vue", "react"}).Bytes()
	if errA != nil || errB != nil {
		t.Fatalf("Bytes errs: %v, %v", errA, errB)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("Bytes order-dependent:\nA:\n%s\nB:\n%s", a, b)
	}
}

func TestBytes_DepsSortedInOutput(t *testing.T) {
	f := New()
	f.Add(Package{
		Spec:    "vue",
		Version: "3.4.21",
		Deps:    []string{"z@1", "a@1", "m@1"},
	})
	raw, err := f.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	s := string(raw)
	zIdx := strings.Index(s, `"z@1"`)
	aIdx := strings.Index(s, `"a@1"`)
	mIdx := strings.Index(s, `"m@1"`)
	if !(aIdx < mIdx && mIdx < zIdx) {
		t.Errorf("deps not sorted in output:\n%s", s)
	}
}

func TestBytes_PackagesSortedInOutput(t *testing.T) {
	f := New()
	f.Add(Package{Spec: "zzz", Version: "1.0.0"})
	f.Add(Package{Spec: "aaa", Version: "1.0.0"})
	raw, _ := f.Bytes()
	s := string(raw)
	if strings.Index(s, `"aaa@1.0.0"`) > strings.Index(s, `"zzz@1.0.0"`) {
		t.Errorf("packages not sorted:\n%s", s)
	}
}

func TestSaveLoad_RoundTripIsByteIdentical(t *testing.T) {
	// Save, Load, Save again — second Save's bytes must equal the
	// first's. Defends the "diff-friendly lockfile" property.
	f := New()
	f.Add(Package{
		Spec: "vue", Version: "3.4.21",
		Resolved: "https://esm.sh/vue@3.4.21", Integrity: "sha256-x",
		Deps: []string{"@vue/shared@3.4.21"},
	})
	f.Add(Package{
		Spec: "@vue/shared", Version: "3.4.21",
		Resolved: "https://esm.sh/@vue/shared@3.4.21", Integrity: "sha256-y",
	})

	path := filepath.Join(t.TempDir(), "nexus.lock")
	if err := f.Save(path); err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	first, _ := os.ReadFile(path)

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := loaded.Save(path); err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	second, _ := os.ReadFile(path)

	if !bytes.Equal(first, second) {
		t.Errorf("round-trip bytes differ:\n--- first ---\n%s\n--- second ---\n%s",
			first, second)
	}
}

func TestLoad_MissingFileReturnsOSError(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.lock"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want os.ErrNotExist", err)
	}
}

func TestLoadOrNew_MissingReturnsEmptyFile(t *testing.T) {
	f, err := LoadOrNew(filepath.Join(t.TempDir(), "nope.lock"))
	if err != nil {
		t.Fatalf("LoadOrNew: %v", err)
	}
	if f == nil || len(f.Packages) != 0 || f.Version != CurrentVersion {
		t.Errorf("LoadOrNew on missing path returned %+v", f)
	}
}

func TestLoad_VersionGreaterThanCurrentRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.lock")
	if err := os.WriteFile(path, []byte(`{"version":99,"packages":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "upgrade nexus") {
		t.Errorf("err = %v, want 'upgrade nexus' hint", err)
	}
}

func TestSave_AtomicWriteSurvivesPathBeingDirectory(t *testing.T) {
	// Defensive: passing a path that's a directory shouldn't
	// silently succeed by writing a .tmp next to it. Save creates
	// the parent if missing, then attempts the rename — which
	// fails when the destination is a non-empty dir. We just
	// confirm an error is returned, not which one.
	dir := t.TempDir()
	target := filepath.Join(dir, "subdir")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	f := New()
	if err := f.Save(target); err == nil {
		t.Error("Save onto directory path should error")
	}
}

// TestFile_ConcurrentAddResolve exercises the exact access pattern that
// crashed `nexus dev`: the resolver runs Resolve across esbuild's
// worker goroutines while the on-demand fetch hook calls Add. Without
// the File.mu guard this panics under -race (and intermittently in
// production) with "concurrent map iteration and map write". Run with
// `go test -race` to catch regressions.
func TestFile_ConcurrentAddResolve(t *testing.T) {
	f := New()
	// Seed so Resolve has entries to iterate.
	for i := 0; i < 50; i++ {
		f.Add(Package{Spec: "seed" + string(rune('a'+i%26)), Version: "1.0.0"})
	}

	done := make(chan struct{})
	go func() { // writer
		for i := 0; i < 2000; i++ {
			f.Add(Package{Spec: "pkg", Version: string(rune('a' + i%26))})
		}
		close(done)
	}()
	for i := 0; i < 2000; i++ { // concurrent readers
		_, _ = f.Resolve("seed", "")     // iterates the map
		_, _ = f.Get("pkg@a")            // keyed read
		_, _ = f.Resolve("pkg", "1.0.0") // keyed read
	}
	<-done
}
