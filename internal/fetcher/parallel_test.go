package fetcher

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulmanoni/viteless/internal/store"
)

// TestFetcher_ParallelRecursionIsBoundedByConcurrency: with a
// fake registry that records max simultaneous in-flight requests,
// we should never exceed f.Concurrency at any moment. Default
// concurrency is 8.
func TestFetcher_ParallelRecursionIsBoundedByConcurrency(t *testing.T) {
	var inflight, maxInflight atomic.Int32
	// Root imports 20 children; each child is a leaf with no
	// further imports. The fan-out should hit the semaphore.
	const fanout = 20
	mux := http.NewServeMux()
	mux.HandleFunc("/root.js", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		for i := 0; i < fanout; i++ {
			fmt.Fprintf(&b, "import './child%d.js';\n", i)
		}
		w.Header().Set("Content-Type", "application/javascript")
		w.Write([]byte(b.String()))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// All children + transitive paths land here.
		cur := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			old := maxInflight.Load()
			if cur <= old || maxInflight.CompareAndSwap(old, cur) {
				break
			}
		}
		// Hold long enough that genuinely-parallel requests overlap.
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/javascript")
		w.Write([]byte("export {};"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &Fetcher{
		Registry:    srv.URL,
		Store:       st,
		HTTP:        srv.Client(),
		Concurrency: 4, // cap at 4 to make the limit observable
	}
	_, err = f.Fetch(context.Background(), srv.URL+"/root.js")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got := maxInflight.Load()
	if got > 4 {
		t.Errorf("max concurrent fetches = %d, want ≤ 4 (semaphore breached)", got)
	}
	if got < 2 {
		t.Errorf("max concurrent fetches = %d, want ≥ 2 (parallelism never engaged)", got)
	}
}

// TestFetcher_ParallelIsActuallyFasterThanSerial: a synthetic
// fan-out with slow leaf responses should complete in roughly
// `total/concurrency * leafDelay`, NOT `total * leafDelay`. The
// hard assertion: parallel beats serial by at least 3× on a
// fan-out of 10 with concurrency=8.
func TestFetcher_ParallelIsActuallyFasterThanSerial(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping perf test in -short mode")
	}
	const (
		fanout    = 10
		leafDelay = 30 * time.Millisecond
	)
	makeServer := func() *httptest.Server {
		mux := http.NewServeMux()
		mux.HandleFunc("/root.js", func(w http.ResponseWriter, r *http.Request) {
			var b strings.Builder
			for i := 0; i < fanout; i++ {
				fmt.Fprintf(&b, "import './leaf%d.js';\n", i)
			}
			w.Header().Set("Content-Type", "application/javascript")
			w.Write([]byte(b.String()))
		})
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(leafDelay)
			w.Header().Set("Content-Type", "application/javascript")
			w.Write([]byte("export {};"))
		})
		return httptest.NewServer(mux)
	}

	// Serial baseline (Concurrency=1).
	srv1 := makeServer()
	defer srv1.Close()
	st1, _ := store.New(t.TempDir())
	fSerial := &Fetcher{Registry: srv1.URL, Store: st1, HTTP: srv1.Client(), Concurrency: 1}
	t0 := time.Now()
	_, err := fSerial.Fetch(context.Background(), srv1.URL+"/root.js")
	if err != nil {
		t.Fatal(err)
	}
	serial := time.Since(t0)

	// Parallel (Concurrency=8).
	srv2 := makeServer()
	defer srv2.Close()
	st2, _ := store.New(t.TempDir())
	fParallel := &Fetcher{Registry: srv2.URL, Store: st2, HTTP: srv2.Client(), Concurrency: 8}
	t0 = time.Now()
	_, err = fParallel.Fetch(context.Background(), srv2.URL+"/root.js")
	if err != nil {
		t.Fatal(err)
	}
	parallel := time.Since(t0)

	ratio := float64(serial) / float64(parallel)
	if ratio < 3 {
		t.Errorf("parallel (%v) not meaningfully faster than serial (%v), ratio=%.2f×, want ≥ 3×",
			parallel, serial, ratio)
	}
	t.Logf("serial=%v parallel=%v ratio=%.2f×", serial, parallel, ratio)
}

