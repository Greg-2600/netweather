package logging_test

import (
	"log/slog"
	"testing"

	"github.com/Greg-2600/netweather/internal/logging"
)

func TestParseLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{in: "", want: slog.LevelInfo},
		{in: "info", want: slog.LevelInfo},
		{in: "INFO", want: slog.LevelInfo},
		{in: " debug ", want: slog.LevelDebug},
		{in: "trace", want: logging.LevelTrace},
		{in: "warn", want: slog.LevelWarn},
		{in: "warning", want: slog.LevelWarn},
		{in: "error", want: slog.LevelError},
		{in: "verbose", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()

			got, err := logging.ParseLevel(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseLevel(%q) = nil error, want error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLevel(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseLevel(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestLevelName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   slog.Level
		want string
	}{
		{in: logging.LevelTrace, want: "TRACE"},
		{in: logging.LevelTrace + 1, want: "TRACE"},
		{in: slog.LevelDebug, want: "DEBUG"},
		{in: slog.LevelInfo, want: "INFO"},
		{in: slog.LevelWarn, want: "WARN"},
		{in: slog.LevelError, want: "ERROR"},
		{in: slog.LevelError + 8, want: "ERROR"},
	}

	for _, tc := range tests {
		if got := logging.LevelName(tc.in); got != tc.want {
			t.Fatalf("LevelName(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
