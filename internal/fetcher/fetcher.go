// Package fetcher pulls ESM modules from a configurable registry
// (default: https://esm.sh) into the local store, following the
// transitive import graph so a single Fetch("vue") resolves vue
// plus every module vue imports.
//
// Version pinning happens via HTTP redirects: a request to
// `https://esm.sh/vue` is answered with a 302 to
// `https://esm.sh/vue@3.4.21`, which is the URL we record in the
// lockfile. Subsequent installs hit the resolved URL directly so a
// week-later "nexus install" doesn't drift onto vue@3.4.22.
//
// All HTTP I/O goes through an injected http.Client so tests can
// drive the fetcher against httptest.NewServer fixtures rather
// than hitting the real registry.
package fetcher

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/paulmanoni/viteless/internal/lockfile"
	"github.com/paulmanoni/viteless/internal/store"
)

// DefaultRegistry is the canonical esm.sh URL — the v0.1 default.
// Override via Fetcher.Registry or NEXUS_REGISTRY at the CLI level.
const DefaultRegistry = "https://esm.sh"

// Fetcher is the HTTP client + store glue. Build one per process
// (cheap; carries no per-call state besides the http.Client which
// has its own pooling).
type Fetcher struct {
	// Registry is the base URL prepended to bare specs. Must NOT
	// have a trailing slash — we append "/" + spec ourselves.
	Registry string

	// Store is where fetched bytes land. Required.
	Store *store.Store

	// HTTP is the client used for all GETs. Defaults to
	// http.DefaultClient if zero; tests inject a client whose
	// Transport points to httptest.NewServer.
	//
	// Important: the client's CheckRedirect MUST allow at least
	// one redirect (the version-pin hop). The default policy
	// (10 hops max, no rewrite of method) is fine.
	HTTP *http.Client

	// URLQuery, when set, is appended to every fetched URL as a
	// query string. esm.sh in particular honors `?target=es2015`
	// to serve pre-lowered code suitable for older JS engines
	// (Goja's case). For the bundler's normal user-code path the
	// default empty value is correct — modern browsers eat the
	// default ES2022-ish output esm.sh serves.
	URLQuery string

	// External names packages that esm.sh should leave as BARE
	// imports in every bundle it serves us, rather than embedding
	// them via internal `/v135/<pkg>@<ver>/...` URLs. Composed into
	// the `?external=<comma-list>` query string esm.sh recognizes.
	//
	// This is the deduplication seam. Without it, vue-flow's body
	// (fetched from esm.sh) contains hard-coded references to the
	// specific Vue version esm.sh chose at fetch time — typically
	// the latest 3.x. If the project ALREADY pinned vue at a
	// different version via `nexus add vue`, that second Vue gets
	// pulled into the bundle alongside the pinned one, and Vue's
	// reactivity system silently splits (singleton state ends up
	// in the wrong copy → "Fa is null" at runtime).
	//
	// With external=vue, vue-flow's body has `import "vue"` left
	// as a bare specifier. esbuild's OnResolve sees it, asks the
	// lockfile for "vue", gets the pinned version, and the project
	// ends up with exactly one Vue copy — regardless of what version
	// any transitive dep was built against.
	//
	// Applies only to top-level bare-spec fetches. Absolute URLs
	// (the recursion path) ignore this field — they're already
	// CDN-internal paths that wouldn't benefit from re-externalizing
	// (and esm.sh ignores ?external= on direct path access anyway).
	//
	// Sensible default: ["vue", "react", "react-dom"] — the three
	// classic peer-dep singletons. Set via CLI in newDepsContext;
	// tests typically leave this empty.
	External []string

	// Concurrency caps the number of in-flight HTTP fetches during
	// transitive recursion. The default (8 when zero) is a sweet
	// spot for esm.sh — high enough to overlap network latency
	// across the tens of sub-package fetches a vue / vuetify install
	// fans out into, low enough to avoid esm.sh's rate-limiter
	// returning 429s on bursty installs.
	//
	// Set to 1 to fall back to the previous serial behavior (useful
	// for deterministic test output where parallel goroutines would
	// race on stdout). Negative values are clamped to the default.
	Concurrency int

	// Progress receives one line per HTTP fetch the recursion makes:
	// cache hits, cache misses with size + duration, and the
	// transitive depth so the operator can SEE what `nexus install
	// vue` is doing during the 30+ requests it fans out into.
	//
	// nil disables progress output entirely — Fetch is silent, same
	// as the legacy behavior. The CLI's runAdd / runInstall wire
	// this to stdout so the human-facing path always shows
	// per-fetch lines.
	Progress io.Writer

	// PinnedVersions maps bare-spec names to the version transitive
	// recursion must use. The companion to External: External tells
	// esm.sh "leave this as a bare import in served bundles";
	// PinnedVersions tells the fetcher "when something recurses into
	// `import \"vue\"`, fetch vue@<pinned>, not the registry's latest."
	//
	// Without this, the package.json pin `vue: ^3.4.0` (fetched as
	// vue@3.4.0) coexists in the lockfile with `vue@3.5.34` (the
	// version esm.sh redirects bare /vue to today), and the resolver
	// errors with AmbiguousError. Or worse, the bundle silently
	// includes both copies and Vue's reactivity globals split at
	// runtime — the same symptom External alone was meant to fix.
	//
	// The CLI populates this from the project's existing lockfile
	// entries before any fetch; build-ui populates it from
	// package.json dependency versions. Keys are bare spec names
	// ("vue", "@vue/runtime-dom"); values are exact versions
	// ("3.4.0") with no semver prefix. Lookup is exact: PinnedVersions
	// only fires when the recursion spec has no version of its own.
	PinnedVersions map[string]string
}

// New returns a Fetcher with sensible defaults. The store argument
// is required; pass nil for registry to get DefaultRegistry.
func New(s *store.Store, registry string) *Fetcher {
	if registry == "" {
		registry = DefaultRegistry
	}
	registry = strings.TrimRight(registry, "/")
	return &Fetcher{
		Registry: registry,
		Store:    s,
		HTTP:     http.DefaultClient,
	}
}

// Result is what Fetch returns: a single resolved package plus the
// set of additional packages found via transitive recursion. The
// caller (typically `nexus add`) writes them all into the lockfile.
//
// Root is the entry the caller asked for. Transitive is keyed by
// the lockfile Key("spec","version") format and may be empty when
// nothing imported beyond the root.
type Result struct {
	Root        lockfile.Package
	Transitive  map[string]lockfile.Package
}

