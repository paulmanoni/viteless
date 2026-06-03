package viteless

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/evanw/esbuild/pkg/api"
)

// shortHash returns a short stable hex digest of s, used to disambiguate
// pre-bundle URLs for different entry sub-paths of the same package.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// shortHashLong is shortHash with more bits, for on-disk cache keys where
// collisions across packages/entry-sets must be vanishingly unlikely.
func shortHashLong(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// Dependency pre-bundling — the unbundled dev server's answer to the
// native-ESM waterfall. Without it the browser fetches one HTTP request per
// dependency module; a Vuetify app fans out to ~1800 intra-package modules,
// which makes the first cold load crawl. Vite solves this with optimizeDeps
// (esbuild-bundle each npm dep into one file up front). This is the same
// idea, on demand:
//
//   - The FIRST time a dependency entry URL is requested, esbuild bundles it
//     — INLINING every sibling module in the SAME package (read from the
//     store) so the package's whole intra-fan-out collapses into one file.
//   - Cross-package imports (e.g. vuetify importing `vue`) are kept EXTERNAL
//     and rewritten to the existing /@dep/ per-module URL. That URL is
//     byte-identical to what the non-prebundled path serves, so a dependency
//     like vue still resolves to ONE shared module instance — the invariant
//     that makes state-preserving HMR work. Pre-bundling never crosses a
//     package boundary, so it can't fork an instance.
//   - Non-JS imports (CSS / fonts / images) inside a package are also kept
//     external (served as their usual CSS/asset modules), since this is a
//     JS bundle with no output path.
//
// Fail-safe: a package that fails to bundle returns an error and the caller
// falls back to serving that dependency per-module. Pre-bundling is a pure
// performance optimisation layered over the working per-module path.

// PrebundlePrefix is the URL namespace pre-bundled package entries are
// served under (distinct from DepPrefix's per-module blobs).
const PrebundlePrefix = "/@pre/"

// prebundler bundles dependency packages on demand and caches the results.
// Safe for concurrent use.
//
// Dedup model: a package's sub-path entries (vuetify, vuetify/components,
// vuetify/directives, …) are NOT bundled independently — that inlined a
// fresh copy of every shared internal module into each entry. Instead all
// known entries of a package are built TOGETHER in one esbuild build with
// code-splitting, so shared internals land in a common chunk both entries
// import. Entries are registered (by ResolveImport via toPrebundle) as the
// browser walks the graph; the build runs lazily on first fetch and is
// rebuilt only if the entry set grew.
type prebundler struct {
	host *DefaultHost

	mu      sync.Mutex
	entries map[string]map[string]bool // pkgKey → set of entry store URLs
	built   map[string]*pkgBuild       // pkgKey → last build (nil until built)

	diskDir string // <store>/prebundle; "" disables cross-restart persistence
}

// pkgBuild is one package's grouped, code-split build: a map of served
// basename → file contents (entries + shared chunks), plus the entry-set
// signature it was built from (to detect when a rebuild is needed) and any
// build error.
type pkgBuild struct {
	files map[string]string // basename (e-<hash>.js / chunk-<hash>.js) → JS
	sig   string            // sorted entry-set fingerprint
	err   error
}

func newPrebundler(h *DefaultHost) *prebundler {
	p := &prebundler{
		host:    h,
		entries: map[string]map[string]bool{},
		built:   map[string]*pkgBuild{},
	}
	// Persist built packages under the shared cache so a `nexus dev`
	// restart reuses them instead of re-running esbuild. The store already
	// persists the dep blobs cross-restart; this adds the bundle output.
	if h.res.Store != nil {
		p.diskDir = filepath.Join(h.res.Store.Root(), "prebundle")
		// Sweep stale builds (deps the project no longer imports) once at
		// startup, off the hot path. loadFromDisk refreshes in-use dirs, so
		// only genuinely-orphaned builds are reclaimed.
		go p.gcDisk()
	}
	return p
}

// definesSig fingerprints the esbuild Defines that feed every build, so a
// changed Mode / VITE_* env invalidates the on-disk cache.
func (p *prebundler) definesSig() string {
	if len(p.host.defines) == 0 {
		return ""
	}
	keys := make([]string, 0, len(p.host.defines))
	for k := range p.host.defines {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(p.host.defines[k])
		b.WriteByte('\n')
	}
	return b.String()
}

// diskKeyDir returns the on-disk directory a package build is cached in,
// keyed by package + entry-set signature + defines signature. A changed
// entry set or defines yields a different dir, so stale builds are never
// served and old dirs simply accrue (swept by `nexus gc` / manual clean).
// Returns "" when persistence is disabled (no store).
func (p *prebundler) diskKeyDir(pkg, sig string) string {
	if p.diskDir == "" {
		return ""
	}
	h := shortHashLong(pkg + "\x00" + sig + "\x00" + p.definesSig())
	// pkg may contain '/' and '@' (scoped, versioned) — hash is filesystem
	// safe; keep a readable prefix for debugging.
	safe := strings.NewReplacer("/", "_", "@", "-").Replace(pkg)
	return filepath.Join(p.diskDir, safe+"-"+h)
}

// loadFromDisk reconstructs a pkgBuild from a cached directory, or nil on
// any miss/corruption (caller rebuilds). The manifest lists the basenames;
// each is a sibling file.
func (p *prebundler) loadFromDisk(pkg, sig string) *pkgBuild {
	dir := p.diskKeyDir(pkg, sig)
	if dir == "" {
		return nil
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "manifest"))
	if err != nil {
		return nil
	}
	files := map[string]string{}
	for _, base := range strings.Split(strings.TrimSpace(string(manifest)), "\n") {
		if base == "" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, base))
		if err != nil {
			return nil // incomplete cache dir — treat as miss
		}
		files[base] = string(body)
	}
	if len(files) == 0 {
		return nil
	}
	// Mark the dir as recently used so the GC retention window is measured
	// from last USE, not last build — a long-lived pinned dep stays cached
	// across many sessions. Touch the directory (not the manifest, whose
	// mtime callers use to detect rebuilds). Best-effort.
	now := timeNow()
	_ = os.Chtimes(dir, now, now)
	return &pkgBuild{files: files, sig: sig}
}

