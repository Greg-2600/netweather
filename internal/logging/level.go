package logging

import (
	"fmt"
	"log/slog"
	"strings"
)

// LevelTrace is below slog.LevelDebug. It is for per-stage wire detail (DNS
// answer, TCP timing, TLS issuer, HTTP signature match) and is never on by
// default.
const LevelTrace = slog.Level(-8)

// ParseLevel accepts the operator-facing level names.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "trace":
		return LevelTrace, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q (want trace, debug, info, warn, error)", s)
}

// LevelName returns the canonical uppercase name. slog's own String() renders
// custom levels as "DEBUG-4", which is correct but unreadable in a log stream.
func LevelName(l slog.Level) string {
	switch {
	case l < slog.LevelDebug:
		return "TRACE"
	case l < slog.LevelInfo:
		return "DEBUG"
	case l < slog.LevelWarn:
		return "INFO"
	case l < slog.LevelError:
		return "WARN"
	default:
		return "ERROR"
	}
}
