package viteless

import (
	"fmt"
	"net/http"
	"path"
	"strings"
)

// Host is everything viteless needs from the embedding framework. Keeping
// it an interface means viteless has ZERO build dependencies (no esbuild,
// no SFC compiler, no dep resolver) — the host plugs those in. nexus
// implements this with its esbuild transform + Vue SFC compiler + the
// nexus.lock/.nexus-cache resolver.
type Host interface {
	// LoadModule returns the SOURCE bytes for a served URL path and a
	// content kind ("js", "css", "asset"). path is the URL path the
	// browser requested (e.g. "/src/App.vue", "/@id/vue"). ok=false →
	// 404. For "js", viteless will rewrite the module's imports; "css"
	// is wrapped into a style-injecting JS module; "asset" is served as
	// a URL string export.
	LoadModule(urlPath string) (src []byte, kind string, ok bool)

	// Transform compiles one source file to browser JS. For .vue it runs
	// the SFC compiler; for .ts/.tsx it strips types; for .js it may be a
	// near-passthrough. Returns the compiled JS. The caller has already
	// decided this is a "js" module.
	Transform(urlPath string, src []byte) ([]byte, error)

	// ResolveImport maps an import specifier found in a module to the URL
	// the browser should fetch (the ResolveFunc contract). Returning ""
	// leaves the specifier untouched.
	ResolveImport(spec string, kind SpecKind, importerURL string) string
}

// Server is the unbundled dev module server. It serves transformed,
// import-rewritten ES modules over HTTP so the browser walks the import
// graph natively — the Vite dev model, zero Node.
type Server struct {
	host Host
	// hmr is the live-reload / hot-update channel (SSE), shared with the
	// client runtime. Optional; nil disables HMR endpoints.
	hmr *HMR
}

// NewServer builds a dev server backed by host.
func NewServer(host Host) *Server {
	return &Server{host: host, hmr: NewHMR()}
}

// HMR returns the server's hot-module channel so the host can broadcast
// updates when files change.
func (s *Server) HMR() *HMR { return s.hmr }

// Handler returns the http.Handler serving modules + the HMR client +
// the HMR event stream. Mount it under the dev origin.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/@viteless/client.js", s.handleClient)
	mux.HandleFunc("/@viteless/hmr", s.handleHMR)
	mux.HandleFunc("/", s.handleModule)
	return mux
}

// handleModule is the core: load → transform (if JS) → rewrite imports →
// serve as an ES module. CSS and assets are wrapped into JS modules so a
// `import "./x.css"` / `import logo from "./x.png"` works natively.
func (s *Server) handleModule(w http.ResponseWriter, r *http.Request) {
	urlPath := r.URL.Path
	src, kind, ok := s.host.LoadModule(urlPath)
	if !ok {
		http.NotFound(w, r)
		return
	}

	switch kind {
	case "css":
		// Serve CSS as a JS module that injects a <style> on import and
		// removes it on hot-dispose — Vite's css-as-module behaviour.
		js := cssModule(urlPath, string(src))
		writeJS(w, []byte(js))
		return
	case "asset":
		// Asset → default-export its own URL so `import u from "x.png"`
		// binds to the fetchable path. The browser already has the bytes
		// at this URL; the module just hands back the string.
		js := fmt.Sprintf("export default %q;\n", urlPath)
		writeJS(w, []byte(js))
		return
	}

	// JS/TS/Vue: compile then rewrite imports to URLs.
	compiled, err := s.host.Transform(urlPath, src)
	if err != nil {
		// Surface the compile error to the browser overlay as a thrown
		// module so the dev sees it instead of a blank screen.
		writeJS(w, []byte(errorModule(urlPath, err.Error())))
		return
	}
	out := RewriteImports(string(compiled), urlPath, ClassifySpec, s.host.ResolveImport)
	writeJS(w, []byte(out))
}

func (s *Server) handleClient(w http.ResponseWriter, r *http.Request) {
	writeJS(w, []byte(clientRuntimeJS))
}

func (s *Server) handleHMR(w http.ResponseWriter, r *http.Request) {
	if s.hmr == nil {
		http.NotFound(w, r)
		return
	}
	s.hmr.ServeSSE(w, r)
}

func writeJS(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

// cssModule wraps CSS text into a JS module that injects + hot-disposes a
// <style data-viteless="<id>">. Keyed by the module URL so a hot update
// replaces rather than stacks.
func cssModule(urlPath, css string) string {
	id := styleID(urlPath)
	var b strings.Builder
	fmt.Fprintf(&b, "const __css = %s;\n", jsString(css))
	fmt.Fprintf(&b, "const __id = %q;\n", id)
	b.WriteString(`let __el = document.querySelector('style[data-viteless="'+__id+'"]');` + "\n")
	b.WriteString("if (!__el) { __el = document.createElement('style'); __el.setAttribute('data-viteless', __id); document.head.appendChild(__el); }\n")
	b.WriteString("__el.textContent = __css;\n")
	b.WriteString("if (import.meta.hot) { import.meta.hot.accept(); }\n")
	return b.String()
}

func errorModule(urlPath, msg string) string {
	return fmt.Sprintf("throw new Error(%s);\n", jsString("[viteless] transform failed for "+urlPath+": "+msg))
}

func styleID(urlPath string) string {
	// Stable, filesystem-safe id from the URL path.
	return strings.TrimPrefix(path.Clean(urlPath), "/")
}

// jsString quotes s as a valid JS double-quoted string literal.
func jsString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