// prebundleTTL is how long an unused on-disk package build is kept before
// gcDisk sweeps it. Generous, since a reused dir is refreshed on every load
// — this only reclaims builds for deps the project no longer imports (e.g.
// after a version bump leaves the old pkg@ver's dir orphaned).
const prebundleTTL = 14 * 24 * time.Hour

// timeNow is a package var so tests can pin the clock.
var timeNow = time.Now

// gcDisk removes prebundle cache directories not used within prebundleTTL.
// Run once at startup (cheap: one ReadDir + a Stat per entry). Best-effort
// — any error is ignored, and an in-use dir is never at risk because
// loadFromDisk refreshes its mtime. Returns the number of dirs removed.
func (p *prebundler) gcDisk() int {
	if p.diskDir == "" {
		return 0
	}
	entries, err := os.ReadDir(p.diskDir)
	if err != nil {
		return 0 // no cache dir yet, or unreadable — nothing to do
	}
	cutoff := timeNow().Add(-prebundleTTL)
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(p.diskDir, e.Name())
		info, err := os.Stat(dir)
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if os.RemoveAll(dir) == nil {
			removed++
		}
	}
	return removed
}

// saveToDisk writes a successful build to its cache dir (best-effort;
// errors are ignored — persistence is an optimisation). Writes files first,
// then the manifest last, so loadFromDisk only succeeds on a complete dir.
func (p *prebundler) saveToDisk(pkg string, b *pkgBuild) {
	if b == nil || b.err != nil || len(b.files) == 0 {
		return
	}
	dir := p.diskKeyDir(pkg, b.sig)
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	var bases []string
	for base, js := range b.files {
		if err := os.WriteFile(filepath.Join(dir, base), []byte(js), 0o644); err != nil {
			return
		}
		bases = append(bases, base)
	}
	sort.Strings(bases)
	_ = os.WriteFile(filepath.Join(dir, "manifest"), []byte(strings.Join(bases, "\n")+"\n"), 0o644)
	// Stamp the dir's mtime via the same clock the GC reads, so freshness
	// is consistent (and pinnable in tests). loadFromDisk refreshes it on
	// reuse; gcDisk sweeps from it.
	now := timeNow()
	_ = os.Chtimes(dir, now, now)
}

