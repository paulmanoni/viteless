// Package lockfile reads and writes nexus.lock — the per-project
// pin file that records exactly which versions of each ESM
// dependency the project has agreed to use.
//
// Format (JSON):
//
//	{
//	  "version": 1,
//	  "packages": {
//	    "vue@3.4.21": {
//	      "spec":      "vue",
//	      "version":   "3.4.21",
//	      "resolved":  "https://esm.sh/vue@3.4.21",
//	      "integrity": "sha256-abcdef…",
//	      "deps":      ["@vue/shared@3.4.21", "@vue/runtime-dom@3.4.21"]
//	    },
//	    "@vue/shared@3.4.21": { … }
//	  }
//	}
//
// Determinism is a first-class requirement: the file's byte content
// must be a pure function of the lockfile state, so a `nexus install`
// on two machines produces byte-identical lockfiles after no-ops.
// We achieve that by:
//
//   - sorting Packages keys lexicographically before marshaling
//   - sorting each entry's Deps slice in place
//   - using 2-space indent + LF newlines (consistent with npm/pnpm)
//   - emitting a trailing newline so editor "newline-at-end" rules
//     don't reformat the file on save
//
// We do NOT use Go's default map iteration (random order) for
// marshaling — we materialize an ordered intermediate struct so the
// json package preserves the sort.
package lockfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// CurrentVersion is the lockfile schema version this package writes.
// A future incompatible change (e.g. nested resolution support)
// would bump this and add a migration path keyed off the field.
const CurrentVersion = 1

// Filename is the conventional on-disk name. Lives at the project
// root, alongside go.mod.
const Filename = "nexus.lock"

// File is the in-memory representation of a nexus.lock. Map keys are
// "<spec>@<version>" tuples that uniquely identify a resolved
// package. The same spec at two different versions is two distinct
// keys — v0.1 doesn't support multi-version resolution but the data
// model wouldn't need to change to add it; only the resolver would
// gain a context-aware lookup path.
type File struct {
	Version  int                `json:"version"`
	Packages map[string]Package `json:"packages"`

	// mu guards Packages against concurrent access. The resolver
	// plugin runs OnResolve across esbuild's worker goroutines, so a
	// Resolve/Get (map read/iterate) can race a concurrent Add (map
	// write) from the on-demand fetch hook — Go panics with "concurrent
	// map iteration and map write". Unexported, so encoding/json skips
	// it and the on-disk format is unchanged; zero value is ready.
	mu sync.RWMutex
}

// Package is one resolved dependency entry.
type Package struct {
	// Spec is the bare import name the user wrote (e.g. "vue",
	// "@vue/runtime-dom"). Stored so we can reverse-map a key back
	// to the original spec without re-parsing.
	Spec string `json:"spec"`

	// Version is the resolved version string after redirect-pinning
	// against the registry. Empty when the registry doesn't return a
	// versioned URL — rare for esm.sh but possible for self-hosted
	// mirrors.
	Version string `json:"version,omitempty"`

	// Resolved is the registry URL the bytes came from after
	// following redirects. The fetcher uses this to refetch in
	// `nexus install` without re-resolving (the bare URL might
	// resolve to a NEWER version, which would defeat the point of
	// a lockfile).
	Resolved string `json:"resolved"`

	// Integrity is the sha256 of the resolved blob bytes, formatted
	// as "sha256-<hex>" to match the npm/pnpm convention. Verified
	// on every fetch.
	Integrity string `json:"integrity"`

	// Deps holds the keys (other "<spec>@<version>" tuples) of the
	// packages this one imports transitively. Sorted on save so the
	// JSON is byte-stable.
	Deps []string `json:"deps,omitempty"`

	// ContentType is the Content-Type the registry served. Carried
	// through so the resolver can dispatch (.css → injected style
	// tag, .js → module) without re-fetching.
	ContentType string `json:"content_type,omitempty"`
}

// New returns an empty lockfile at CurrentVersion. Used when no
// existing nexus.lock is present (first `nexus add`).
func New() *File {
	return &File{Version: CurrentVersion, Packages: map[string]Package{}}
}