// Fetch resolves `spec` (e.g. "vue", "vue@3.4.21", "@vue/runtime-dom")
// against the registry, downloads the bytes, hashes them, stashes in
// the store, parses imports, and recurses. The returned Result
// carries every package fetched along the way.
//
// Already-cached blobs are NOT re-downloaded — Fetch reads them
// straight from the store. Already-cached URLs with KNOWN integrity
// (lockfile entry) get their hash verified against the cached blob
// to catch tampering before the resolver hands them to esbuild;
// that's wired up in the CLI layer, not here, since the fetcher
// doesn't see the lockfile by design (separation of concerns).
func (f *Fetcher) Fetch(ctx context.Context, spec string) (Result, error) {
	concurrency := f.Concurrency
	if concurrency <= 0 {
		concurrency = 8
	}
	w := &walkContext{
		sem:     make(chan struct{}, concurrency),
		visited: map[string]lockfile.Package{},
	}
	root, err := f.fetchOne(ctx, spec, w)
	if err != nil {
		return Result{}, err
	}
	// The root is in visited too; strip it from Transitive so the
	// caller has a clean (Root, Transitive) split. Use the
	// thread-safe accessor since the recursion goroutines may have
	// just finished writing.
	w.mu.Lock()
	delete(w.visited, lockfile.Key(root.Spec, root.Version))
	transitive := w.visited
	w.mu.Unlock()
	return Result{Root: root, Transitive: transitive}, nil
}

// walkContext is the shared state for one Fetch invocation —
// bounded-concurrency semaphore, dedup map for already-finished
// fetches, and per-resolved-URL "in-flight" tracker so concurrent
// import paths into the same transitive don't race into duplicate
// HTTP round-trips.
//
// One walkContext per top-level Fetch call. The same Fetcher can
// host multiple concurrent Fetch invocations safely because the
// only Fetcher-level mutable state (the http.Client) is itself
// concurrent-safe.
type walkContext struct {
	// sem caps concurrency across the WHOLE recursion tree. Same
	// semaphore is shared down the goroutine chain so 8 active
	// fetches at the leaf level still count against the cap.
	sem chan struct{}

	// visited holds completed lockfile.Package entries keyed by
	// lockfile.Key(spec, version). Mutated under mu since multiple
	// goroutines append simultaneously.
	visited map[string]lockfile.Package
	mu      sync.Mutex

	// inflight tracks per-resolved-URL fetches that are currently
	// in progress. When goroutine A claims a URL, goroutine B
	// reaching the same URL waits on A's done channel + returns
	// A's result — avoids redundant HTTP requests + cache writes
	// when diamond imports converge on the same blob.
	inflight sync.Map // resolvedURL → *pendingFetch
}

// pendingFetch is the per-URL coordination primitive used by
// inflight. The first goroutine to LoadOrStore wins the right
// to do the actual fetch; subsequent goroutines wait on done
// and return the cached result/err.
type pendingFetch struct {
	done   chan struct{}
	result lockfile.Package
	err    error
}

