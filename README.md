# viteless

A zero-Node, native-ESM dev server for Go web frameworks — the dev-time
architecture [Vite](https://vitejs.dev) uses, reimplemented in Go.

In production you bundle. In development, **viteless serves each source
file at its own URL**, transformed on request, with its `import`
specifiers rewritten to browser-resolvable URLs. The browser then walks
the import graph natively. Because every dependency resolves to **one
URL**, all consumers share one module instance for free — which is what
makes Vite-style hot module replacement (re-import a module URL, swap it)
work without bridges, global shims, or instance-sharing hacks.

It is **dependency-free**: viteless owns HTTP serving, import rewriting,
and the HMR protocol; the embedding framework plugs in transform +
resolve via a small `Host` interface. No esbuild, no compiler, no
resolver baked in — and no Node anywhere.

## Install

```
go get github.com/paulmanoni/viteless
```

## Use

Implement `Host` and mount the handler:

```go
type myHost struct{ /* esbuild transform, SFC compiler, dep resolver */ }

func (h *myHost) LoadModule(urlPath string) (src []byte, kind string, ok bool) { /* … */ }
func (h *myHost) Transform(urlPath string, src []byte) ([]byte, error)         { /* … */ }
func (h *myHost) ResolveImport(spec string, kind viteless.SpecKind, importer string) string { /* … */ }

srv := viteless.NewServer(&myHost{})
http.Handle("/", srv.Handler())

// On file change, tell the browser to hot-update the module:
srv.HMR().Broadcast(viteless.Update{Type: "update", Path: "/src/App.vue"})
```

Inject the client runtime into your dev HTML:

```html
<script type="module" src="/@viteless/client.js"></script>
```

## What it handles

- **JS/TS/Vue modules** — transformed by the host, then imports rewritten
  to URLs (`import { x } from "vue"` → `import { x } from "/@id/vue"`).
- **CSS** — served as a JS module that injects a `<style>` and
  self-accepts HMR (edit CSS → swap in place, no reload).
- **Assets** (png/svg/fonts/…) — served as a `default`-exported URL.
- **HMR** — SSE channel + a tiny `import.meta.hot` client runtime
  (`accept` / `dispose` / `invalidate`).

The import rewriter recognizes static, side-effect, re-export, and
dynamic `import()` forms, and ignores `import`/`from` inside comments,
strings, and larger identifiers.

## Status

Proof of life: the import rewriter, module server, CSS/asset handling,
and HMR channel are implemented and unit-tested (no browser required).
Browser-level HMR application is wired (client runtime) and ready for an
end-to-end host integration.

## License

MIT
