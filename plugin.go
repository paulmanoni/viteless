package viteless

import "fmt"

// Plugin is the minimal plugin contract — a name. A plugin opts into the
// pipeline by ALSO implementing one or more of the optional capability
// interfaces below, mirroring Vite/Rollup's plugin hooks. The same Plugin
// runs in both the dev server and the production build.
type Plugin interface {
	Name() string
}

// PrePostPlugin lets a plugin run before ("pre") or after ("post") the
// normal plugins. Return "pre", "post", or "" (normal). Mirrors Vite's
// plugin `enforce`.
type PrePostPlugin interface {
	Enforce() string
}

// ConfigPlugin mutates resolved configuration before the server/build
// starts (Vite's config / configResolved).
type ConfigPlugin interface {
	Config(*ResolvedConfig)
}

// ResolveIdPlugin maps an import specifier to a module id (a served URL in
// viteless). ok=false passes to the next plugin, then the default
// resolver. To create a virtual module, return a server-absolute id like
// "/@id/virtual:foo" and serve it from a LoadPlugin.
type ResolveIdPlugin interface {
	ResolveId(spec, importer string) (id string, ok bool)
}

// LoadPlugin returns the source for a module id (e.g. a virtual module).
// ok=false passes through to the next plugin, then the filesystem.
type LoadPlugin interface {
	Load(id string) (code string, ok bool, err error)
}

// TransformPlugin rewrites a module's code. changed=false leaves the input
// untouched. Runs on code modules (js/ts/vue→js and css) in dev, and via
// the esbuild bridge in build.
type TransformPlugin interface {
	Transform(code, id string) (out string, changed bool, err error)
}

// HTMLPlugin rewrites the HTML shell (Vite's transformIndexHtml).
type HTMLPlugin interface {
	TransformIndexHtml(html string) (string, error)
}

// HotUpdatePlugin customizes HMR for a changed file. Returning a non-nil
// slice overrides the default update set; nil defers to the default.
type HotUpdatePlugin interface {
	HandleHotUpdate(*HotUpdate) []Update
}

// ResolvedConfig is the configuration surface exposed to Config hooks.
type ResolvedConfig struct {
	Root string
	Mode string
	Env  map[string]string
}

// HotUpdate describes a changed file passed to HandleHotUpdate hooks.
type HotUpdate struct {
	File string // absolute filesystem path
	Path string // served URL path
	Type string // "update" | "css"
}

// pluginContainer holds the ordered plugin set and runs the hook chains.
// It is shared by the dev server and the build bridge so a plugin behaves
// identically in both. A nil *pluginContainer is a valid no-op.
type pluginContainer struct {
	plugins []Plugin
}

// newPluginContainer orders plugins by enforce (pre → normal → post) and
// returns nil when there are none.
func newPluginContainer(plugins []Plugin) *pluginContainer {
	plugins = orderPlugins(plugins)
	if len(plugins) == 0 {
		return nil
	}
	return &pluginContainer{plugins: plugins}
}

func orderPlugins(in []Plugin) []Plugin {
	var pre, normal, post []Plugin
	for _, p := range in {
		if p == nil {
			continue
		}
		switch enforceOf(p) {
		case "pre":
			pre = append(pre, p)
		case "post":
			post = append(post, p)
		default:
			normal = append(normal, p)
		}
	}
	out := make([]Plugin, 0, len(pre)+len(normal)+len(post))
	out = append(out, pre...)
	out = append(out, normal...)
	return append(out, post...)
}

func enforceOf(p Plugin) string {
	if e, ok := p.(PrePostPlugin); ok {
		return e.Enforce()
	}
	return ""
}

// applyConfig runs every Config hook against cfg.
func (c *pluginContainer) applyConfig(cfg *ResolvedConfig) {
	if c == nil {
		return
	}
	for _, p := range c.plugins {
		if cp, ok := p.(ConfigPlugin); ok {
			cp.Config(cfg)
		}
	}
}

// resolveId returns the first plugin id for spec, if any.
func (c *pluginContainer) resolveId(spec, importer string) (string, bool) {
	if c == nil {
		return "", false
	}
	for _, p := range c.plugins {
		if rp, ok := p.(ResolveIdPlugin); ok {
			if id, ok := rp.ResolveId(spec, importer); ok {
				return id, true
			}
		}
	}
	return "", false
}

// load returns the first plugin source for id, if any.
func (c *pluginContainer) load(id string) (string, bool, error) {
	if c == nil {
		return "", false, nil
	}
	for _, p := range c.plugins {
		if lp, ok := p.(LoadPlugin); ok {
			code, ok, err := lp.Load(id)
			if err != nil {
				return "", false, fmt.Errorf("plugin %s: load %s: %w", p.Name(), id, err)
			}
			if ok {
				return code, true, nil
			}
		}
	}
	return "", false, nil
}

// transform folds code through every transform hook in order.
func (c *pluginContainer) transform(code, id string) (string, error) {
	if c == nil {
		return code, nil
	}
	cur := code
	for _, p := range c.plugins {
		if tp, ok := p.(TransformPlugin); ok {
			out, changed, err := tp.Transform(cur, id)
			if err != nil {
				return "", fmt.Errorf("plugin %s: transform %s: %w", p.Name(), id, err)
			}
			if changed {
				cur = out
			}
		}
	}
	return cur, nil
}

// transformIndexHtml folds html through every HTML hook in order.
func (c *pluginContainer) transformIndexHtml(html string) (string, error) {
	if c == nil {
		return html, nil
	}
	cur := html
	for _, p := range c.plugins {
		if hp, ok := p.(HTMLPlugin); ok {
			out, err := hp.TransformIndexHtml(cur)
			if err != nil {
				return "", fmt.Errorf("plugin %s: transformIndexHtml: %w", p.Name(), err)
			}
			cur = out
		}
	}
	return cur, nil
}

// handleHotUpdate returns the first plugin's HMR override for u, if any.
func (c *pluginContainer) handleHotUpdate(u *HotUpdate) ([]Update, bool) {
	if c == nil {
		return nil, false
	}
	for _, p := range c.plugins {
		if hp, ok := p.(HotUpdatePlugin); ok {
			if ups := hp.HandleHotUpdate(u); ups != nil {
				return ups, true
			}
		}
	}
	return nil, false
}