// fetchOne is the recursive worker. visited is the dedup map across
// the whole traversal — keyed by resolved URL so a diamond import
// (A imports B, A imports C, B imports C) only fetches C once.
//
// We dedupe by RESOLVED URL rather than by spec because two
// different specs ("vue" and "vue@3.4.21") can resolve to the same
// final URL after redirect-pinning; storing them separately in
// `visited` would cause double-fetches.
func (f *Fetcher) fetchOne(ctx context.Context, spec string, w *walkContext) (lockfile.Package, error) {
	reqURL, err := f.specToURL(spec)
	if err != nil {
		return lockfile.Package{}, err
	}

	// FAST PATH: try the local cache by reqURL BEFORE any HTTP work,
	// for both absolute URLs AND bare specs. On hit, reqURL IS the
	// cache key — we trust it and skip the HEAD entirely.
	//
	// Originally guarded to absolute URLs only because bare specs
	// MAY redirect to a canonical versioned form. But the install
	// flow is dominated by VERSIONED bare specs (e.g.
	// `vue@3.5.34?external=...`) that won't redirect since they're
	// already canonical, and those were each paying ~200ms of HEAD
	// per cached blob. For vuetify (~1000 transitive entries, mostly
	// cached on the second install) that adds up to MINUTES of wall
	// time on what should be a no-op refresh.
	//
	// Unversioned bare specs (`vue` with no `@version`) miss this
	// fast-path on a cold cache because the canonical URL differs
	// from reqURL — they fall through to resolve() and get pinned
	// the first time. Once in the lockfile, subsequent installs
	// always feed in the versioned form, hitting the fast-path.
	//
	// In-flight dedup runs FIRST so a goroutine racing with us on
	// the same URL waits on our done channel instead of paying for
	// a redundant resolve() + cache-check after both miss together.
	var (
		resolved    string
		contentType string
		etag        string
		esmPath     string
		preCached   bool
		preBodyPath string
		preMeta     store.Metadata
	)
	if got, loaded := w.inflight.Load(reqURL); loaded {
		p := got.(*pendingFetch)
		select {
		case <-p.done:
			return p.result, p.err
		case <-ctx.Done():
			return lockfile.Package{}, ctx.Err()
		}
	}
	if path, meta, gerr := f.Store.Get(reqURL); gerr == nil {
		resolved = reqURL
		contentType = meta.ContentType
		preCached = true
		preBodyPath = path
		preMeta = meta
	} else if !errors.Is(gerr, store.ErrNotCached) {
		return lockfile.Package{}, fmt.Errorf("fetcher: read cache for %s: %w", reqURL, gerr)
	}

	// Track semaphore ownership so we can release at the right
	// moment regardless of which exit path we take. The slot is
	// acquired ONLY for network work (resolve + body GET) — never
	// held across recursion, which would deadlock when child
	// goroutines also need slots while their parent is blocked
	// waiting for them. Real bug: pinia's ~20-node tree at
	// Concurrency=8 hung indefinitely with the previous design.
	slotHeld := false
	releaseSlot := func() {
		if slotHeld {
			<-w.sem
			slotHeld = false
		}
	}
	defer releaseSlot()

	if !preCached {
		select {
		case w.sem <- struct{}{}:
			slotHeld = true
		case <-ctx.Done():
			return lockfile.Package{}, ctx.Err()
		}
		// HEAD request first to get the redirect-pinned URL without
		// downloading the body. esm.sh + ETag-aware registries are
		// fast for HEAD; we skip a body transfer when the URL is
		// already in the cache (next branch below).
		resolved, contentType, etag, esmPath, err = f.resolve(ctx, reqURL)
		if err != nil {
			// CSS-only-package fallback: bare specs that 404 on
			// esm.sh might still be valid packages — they just
			// have no JS to serve. @mdi/font, bootstrap-icons,
			// katex (CSS-side), etc. all fall here. Try the
			// package.json to discover a `style` / asset entry
			// and retry with the file-path spec.
			if !isAbsoluteURL(spec) && !isFilePathSpec(spec) {
				if herr := (*HTTPError)(nil); errors.As(err, &herr) && herr.Status == http.StatusNotFound {
					if fallbackSpec, derr := f.discoverPackageEntry(ctx, herr.URL); derr == nil && fallbackSpec != "" {
						// Retry with the discovered asset entry.
						// Goes through the full fetchOne flow so
						// progress logging + dedup + recursion
						// all work the same way.
						return f.fetchOne(ctx, fallbackSpec, w)
					}
				}
			}
			return lockfile.Package{}, err
		}
	}

	// Canonicalize the resolved URL when the registry exposed a
	// versioned path via X-ESM-Path but the request URL itself was
	// unversioned (e.g. /vue?external=vue → header reveals it's
	// vue@3.5.34). Without this, the lockfile records the floating
	// URL and a `nexus install` six months later silently drifts onto
	// whatever vue esm.sh is serving then — defeating the lockfile.
	//
	// Only applies to bare-spec fetches. Absolute-URL recursion
	// already carries the version in the URL path; the X-ESM-Path
	// header for those just points at the internal content path
	// (e.g. /vue@3.5.34/es2022/vue.mjs) which we'd misuse if we
	// also canonicalized it.
	if !preCached && esmPath != "" && extractVersionFromURL(resolved) == "" {
		if canon := canonicalizeResolvedURL(resolved, esmPath); canon != "" {
			resolved = canon
		}
	}

	// In-flight dedup: if another goroutine in this same Fetch
	// invocation is already pulling THIS resolved URL, wait on
	// its done channel and return its result. Avoids the
	// double-fetch race when diamond imports (A imports B, A
	// imports C, B imports C) converge on the same transitive.
	pending := &pendingFetch{done: make(chan struct{})}
	got, loaded := w.inflight.LoadOrStore(resolved, pending)
	if loaded {
		// Release the semaphore slot BEFORE waiting on a peer —
		// blocking with a slot held would starve other goroutines.
		releaseSlot()
		p := got.(*pendingFetch)
		select {
		case <-p.done:
			return p.result, p.err
		case <-ctx.Done():
			return lockfile.Package{}, ctx.Err()
		}
	}
	// We claimed the slot. Make sure done gets closed even on
	// error paths so waiters don't block forever.
	defer close(pending.done)
	defer func() {
		// On error, leave the slot but with the error set so
		// retries from the same Fetch invocation see the same
		// failure consistently. Subsequent Fetch calls get a
		// fresh walkContext + fresh inflight map.
	}()

	// Final dedup check against finished entries — a sibling
	// goroutine may have completed AND populated visited between
	// our resolve() and the LoadOrStore above. The window is
	// narrow; this catches it.
	w.mu.Lock()
	for _, p := range w.visited {
		if p.Resolved == resolved {
			w.mu.Unlock()
			pending.result = p
			return p, nil
		}
	}
	w.mu.Unlock()

	// Already in the store? (Skip the lookup when the fast-path
	// above already established a cache hit by the absolute URL.)
	var bodyPath string
	var meta store.Metadata
	var gotErr error
	if preCached {
		bodyPath = preBodyPath
		meta = preMeta
	} else {
		bodyPath, meta, gotErr = f.Store.Get(resolved)
		if gotErr != nil && !errors.Is(gotErr, store.ErrNotCached) {
			return lockfile.Package{}, fmt.Errorf("fetcher: read cache for %s: %w", resolved, gotErr)
		}
	}
	var content []byte
	var hash string
	if preCached || gotErr == nil {
		// Cache hit — read the blob bytes (we need the body for
		// import-recursion).
		content, err = readFile(bodyPath)
		if err != nil {
			return lockfile.Package{}, fmt.Errorf("fetcher: read cached blob %s: %w", resolved, err)
		}
		hash = meta.ContentSHA256
		if contentType == "" {
			contentType = meta.ContentType
		}
		f.logProgress("✓ cached", resolved, len(content), 0)
	} else {
		// Cache miss — pull the body.
		started := time.Now()
		content, err = f.get(ctx, resolved)
		if err != nil {
			return lockfile.Package{}, err
		}
		f.logProgress("↓ fetched", resolved, len(content), time.Since(started))
		path, putErr := f.Store.Put(resolved, bytesReader(content), "", store.Metadata{
			URL:         reqURL,
			ResolvedURL: resolved,
			ContentType: contentType,
			ETag:        etag,
		})
		if putErr != nil {
			return lockfile.Package{}, fmt.Errorf("fetcher: store %s: %w", resolved, putErr)
		}
		_, meta, _ = f.Store.Get(resolved)
		_ = path
		hash = meta.ContentSHA256
	}
	// HTTP work is done — release the semaphore so child fetches
	// can use the slot during the recursion below.
	releaseSlot()

	// For file-path specs we record the WHOLE spec (pkg + path) as
	// the lockfile Spec so subsequent `nexus install` can re-fetch
	// the exact file. Plain bare specs use the parsed package name
	// + version so dedup + PinnedVersions lookups continue to key
	// off the package identity, not its path.
	var pkgSpec, pkgVersion string
	if _, _, _, isPath := splitSpecPath(spec); isPath {
		pkgSpec = spec
		// Version stays empty for file-path entries — the URL is
		// what's pinned, not a semver. extractVersionFromURL may
		// still find a version embedded in the resolved URL; we
		// honor it for visibility but it's not load-bearing.
		pkgVersion = extractVersionFromURL(resolved)
	} else {
		pkgSpec, pkgVersion = parseSpec(spec)
		if v := extractVersionFromURL(resolved); v != "" {
			pkgVersion = v
		}
	}
	pkg := lockfile.Package{
		Spec:        pkgSpec,
		Version:     pkgVersion,
		Resolved:    resolved,
		Integrity:   "sha256-" + hash,
		ContentType: contentType,
	}
	// Record now under the lockfile key so concurrent recursion
	// paths racing on a DIFFERENT URL but the SAME (spec,version)
	// key find this entry.
	w.mu.Lock()
	w.visited[lockfile.Key(pkg.Spec, pkg.Version)] = pkg
	w.mu.Unlock()

	finalPkg, err := f.recurseImports(ctx, pkg, content, w)
	if err != nil {
		pending.err = err
		return lockfile.Package{}, err
	}
	pending.result = finalPkg
	return finalPkg, nil
}

