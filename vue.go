package viteless

import (
	"context"
	"fmt"
	"runtime"

	"github.com/evanw/esbuild/pkg/api"

	"github.com/paulmanoni/viteless/internal/fetcher"
	"github.com/paulmanoni/viteless/internal/lockfile"
	"github.com/paulmanoni/viteless/internal/sfc/vue"
	"github.com/paulmanoni/viteless/internal/store"
)

// bootstrapVue fetches + bundles @vue/compiler-sfc into a self-contained
// IIFE (cached on disk) ready to instantiate one or more QuickJS-backed
// SFC compilers. Vue support requires the CDN store (the compiler bundle
// is fetched from the registry once and cached).
func bootstrapVue(st *store.Store, registry string) ([]byte, error) {
	if st == nil {
		return nil, fmt.Errorf("viteless: Vue support requires the CDN dep mode (no store available)")
	}
	if registry == "" {
		registry = fetcher.DefaultRegistry
	}
	bundle, err := vue.Bootstrap(context.Background(), vue.BootstrapOptions{
		Store:   st,
		Fetcher: fetcher.New(st, registry),
	})
	if err != nil {
		return nil, fmt.Errorf("viteless: bootstrap @vue/compiler-sfc: %w", err)
	}
	return bundle, nil
}

// newVueCompiler builds a single Vue SFC compiler — used by the dev Host,
// which serializes compiles behind one QuickJS context.
func newVueCompiler(st *store.Store, registry string) (vue.SFCCompiler, error) {
	bundle, err := bootstrapVue(st, registry)
	if err != nil {
		return nil, err
	}
	return vue.NewQJSCompiler(bundle, vue.DefaultCompilerVersion)
}

// vueBuildPlugin returns the esbuild plugin that compiles .vue SFCs for a
// production Build, backed by a pool of compilers so esbuild's parallel
// OnLoad dispatch isn't serialized on a single QuickJS context. The
// returned closer disposes the pool.
func vueBuildPlugin(lf *lockfile.File, st *store.Store, registry string) (*api.Plugin, func(), error) {
	_ = lf // resolution goes through the resolver plugin; bootstrap needs only the store
	bundle, err := bootstrapVue(st, registry)
	if err != nil {
		return nil, nil, err
	}
	size := runtime.GOMAXPROCS(0)
	if size > 4 {
		size = 4
	}
	pool, err := vue.NewPool(func() (vue.SFCCompiler, error) {
		return vue.NewQJSCompiler(bundle, vue.DefaultCompilerVersion)
	}, size)
	if err != nil {
		return nil, nil, fmt.Errorf("viteless: vue compiler pool: %w", err)
	}
	plugin, err := vue.Plugin(pool)
	if err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("viteless: vue plugin: %w", err)
	}
	return &plugin, pool.Close, nil
}
