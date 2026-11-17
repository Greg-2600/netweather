package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// requestIDKey carries the per-request id in the context.
type requestIDKey struct{}

// requestIDFrom returns the id assigned to this request by withRequestID, or an
// empty string when a test built the handler without it.
func requestIDFrom(r *http.Request) string {
	id, _ := r.Context().Value(requestIDKey{}).(string)
	return id
}

// withRequestID assigns every request an id, exposes it as X-Request-ID, and
// puts it in the context so a page and its log line can name the same request.
//
// The id is generated here rather than adopted from a client header: an
// attacker-supplied value would let one client write another's id into the
// logs, which is log poisoning rather than correlation. The id is eight random
// bytes, so a collision is not a practical concern at this scale.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A failure here means the system entropy source is unusable. The
		// request still proceeds; the page says "unavailable" rather than
		// printing a made-up id.
		return ""
	}
	return hex.EncodeToString(b[:])
}