// recurseImports walks every import found in pkg's body and
// concurrently fetches each child, bounded by w.sem. Updates
// pkg.Deps with each child's lockfile key (under w.mu) and
// returns the finalized package value.
//
// Two scan paths, one per body type:
//
//   - JS-shape: ExtractImports finds import / export / dynamic
//     import() specifiers.
//   - CSS-shape: ExtractCSSImports finds @import + url(...)
//     references. Without this, font packages and stylesheet-
//     with-image packages would fetch their CSS but not the
//     referenced woff2 / png / svg assets, and esbuild would
//     error at bundle time with "not in cache" for every url().
//
// We treat sourcemaps + JSON + data files as terminal — no
// recursion.
//
// Parallel fan-out: each child fetch acquires a w.sem slot,
// processes, releases. The semaphore is shared across the
// entire walk tree so 8 active fetches at the leaf still count
// against the same cap that 8 fetches at the root would.
func (f *Fetcher) recurseImports(ctx context.Context, pkg lockfile.Package, body []byte, w *walkContext) (lockfile.Package, error) {
	var imports []string
	switch {
	case isJSContent(pkg.ContentType, pkg.Resolved):
		imports = ExtractImports(string(body))
	case isCSSContent(pkg.ContentType, pkg.Resolved):
		imports = ExtractCSSImports(string(body))
	}

	if len(imports) == 0 {
		w.mu.Lock()
		w.visited[lockfile.Key(pkg.Spec, pkg.Version)] = pkg
		w.mu.Unlock()
		return pkg, nil
	}

	// Resolve each import to a child spec up-front (cheap), then
	// dispatch in parallel. resolveAgainst failures are logged
	// out — esbuild's own resolver may still handle them later
	// (data URIs, package-conditional exports).
	type childJob struct {
		imp       string
		childSpec string
	}
	var jobs []childJob
	for _, imp := range imports {
		childSpec, rerr := resolveAgainst(pkg.Resolved, imp)
		if rerr != nil {
			continue
		}
		jobs = append(jobs, childJob{imp: imp, childSpec: childSpec})
	}

	type childResult struct {
		imp   string
		child lockfile.Package
		err   error
	}
	results := make([]childResult, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		// IMPORTANT: do NOT hold w.sem across the recursive fetchOne
		// call. The semaphore bounds concurrent HTTP fetches, which
		// fetchOne acquires/releases internally around its actual
		// network work. Holding the slot here would deadlock:
		// parents holding all 8 slots while waiting for their
		// grandchildren — which also need slots — to complete.
		// (Real symptom: pinia's ~20-node tree at Concurrency=8 hung
		// indefinitely on the second-level recursion.)
		go func(i int, j childJob) {
			defer wg.Done()
			child, err := f.fetchOne(ctx, j.childSpec, w)
			results[i] = childResult{imp: j.imp, child: child, err: err}
		}(i, j)
	}
	wg.Wait()

	// First error wins. Wrap with the parent spec so the user
	// sees the full recursion chain in the error message.
	for _, r := range results {
		if r.err != nil {
			return lockfile.Package{}, fmt.Errorf("fetcher: recurse from %s into %s: %w", pkg.Spec, r.imp, r.err)
		}
		depKey := lockfile.Key(r.child.Spec, r.child.Version)
		pkg.Deps = appendUnique(pkg.Deps, depKey)
	}

	w.mu.Lock()
	w.visited[lockfile.Key(pkg.Spec, pkg.Version)] = pkg
	w.mu.Unlock()
	return pkg, nil
}

// isAbsoluteURL reports whether spec is an absolute http(s) URL.
// Used by the fast-path cache-hit check — bare specs need a HEAD
// to learn their resolved URL before they can be looked up in
// the cache.
func isAbsoluteURL(spec string) bool {
	return strings.HasPrefix(spec, "http://") || strings.HasPrefix(spec, "https://")
}

// specToURL builds the registry URL for a bare spec. Three shapes:
//
//	"vue"               → <registry>/vue?<URLQuery>&external=...
//	"vue@3.4.21"        → <registry>/vue@3.4.21?<URLQuery>&external=...
//	"https://esm.sh/x"  → "https://esm.sh/x?<URLQuery>"   (already absolute)
//
// Absolute URLs are passed through so recursion into CDN-internal
// imports works without re-prefixing. URLQuery (when set) is
// appended consistently regardless of input shape so esm.sh's
// `?target=es2015` lowering applies to EVERY fetched URL — the
// Vue compiler bootstrap depends on this.
//
// External — when set — applies ONLY to the bare-spec shapes
// (top-level package fetches and bare-spec recursion). esm.sh
// returns the package body with the external specifiers left as
// bare `import "<name>"` statements rather than embedded as
// `/v135/<name>@<ver>/...` paths, which is the deduplication
// mechanism we rely on for Vue + React singletons.
func (f *Fetcher) specToURL(spec string) (string, error) {
	if spec == "" {
		return "", errors.New("fetcher: empty spec")
	}
	if strings.HasPrefix(spec, "http://") || strings.HasPrefix(spec, "https://") {
		// Recursion path: an absolute URL the parent body referenced.
		// esm.sh interprets `?external=` only on package-shape URLs
		// (https://esm.sh/<spec>); appending it to internal paths
		// like /v135/.../foo.mjs is at best ignored, at worst a
		// 404. Apply only URLQuery here.
		return appendURLQuery(spec, f.URLQuery), nil
	}
	// File-path spec: bare pkg (or @scope/pkg) followed by /<file/path>.
	// Pass through to the registry without ?external= (CSS / fonts /
	// JSON assets have no JS imports to externalize) and without
	// PinnedVersions (the explicit path is the operator's deliberate
	// intent — `nexus add @mdi/font/css/materialdesignicons.min.css`
	// means "fetch THAT file", not "look up the latest @mdi/font and
	// guess at the CSS path"). esm.sh serves arbitrary files inside
	// a package via direct path access, no module-style query needed.
	if isFilePathSpec(spec) {
		return f.Registry + "/" + spec, nil
	}
	// Apply PinnedVersions when the caller passed a bare name with no
	// version of its own — typically a transitive `import "vue"` that
	// would otherwise resolve to whatever esm.sh redirects to today.
	// If the caller wrote spec="vue@3.4.0" the version is honored
	// verbatim; PinnedVersions never overrides an explicit ask.
	if name, version := parseSpec(spec); version == "" && len(f.PinnedVersions) > 0 {
		if pinned := f.PinnedVersions[name]; pinned != "" {
			spec = name + "@" + pinned
		}
	}
	raw := f.Registry + "/" + spec
	return appendURLQuery(raw, f.composeBareSpecQuery()), nil
}

