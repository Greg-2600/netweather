package logging_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/logging"
)

func TestScopeValidation(t *testing.T) {
	t.Parallel()

	scopes := logging.NewScopes("")

	if _, err := scopes.Add(logging.Scope{Level: "debug"}, time.Minute); err == nil {
		t.Fatal("a scope with no selectors must be rejected")
	}
	if _, err := scopes.Add(logging.Scope{Target: "chase.com", Level: "verbose"}, time.Minute); err == nil {
		t.Fatal("an unknown level must be rejected")
	}
	if _, err := scopes.Add(logging.Scope{Target: "chase.com", Level: "debug"}, 0); err == nil {
		t.Fatal("ttl is mandatory")
	}
	if _, err := scopes.Add(logging.Scope{Target: "chase.com", Level: "debug"}, 2*time.Hour); err == nil {
		t.Fatal("a ttl above the ceiling must be rejected")
	}

	sc, err := scopes.Add(logging.Scope{Target: "chase.com", Level: "trace"}, 10*time.Minute)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if sc.ID == "" || sc.ExpiresAt.IsZero() || sc.CreatedAt.IsZero() {
		t.Fatalf("scope = %+v", sc)
	}
	if !sc.Selectors() {
		t.Fatal("selector bookkeeping lost the target")
	}
}

