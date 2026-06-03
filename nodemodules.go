package viteless

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/evanw/esbuild/pkg/api"
)

// nmPrefix is the URL namespace optimized node_modules dependencies are
// served under (Vite serves these from .vite/deps; viteless serves them
// from an on-disk cache via this prefix).
const nmPrefix = "/@nm/"

// nmOptimizer is the dev-mode node_modules dependency optimizer. Raw
// node_modules packages can't be served to a browser (CJS, deep internal
// requires), so — exactly like Vite's optimizeDeps — it esbuild-bundles
// every bare dependency the app imports into ESM. Crucially it bundles
// them TOGETHER with code-splitting on, so a dependency shared across
// packages (react, react-dom, react/jsx-runtime all need one react
// instance) is hoisted into a single shared chunk → one instance, no
// "multiple copies of React" / null hooks dispatcher.
type nmOptimizer struct {
	root     string // project root (esbuild's resolve base for node_modules)
	cacheDir string // where proxy entries + optimized output live
	devMode  bool
	logf     func(string, ...any)

	mu       sync.Mutex
	specs    map[string]bool // every bare import requested so far
	builtSig string          // signature of the spec set last built
}

func newNMOptimizer(root, cacheDir string, devMode bool, logf func(string, ...any)) *nmOptimizer {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &nmOptimizer{
		root:     root,
		cacheDir: cacheDir,
		devMode:  devMode,
		logf:     logf,
		specs:    map[string]bool{},
	}
}

var nmSanitizeRE = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// sanitizeSpec turns a bare import (pkg or pkg/subpath) into a safe,
// reversible-enough output basename.
func sanitizeSpec(s string) string {
	return strings.Trim(nmSanitizeRE.ReplaceAllString(s, "_"), "_")
}

// resolve records a bare spec as needed and returns the served URL its
// optimized bundle will live at. The actual (re)build happens lazily in
// load, so a burst of resolve calls during one module's import rewrite
// collapses into a single optimize pass.
func (o *nmOptimizer) resolve(spec string) string {
	o.mu.Lock()
	if !o.specs[spec] {
		o.specs[spec] = true
		o.builtSig = "" // mark dirty
	}
	o.mu.Unlock()
	return nmPrefix + sanitizeSpec(spec) + ".js"
}

// load serves an optimized entry or shared chunk under /@nm/, rebuilding
// the dependency bundle first if the requested spec set changed.
func (o *nmOptimizer) load(urlPath string) ([]byte, bool) {
	rel := strings.TrimPrefix(urlPath, nmPrefix)
	if i := strings.IndexByte(rel, '?'); i >= 0 {
		rel = rel[:i]
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.ensureBuilt(); err != nil {
		o.logf("viteless: node_modules optimize: %v", err)
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(o.cacheDir, "out", filepath.FromSlash(rel)))
	if err != nil {
		return nil, false
	}
	return b, true
}

// signature is a stable fingerprint of the current spec set.
func (o *nmOptimizer) signature() string {
	ss := make([]string, 0, len(o.specs))
	for s := range o.specs {
		ss = append(ss, s)
	}
	sort.Strings(ss)
	return strings.Join(ss, "\n")
}

// ensureBuilt (re)optimizes the whole dependency set when it has changed.
// Caller holds o.mu.
func (o *nmOptimizer) ensureBuilt() error {
	sig := o.signature()
	if sig == o.builtSig && sig != "" {
		return nil
	}
	if len(o.specs) == 0 {
		return nil
	}

	entriesDir := filepath.Join(o.cacheDir, "entries")
	if err := os.RemoveAll(entriesDir); err != nil {
		return err
	}
	if err := os.MkdirAll(entriesDir, 0o755); err != nil {
		return err
	}

	var entryPoints []api.EntryPoint
	for spec := range o.specs {
		name := sanitizeSpec(spec)
		// Re-export proxy: namespace + a default that is the real default
		// when present, else the namespace itself (CJS interop), so both
		// `import d from "pkg"` and `import { x } from "pkg"` work.
		proxy := fmt.Sprintf(
			"import * as _all from %q;\nexport * from %q;\nexport default (_all && _all.default !== undefined) ? _all.default : _all;\n",
			spec, spec,
		)
		p := filepath.Join(entriesDir, name+".js")
		if err := os.WriteFile(p, []byte(proxy), 0o644); err != nil {
			return err
		}
		entryPoints = append(entryPoints, api.EntryPoint{InputPath: p, OutputPath: name})
	}

	nodeEnv := "development"
	if !o.devMode {
		nodeEnv = "production"
	}
	r := api.Build(api.BuildOptions{
		EntryPointsAdvanced: entryPoints,
		Outdir:              filepath.Join(o.cacheDir, "out"),
		AbsWorkingDir:       o.root,
		// The proxy entry files live in the cache dir (outside the
		// project), so esbuild's importer-relative node_modules walk
		// wouldn't find the project's packages. NodePaths adds the
		// project's node_modules as an explicit resolution root.
		NodePaths: []string{filepath.Join(o.root, "node_modules")},
		Bundle:    true,
		Write:               true,
		Splitting:           true, // shared deps → one chunk → singletons preserved
		Format:              api.FormatESModule,
		Target:              api.ES2022,
		ChunkNames:          "chunks/[name]-[hash]",
		Define: map[string]string{
			"process.env.NODE_ENV": `"` + nodeEnv + `"`,
		},
		LogLevel: api.LogLevelSilent,
	})
	if len(r.Errors) > 0 {
		return fmt.Errorf("%s", r.Errors[0].Text)
	}
	o.builtSig = sig
	return nil
}
