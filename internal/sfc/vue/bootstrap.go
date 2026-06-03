package vue

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/evanw/esbuild/pkg/api"

	"github.com/paulmanoni/viteless/internal/fetcher"
	"github.com/paulmanoni/viteless/internal/lockfile"
	"github.com/paulmanoni/viteless/internal/resolver"
	"github.com/paulmanoni/viteless/internal/store"
)

// adapterTag is a short content hash of the embedded adapter.js,
// mixed into the cached-bundle path so editing the adapter
// invalidates stale bundles. Without this the cache keys on the
// compiler version alone, and an adapter change silently reuses an
// old bundle.
func adapterTag() string {
	sum := sha256.Sum256([]byte(adapterJS))
	return hex.EncodeToString(sum[:6])
}

// DefaultCompilerVersion is the @vue/compiler-sfc version Bootstrap
// pins by default. Bumped per release; users override via
// BootstrapOptions.Version. We pin a known-good version rather than
// resolving "latest" so a fresh clone produces deterministic builds
// matching whatever the project's nexus.lock says.
const DefaultCompilerVersion = "3.4.21"

// adapterJS is the JavaScript bridge between compile.go and
// @vue/compiler-sfc. Embedded so the binary is fully self-
// contained: a fresh `nexus build` doesn't need a network hop
// to fetch the adapter source. The compiler bundle itself is
// still fetched once + cached.
//
//go:embed adapter.js
var adapterJS string

// BootstrapOptions configures the one-time compiler-bundle build.
type BootstrapOptions struct {
	// Store is the disk cache the fetcher + resolver share.
	// Required.
	Store *store.Store

	// Fetcher pulls @vue/compiler-sfc from the registry the user
	// configured (NEXUS_REGISTRY). Required.
	Fetcher *fetcher.Fetcher

	// Version is the @vue/compiler-sfc version to pin. Defaults
	// to DefaultCompilerVersion. Pass the exact "3.4.21" shape;
	// no `^` or `~` range support in v0.1.
	Version string

	// BundleDir is the directory the produced compiler bundle is
	// cached in. Defaults to filepath.Join(store.Root(),
	// "sfc-vue") so the bundle sits alongside the rest of the
	// content-addressed cache. Tests pass a t.TempDir().
	BundleDir string
}

// Bootstrap materializes @vue/compiler-sfc into a single self-
// contained JS bundle the QuickJS runtime can load. Performs the
// network fetch + esbuild bundle the first time it's called for
// a given version; subsequent calls for the same version short-
// circuit to the cached bundle.
//
// Lifecycle:
//
//  1. Check <BundleDir>/<version>/compiler.bundle.js — if
//     present, read + return its bytes.
//  2. Fetch @vue/compiler-sfc@<version> via the supplied
//     Fetcher (recursive — populates the store with everything
//     compiler-sfc transitively imports).
//  3. Build an in-memory lockfile from the fetch result.
//  4. Synthesize an esbuild Build with the embedded adapter.js
//     as stdin + the resolver plugin pointing at the in-memory
//     lockfile.
//  5. esbuild bundles everything into one IIFE; write to disk;
//     return bytes.
//
// First call typically takes 1-5 seconds (network + bundle);
// cached calls return in microseconds.
//
// Unlike the original Goja-targeted bootstrap, we do NOT append
// ?target=es2015 to fetched URLs. QuickJS supports async
// generators natively, so the unmodified esm.sh bytes work fine
// and the @babel/parser feature-detection that broke Goja
// resolves correctly.
func Bootstrap(ctx context.Context, opts BootstrapOptions) ([]byte, error) {
	if opts.Store == nil {
		return nil, errors.New("vue: Bootstrap requires Store")
	}
	if opts.Fetcher == nil {
		return nil, errors.New("vue: Bootstrap requires Fetcher")
	}
	version := opts.Version
	if version == "" {
		version = DefaultCompilerVersion
	}
	bundleDir := opts.BundleDir
	if bundleDir == "" {
		bundleDir = filepath.Join(opts.Store.Root(), "sfc-vue")
	}
	cachedPath := filepath.Join(bundleDir, version+"-"+adapterTag(), "compiler.bundle.js")

	// Cache hit fast path.
	if b, err := os.ReadFile(cachedPath); err == nil && len(b) > 0 {
		return b, nil
	}

	// Cache miss: fetch + bundle.
	bundle, err := buildCompilerBundle(ctx, opts.Fetcher, opts.Store, version)
	if err != nil {
		return nil, fmt.Errorf("vue: bootstrap: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(cachedPath), 0o755); err != nil {
		return nil, fmt.Errorf("vue: mkdir bundle dir: %w", err)
	}
	if err := os.WriteFile(cachedPath, bundle, 0o644); err != nil {
		return nil, fmt.Errorf("vue: write cached bundle: %w", err)
	}
	return bundle, nil
}

// buildCompilerBundle does the actual fetch + bundle work. Split
// from Bootstrap so the cache-hit branch is trivially obvious in
// the public function.
func buildCompilerBundle(ctx context.Context, f *fetcher.Fetcher, s *store.Store, version string) ([]byte, error) {
	spec := "@vue/compiler-sfc@" + version

	// Fetch + recurse. Populates the store with every file
	// compiler-sfc transitively imports. No URL-query mutation
	// needed since QuickJS handles modern JS natively.
	res, err := f.Fetch(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", spec, err)
	}

	// Build an in-memory lockfile from the fetch result. The
	// resolver plugin reads it during the bundle to translate
	// bare imports → cached blob paths.
	lf := lockfile.New()
	lf.Add(res.Root)
	for _, dep := range res.Transitive {
		lf.Add(dep)
	}

	plugin, err := resolver.New(resolver.Options{Lockfile: lf, Store: s})
	if err != nil {
		return nil, fmt.Errorf("build resolver: %w", err)
	}

	cwd, _ := os.Getwd()
	result := api.Build(api.BuildOptions{
		Stdin: &api.StdinOptions{
			Contents:   adapterJS,
			ResolveDir: cwd,
			Sourcefile: "nexus-vue-adapter.js",
			Loader:     api.LoaderJS,
		},
		Bundle:           true,
		Write:            false,
		Format:           api.FormatIIFE,
		Target:           api.ES2022,
		Plugins:          []api.Plugin{plugin},
		LogLevel:         api.LogLevelSilent,
		MinifyWhitespace: true,
		Define: map[string]string{
			"process.env.NODE_ENV": `"production"`,
		},
	})
	if len(result.Errors) > 0 {
		return nil, fmt.Errorf("esbuild: %s", result.Errors[0].Text)
	}
	if len(result.OutputFiles) == 0 {
		return nil, errors.New("esbuild produced no output")
	}
	return result.OutputFiles[0].Contents, nil
}
