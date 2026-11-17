package api //nolint:testpackage // white-box tests cover store internals with no HTTP surface

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIdempotencyStoreRoundTrip(t *testing.T) {
	t.Parallel()

	store := newIdempotencyStore()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }

	if _, ok := store.get("missing"); ok {
		t.Fatal("an empty store returned an entry")
	}

	store.put("k", idempotencyEntry{status: http.StatusCreated, body: []byte("body")})
	entry, ok := store.get("k")
	if !ok {
		t.Fatal("stored entry was not found")
	}
	if entry.status != http.StatusCreated || string(entry.body) != "body" {
		t.Fatalf("entry = %+v", entry)
	}

	// Expiry is lazy: the entry is gone on the next read.
	now = now.Add(idempotencyTTL + time.Second)
	if _, ok := store.get("k"); ok {
		t.Fatal("expired entry was returned")
	}
}

func TestIdempotencyStoreEvictsAtCapacity(t *testing.T) {
	t.Parallel()

	store := newIdempotencyStore()
	store.cap = 3

	for i := 0; i < 10; i++ {
		store.put(strings.Repeat("k", i+1), idempotencyEntry{status: http.StatusOK})
	}

	store.mu.Lock()
	size := len(store.entries)
	store.mu.Unlock()
	if size > store.cap {
		t.Fatalf("store size = %d, cap = %d", size, store.cap)
	}
}

func TestCaptureWriterCapsTheBody(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	w := &captureWriter{ResponseWriter: rec}

	half := bytes.Repeat([]byte("a"), maxIdempotentBody/2)
	if _, err := w.Write(half); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(half); err != nil {
		t.Fatal(err)
	}
	if w.capped {
		t.Fatal("exactly the limit should not be capped")
	}
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if !w.capped {
		t.Fatal("crossing the limit did not cap the capture")
	}
	if w.body.Len() != maxIdempotentBody {
		t.Fatalf("captured %d bytes, want %d", w.body.Len(), maxIdempotentBody)
	}
	if w.status != http.StatusOK {
		t.Fatalf("implicit status = %d", w.status)
	}

	// Writes past the cap still reach the client.
	if rec.Body.Len() != maxIdempotentBody+1 {
		t.Fatalf("client received %d bytes", rec.Body.Len())
	}
}

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (f *flushRecorder) Flush() { f.flushed = true }

func TestCaptureWriterForwardsFlushAndUnwrap(t *testing.T) {
	t.Parallel()

	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	w := &captureWriter{ResponseWriter: rec}

	w.Flush()
	if !rec.flushed {
		t.Fatal("Flush did not reach the wrapped writer")
	}
	if w.Unwrap() != rec {
		t.Fatal("Unwrap did not return the wrapped writer")
	}

	// A writer without http.Flusher must not panic.
	plain := &captureWriter{ResponseWriter: httptest.NewRecorder()}
	plain.Flush()
}

func TestWithIdempotencyNilStorePassesThrough(t *testing.T) {
	t.Parallel()

	called := false
	h := withIdempotency(nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", nil))
	if !called {
		t.Fatal("nil store should pass the request through")
	}
}

func TestWithIdempotencyIgnoresSafeMethods(t *testing.T) {
	t.Parallel()

	store := newIdempotencyStore()
	h := withIdempotency(store, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		req := httptest.NewRequest(method, "/x", nil)
		req.Header.Set("Idempotency-Key", "same")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", method, rec.Code)
		}
	}

	store.mu.Lock()
	size := len(store.entries)
	store.mu.Unlock()
	if size != 0 {
		t.Fatalf("safe methods populated the store: %d", size)
	}
}

func TestWithIdempotencyDoesNotCacheServerErrors(t *testing.T) {
	t.Parallel()

	store := newIdempotencyStore()
	calls := 0
	h := withIdempotency(store, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadGateway)
	}))

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		req.Header.Set("Idempotency-Key", "fail")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if calls != 2 {
		t.Fatalf("a 5xx was cached: handler ran %d times, want 2", calls)
	}
}
