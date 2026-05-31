package viteless

import (
	"fmt"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
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
	// content kind ("js", "css", "asset", "html"). path is the URL path
	// the browser requested (e.g. "/src/App.vue", "/@id/vue", "/"). ok=
	// false → the request falls through to the proxy (if configured) or
	// 404. For "js", viteless rewrites the module's imports; "css" is
	// wrapped into a style-injecting JS module; "asset" is served as a
	// URL-string export (or raw bytes when fetched with ?url); "html" is
	// transformed (HMR client injected) and served as text/html.
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
	// proxy forwards requests viteless can't serve itself (anything the
	// host's LoadModule declines) to the application backend — Vite's
	// server.proxy, so same-origin API calls (/graphql, /api/…) reach the
	// Go app while the SPA is served unbundled. nil → decline → 404.
	proxy *httputil.ReverseProxy
}

// Option configures a Server at construction.
type Option func(*Server)

// WithProxy forwards every request the host doesn't serve to target (the
// running application backend, e.g. "http://127.0.0.1:8080"). This keeps
// the browser on ONE origin: the SPA's modules come from viteless, the
// API calls reverse-proxy to the app. Invalid targets disable the proxy.
func WithProxy(target string) Option {
	return func(s *Server) {
		u, err := url.Parse(target)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return
		}
		s.proxy = httputil.NewSingleHostReverseProxy(u)
	}
}

// NewServer builds a dev server backed by host.
func NewServer(host Host, opts ...Option) *Server {
	s := &Server{host: host, hmr: NewHMR()}
	for _, o := range opts {
		o(s)
	}
	return s
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
// `import "./x.css"` / `import logo from "./x.png"` works natively. HTML
// is transformed (HMR client injected). Anything the host declines is
// proxied to the backend (if configured) or 404s.
//
// Query suffixes (Vite-compatible):
//
//	?raw  — return the file's raw source as a default-exported string
//	?url  — return the asset's RAW bytes (this is the fetch the default
//	        import's URL string points at)
func (s *Server) handleModule(w http.ResponseWriter, r *http.Request) {
	urlPath := r.URL.Path
	q := r.URL.Query()
	wantRaw := q.Has("raw")
	wantURL := q.Has("url")

	src, kind, ok := s.host.LoadModule(urlPath)
	if !ok {
		if s.proxy != nil {
			s.proxy.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}

	// ?raw — hand back the unprocessed source as a string module,
	// regardless of kind. Mirrors Vite's `import src from "./x.svg?raw"`.
	if wantRaw {
		writeJS(w, []byte("export default "+jsString(string(src))+";\n"))
		return
	}

	switch kind {
	case "html":
		out := s.TransformHTML(src)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(out)
		return
	case "css":
		// Serve CSS as a JS module that injects a <style> on import and
		// hot-accepts. The hot context preamble makes import.meta.hot
		// available so the self-accept actually registers.
		js := hotPreamble(urlPath) + cssModule(urlPath, string(src))
		writeJS(w, []byte(js))
		return
	case "asset":
		if wantURL {
			// The actual asset fetch: serve raw bytes with a sniffed
			// content type so <img src>, fonts, etc. render.
			serveRaw(w, urlPath, src)
			return
		}
		// Default import → a JS module exporting the asset's own ?url
		// path, so `import logo from "./x.png"` binds to a fetchable
		// URL that yields the bytes (the wantURL branch above).
		js := fmt.Sprintf("export default %q;\n", urlPath+"?url")
		writeJS(w, []byte(js))
		return
	}

	// JS/TS/Vue: compile then rewrite imports to URLs.
	compiled, err := s.host.Transform(urlPath, src)
	if err != nil {
		// Surface the compile error as an overlay module so the dev sees
		// it instead of a blank screen.
		writeJS(w, []byte(errorModule(urlPath, err.Error())))
		return
	}
	out := RewriteImports(string(compiled), urlPath, ClassifySpec, s.host.ResolveImport)
	writeJS(w, []byte(hotPreamble(urlPath)+out))
}

// TransformHTML prepares an index.html for the unbundled dev server: it
// injects the viteless HMR client as the FIRST module script so it runs
// before any app module evaluates (app modules carry an import.meta.hot
// preamble that calls into the client's __viteless_hot). The entry
// `<script type="module" src="/src/main.ts">` is left untouched — it's
// already a viteless-served URL.
func (s *Server) TransformHTML(html []byte) []byte {
	const clientTag = `<script type="module" src="/@viteless/client.js"></script>`
	h := string(html)
	// Insert before the first <script> so the client is guaranteed to
	// execute ahead of the entry module.
	if i := strings.Index(h, "<script"); i >= 0 {
		return []byte(h[:i] + clientTag + "\n  " + h[i:])
	}
	if i := strings.Index(h, "</head>"); i >= 0 {
		return []byte(h[:i] + clientTag + "\n" + h[i:])
	}
	if i := strings.Index(h, "</body>"); i >= 0 {
		return []byte(h[:i] + clientTag + "\n" + h[i:])
	}
	return []byte(clientTag + "\n" + h)
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

// serveRaw writes asset bytes with a content type sniffed from the URL
// extension (falling back to octet-stream). Used by the ?url fetch.
func serveRaw(w http.ResponseWriter, urlPath string, body []byte) {
	ct := mime.TypeByExtension(path.Ext(urlPath))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(body)
}

// hotPreamble is prepended to every served JS/CSS module so the standard
// import.meta.hot API is available. __viteless_hot is defined by the
// client runtime, which the transformed HTML loads first.
func hotPreamble(urlPath string) string {
	return "import.meta.hot = __viteless_hot(" + jsString(urlPath) + ");\n"
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

// errorModule renders a full-screen error overlay instead of leaving a
// blank page when a transform/compile fails. The overlay is dismissible
// and is cleared automatically by the HMR client on the next update.
func errorModule(urlPath, msg string) string {
	full := "[viteless] transform failed for " + urlPath + "\n\n" + msg
	var b strings.Builder
	b.WriteString("const __msg = " + jsString(full) + ";\n")
	b.WriteString("(function(){\n")
	b.WriteString("  function show(){\n")
	b.WriteString("    let el = document.getElementById('__viteless_error');\n")
	b.WriteString("    if(!el){ el = document.createElement('div'); el.id='__viteless_error';\n")
	b.WriteString("      el.style.cssText='position:fixed;inset:0;z-index:99999;background:rgba(0,0,0,.85);color:#ff8888;font:13px/1.5 ui-monospace,Menlo,monospace;padding:24px;white-space:pre-wrap;overflow:auto;cursor:pointer';\n")
	b.WriteString("      el.title='click to dismiss'; el.addEventListener('click',function(){el.remove();});\n")
	b.WriteString("      (document.body||document.documentElement).appendChild(el);\n")
	b.WriteString("    }\n")
	b.WriteString("    el.textContent = __msg;\n")
	b.WriteString("  }\n")
	b.WriteString("  if(document.body){show();}else{document.addEventListener('DOMContentLoaded',show);}\n")
	b.WriteString("})();\n")
	return b.String()
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