// Load reads and parses nexus.lock from path. Returns (nil, os
// ErrNotExist) wrapped when the file doesn't exist so callers can
// distinguish "no lockfile" from "broken lockfile".
//
// Tolerates a missing Version field (treats it as 0) for forward-
// compatibility with hand-edited files; a future schema bump would
// reject 0 explicitly.
func Load(path string) (*File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("lockfile: parse %s: %w", path, err)
	}
	if f.Packages == nil {
		f.Packages = map[string]Package{}
	}
	if f.Version == 0 {
		f.Version = CurrentVersion
	}
	if f.Version > CurrentVersion {
		return nil, fmt.Errorf("lockfile: %s has version %d, this build supports up to %d — upgrade nexus",
			path, f.Version, CurrentVersion)
	}
	return &f, nil
}

// LoadOrNew loads the lockfile at path, or returns a fresh empty
// File if the path doesn't exist. Convenience wrapper for the
// common "load if present, create if absent" pattern in `nexus add`
// and friends.
func LoadOrNew(path string) (*File, error) {
	f, err := Load(path)
	if err == nil {
		return f, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return New(), nil
	}
	return nil, err
}

// Save writes the lockfile atomically to path. Sorts Packages keys
// and each Deps slice for determinism, indents with 2 spaces, and
// appends a trailing newline. The write is atomic (temp file +
// rename in the same directory) so a crash mid-write never produces
// a partial nexus.lock.
func (f *File) Save(path string) error {
	if f == nil {
		return errors.New("lockfile: Save on nil")
	}
	raw, err := f.Bytes()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("lockfile: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("lockfile: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("lockfile: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("lockfile: close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("lockfile: rename: %w", err)
	}
	return nil
}

// Bytes returns the serialized form Save would write. Exposed
// separately so callers (e.g. `nexus add --dry-run`) can preview
// the output without touching disk, and so tests can assert
// byte-stability cheaply.
func (f *File) Bytes() ([]byte, error) {
	// Materialize an ordered view of the map for deterministic
	// marshaling. json.Marshal on a map iterates the runtime
	// hash table in random order; we can't rely on that even
	// though Go 1.12+ sorts string-keyed maps in encoding/json
	// (sticking to the explicit sort keeps the contract
	// independent of std-lib implementation choices).
	f.mu.RLock()
	defer f.mu.RUnlock()
	keys := make([]string, 0, len(f.Packages))
	for k := range f.Packages {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	ordered := orderedFile{
		Version:  f.Version,
		Packages: make([]orderedEntry, 0, len(keys)),
	}
	for _, k := range keys {
		pkg := f.Packages[k]
		// Sort Deps in place so repeated saves of the same logical
		// state produce identical bytes. We sort a copy to leave
		// the caller's slice untouched.
		deps := append([]string(nil), pkg.Deps...)
		sort.Strings(deps)
		ordered.Packages = append(ordered.Packages, orderedEntry{
			Key: k,
			Pkg: Package{
				Spec:        pkg.Spec,
				Version:     pkg.Version,
				Resolved:    pkg.Resolved,
				Integrity:   pkg.Integrity,
				Deps:        deps,
				ContentType: pkg.ContentType,
			},
		})
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	// SetEscapeHTML(false) so URLs with & in them (rare for esm.sh
	// but legal) don't get turned into & — the lockfile is for
	// humans + tooling, not browsers, and the HTML-safe escaping
	// hurts readability.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ordered); err != nil {
		return nil, fmt.Errorf("lockfile: marshal: %w", err)
	}
	// json.Encoder already appends a newline; nothing else to do.
	return buf.Bytes(), nil
}

// Add registers a package under "<spec>@<version>". When version is
// empty, the key is just the spec — used for unversioned cases like
// "@types/node" with no resolved version available. Overwrites any
// existing entry for the same key; the caller is expected to have
// already de-duplicated upstream.
func (f *File) Add(pkg Package) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Packages == nil {
		f.Packages = map[string]Package{}
	}
	f.Packages[Key(pkg.Spec, pkg.Version)] = pkg
}

// Remove deletes the entry at key. No-op if key isn't present.
// Returns true when a deletion actually happened so callers can
// distinguish "removed" from "wasn't there".
func (f *File) Remove(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Packages == nil {
		return false
	}
	_, ok := f.Packages[key]
	if !ok {
		return false
	}
	delete(f.Packages, key)
	return true
}

// Resolve returns the Package entry the resolver should use for the
// given bare spec. The lookup model is:
//
//  1. Exact "<spec>@<version>" match if version is non-empty.
//  2. Otherwise: any entry whose Spec field equals spec — and only
//     one. Two matches → ErrAmbiguous, since v0.1 disallows
//     nested multi-version resolution.
//
// Returns ErrNotResolved when the lockfile knows nothing about the
// spec — caller should treat this as "not in the lockfile yet" and
// either fetch (during `nexus add`) or fail (during `nexus build`).
func (f *File) Resolve(spec, version string) (Package, error) {
	if f == nil {
		return Package{}, ErrNotResolved
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if len(f.Packages) == 0 {
		return Package{}, ErrNotResolved
	}
	if version != "" {
		if p, ok := f.Packages[Key(spec, version)]; ok {
			return p, nil
		}
		return Package{}, ErrNotResolved
	}
	var matches []Package
	for _, p := range f.Packages {
		if p.Spec == spec {
			matches = append(matches, p)
		}
	}
	switch len(matches) {
	case 0:
		return Package{}, ErrNotResolved
	case 1:
		return matches[0], nil
	default:
		versions := make([]string, len(matches))
		for i, m := range matches {
			versions[i] = m.Version
		}
		sort.Strings(versions)
		return Package{}, &AmbiguousError{Spec: spec, Versions: versions}
	}
}

// Get returns the entry at key plus whether it exists. Simpler
// accessor for callers that already have a "<spec>@<version>"
// string in hand.
func (f *File) Get(key string) (Package, bool) {
	if f == nil {
		return Package{}, false
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.Packages == nil {
		return Package{}, false
	}
	p, ok := f.Packages[key]
	return p, ok
}

// Key formats the canonical "<spec>@<version>" tuple used as the
// map key. Exported so callers can construct keys without
// duplicating the format.
func Key(spec, version string) string {
	if version == "" {
		return spec
	}
	return spec + "@" + version
}

// SplitKey is the inverse of Key. Returns spec, version. An
// unversioned key returns (key, "").
//
// Care: scoped packages contain "@" in the SPEC (e.g.
// "@vue/runtime-dom@3.4.21"). We split on the LAST "@" only when
// it isn't at position 0.
func SplitKey(key string) (spec, version string) {
	if i := strings.LastIndex(key, "@"); i > 0 {
		return key[:i], key[i+1:]
	}
	return key, ""
}

// --- intermediate types for ordered marshaling ---------------------

// orderedEntry pairs a key with its package so we can emit them as
// a JSON object with stable key order. We can't just hand a
// []Package to json.Marshal — the spec is in the key, not the body.
type orderedEntry struct {
	Key string
	Pkg Package
}

// orderedFile is the marshal-time view of File. We implement
// MarshalJSON for the packages list so it emerges as an object,
// not an array — the JSON shape on disk stays the same as the
// public File type.
type orderedFile struct {
	Version  int            `json:"version"`
	Packages []orderedEntry `json:"-"`
}

func (o orderedFile) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`{"version":`)
	v, _ := json.Marshal(o.Version)
	buf.Write(v)
	buf.WriteString(`,"packages":{`)
	for i, e := range o.Packages {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, err := json.Marshal(e.Key)
		if err != nil {
			return nil, err
		}
		buf.Write(k)
		buf.WriteByte(':')
		pkg, err := json.Marshal(e.Pkg)
		if err != nil {
			return nil, err
		}
		buf.Write(pkg)
	}
	buf.WriteString("}}")
	return buf.Bytes(), nil
}

// --- errors --------------------------------------------------------

// ErrNotResolved is returned by Resolve when no entry matches.
// Callers detect it with errors.Is.
var ErrNotResolved = errors.New("lockfile: not resolved")

// AmbiguousError fires when a spec resolves to multiple versions
// but the caller didn't pin one. v0.1 disallows nested resolution
// (a single project must agree on one version per package); this
// error tells the user which versions conflict.
type AmbiguousError struct {
	Spec     string
	Versions []string
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("lockfile: %s is pinned at multiple versions: %s — nexus v0.1 disallows nested resolution",
		e.Spec, strings.Join(e.Versions, ", "))
}