// register records an entry store URL under its package so the next build
// of that package covers it. Returns the package key + the entry's stable
// served basename (e-<hash>.js).
func (p *prebundler) register(entryStoreURL string) (pkg, base string) {
	pkg = pkgKey(entryStoreURL)
	base = "e-" + shortHash(entryStoreURL) + ".js"
	p.mu.Lock()
	if p.entries[pkg] == nil {
		p.entries[pkg] = map[string]bool{}
	}
	p.entries[pkg][entryStoreURL] = true
	p.mu.Unlock()
	return pkg, base
}

// serve returns the JS for a served prebundle basename within a package,
// building (or rebuilding) the package's grouped split bundle as needed.
func (p *prebundler) serve(pkg, base string) (string, error) {
	p.mu.Lock()
	want := p.entrySig(pkg)
	b := p.built[pkg]
	if b == nil || b.sig != want {
		entries := make([]string, 0, len(p.entries[pkg]))
		for u := range p.entries[pkg] {
			entries = append(entries, u)
		}
		p.mu.Unlock()
		// Cross-restart cache: a prior `nexus dev` may have built this exact
		// (package, entry-set, defines) before. Reuse it instead of running
		// esbuild again.
		nb := p.loadFromDisk(pkg, want)
		if nb == nil {
			nb = p.buildPackage(pkg, entries, want) // network/CPU outside the lock
			p.saveToDisk(pkg, nb)
		}
		p.mu.Lock()
		// Keep the freshest build (another goroutine may have raced).
		if cur := p.built[pkg]; cur == nil || cur.sig != want {
			p.built[pkg] = nb
		}
		b = p.built[pkg]
	}
	p.mu.Unlock()

	if b.err != nil {
		return "", b.err
	}
	if js, ok := b.files[base]; ok {
		return js, nil
	}
	return "", fmt.Errorf("prebundle: %s not in package build %s", base, pkg)
}

// entrySig fingerprints a package's current entry set (caller holds mu).
func (p *prebundler) entrySig(pkg string) string {
	es := make([]string, 0, len(p.entries[pkg]))
	for u := range p.entries[pkg] {
		es = append(es, u)
	}
	sort.Strings(es)
	return strings.Join(es, "\n")
}

// pkgKeyRE pulls "pkg@ver" (incl. scoped) out of an esm.sh URL:
//
//	https://esm.sh/vue@3.5.34/es2022/vue.mjs                  → vue@3.5.34
//	https://esm.sh/@vue/runtime-core@3.5.34/es2022/x.mjs      → @vue/runtime-core@3.5.34
//	https://esm.sh/vuetify@3.11.7/X-ZX.../es2022/components.mjs → vuetify@3.11.7
//
// Version segment stops at the first /, ?, or & so a query string
// (?external=vue,react-dom) or sub-path never leaks into the package key.
var pkgKeyRE = regexp.MustCompile(`esm\.sh/((?:@[^/]+/)?[^/@?&]+@[^/?&]+)`)

// pkgKey returns the "name@version" identity of a registry URL, or "" when
// the URL isn't a recognizable esm.sh package URL.
func pkgKey(url string) string {
	m := pkgKeyRE.FindStringSubmatch(url)
	if m == nil {
		return ""
	}
	return m[1]
}

