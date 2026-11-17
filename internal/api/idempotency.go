package api

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// idempotencyTTL is how long a key remains replayable. A retrying proxy retries
// within seconds; a day is generous and keeps memory bounded.
const idempotencyTTL = 24 * time.Hour

// idempotencyCap bounds the store. Entries are action responses (a created
// scope, a pause acknowledgement), never export streams.
const idempotencyCap = 4096

// maxIdempotentBody is the largest response body worth replaying. Anything
// bigger is not an action result.
const maxIdempotentBody = 1 << 20

type idempotencyEntry struct {
	status  int
	header  http.Header
	body    []byte
	expires time.Time
}

// idempotencyStore remembers action responses by key. It is deliberately small
// and in-memory: the guarantee is "a retry within this process's lifetime does
// not act twice", and the docs say exactly that.
type idempotencyStore struct {
	mu      sync.Mutex
	entries map[string]idempotencyEntry
	now     func() time.Time
	cap     int
}

func newIdempotencyStore() *idempotencyStore {
	return &idempotencyStore{
		entries: make(map[string]idempotencyEntry),
		now:     time.Now,
		cap:     idempotencyCap,
	}
}

func (s *idempotencyStore) get(key string) (idempotencyEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[key]
	if !ok {
		return idempotencyEntry{}, false
	}
	if !s.now().Before(entry.expires) {
		delete(s.entries, key)
		return idempotencyEntry{}, false
	}
	return entry, true
}

func (s *idempotencyStore) put(key string, entry idempotencyEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.entries) >= s.cap {
		// Drop expired entries first; if the store is still full, drop one
		// arbitrary entry rather than growing without bound.
		now := s.now()
		for k, e := range s.entries {
			if !now.Before(e.expires) {
				delete(s.entries, k)
			}
		}
		if len(s.entries) >= s.cap {
			for k := range s.entries {
				delete(s.entries, k)
				break
			}
		}
	}
	entry.expires = s.now().Add(idempotencyTTL)
	s.entries[key] = entry
}

// captureWriter records a response while forwarding it to the client.
type captureWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
	capped bool
}

func (w *captureWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *captureWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if !w.capped {
		room := maxIdempotentBody - w.body.Len()
		if room > 0 {
			if len(p) > room {
				w.body.Write(p[:room])
				w.capped = true
			} else {
				w.body.Write(p)
			}
		} else {
			w.capped = true
		}
	}
	return w.ResponseWriter.Write(p)
}

// Flush keeps streaming routes unaffected; actions never stream today, but the
// wrapper must not hide http.Flusher.
func (w *captureWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *captureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// withIdempotency replays a previous response for a repeated Idempotency-Key on
// a mutating request. GET/HEAD are ignored: they are already safe.
//
// A key is scoped by method and path, so the same key on a different action is
// a different request rather than a surprising replay.
func withIdempotency(store *idempotencyStore, next http.Handler) http.Handler {
	if store == nil {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}

		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			next.ServeHTTP(w, r)
			return
		}
		if len(key) > 128 {
			writeError(w, r, http.StatusBadRequest, CodeInvalidArgument,
				"Idempotency-Key must be 128 characters or fewer")
			return
		}

		scoped := fmt.Sprintf("%s %s %s", r.Method, r.URL.Path, key)
		if entry, ok := store.get(scoped); ok {
			w.Header().Set("Idempotent-Replayed", "true")
			for name, values := range entry.header {
				if strings.EqualFold(name, "Content-Length") {
					continue
				}
				for _, value := range values {
					w.Header().Add(name, value)
				}
			}
			w.WriteHeader(entry.status)
			_, _ = w.Write(entry.body)
			return
		}

		captured := &captureWriter{ResponseWriter: w}
		next.ServeHTTP(captured, r)

		status := captured.status
		if status == 0 {
			status = http.StatusOK
		}
		// Cache only deterministic outcomes. A 5xx is a transient failure and a
		// retry should be allowed to succeed.
		if status < 500 && !captured.capped {
			store.put(scoped, idempotencyEntry{
				status: status,
				header: w.Header().Clone(),
				body:   append([]byte(nil), captured.body.Bytes()...),
			})
		}
	})
}