// isFilePathSpec reports whether spec includes a file path inside a
// package, instead of being a bare package reference. Recognized
// shapes:
//
//	pkg/path                       → true   (vuetify/styles)
//	pkg@version/path               → true   (vue@3.5.26/dist/vue.esm-browser.js)
//	@scope/pkg/path                → true   (@mdi/font/css/materialdesignicons.min.css)
//	@scope/pkg@version/path        → true
//	pkg                            → false  (bare package)
//	@scope/pkg                     → false  (scoped bare package)
//	@scope/pkg@version             → false  (versioned scoped package)
//
// The distinction matters because file-path specs bypass
// ?external= (no JS imports to externalize) and PinnedVersions
// lookups (the path is an explicit ask), so the URL composition
// is "pass through to the registry verbatim" instead of the
// module-shaped construction the bare-spec path uses.
func isFilePathSpec(spec string) bool {
	// Strip leading "@" so the same slash-counting logic handles
	// both scoped and unscoped specs without branching.
	s := spec
	scoped := false
	if strings.HasPrefix(s, "@") {
		s = s[1:]
		scoped = true
	}
	firstSlash := strings.IndexByte(s, '/')
	if firstSlash < 0 {
		return false
	}
	if scoped {
		// "scope/pkg" up to the first slash IS the package name.
		// The path indicator is a SECOND slash anywhere after it.
		return strings.IndexByte(s[firstSlash+1:], '/') >= 0
	}
	// Unscoped: any "/" means the rest is a file path.
	return true
}

// splitSpecPath separates a file-path spec into its package-name,
// version, and path components — handling both scoped + unscoped +
// versioned + unversioned shapes. Returns ("", "", "", false) when
// spec isn't a file-path spec (caller should fall back to plain
// parseSpec).
//
//	"vuetify/styles"                       → ("vuetify", "", "styles")
//	"vue@3.5.26/dist/vue.esm-browser.js"   → ("vue", "3.5.26", "dist/vue.esm-browser.js")
//	"@mdi/font/css/foo.css"                → ("@mdi/font", "", "css/foo.css")
//	"@mdi/font@7.4.47/css/foo.css"         → ("@mdi/font", "7.4.47", "css/foo.css")
//
// Used by the lockfile-population path so the recorded `Spec` is
// the package-name only (not the full path), which keeps PinnedVersions
// + dedup lookups working correctly — multiple `@mdi/font/css/*`
// entries should resolve to one `@mdi/font` pin.
func splitSpecPath(spec string) (name, version, path string, ok bool) {
	if !isFilePathSpec(spec) {
		return "", "", "", false
	}
	// Find the boundary between <package-shape> and <file/path>.
	// For scoped specs the package-shape is "@scope/pkg[@version]" —
	// the path begins after the second top-level slash. For unscoped
	// specs the package-shape is "pkg[@version]" — path begins
	// after the first slash.
	rest := spec
	scoped := strings.HasPrefix(rest, "@")
	if scoped {
		first := strings.IndexByte(rest[1:], '/')
		if first < 0 {
			return "", "", "", false
		}
		second := strings.IndexByte(rest[1+first+1:], '/')
		if second < 0 {
			return "", "", "", false
		}
		boundary := 1 + first + 1 + second
		pkgShape := rest[:boundary]
		path = rest[boundary+1:]
		name, version = parseSpec(pkgShape)
		return name, version, path, true
	}
	first := strings.IndexByte(rest, '/')
	if first < 0 {
		return "", "", "", false
	}
	pkgShape := rest[:first]
	path = rest[first+1:]
	name, version = parseSpec(pkgShape)
	return name, version, path, true
}

// composeBareSpecQuery joins URLQuery and (when External is set)
// the `external=<comma-list>` clause into one query-string body
// without the leading "?". Empty when both inputs are empty.
//
// External names are joined with "," — esm.sh's documented
// separator. Names are written verbatim; scoped names ("@vue/x")
// would need URL-encoding if esm.sh balked at the "@", but it
// doesn't — accepts them as-is. We don't filter or validate the
// names; the CLI populates this from a known-good list.
func (f *Fetcher) composeBareSpecQuery() string {
	parts := make([]string, 0, 2)
	if f.URLQuery != "" {
		parts = append(parts, f.URLQuery)
	}
	if len(f.External) > 0 {
		parts = append(parts, "external="+strings.Join(f.External, ","))
	}
	return strings.Join(parts, "&")
}

// appendURLQuery suffixes the query (without leading "?") onto u
// using "?" or "&" depending on whether u already has a query.
// Empty query returns u unchanged. Malformed URLs are returned
// unchanged too — the subsequent fetch will surface a clearer
// error than we could synthesize here.
func appendURLQuery(u, query string) string {
	if query == "" {
		return u
	}
	if strings.Contains(u, "?") {
		return u + "&" + query
	}
	return u + "?" + query
}

// resolve follows redirects without downloading the body. Returns
// the final URL the body would come from, plus Content-Type, ETag
// and X-ESM-Path headers if present.
//
// We use GET (not HEAD) because some CDNs (Cloudflare, esm.sh's
// edge) serve headers for HEAD that differ from GET — Content-Type
// is sometimes "application/octet-stream" on HEAD but
// "application/javascript" on GET. Using GET costs an extra body
// download we then discard, but the bodies are small and we'd
// fetch them anyway on cache miss.
//
// Closing the body without reading it (when we know we're going to
// re-fetch) is fine — Go's HTTP client returns connections to the
// pool either way.
//
// esmPath is esm.sh's X-ESM-Path response header — present on every
// response and exposes the resolved-version path even when the
// request URL didn't include the version. We need it because esm.sh
// stopped using 302 redirects for unversioned package URLs in 2025;
// /vue now returns the stub at /vue directly with the version
// disclosed only via this header, and we can't extract a version
// from the request URL alone.
func (f *Fetcher) resolve(ctx context.Context, reqURL string) (resolved, contentType, etag, esmPath string, err error) {
	var lastErr error
	for attempt := 0; attempt < maxFetchAttempts; attempt++ {
		if attempt > 0 {
			if waitErr := sleepBackoff(ctx, attempt); waitErr != nil {
				return "", "", "", "", waitErr
			}
		}
		resolved, contentType, etag, esmPath, err = f.resolveOnce(ctx, reqURL)
		if err == nil {
			return resolved, contentType, etag, esmPath, nil
		}
		lastErr = err
		if !isRetryableFetchError(err) {
			return "", "", "", "", err
		}
	}
	return "", "", "", "", fmt.Errorf("fetcher: resolve %s: %w (after %d attempts)", reqURL, lastErr, maxFetchAttempts)
}