// buildPackage runs ONE esbuild build over ALL of a package's known entry
// store URLs with code-splitting on, so modules shared between sub-path
// entries (vuetify vs vuetify/components) land in a common chunk instead of
// being inlined into each entry. Returns a pkgBuild mapping served basename
// → JS (one e-<hash>.js per entry + chunk-*.js shared chunks). sig is the
// entry-set fingerprint the build was made from.
//
// Resolution rules (unchanged from the per-entry build): same-package JS is
// inlined/split internally; cross-package and non-JS imports stay EXTERNAL
// pointed at /@dep/ URLs, preserving the single shared instance for vue and
// serving CSS/assets as their usual modules.
func (p *prebundler) buildPackage(entryPkg string, entryURLs []string, sig string) *pkgBuild {
	if entryPkg == "" || len(entryURLs) == 0 {
		return &pkgBuild{sig: sig, err: fmt.Errorf("prebundle: no entries for %q", entryPkg)}
	}

	const ns = "nexus-prebundle"
	// Map each entry's synthetic input path → its store URL. OutputPath
	// e-<hash> matches the served basename register() hands out.
	entryURLByInput := map[string]string{}
	var eps []api.EntryPoint
	for _, u := range entryURLs {
		input := "nexus-entry:" + u // synthetic; claimed in OnResolve below
		entryURLByInput[input] = u
		eps = append(eps, api.EntryPoint{InputPath: input, OutputPath: "e-" + shortHash(u)})
	}

	plugin := api.Plugin{
		Name: "nexus-prebundle",
		Setup: func(b api.PluginBuild) {
			b.OnResolve(api.OnResolveOptions{Filter: ".*"}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
				// Entry points arrive as our synthetic "nexus-entry:" inputs.
				if storeURL, ok := entryURLByInput[args.Path]; ok {
					return api.OnResolveResult{Path: storeURL, Namespace: ns}, nil
				}
				// Otherwise resolve against the importer's registry URL.
				base := ""
				if args.Namespace == ns && args.Importer != "" {
					base = args.Importer
				}
				storeURL, found, _ := p.host.res.ResolveURL(args.Path, base)
				if !found || storeURL == "" {
					return api.OnResolveResult{}, nil // leave to esbuild default
				}
				// Same package + JS → keep internal (inlined or split into a
				// shared chunk by esbuild).
				if pkgKey(storeURL) == entryPkg && p.host.depURLIsJS(storeURL) {
					return api.OnResolveResult{Path: storeURL, Namespace: ns}, nil
				}
				// Cross-package or non-JS → EXTERNAL /@dep/ URL (shared
				// instance for vue; CSS/asset served as usual).
				return api.OnResolveResult{Path: p.host.toDepPath(storeURL), External: true}, nil
			})
			b.OnLoad(api.OnLoadOptions{Filter: ".*", Namespace: ns}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
				body, _, ok := p.host.loadDepBytes(args.Path)
				if !ok {
					return api.OnLoadResult{}, fmt.Errorf("prebundle: blob not in cache: %s", args.Path)
				}
				s := string(body)
				return api.OnLoadResult{Contents: &s, Loader: api.LoaderJS}, nil
			})
		},
	}

	result := api.Build(api.BuildOptions{
		EntryPointsAdvanced: eps,
		Bundle:              true,
		Write:               false,
		Splitting:           true,             // shared internals → common chunk
		Outdir:              "/nexus-pre-out", // virtual; Write:false
		ChunkNames:          "chunk-[hash]",
		Format:              api.FormatESModule,
		Target:              api.ES2022,
		Platform:            api.PlatformBrowser,
		Plugins:             []api.Plugin{plugin},
		LogLevel:            api.LogLevelSilent,
		Define:              p.host.defines,
	})
	if len(result.Errors) > 0 {
		return &pkgBuild{sig: sig, err: fmt.Errorf("prebundle %s: %s", entryPkg, result.Errors[0].Text)}
	}
	if len(result.OutputFiles) == 0 {
		return &pkgBuild{sig: sig, err: fmt.Errorf("prebundle %s: no output", entryPkg)}
	}

	// Map outputs by basename. esbuild names entry outputs e-<hash>.js (our
	// OutputPath) and shared chunks chunk-<hash>.js; both are imported by
	// basename within the package, so a flat basename map suffices. Rewrite
	// the entry/chunk cross-imports (relative "./chunk-X.js") to the served
	// /@pre/<pkg>/ path so the browser fetches siblings from this package.
	files := map[string]string{}
	for _, f := range result.OutputFiles {
		base := f.Path[strings.LastIndexByte(f.Path, '/')+1:]
		files[base] = rewritePkgChunkImports(string(f.Contents), entryPkg)
	}
	return &pkgBuild{files: files, sig: sig}
}

