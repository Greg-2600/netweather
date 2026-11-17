package logging_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/logging"
)

func TestLoggerWritesValidJSON(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	cfg := logging.DefaultConfig()
	cfg.Format = "json"
	cfg.Level = slog.LevelDebug

	lg, err := logging.New(cfg, &out)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lg.Info("probe", "target", "chase.com", "latency_ms", 42)
	if err := lg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	line := strings.TrimSpace(out.String())
	if line == "" {
		t.Fatal("no output")
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("output is not one JSON object: %v\n%s", err, line)
	}
	if m["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", m["level"])
	}
	if m["msg"] != "probe" {
		t.Errorf("msg = %v", m["msg"])
	}
	if m["target"] != "chase.com" {
		t.Errorf("target = %v", m["target"])
	}
	if _, err := time.Parse(time.RFC3339, m["time"].(string)); err != nil {
		t.Errorf("time %q is not RFC3339: %v", m["time"], err)
	}
}

func TestLoggerRedactsSecretsEndToEnd(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	cfg := logging.DefaultConfig()
	cfg.Format = "json"

	lg, err := logging.New(cfg, &out)
	if err != nil {
		t.Fatal(err)
	}
	lg.Info("feed poll",
		"url", "https://user:pw@api.example.com/v2?token=abc&since=5",
		"authorization", "Bearer sk-live-123",
	)
	_ = lg.Close()

	s := out.String()
	for _, secret := range []string{"pw", "abc", "sk-live-123"} {
		if strings.Contains(s, secret) {
			t.Errorf("secret %q leaked into the log: %s", secret, s)
		}
	}
	if !strings.Contains(s, "since=5") {
		t.Errorf("benign parameter dropped: %s", s)
	}
}

func TestLoggerLevelFiltering(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	cfg := logging.DefaultConfig()
	cfg.Format = "json"
	cfg.Level = slog.LevelInfo

	lg, err := logging.New(cfg, &out)
	if err != nil {
		t.Fatal(err)
	}
	lg.Debug("should not appear")
	lg.Info("should appear")
	_ = lg.Close()

	s := out.String()
	if strings.Contains(s, "should not appear") {
		t.Errorf("debug line passed an info filter: %s", s)
	}
	if !strings.Contains(s, "should appear") {
		t.Errorf("info line was filtered: %s", s)
	}
}

func TestLoggerTraceLevel(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	cfg := logging.DefaultConfig()
	cfg.Format = "json"
	cfg.Level = logging.LevelTrace

	lg, err := logging.New(cfg, &out)
	if err != nil {
		t.Fatal(err)
	}
	lg.Log(t.Context(), logging.LevelTrace, "wire", "stage", "tls")
	_ = lg.Close()

	s := out.String()
	if !strings.Contains(s, `"level":"TRACE"`) {
		t.Fatalf("trace level not rendered: %s", s)
	}
}

func TestLoggerFeedsRing(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	cfg := logging.DefaultConfig()
	cfg.Format = "json"
	cfg.RingRecords = 5

	lg, err := logging.New(cfg, &out)
	if err != nil {
		t.Fatal(err)
	}
	lg.Warn("feed stale", "feed", "radar")
	_ = lg.Close()

	if lg.Ring == nil {
		t.Fatal("Ring is nil")
	}
	got := lg.Ring.Query(logging.LevelTrace, "radar", 0)
	if len(got) != 1 || got[0].Message != "feed stale" {
		t.Fatalf("ring did not capture the line: %#v", got)
	}
}

func TestLoggerFileSink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "netweather.log")

	var out bytes.Buffer
	cfg := logging.DefaultConfig()
	cfg.Format = "json"
	cfg.File = path
	cfg.MaxMB = 1
	cfg.Keep = 2

	lg, err := logging.New(cfg, &out)
	if err != nil {
		t.Fatal(err)
	}
	lg.Info("file-line", "target", "united.com")
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(data), "file-line") {
		t.Fatalf("file sink lost the record: %s", data)
	}
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*logging.Config)
		wantErr bool
	}{
		{name: "defaults are valid", mutate: func(*logging.Config) {}},
		{name: "bad format", mutate: func(c *logging.Config) { c.Format = "xml" }, wantErr: true},
		{name: "file without size", mutate: func(c *logging.Config) { c.File = "x.log"; c.MaxMB = 0 }, wantErr: true},
		{name: "file without keep", mutate: func(c *logging.Config) { c.File = "x.log"; c.Keep = 0 }, wantErr: true},
		{name: "negative ring", mutate: func(c *logging.Config) { c.RingRecords = -1 }, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := logging.DefaultConfig()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
