package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Greg-2600/netweather/internal/logging"
)

type ctxKey int

const requestIDKey ctxKey = iota

func requestIDFrom(r *http.Request) string {
	if id, ok := r.Context().Value(requestIDKey).(string); ok {
		return id
	}
	return ""
}

// withRequestID accepts a caller-supplied X-Request-Id when it is boring, and
// generates one otherwise. The ID joins the access log, the error body, and any
// stack trace.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if !validRequestID(id) {
			id = logging.NewTraceID()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func validRequestID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// withSecurityHeaders sets the responses that cost nothing and close whole bug
// classes. The API is same-origin by design, so no CORS headers are emitted.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// withRecover turns a panic into a logged 500 rather than a dropped connection.
func withRecover(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			logger.Error("handler panic",
				"event", "panic",
				"component", "http",
				"request_id", requestIDFrom(r),
				"method", r.Method,
				"route", r.URL.Path,
				"panic", rec,
				"stack", string(debug.Stack()),
			)
			writeError(w, r, http.StatusInternalServerError, CodeInternal, "internal error; quote the request id")
		}()
		next.ServeHTTP(w, r)
	})
}

// statusWriter records what was actually sent for the access log.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach Flush/SetWriteDeadline, which SSE
// will need.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// withAccessLog is DEBUG per request and WARN for 5xx: a dashboard can produce a
// lot of request lines, and none of them are more interesting than a state
// change.
func withAccessLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)

		level := slog.LevelDebug
		if sw.status >= 500 {
			level = slog.LevelWarn
		}
		logger.Log(r.Context(), level, "http request",
			"event", "http_request",
			"component", "http",
			"request_id", requestIDFrom(r),
			"method", r.Method,
			"route", r.URL.Path, // path only: query strings can carry parameters we do not want in logs
			"status", sw.status,
			"bytes", sw.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// withBearerAuth enforces the API token with a constant-time comparison over
// SHA-256 digests, so neither the value nor its length leaks through timing.
func withBearerAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next // only reachable on a loopback bind, enforced by Config.Validate
	}
	want := sha256.Sum256([]byte(token))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="netweather"`)
			writeError(w, r, http.StatusUnauthorized, CodeUnauthorized, "missing bearer token")
			return
		}
		gotHash := sha256.Sum256([]byte(got))
		if subtle.ConstantTimeCompare(gotHash[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="netweather"`)
			writeError(w, r, http.StatusUnauthorized, CodeUnauthorized, "invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return "", false
	}
	tok := strings.TrimSpace(h[7:])
	return tok, tok != ""
}
