package fetcher

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestIsRetryableFetchError_ConnectionReset covers the headline
// transient: Cloudflare-flavoured `read: connection reset by peer`
// during a recursive transitive fetch.
func TestIsRetryableFetchError_ConnectionReset(t *testing.T) {
	err := errors.New("fetcher: GET https://esm.sh/foo: read: connection reset by peer")
	if !isRetryableFetchError(err) {
		t.Errorf("connection reset should be retryable")
	}
}

func TestIsRetryableFetchError_EOF(t *testing.T) {
	err := errors.New("read: EOF")
	if !isRetryableFetchError(err) {
		t.Errorf("EOF should be retryable")
	}
}

func TestIsRetryableFetchError_Timeout(t *testing.T) {
	err := errors.New("dial tcp: i/o timeout")
	if !isRetryableFetchError(err) {
		t.Errorf("i/o timeout should be retryable")
	}
}

func TestIsRetryableFetchError_HTTPStatus5xx(t *testing.T) {
	err := &HTTPError{URL: "https://esm.sh/foo", Status: 503}
	if !isRetryableFetchError(err) {
		t.Errorf("5xx should be retryable")
	}
}

func TestIsRetryableFetchError_HTTPStatus429(t *testing.T) {
	err := &HTTPError{URL: "https://esm.sh/foo", Status: 429}
	if !isRetryableFetchError(err) {
		t.Errorf("429 should be retryable")
	}
}

func TestIsRetryableFetchError_HTTPStatus404NotRetryable(t *testing.T) {
	err := &HTTPError{URL: "https://esm.sh/foo", Status: 404}
	if isRetryableFetchError(err) {
		t.Errorf("404 must NOT be retryable — stable miss, retries would just delay the failure")
	}
}

func TestIsRetryableFetchError_ContextCanceledNotRetryable(t *testing.T) {
	if isRetryableFetchError(context.Canceled) {
		t.Errorf("context.Canceled must NOT be retryable — caller asked to stop")
	}
}

// TestFetcher_GetRetriesOnTransient503 drives the integration:
// httptest server returns 503 twice, then 200. The fetcher should
// transparently retry and return the success body without the
// caller seeing the intermediate failures.
func TestFetcher_GetRetriesOnTransient503(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			http.Error(w, "transient", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	// Override the package-level backoff to keep the test fast —
	// the default 1s, 2s, 4s would make the suite take 7s.
	swapBackoffForTest(t, 1*time.Millisecond)

	f := &Fetcher{HTTP: srv.Client()}
	body, err := f.get(context.Background(), srv.URL+"/whatever")
	if err != nil {
		t.Fatalf("get should succeed after 2 retries, got %v", err)
	}
	if string(body) != "ok" {
		t.Errorf("body: got %q, want ok", body)
	}
	if calls.Load() != 3 {
		t.Errorf("expected 3 attempts (2 fails + 1 success), got %d", calls.Load())
	}
}

// TestFetcher_GetFailsAfterMaxAttempts: persistently 503ing
// server hits the retry cap and surfaces the error with the
// "(after N attempts)" suffix so logs read clearly.
func TestFetcher_GetFailsAfterMaxAttempts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "always down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	swapBackoffForTest(t, 1*time.Millisecond)

	f := &Fetcher{HTTP: srv.Client()}
	_, err := f.get(context.Background(), srv.URL+"/whatever")
	if err == nil {
		t.Fatal("expected error after retry cap")
	}
	if !strings.Contains(err.Error(), "after") {
		t.Errorf("error should mention attempt count, got: %v", err)
	}
}

// TestFetcher_Get404FailsImmediately: a 404 stable miss must NOT
// burn the retry budget. The test asserts only ONE request hits
// the server even though the response is an error.
func TestFetcher_Get404FailsImmediately(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	swapBackoffForTest(t, 1*time.Millisecond)

	f := &Fetcher{HTTP: srv.Client()}
	_, err := f.get(context.Background(), srv.URL+"/whatever")
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if calls.Load() != 1 {
		t.Errorf("404 should fail-fast, got %d attempts", calls.Load())
	}
}

// swapBackoffForTest replaces the package-level backoff with a
// tiny sleep so tests don't take seconds each. The original
// schedule is restored after the test via t.Cleanup.
//
// sleepBackoff is a package-level var in fetcher.go (not a func
// const) so this works without import-shenanigans.
func swapBackoffForTest(t *testing.T, delay time.Duration) {
	t.Helper()
	orig := sleepBackoff
	sleepBackoff = func(ctx context.Context, attempt int) error {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	t.Cleanup(func() { sleepBackoff = orig })
}
