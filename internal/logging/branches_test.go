package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/logging"
)

func TestNewRejectsInvalidSetup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	notADir := filepath.Join(dir, "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(*logging.Config)
	}{
		{name: "bad format", mutate: func(c *logging.Config) { c.Format = "xml" }},
		{name: "file parent is a file", mutate: func(c *logging.Config) { c.File = filepath.Join(notADir, "x.log") }},
		{name: "file path is a directory", mutate: func(c *logging.Config) { c.File = dir }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := logging.DefaultConfig()
			tc.mutate(&cfg)
			lg, err := logging.New(cfg, &bytes.Buffer{})
			if err == nil {
				_ = lg.Close()
				t.Fatal("New succeeded, want an error")
			}
		})
	}
}

func TestNewAutoFormat(t *testing.T) {
	t.Parallel()

	t.Run("non-file writer is text", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		cfg := logging.DefaultConfig()
		cfg.Format = "auto"

		lg, err := logging.New(cfg, &out)
		if err != nil {
			t.Fatal(err)
		}
		lg.Info("hello")
		if err := lg.Close(); err != nil {
			t.Fatal(err)
		}

		if strings.HasPrefix(strings.TrimSpace(out.String()), "{") {
			t.Fatalf("non-file writer produced JSON: %s", out.String())
		}
	})

	t.Run("regular file is json", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "auto.log")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()

		cfg := logging.DefaultConfig()
		cfg.Format = "auto"

		lg, err := logging.New(cfg, f)
		if err != nil {
			t.Fatal(err)
		}
		lg.Info("hello")
		if err := lg.Close(); err != nil {
			t.Fatal(err)
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(raw), &m); err != nil {
			t.Fatalf("regular file was not written as JSON: %v\n%s", err, raw)
		}
	})

	t.Run("stat failure falls back to text", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "closed.log")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}

		cfg := logging.DefaultConfig()
		cfg.Format = "auto"

		lg, err := logging.New(cfg, f)
		if err != nil {
			t.Fatal(err)
		}
		lg.Info("hello")
		if err := lg.Close(); err != nil {
			t.Fatal(err)
		}
		if got := lg.WriteErrors(); got == 0 {
			t.Fatal("writing to the closed file was not counted")
		}
	})
}

func TestLoggerWithoutSinks(t *testing.T) {
	t.Parallel()

	lg := &logging.Logger{}
	if got := lg.ActiveScopes(); got != nil {
		t.Fatalf("ActiveScopes = %+v, want nil", got)
	}

	ch, cancel := lg.Subscribe()
	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("Subscribe without a ring left the channel open")
	}

	lg.UseScopes(logging.NewScopes(""))
	if got := lg.ActiveScopes(); got != nil {
		t.Fatalf("UseScopes on a zero logger changed state: %+v", got)
	}

	if got := lg.Dropped(); got != 0 {
		t.Fatalf("Dropped = %d, want 0", got)
	}
	if got := lg.WriteErrors(); got != 0 {
		t.Fatalf("WriteErrors = %d, want 0", got)
	}

	var nilLogger *logging.Logger
	nilLogger.UseScopes(nil)
	if got := nilLogger.ActiveScopes(); got != nil {
		t.Fatalf("nil ActiveScopes = %+v, want nil", got)
	}
}