func (f *Fetcher) resolveOnce(ctx context.Context, reqURL string) (resolved, contentType, etag, esmPath string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", "", "", "", err
	}
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return "", "", "", "", fmt.Errorf("fetcher: GET %s: %w", reqURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Capture the POST-redirect URL even on error so callers
		// (the CSS-only-package fallback in fetchOne) can extract
		// the resolved version from a 404 on a bare spec. esm.sh
		// happily redirects `vue` → `vue@3.5.34` and only THEN
		// 404s if the package can't be served as a JS module.
		return "", "", "", "", &HTTPError{URL: resp.Request.URL.String(), Status: resp.StatusCode}
	}
	// resp.Request.URL is the final URL after redirects.
	final := resp.Request.URL.String()
	return final, resp.Header.Get("Content-Type"), resp.Header.Get("ETag"), resp.Header.Get("X-ESM-Path"), nil
}

// get downloads the body at url and returns its bytes. Errors carry
// the URL for diagnosis.
//
// Transient transport-level errors (connection reset, EOF, timeout)
// and rate-limit / server-error response codes (429, 5xx) retry
// with exponential backoff up to maxFetchAttempts. The transitive
// walk through esm.sh routinely sees Cloudflare flaking on one of
// the many requests it makes; without retries a single TCP RST
// fails the entire add. 4xx responses (404, 403, …) skip the
// retry loop — those are stable misses, not flakes.
func (f *Fetcher) get(ctx context.Context, url string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < maxFetchAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, attempt); err != nil {
				return nil, err
			}
		}
		body, err := f.getOnce(ctx, url)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !isRetryableFetchError(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("fetcher: GET %s: %w (after %d attempts)", url, lastErr, maxFetchAttempts)
}

// getOnce is the single-attempt body fetch. Pulled out so the
// retry loop in get() can decide whether to back off and retry
// without duplicating the request construction or close logic.
func (f *Fetcher) getOnce(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetcher: GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{URL: url, Status: resp.StatusCode}
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("fetcher: read body %s: %w", url, err)
	}
	return body, nil
}

// maxFetchAttempts caps the retry loop. 3 tries (initial + two
// retries) with the exponential schedule below covers a ~3.5s
// window — enough to ride out the typical Cloudflare hiccup
// without making a long string of "real" failures take forever.
const maxFetchAttempts = 3

// sleepBackoff waits (1s, 2s, 4s, …) between attempts, honoring
// ctx so a cancelled add doesn't burn the whole backoff window.
// Pure exponential; the failures we're papering over are
// independent CDN edge hiccups, no jitter needed for thundering
// herd at the per-process scale we hit.
//
// Exposed as a package-level var so tests can swap in a tiny
// sleep without making the suite take seven seconds per case.
// Production callers don't need to touch it.
var sleepBackoff = func(ctx context.Context, attempt int) error {
	delay := time.Duration(1<<attempt) * time.Second
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// isRetryableFetchError reports whether err looks like a
// transient failure worth retrying. Conservative whitelist: only
// known-transient signatures count, everything else fails fast so
// a real 404 doesn't sit through three sleeps.
//
// Retryable:
//   - net.OpError "connection reset by peer", "broken pipe", EOF
//   - context.DeadlineExceeded — server too slow, try again
//   - HTTPError 429 (rate limit) or 5xx
//
// Not retryable:
//   - HTTPError 4xx (except 429) — stable; bad URL or auth
//   - JSON / protocol errors — body is well-formed but wrong
//   - context.Canceled — caller asked to stop
func isRetryableFetchError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var herr *HTTPError
	if errors.As(err, &herr) {
		if herr.Status == http.StatusTooManyRequests {
			return true
		}
		if herr.Status >= 500 && herr.Status < 600 {
			return true
		}
		return false
	}
	// net.OpError wraps the syscall-level error; check the
	// stringified form (cross-platform — syscall.ECONNRESET works
	// on linux/darwin but not all targets, so substring-match the
	// message text the way curl + every other HTTP client does).
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection reset by peer"):
		return true
	case strings.Contains(msg, "broken pipe"):
		return true
	case strings.Contains(msg, "EOF"):
		return true
	case strings.Contains(msg, "i/o timeout"):
		return true
	case strings.Contains(msg, "connection refused"):
		return true
	case strings.Contains(msg, "no such host"):
		// DNS flake — retry; if the host truly doesn't exist the
		// retries are cheap and consistent failure becomes a
		// stable error after the loop exits.
		return true
	}
	return false
}

// resolveAgainst joins an import specifier to its parent's URL.
//
//	parent="https://esm.sh/vue@3.4.21"  imp="./shared.js"
//	  →    "https://esm.sh/shared.js"
//
//	parent="https://esm.sh/vue@3.4.21"  imp="/v135/x/y.js"
//	  →    "https://esm.sh/v135/x/y.js"
//
//	parent="https://esm.sh/vue@3.4.21"  imp="lodash"
//	  →    "lodash"                          (bare spec — caller skips)
//
// Bare specs are returned as-is and the caller drops them — we
// only recurse into URLs we can reach via HTTP. esbuild's plugin
// handles the bare-spec case at bundle time by going through the
// lockfile.
func resolveAgainst(parentURL, imp string) (string, error) {
	if strings.HasPrefix(imp, "http://") || strings.HasPrefix(imp, "https://") {
		return imp, nil
	}
	if strings.HasPrefix(imp, ".") || strings.HasPrefix(imp, "/") {
		base, err := url.Parse(parentURL)
		if err != nil {
			return "", err
		}
		rel, err := url.Parse(imp)
		if err != nil {
			return "", err
		}
		return base.ResolveReference(rel).String(), nil
	}
	// Bare spec — return unchanged so caller can skip it.
	return imp, nil
}

// parseSpec splits "<name>" or "<name>@<version>" into (name, ver).
// Handles scoped names: "@vue/x@3.4.21" → ("@vue/x", "3.4.21").
func parseSpec(spec string) (name, version string) {
	if i := strings.LastIndex(spec, "@"); i > 0 {
		return spec[:i], spec[i+1:]
	}
	return spec, ""
}

