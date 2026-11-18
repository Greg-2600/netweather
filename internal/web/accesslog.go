package web

import (
	"log/slog"
	"net/http"
	"time"
)

// statusRecorder observes the status and byte count without taking capabilities
// away from the handler. The event stream type-asserts http.Flusher and the
// response controller walks Unwrap to clear the write deadline, so a plain
// wrapper would break live updates.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// withAccessLog writes one line per request at DEBUG, and at WARN for a server
// error. It logs the path rather than the full URL because a query string can
// carry parameters that do not belong in a log, and it carries the same
// request_id as the page and any error line (LOGGING.md §4).
func withAccessLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		level := slog.LevelDebug
		if rec.status >= http.StatusInternalServerError {
			level = slog.LevelWarn
		}
		logger.Log(r.Context(), level, "http request",
			"event", "http_request",
			"component", "http",
			"request_id", requestIDFrom(r),
			"method", r.Method,
			"route", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}