// pkgChunkImportRE matches a relative import/export of a sibling chunk or
// entry inside a package build, e.g. `from"./chunk-ABC.js"` or
// `from "./e-123.js"`. esbuild emits these between split outputs.
var pkgChunkImportRE = regexp.MustCompile(`(from\s*|import\s*|^\s*import\s+)(["'])\.?/?((?:chunk|e)-[A-Za-z0-9]+\.js)(["'])`)

// rewritePkgChunkImports rewrites a split output's relative sibling imports
// (./chunk-*.js, ./e-*.js) to absolute /@pre/<pkg>/ served paths so the
// browser fetches the shared chunk from this package's namespace.
func rewritePkgChunkImports(js, pkg string) string {
	prefix := PrebundlePrefix + pkg + "/"
	return pkgChunkImportRE.ReplaceAllString(js, `$1$2`+prefix+`$3$4`)
}

// prebundleEligible reports whether a resolved dependency store URL should be
// pre-bundled rather than served per-module. Only JS package modules are
// eligible. The ENTIRE Vue family (vue + @vue/*) is excluded and kept
// per-module: those packages carry the single-instance invariant (one
// reactivity system, one runtime-core that installs __VUE_HMR_RUNTIME__,
// one set of vnode shapes). Pre-bundling them risks inlining a second copy
// of runtime-core/reactivity into a sibling bundle and forking the
// instance, which would silently break reactivity + HMR. Everything else
// (vuetify, pinia, apollo, …) is safe to bundle — it imports the Vue family
// by URL, so it shares the one per-module instance.
func prebundleEligible(storeURL string) bool {
	pk := pkgKey(storeURL)
	if pk == "" {
		return false
	}
	if pk == "vue" || strings.HasPrefix(pk, "vue@") || strings.HasPrefix(pk, "@vue/") {
		return false
	}
	// React, like Vue, depends on a single shared internals instance
	// (ReactSharedInternals / the hooks dispatcher) across react,
	// react-dom, and react/jsx-runtime. Pre-bundling each entry inlines
	// its own react copy, so the dispatcher react-dom sets isn't the one
	// a component's useState reads → "i.H is null". Serve the React
	// family per-module so the browser dedupes them to one instance by URL.
	if pk == "react" || strings.HasPrefix(pk, "react@") ||
		pk == "react-dom" || strings.HasPrefix(pk, "react-dom@") ||
		pk == "scheduler" || strings.HasPrefix(pk, "scheduler@") {
		return false
	}
	return true
}

// --- Host helpers shared with the per-module path -----------------------

// loadDepBytes reads a cached dependency blob by its canonical store URL,
// returning the bytes + module kind ("js"/"css"/"asset"). Mirrors loadDep's
// store read (incl. the on-demand cold-miss fetch) without the URL encoding
// or CSS url() rewriting — the prebundler works in store-URL space.
func (h *DefaultHost) loadDepBytes(storeURL string) ([]byte, string, bool) {
	blob, meta, err := h.res.Store.Get(storeURL)
	if err != nil {
		if h.res.FetchOnDemand != nil {
			if c, ferr := h.res.FetchOnDemand(storeURL); ferr == nil && c != "" {
				blob, meta, err = h.res.Store.Get(c)
			}
		}
		if err != nil {
			return nil, "", false
		}
	}
	body, rerr := os.ReadFile(blob)
	if rerr != nil {
		return nil, "", false
	}
	return body, kindForContentType(meta.ContentType, storeURL), true
}

// depURLIsJS reports whether a dependency store URL is a JS module (vs CSS or
// an asset), used to decide what the prebundler may inline.
func (h *DefaultHost) depURLIsJS(storeURL string) bool {
	_, meta, err := h.res.Store.Get(storeURL)
	if err != nil {
		// Unknown → infer from extension; default to JS (esm.sh's norm).
		return kindForContentType("", storeURL) == "js"
	}
	return kindForContentType(meta.ContentType, storeURL) == "js"
}