// canonicalizeResolvedURL rewrites an unversioned esm.sh URL like
// "https://esm.sh/vue?external=vue" into its versioned form using
// the X-ESM-Path response header, e.g. "/vue@3.5.34/es2022/vue.mjs"
// → "https://esm.sh/vue@3.5.34?external=vue". The query string is
// preserved verbatim so the canonical URL still hits the right
// content variant (external=, target=, etc.) on subsequent installs.
//
// Returns "" when esmPath doesn't follow the expected esm.sh shape
// (leading "/<pkg>@<version>/...") so callers fall back to the
// uncanonicalized URL — better to keep going than to corrupt the
// lockfile with a guess. Scoped packages are recognized via the
// "@" prefix; their canonical key includes the scope segment too.
func canonicalizeResolvedURL(resolved, esmPath string) string {
	canon := canonicalSpecFromESMPath(esmPath)
	if canon == "" {
		return ""
	}
	u, err := url.Parse(resolved)
	if err != nil {
		return ""
	}
	u.Path = "/" + canon
	return u.String()
}

// canonicalSpecFromESMPath extracts the "<pkg>@<version>" prefix from
// an X-ESM-Path value. Two shapes:
//
//	"/vue@3.5.34/es2022/vue.mjs"           → "vue@3.5.34"
//	"/@vue/runtime-dom@3.5.34/es2022/..."  → "@vue/runtime-dom@3.5.34"
//
// Returns "" when the path doesn't contain a versioned leading
// package segment — a sign that the registry isn't esm.sh (or that
// its X-ESM-Path conventions have changed and we should fall back
// to the URL-as-is).
func canonicalSpecFromESMPath(esmPath string) string {
	trimmed := strings.TrimPrefix(esmPath, "/")
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "@") {
		// Scoped: "@scope/name@version/..." → leading two slash-
		// separated segments belong to the spec.
		parts := strings.SplitN(trimmed, "/", 3)
		if len(parts) < 2 {
			return ""
		}
		head := parts[0] + "/" + parts[1]
		if !strings.Contains(parts[1], "@") {
			return ""
		}
		return head
	}
	// Unscoped: "name@version/..." → first slash-separated segment.
	first := trimmed
	if i := strings.Index(trimmed, "/"); i >= 0 {
		first = trimmed[:i]
	}
	if !strings.Contains(first, "@") {
		return ""
	}
	return first
}

// extractVersionFromURL pulls the version out of a resolved URL
// like "https://esm.sh/vue@3.4.21" → "3.4.21". Returns "" when the
// URL has no @<ver> segment (rare; would mean the registry didn't
// version-pin via redirect).
//
// The "@" search starts after the last "/" so a scoped name in the
// URL path (e.g. "@vue/runtime-dom@3.4.21") doesn't confuse the
// split — we look for "@version" specifically.
func extractVersionFromURL(u string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return ""
	}
	path := parsed.Path
	// Walk segments right-to-left; the first "name@version" is
	// our pin. For a scoped package URL "/@vue/runtime-dom@3.4.21"
	// the last segment is "runtime-dom@3.4.21".
	segs := strings.Split(path, "/")
	for i := len(segs) - 1; i >= 0; i-- {
		seg := segs[i]
		if seg == "" || strings.HasPrefix(seg, "@") {
			continue
		}
		if at := strings.Index(seg, "@"); at >= 0 {
			return seg[at+1:]
		}
	}
	return ""
}

// isCSSContent decides whether a body should be scanned for CSS
// imports (@import + url(...)) by the recursive fetcher. Symmetric
// with isJSContent — Content-Type leads, extension is the fallback
// for CDNs that mislabel.
func isCSSContent(contentType, urlString string) bool {
	if strings.Contains(strings.ToLower(contentType), "css") {
		return true
	}
	parsed, err := url.Parse(urlString)
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(parsed.Path), ".css")
}

// isJSContent decides whether to parse a fetched body for imports.
// Content-Type is the primary signal; falls back to the URL's path
// extension when the registry returned a vague type
// ("application/octet-stream").
//
// We treat as JS:  application/javascript, text/javascript,
// application/typescript, .js/.mjs/.ts/.tsx/.jsx
//
// We skip:  CSS, source maps, JSON (esm.sh sometimes serves
// import-maps as JSON), and anything not in the above list.
func isJSContent(contentType, urlString string) bool {
	ct := strings.ToLower(contentType)
	switch {
	case strings.Contains(ct, "javascript"),
		strings.Contains(ct, "typescript"),
		strings.Contains(ct, "ecmascript"):
		return true
	case strings.Contains(ct, "css"),
		strings.Contains(ct, "json"),
		strings.Contains(ct, "octet-stream"):
		// Fall through to extension sniff for octet-stream.
		if !strings.Contains(ct, "octet-stream") {
			return false
		}
	}
	parsed, err := url.Parse(urlString)
	if err != nil {
		return false
	}
	p := strings.ToLower(parsed.Path)
	for _, ext := range []string{".js", ".mjs", ".ts", ".tsx", ".jsx"} {
		if strings.HasSuffix(p, ext) {
			return true
		}
	}
	return false
}

// appendUnique appends s to xs only if it isn't already there.
// Used to build deps lists without dupes. O(n) per call; n is
// per-package import count (single digits in practice).
func appendUnique(xs []string, s string) []string {
	for _, x := range xs {
		if x == s {
			return xs
		}
	}
	return append(xs, s)
}

// readFile is a thin wrapper over os.ReadFile that the test can
// stub if needed; today it just delegates.
func readFile(path string) ([]byte, error) {
	return readFileImpl(path)
}

var readFileImpl = osReadFile

// --- HTTP error -----------------------------------------------------

// HTTPError is the typed error returned for non-2xx responses.
// Carries enough detail (URL + status) for diagnosis without a
// stack trace.
type HTTPError struct {
	URL    string
	Status int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("fetcher: %s returned HTTP %d", e.URL, e.Status)
}

// IntegrityHex strips the "sha256-" prefix from a lockfile
// integrity string. Helper for callers handing the bare hex to
// store.Put.
func IntegrityHex(integrity string) string {
	if strings.HasPrefix(integrity, "sha256-") {
		s := integrity[len("sha256-"):]
		// Sanity: must be 64 hex chars. If not, return empty so
		// store.Put treats it as "no expected hash" rather than
		// always-failing on a malformed pin.
		if len(s) == 64 {
			if _, err := hex.DecodeString(s); err == nil {
				return s
			}
		}
	}
	return ""
}


