package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"
)

type loggerKey struct{}

type requestIDKey struct{}

// WithRequestID stores the request id in ctx, so a runtime action called from a
// handler can name the request that caused it without the handler having to
// thread an extra argument through the interface.
func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFrom returns the request id stored by WithRequestID, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// WithLogger stores a logger in ctx. Handlers and probe workers read it with
// FromContext so every line inherits request_id / target / vantage.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, l)
}

// FromContext returns the context logger, falling back to slog.Default().
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// With attaches attributes to the context logger. Use it at a boundary:
// ctx = logging.With(ctx, "target", name, "vantage", id).
func With(ctx context.Context, args ...any) context.Context {
	return WithLogger(ctx, FromContext(ctx).With(args...))
}

// NewTraceID returns a 16-hex-character identifier for one probe attempt or one
// HTTP request. It is the join key between a log line and a stored probe row.
func NewTraceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; a timestamp still gives ordering.
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