func TestDiagnosticsIncludesFileAndScopes(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "diag.log")
	cfg := logging.DefaultConfig()
	cfg.Format = "json"
	cfg.File = path
	cfg.RingRecords = 5
	cfg.AddSource = true

	lg, err := logging.New(cfg, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	scopes := logging.NewScopes("")
	if _, err := scopes.Add(logging.Scope{Target: "chase.com", Level: "debug"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	lg.UseScopes(scopes)
	lg.Info("with-source")

	d := lg.Diagnostics()
	if d.File != path {
		t.Fatalf("File = %q, want %q", d.File, path)
	}
	if d.Scopes != 1 {
		t.Fatalf("Scopes = %d, want 1", d.Scopes)
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"source"`) {
		t.Fatalf("AddSource did not add a source attribute: %s", raw)
	}
}

func TestLoggerSubscribeWithRing(t *testing.T) {
	t.Parallel()

	cfg := logging.DefaultConfig()
	cfg.Format = "text"
	cfg.RingRecords = 5

	lg, err := logging.New(cfg, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lg.Close() }()

	ch, cancel := lg.Subscribe()
	defer cancel()

	lg.Info("streamed")
	select {
	case rec := <-ch:
		if rec.Message != "streamed" {
			t.Fatalf("record = %+v", rec)
		}
	case <-time.After(time.Second):
		t.Fatal("no record delivered")
	}

	if got := lg.ActiveScopes(); got != nil {
		t.Fatalf("ActiveScopes = %+v, want nil", got)
	}
}

func TestRingValueConversions(t *testing.T) {
	t.Parallel()

	ts := testTime()
	tests := []struct {
		name string
		attr slog.Attr
		want string
	}{
		{name: "string", attr: slog.String("k", "v"), want: "v"},
		{name: "int", attr: slog.Int("k", -7), want: "-7"},
		{name: "uint", attr: slog.Uint64("k", 7), want: "7"},
		{name: "float", attr: slog.Float64("k", 1.5), want: "1.5"},
		{name: "bool", attr: slog.Bool("k", true), want: "true"},
		{name: "duration", attr: slog.Duration("k", 1500*time.Millisecond), want: "1.5s"},
		{name: "time", attr: slog.Time("k", ts), want: ts.Format(time.RFC3339Nano)},
		{name: "any", attr: slog.Any("k", []int{1, 2}), want: "[1 2]"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := logging.NewRing(10, 0)
			rec := slog.NewRecord(ts, slog.LevelInfo, "msg", 0)
			rec.AddAttrs(tc.attr)
			if err := r.Handle(context.Background(), rec); err != nil {
				t.Fatal(err)
			}

			got := r.Snapshot()
			if len(got) != 1 {
				t.Fatalf("len = %d, want 1", len(got))
			}
			if got[0].Attrs["k"] != tc.want {
				t.Fatalf("Attrs[k] = %q, want %q", got[0].Attrs["k"], tc.want)
			}
		})
	}
}

func TestRingFlattensGroupsAndSkipsEmptyKeys(t *testing.T) {
	t.Parallel()

	r := logging.NewRing(10, 0)
	rec := slog.NewRecord(testTime(), slog.LevelInfo, "msg", 0)
	rec.AddAttrs(
		slog.Group("outer", slog.Group("inner", slog.String("k", "v"))),
		slog.Group("", slog.String("flat", "f")),
		slog.String("", "dropped"),
		slog.String("kept", "yes"),
	)
	if err := r.Handle(context.Background(), rec); err != nil {
		t.Fatal(err)
	}

	attrs := r.Snapshot()[0].Attrs
	if attrs["outer.inner.k"] != "v" {
		t.Fatalf("nested group = %v", attrs)
	}
	if attrs["flat"] != "f" {
		t.Fatalf("anonymous group = %v", attrs)
	}
	if attrs["kept"] != "yes" {
		t.Fatalf("kept attr missing: %v", attrs)
	}
	if _, ok := attrs[""]; ok {
		t.Fatalf("empty key was stored: %v", attrs)
	}
}

func TestRingBoundaries(t *testing.T) {
	t.Parallel()

	if got := logging.NewRing(0, 0).Capacity(); got != 1 {
		t.Fatalf("Capacity = %d, want 1", got)
	}

	r := logging.NewRing(10, 0)
	if got := r.WithAttrs(nil); got != r {
		t.Fatal("WithAttrs(nil) allocated a new handler")
	}
	if got := r.WithGroup(""); got != r {
		t.Fatal("WithGroup(\"\") allocated a new handler")
	}

	small := logging.NewRing(10, 1)
	oversize := slog.NewRecord(testTime(), slog.LevelInfo, strings.Repeat("x", 2048), 0)
	if err := small.Handle(context.Background(), oversize); err != nil {
		t.Fatal(err)
	}
	if got := small.Len(); got != 0 {
		t.Fatalf("oversize record was retained: %d", got)
	}

	query := logging.NewRing(10, 0)
	byKey := slog.NewRecord(testTime(), slog.LevelInfo, "msg", 0)
	byKey.AddAttrs(slog.String("needle_key", "haystack"))
	if err := query.Handle(context.Background(), byKey); err != nil {
		t.Fatal(err)
	}
	if got := query.Query(logging.LevelTrace, "needle_key", 0); len(got) != 1 {
		t.Fatalf("query by attribute key returned %d records, want 1", len(got))
	}
}

func TestAsyncWriterBufferDefaults(t *testing.T) {
	t.Parallel()

	for _, buffer := range []int{0, -1} {
		var buf bytes.Buffer
		w := logging.NewAsyncWriter(&buf, buffer)
		if _, err := w.Write([]byte("a")); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if got := buf.String(); got != "a" {
			t.Fatalf("buffer %d: got %q, want %q", buffer, got, "a")
		}
	}
}

func TestLimiterDefaults(t *testing.T) {
	t.Parallel()

	l := logging.NewLimiterWithClock(0, 0, nil)
	if !l.Allow("k") {
		t.Fatal("first event should be allowed with default settings")
	}
	if l.Allow("k") {
		t.Fatal("default max of 1 should suppress the second event")
	}
}

func TestLimiterPrunesExpiredEntries(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 16, 3, 0, 0, 0, time.UTC)
	l := logging.NewLimiterWithClock(time.Minute, 1, func() time.Time { return now })

	_ = l.Allow("old")
	_ = l.Allow("old")
	if got := l.Suppressed("old"); got != 1 {
		t.Fatalf("Suppressed = %d, want 1", got)
	}

	now = now.Add(3 * time.Minute)
	for i := 0; i < 1024; i++ {
		_ = l.Allow("new")
	}
	if got := l.Suppressed("old"); got != 0 {
		t.Fatalf("expired entry was not pruned: Suppressed = %d", got)
	}
}

func TestRotatingFileDefaultsAndDoubleClose(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "defaults.log")
	rf, err := logging.NewRotatingFile(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rf.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := rf.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rf.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestNewRotatingFileRejectsBadPaths(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	notADir := filepath.Join(dir, "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
	}{
		{name: "parent is a file", path: filepath.Join(notADir, "x.log")},
		{name: "path is a directory", path: dir},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if rf, err := logging.NewRotatingFile(tc.path, 1, 1); err == nil {
				_ = rf.Close()
				t.Fatal("NewRotatingFile succeeded, want an error")
			}
		})
	}
}

func TestScopesLoadWithoutPath(t *testing.T) {
	t.Parallel()

	scopes := logging.NewScopes("")
	dropped, err := scopes.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped = %+v, want none", dropped)
	}
}

func TestScopesClear(t *testing.T) {
	t.Parallel()

	scopes := logging.NewScopes("")
	if _, err := scopes.Add(logging.Scope{Target: "chase.com", Level: "debug"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if !scopes.Active() {
		t.Fatal("scope is not active")
	}

	scopes.Clear()
	if scopes.Active() {
		t.Fatal("Clear left a scope active")
	}
	if got := scopes.List(); len(got) != 0 {
		t.Fatalf("List after Clear = %+v", got)
	}
}

func TestScopeSelectorMatching(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		scope logging.Scope
		match slog.Attr
		other slog.Attr
	}{
		{
			name:  "target",
			scope: logging.Scope{Target: "chase.com", Level: "debug"},
			match: slog.String("target", "chase.com"),
			other: slog.String("target", "united.com"),
		},
		{
			name:  "vantage",
			scope: logging.Scope{Vantage: "tor", Level: "debug"},
			match: slog.String("vantage", "tor"),
			other: slog.String("vantage", "direct"),
		},
		{
			name:  "component",
			scope: logging.Scope{Component: "feeds", Level: "debug"},
			match: slog.String("component", "feeds"),
			other: slog.String("component", "probe"),
		},
		{
			name:  "error class",
			scope: logging.Scope{ErrorClass: "tcp_timeout", Level: "debug"},
			match: slog.String("error_class", "tcp_timeout"),
			other: slog.String("error_class", "dns_timeout"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			scopes := logging.NewScopes("")
			if _, err := scopes.Add(tc.scope, time.Minute); err != nil {
				t.Fatal(err)
			}

			rec := slog.NewRecord(time.Now(), slog.LevelDebug, "msg", 0)
			rec.AddAttrs(tc.match)
			if !scopes.Match(rec, nil) {
				t.Fatalf("matching %s did not match", tc.name)
			}

			other := slog.NewRecord(time.Now(), slog.LevelDebug, "msg", 0)
			other.AddAttrs(tc.other)
			if scopes.Match(other, nil) {
				t.Fatalf("non-matching %s matched", tc.name)
			}
		})
	}
}

func TestScopeMatchIgnoresExpiredScopes(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	scopes := logging.NewScopes("")
	scopes.SetClock(func() time.Time { return now })

	if _, err := scopes.Add(logging.Scope{Target: "chase.com", Level: "debug"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)

	rec := slog.NewRecord(now, slog.LevelDebug, "msg", 0)
	rec.AddAttrs(slog.String("target", "chase.com"))
	if scopes.Match(rec, nil) {
		t.Fatal("an expired scope matched a record")
	}
}

func TestScopedLoggerWithAttrsAndGroup(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	cfg := logging.DefaultConfig()
	cfg.Format = "text"
	cfg.Level = slog.LevelInfo

	lg, err := logging.New(cfg, &out)
	if err != nil {
		t.Fatal(err)
	}
	scopes := logging.NewScopes("")
	lg.UseScopes(scopes)
	if _, err := scopes.Add(logging.Scope{Target: "chase.com", Level: "debug"}, time.Minute); err != nil {
		t.Fatal(err)
	}

	lg.With("target", "chase.com").Debug("attrs-line")
	lg.WithGroup("grp").Debug("group-line", "target", "chase.com")
	lg.WithGroup("grp").Debug("group-other", "target", "united.com")
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}

	body := out.String()
	if !strings.Contains(body, "attrs-line") {
		t.Fatalf("With attrs did not reach the scoped handler:\n%s", body)
	}
	if !strings.Contains(body, "group-line") {
		t.Fatalf("WithGroup did not reach the scoped handler:\n%s", body)
	}
	if strings.Contains(body, "group-other") {
		t.Fatalf("a non-matching grouped record passed:\n%s", body)
	}
}

func TestRedactAttrMalformedAndTextValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
		in   string
	}{
		{name: "malformed url", key: "url", in: "https://example.com/\x7f?token=abc"},
		{name: "text error", key: "error", in: "request to https://user:pw@example.com/?token=abc failed"},
		{name: "text detail", key: "detail", in: "redirect to https://example.com/?secret=abc"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := logging.RedactAttr(nil, slog.String(tc.key, tc.in)).Value.String()
			for _, secret := range []string{"abc", "pw", "user"} {
				if strings.Contains(got, secret) {
					t.Fatalf("%q leaked %q: %q", tc.name, secret, got)
				}
			}
		})
	}
}