func TestScopeMatchSemantics(t *testing.T) {
	t.Parallel()

	scopes := logging.NewScopes("")
	if _, err := scopes.Add(logging.Scope{Target: "chase.com", Level: "debug"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := scopes.Add(logging.Scope{Component: "feeds", Level: "trace", ErrorClass: "tcp_timeout"}, time.Minute); err != nil {
		t.Fatal(err)
	}

	debug := slog.NewRecord(time.Now(), slog.LevelDebug, "probe", 0)
	debug.AddAttrs(slog.String("target", "chase.com"))

	trace := slog.NewRecord(time.Now(), logging.LevelTrace, "wire", 0)
	trace.AddAttrs(slog.String("target", "chase.com"))

	if !scopes.Match(debug, nil) {
		t.Fatal("a debug record for the scoped target did not match")
	}
	if scopes.Match(trace, nil) {
		t.Fatal("a trace record matched a debug-level scope")
	}

	wrong := slog.NewRecord(time.Now(), slog.LevelDebug, "probe", 0)
	wrong.AddAttrs(slog.String("target", "united.com"))
	if scopes.Match(wrong, nil) {
		t.Fatal("a record for another target matched")
	}

	feed := slog.NewRecord(time.Now(), logging.LevelTrace, "feed", 0)
	feed.AddAttrs(slog.String("component", "feeds"), slog.String("error_class", "tcp_timeout"))
	if !scopes.Match(feed, nil) {
		t.Fatal("a component+error_class scope did not match")
	}
	feedWrong := slog.NewRecord(time.Now(), logging.LevelTrace, "feed", 0)
	feedWrong.AddAttrs(slog.String("component", "feeds"), slog.String("error_class", "dns_timeout"))
	if scopes.Match(feedWrong, nil) {
		t.Fatal("a different error_class matched")
	}

	// Attributes attached with WithAttrs must be considered too.
	if !scopes.Match(slog.NewRecord(time.Now(), slog.LevelDebug, "x", 0),
		[]slog.Attr{slog.String("target", "chase.com")}) {
		t.Fatal("WithAttrs attributes were ignored")
	}
}

func TestScopeExpiryAndPrune(t *testing.T) {
	t.Parallel()

	scopes := logging.NewScopes("")
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	scopes.SetClock(func() time.Time { return now })

	sc, err := scopes.Add(logging.Scope{Target: "chase.com", Level: "debug"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !scopes.Active() {
		t.Fatal("scope is not active")
	}

	now = now.Add(2 * time.Minute)

	// Prune reports what expired once; List must not have already discarded it.
	expired := scopes.Prune()
	if len(expired) != 1 || expired[0].ID != sc.ID {
		t.Fatalf("expired = %+v", expired)
	}
	if len(scopes.Prune()) != 0 {
		t.Fatal("prune repeated the same expiry")
	}
	if scopes.Active() {
		t.Fatal("expired scope still reports active")
	}
	if list := scopes.List(); len(list) != 0 {
		t.Fatalf("expired scope still listed: %+v", list)
	}
}

func TestScopePersistenceAndLoad(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "debug.json")
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	first := logging.NewScopes(path)
	first.SetClock(func() time.Time { return now })
	live, err := first.Add(logging.Scope{Target: "chase.com", Level: "debug"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	expired, err := first.Add(logging.Scope{Target: "united.com", Level: "trace"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// Restart thirty minutes later: the long scope survives, the short one expires.
	second := logging.NewScopes(path)
	second.SetClock(func() time.Time { return now.Add(30 * time.Minute) })
	dropped, err := second.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(dropped) != 1 || dropped[0].ID != expired.ID {
		t.Fatalf("dropped = %+v", dropped)
	}
	list := second.List()
	if len(list) != 1 || list[0].ID != live.ID {
		t.Fatalf("loaded = %+v", list)
	}

	// Removing the live scope is persisted too.
	if !second.Remove(live.ID) {
		t.Fatal("Remove reported unknown")
	}
	if second.Remove(live.ID) {
		t.Fatal("Remove is not idempotent")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), live.ID) {
		t.Fatalf("removed scope survived in %s", raw)
	}
}

func TestLoadIgnoresMissingAndCorruptFiles(t *testing.T) {
	t.Parallel()

	missing := logging.NewScopes(filepath.Join(t.TempDir(), "absent.json"))
	if dropped, err := missing.Load(); err != nil || len(dropped) != 0 {
		t.Fatalf("missing file = (%+v, %v)", dropped, err)
	}

	corruptPath := filepath.Join(t.TempDir(), "debug.json")
	if err := os.WriteFile(corruptPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	corrupt := logging.NewScopes(corruptPath)
	if _, err := corrupt.Load(); err == nil {
		t.Fatal("a corrupt debug file should be reported")
	}
}

func TestScopedLoggerPassesMatchingDebugRecords(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	cfg := logging.DefaultConfig()
	cfg.Level = slog.LevelInfo
	cfg.Format = "text"
	lg, err := logging.New(cfg, &out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lg.Close() }()

	scopes := logging.NewScopes("")
	lg.UseScopes(scopes)

	lg.Debug("hidden", "target", "chase.com")
	lg.Info("visible", "target", "chase.com")

	sc, err := scopes.Add(logging.Scope{Target: "chase.com", Level: "debug"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	lg.Debug("scoped", "target", "chase.com", "event", "probe")
	lg.Debug("other", "target", "united.com")

	// Flush the async writer before reading.
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}

	body := out.String()
	if strings.Contains(body, "hidden") {
		t.Fatalf("debug leaked before any scope:\n%s", body)
	}
	if !strings.Contains(body, "visible") {
		t.Fatalf("INFO was dropped:\n%s", body)
	}
	if !strings.Contains(body, "scoped") {
		t.Fatalf("matching debug record was dropped:\n%s", body)
	}
	if strings.Contains(body, "other") {
		t.Fatalf("non-matching debug record passed:\n%s", body)
	}

	// Ending the scope restores the configured level immediately.
	scopes.Remove(sc.ID)
	lg.Debug("after", "target", "chase.com")
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "after") {
		t.Fatal("debug passed after the scope ended")
	}
}

func TestScopedLevelsRespectTrace(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	cfg := logging.DefaultConfig()
	cfg.Level = slog.LevelInfo
	cfg.Format = "text"
	lg, err := logging.New(cfg, &out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lg.Close() }()

	scopes := logging.NewScopes("")
	lg.UseScopes(scopes)

	if _, err := scopes.Add(logging.Scope{Component: "agent", Level: "debug"}, time.Minute); err != nil {
		t.Fatal(err)
	}

	lg.Log(context.Background(), logging.LevelTrace, "wire", "component", "agent")
	lg.Debug("debug", "component", "agent")

	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	body := out.String()
	if strings.Contains(body, "wire") {
		t.Fatalf("TRACE passed a debug-level scope:\n%s", body)
	}
	if !strings.Contains(body, "debug") {
		t.Fatalf("DEBUG did not pass:\n%s", body)
	}
}
