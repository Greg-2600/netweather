package logging_test

import (
	"log/slog"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/logging"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestFromEnvDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := logging.FromEnv(envFrom(nil))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.Level != slog.LevelInfo {
		t.Errorf("Level = %v, want INFO", cfg.Level)
	}
	if cfg.Format != "auto" {
		t.Errorf("Format = %q, want auto", cfg.Format)
	}
	if cfg.RingRecords != 2000 {
		t.Errorf("RingRecords = %d, want 2000", cfg.RingRecords)
	}
}

func TestFromEnvOverrides(t *testing.T) {
	t.Parallel()

	cfg, err := logging.FromEnv(envFrom(map[string]string{
		"NETWEATHER_LOG_LEVEL":  "trace",
		"NETWEATHER_LOG_FORMAT": "json",
		"NETWEATHER_LOG_FILE":   "/data/netweather.log",
		"NETWEATHER_LOG_MAX_MB": "25",
		"NETWEATHER_LOG_KEEP":   "5",
		"NETWEATHER_LOG_RING":   "500",
		"NETWEATHER_LOG_SOURCE": "1",
	}))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}

	if cfg.Level != logging.LevelTrace {
		t.Errorf("Level = %v, want TRACE", cfg.Level)
	}
	if cfg.Format != "json" || cfg.File != "/data/netweather.log" {
		t.Errorf("sink config wrong: %+v", cfg)
	}
	if cfg.MaxMB != 25 || cfg.Keep != 5 || cfg.RingRecords != 500 {
		t.Errorf("sizes wrong: %+v", cfg)
	}
	if !cfg.AddSource {
		t.Error("AddSource not enabled")
	}
}

func TestFromEnvRejectsBadValues(t *testing.T) {
	t.Parallel()

	tests := []map[string]string{
		{"NETWEATHER_LOG_LEVEL": "loud"},
		{"NETWEATHER_LOG_MAX_MB": "many"},
		{"NETWEATHER_LOG_KEEP": "-1"},
		{"NETWEATHER_LOG_RING": "lots"},
	}

	for _, env := range tests {
		if _, err := logging.FromEnv(envFrom(env)); err == nil {
			t.Errorf("FromEnv(%v) = nil error, want error", env)
		}
	}
}

func TestFromEnvBadFormatFailsValidation(t *testing.T) {
	t.Parallel()

	if _, err := logging.FromEnv(envFrom(map[string]string{"NETWEATHER_LOG_FORMAT": "xml"})); err == nil {
		t.Fatal("want validation error for xml format")
	}
}

func TestDefaultConfigIsValid(t *testing.T) {
	t.Parallel()

	if err := logging.DefaultConfig().Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
}

func TestTimeFormatIsSortable(t *testing.T) {
	t.Parallel()

	// The timestamp must sort lexicographically as well as parse, because
	// operators pipe logs through sort and cut.
	ts := time.Date(2026, 11, 16, 3, 14, 15, 123000000, time.UTC)
	got := ts.Format("2006-01-02T15:04:05.000Z07:00")
	if _, err := time.Parse(time.RFC3339, got); err != nil {
		t.Fatalf("format %q does not parse: %v", got, err)
	}
	if got != "2026-11-16T03:14:15.123Z" {
		t.Fatalf("format = %q", got)
	}
}