// TestFetcher_DeepRecursionDoesNotDeadlock: regression for the
// semaphore deadlock found when installing pinia (~20-node tree
// at Concurrency=8). Earlier versions held the semaphore slot
// ACROSS the recursive fetchOne call, so parents+children
// competed for the same 8 slots and deeper trees stalled
// forever. Fix: only hold the slot for HTTP work, never across
// recursion.
//
// This test creates a tree DEEPER than Concurrency to prove the
// slot is released before recursion: with the old design, a
// 4-deep chain at Concurrency=2 would deadlock at depth 3.
func TestFetcher_DeepRecursionDoesNotDeadlock(t *testing.T) {
	mux := http.NewServeMux()
	// 6-level chain: each level imports the next. With
	// Concurrency=2, the old (broken) implementation would hang
	// at level 3 — both slots held by ancestors waiting for
	// descendants who can't acquire slots.
	mux.HandleFunc("/level0.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Write([]byte(`import './level1.js';`))
	})
	mux.HandleFunc("/level1.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Write([]byte(`import './level2.js';`))
	})
	mux.HandleFunc("/level2.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Write([]byte(`import './level3.js';`))
	})
	mux.HandleFunc("/level3.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Write([]byte(`import './level4.js';`))
	})
	mux.HandleFunc("/level4.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Write([]byte(`import './level5.js';`))
	})
	mux.HandleFunc("/level5.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Write([]byte(`export const leaf = true;`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st, _ := store.New(t.TempDir())
	f := &Fetcher{
		Registry:    srv.URL,
		Store:       st,
		HTTP:        srv.Client(),
		Concurrency: 2, // shallower than the chain depth
	}

	// Hard timeout — if the deadlock returns, this test stalls
	// rather than hangs forever and the harness will report it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := f.Fetch(ctx, srv.URL+"/level0.js")
	if err != nil {
		t.Fatalf("Fetch: %v (deadlock returning? slot was held across recursion)", err)
	}
	// All 6 levels should be in the result.
	if len(res.Transitive) != 5 {
		t.Errorf("expected 5 transitive packages, got %d", len(res.Transitive))
	}
}

// TestFetcher_InflightDedupAvoidsDoubleFetch: when two children
// of the same root both import the same transitive (diamond
// import), the transitive's body should be requested ONCE not
// twice — the in-flight dedup wins the race.
func TestFetcher_InflightDedupAvoidsDoubleFetch(t *testing.T) {
	var sharedHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/root.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Write([]byte(`
			import './a.js';
			import './b.js';
		`))
	})
	mux.HandleFunc("/a.js", func(w http.ResponseWriter, r *http.Request) {
		// Slow enough that b also lands in-flight before a finishes.
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/javascript")
		w.Write([]byte(`import './shared.js';`))
	})
	mux.HandleFunc("/b.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Write([]byte(`import './shared.js';`))
	})
	mux.HandleFunc("/shared.js", func(w http.ResponseWriter, r *http.Request) {
		sharedHits.Add(1)
		// Slow body so concurrent in-flight requests overlap on the
		// server side, exercising the dedup path.
		time.Sleep(40 * time.Millisecond)
		w.Header().Set("Content-Type", "application/javascript")
		w.Write([]byte(`export const x = 1;`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st, _ := store.New(t.TempDir())
	f := &Fetcher{Registry: srv.URL, Store: st, HTTP: srv.Client(), Concurrency: 8}
	_, err := f.Fetch(context.Background(), srv.URL+"/root.js")
	if err != nil {
		t.Fatal(err)
	}
	// shared.js may legitimately be hit once for HEAD + once for
	// GET (the resolve + get split). What we're guarding against
	// is two FULL fetch flows — that would mean dedup didn't fire.
	got := sharedHits.Load()
	if got > 2 {
		t.Errorf("shared.js hit %d times, want ≤ 2 (in-flight dedup didn't fire — diamond import double-fetched)", got)
	}
}

// TestFetcher_CacheHitSkipsHEADForAbsoluteURLs is the fast-path
// guarantee: when an absolute URL is already in the cache, the
// fetcher should not do a HEAD request. Counts HEAD vs GET vs
// the implicit resolve() request to be sure.
func TestFetcher_CacheHitSkipsHEADForAbsoluteURLs(t *testing.T) {
	var requests atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/some/asset.js", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/javascript")
		w.Write([]byte("export const x = 1;"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st, _ := store.New(t.TempDir())
	f := &Fetcher{Registry: srv.URL, Store: st, HTTP: srv.Client(), Concurrency: 1}

	// First call: cache miss, expect 1+ requests (resolve + GET).
	_, err := f.Fetch(context.Background(), srv.URL+"/some/asset.js")
	if err != nil {
		t.Fatal(err)
	}
	firstCount := requests.Load()
	if firstCount < 1 {
		t.Fatalf("first fetch should hit the server, got %d", firstCount)
	}

	// Second call: cache hit, fast-path should skip ALL HTTP.
	requests.Store(0)
	_, err = f.Fetch(context.Background(), srv.URL+"/some/asset.js")
	if err != nil {
		t.Fatal(err)
	}
	secondCount := requests.Load()
	if secondCount != 0 {
		t.Errorf("cached fetch should make 0 HTTP requests, got %d (fast-path didn't fire)", secondCount)
	}
}
