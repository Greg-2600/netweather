package logging_test

import (
	"io"
	"log/slog"
	"testing"

	"github.com/Greg-2600/netweather/internal/logging"
)

// BenchmarkLogger measures one enabled INFO line through every sink: level
// check, JSON encode, redaction, ring copy, and the async console hand-off
// (LOGGING.md §12). It exists because the probe path logs every measurement,
// which makes this the hot path the budget table is about.
func BenchmarkLogger(b *testing.B) {
	cfg := logging.DefaultConfig()
	cfg.Format = "json"
	cfg.Level = slog.LevelInfo

	lg, err := logging.New(cfg, io.Discard)
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	defer func() { _ = lg.Close() }()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lg.Info("probe",
			"target", "chase.com",
			"state", "down",
			"latency_ms", 42,
			"error_class", "tcp_refused")
	}
}

// BenchmarkLoggerDisabledLevel measures the other half of the budget: a record
// below the configured level must cost one Enabled check and no encoding.
func BenchmarkLoggerDisabledLevel(b *testing.B) {
	cfg := logging.DefaultConfig()
	cfg.Format = "json"
	cfg.Level = slog.LevelInfo

	lg, err := logging.New(cfg, io.Discard)
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	defer func() { _ = lg.Close() }()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lg.Debug("probe", "target", "chase.com", "latency_ms", 42)
	}
}
