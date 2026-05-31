package viteless

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// HMR is the hot-module channel: an SSE fanout the server uses to push
// update messages to connected browsers. The host broadcasts on file
// change; the injected client runtime applies them.
type HMR struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

// NewHMR builds an empty channel.
func NewHMR() *HMR {
	return &HMR{clients: make(map[chan []byte]struct{})}
}

// Update is a hot-update message sent to the browser.
//
//	{"type":"update","path":"/src/Foo.vue"}  → re-import the module URL
//	{"type":"css","path":"/src/Foo.css"}     → re-import the CSS module
//	{"type":"reload"}                        → full page reload
type Update struct {
	Type string `json:"type"`
	Path string `json:"path,omitempty"`
}

// Broadcast sends u to every connected client. Slow clients (full buffer)
// are skipped for this message — they'll catch the next, and a missed
// frame just means a momentarily stale module, not a broken loop.
func (h *HMR) Broadcast(u Update) {
	b, err := json.Marshal(u)
	if err != nil {
		return
	}
	h.mu.Lock()
	for ch := range h.clients {
		select {
		case ch <- b:
		default:
		}
	}
	h.mu.Unlock()
}

// Reload is a convenience for a full-page reload broadcast.
func (h *HMR) Reload() { h.Broadcast(Update{Type: "reload"}) }

// ServeSSE handles the browser's EventSource connection.
func (h *HMR) ServeSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan []byte, 16)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.clients, ch)
		h.mu.Unlock()
	}()

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
	}
}

// clientRuntimeJS is the browser-side HMR runtime, served at
// /@viteless/client.js and injected into the dev HTML. It connects to the
// SSE stream and applies updates by re-importing module URLs. A module
// opts into hot updates via the standard import.meta.hot API, which this
// runtime provides.
const clientRuntimeJS = `// viteless dev client
(function () {
  const registry = new Map(); // url -> { accept, dispose }
  // import.meta.hot is created per-module by the server-injected preamble
  // calling __viteless_hot(url). Kept minimal: accept + dispose.
  window.__viteless_hot = function (url) {
    let cb = null, disposeCb = null;
    registry.set(url, { get accept() { return cb; }, get dispose() { return disposeCb; } });
    return {
      accept(fn) { cb = fn || (() => {}); },
      dispose(fn) { disposeCb = fn; },
      invalidate() { location.reload(); },
    };
  };
  async function applyUpdate(path) {
    const entry = registry.get(path);
    try {
      const mod = await import(path + (path.includes('?') ? '&' : '?') + 't=' + Date.now());
      if (entry && entry.accept) { entry.accept(mod); console.debug('[viteless] hot', path); }
      else { location.reload(); }
    } catch (e) { console.warn('[viteless] update failed, reloading', e); location.reload(); }
  }
  function connect() {
    const es = new EventSource('/@viteless/hmr');
    es.onmessage = (e) => {
      let msg; try { msg = JSON.parse(e.data); } catch { return; }
      if (msg.type === 'reload') { location.reload(); return; }
      if (msg.type === 'update' || msg.type === 'css') { applyUpdate(msg.path); return; }
    };
    es.onerror = () => {};
  }
  connect();
})();
`