// logProgress prints a single line to f.Progress for each fetched
// blob — used by the CLI to show "what's happening" during the
// transitive recursion. Operators see one line per HTTP request
// instead of staring at silence while vue's 35+ subpackages
// download.
//
// Verb is the leading icon + word ("↓ fetched" / "✓ cached").
// Pretty-prints the URL by trimming the registry prefix, and
// shows size in human-readable units. duration=0 means no
// network round-trip (cache hit) so we omit the timing.
//
// nil Progress writer = silent (legacy behavior). The CLI sets
// it; library callers default to no output.
func (f *Fetcher) logProgress(verb, resolved string, size int, duration time.Duration) {
	if f.Progress == nil {
		return
	}
	displayURL := resolved
	if f.Registry != "" {
		displayURL = strings.TrimPrefix(displayURL, f.Registry)
		displayURL = strings.TrimPrefix(displayURL, "/")
	}
	// Truncate very long display URLs so the line doesn't wrap on
	// 80-col terminals. The full URL goes to the lockfile anyway.
	if len(displayURL) > 60 {
		displayURL = "…" + displayURL[len(displayURL)-59:]
	}
	if duration > 0 {
		fmt.Fprintf(f.Progress, "  %s  %-60s  %s  %dms\n",
			verb, displayURL, humanBytes(size), duration.Milliseconds())
	} else {
		fmt.Fprintf(f.Progress, "  %s  %-60s  %s\n",
			verb, displayURL, humanBytes(size))
	}
}

// humanBytes formats a byte count as a tight 7-char field. KB / MB
// (decimal) match what `ls -lh` and most package managers use.
func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%5.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%5.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%5d  B", n)
	}
}

// discoverPackageEntry handles the "esm.sh 404s on a bare spec
// because the package has no JS to serve" case (canonical
// example: @mdi/font ships only CSS + woff2 files). Fetches the
// package's package.json from the registry, inspects it for
// asset-entry fields, and returns a file-path spec the caller
// can retry with.
//
// Inspection order (first match wins):
//
//   1. `style`             ← npm convention for CSS-only packages
//   2. `exports["./style"]` (string OR { default / require / etc. })
//   3. `exports["."].style`
//   4. `main`              ← when it ends in .css (rare; some
//                            ancient packages put the CSS path
//                            there before `style` was a thing)
//
// resolvedURL must be the post-redirect URL from the failing
// resolve() — esm.sh's redirect-to-versioned step is what gives
// us `@mdi/font@7.4.47?external=...` to extract the canonical
// `<scope>/<pkg>@<version>` from. Returns ("", nil) when the
// package has no recognizable asset entry — caller propagates
// the original 404 unchanged.
//
// All work is best-effort: network failure on the package.json
// fetch returns ("", nil) too. The original 404 is more
// actionable to operators than a "tried to fall back but
// couldn't" error.
func (f *Fetcher) discoverPackageEntry(ctx context.Context, resolvedURL string) (string, error) {
	canonical := canonicalSpecFromResolvedURL(resolvedURL)
	if canonical == "" {
		return "", nil
	}
	pkgJSONURL := f.Registry + "/" + canonical + "/package.json"
	body, err := f.get(ctx, pkgJSONURL)
	if err != nil {
		// Best-effort — don't surface the secondary error.
		return "", nil
	}
	entry := pickAssetEntry(body)
	if entry == "" {
		return "", nil
	}
	// Trim leading "./" so the composed spec doesn't carry the
	// relative-path prefix into the registry URL.
	entry = strings.TrimPrefix(entry, "./")
	return canonical + "/" + entry, nil
}

// canonicalSpecFromResolvedURL extracts the bare
// `<pkg>@<version>` from a resolved esm.sh URL. Mirrors
// canonicalSpecFromESMPath but operates on the absolute URL
// form (which is what HTTPError carries after a 4xx response).
//
//	"https://esm.sh/@mdi/font@7.4.47?external=vue"     → "@mdi/font@7.4.47"
//	"https://esm.sh/vue@3.5.34/es2022/vue.mjs"          → "vue@3.5.34"
//	"https://esm.sh/something-without-version"          → ""
func canonicalSpecFromResolvedURL(resolvedURL string) string {
	u, err := url.Parse(resolvedURL)
	if err != nil {
		return ""
	}
	// Path always starts with "/". Strip it.
	p := strings.TrimPrefix(u.Path, "/")
	// Find the first "@" in the path. For scoped packages
	// ("@mdi/font@7.4.47/...") the package name starts with "@"
	// so we look for the SECOND "@". For unscoped packages
	// ("vue@3.5.34/...") it's the first.
	startAt := 0
	if strings.HasPrefix(p, "@") {
		startAt = 1
	}
	atIdx := strings.IndexByte(p[startAt:], '@')
	if atIdx < 0 {
		return "" // no version embedded
	}
	atIdx += startAt
	// Find the end of the version (next "/" or end of string).
	endIdx := strings.IndexByte(p[atIdx:], '/')
	if endIdx < 0 {
		return p
	}
	return p[:atIdx+endIdx]
}

// pickAssetEntry parses a package.json body and returns the
// most-likely CSS/asset entry path. Returns "" when nothing
// recognizable is found.
//
// We do JSON decode → map walk rather than full struct binding
// because the `exports` field has wildly variable shapes
// (string / map / nested-map) across npm packages, and a typed
// struct would need a union type Go doesn't natively have.
func pickAssetEntry(body []byte) string {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return ""
	}
	// 1. Top-level `style` — the strong convention for CSS-only
	//    packages.
	if s, ok := raw["style"].(string); ok && s != "" {
		return s
	}
	// 2. exports["./style"] in its various shapes.
	if exports, ok := raw["exports"].(map[string]any); ok {
		if entry := readExportEntry(exports["./style"]); entry != "" {
			return entry
		}
		// 3. exports["."]["style"]
		if dot, ok := exports["."].(map[string]any); ok {
			if entry := readExportEntry(dot["style"]); entry != "" {
				return entry
			}
		}
	}
	// 4. `main` if it ends in .css.
	if m, ok := raw["main"].(string); ok && strings.HasSuffix(m, ".css") {
		return m
	}
	return ""
}

// readExportEntry handles the polymorphic shapes the `exports`
// field uses. Accepts a plain string OR a conditional-exports
// map (`{ "default": "...", "require": "...", ... }`).
func readExportEntry(v any) string {
	switch val := v.(type) {
	case string:
		return val
	case map[string]any:
		// Prefer `default`; fall back to any string-valued entry.
		if s, ok := val["default"].(string); ok && s != "" {
			return s
		}
		for _, x := range val {
			if s, ok := x.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}
