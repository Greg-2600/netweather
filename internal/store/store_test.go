package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
)

func openTest(t *testing.T) (*store.Store, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "netweather.db")
	s, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func seed(t *testing.T, s *store.Store) {
	t.Helper()

	ctx := context.Background()
	if err := s.EnsureVantage(ctx, "local", "direct", "primary"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTarget(ctx, store.Target{
		ID: "t1", Host: "chase.com", Port: 443, Probe: "http",
		Name: "Chase", Class: "service", Tier: 2, IntervalS: 300,
	}); err != nil {
		t.Fatal(err)
	}
}

func probeAt(ts time.Time, st state.State) store.Probe {
	return store.Probe{
		TargetID:   "t1",
		VantageID:  "local",
		TS:         ts,
		State:      st,
		Stage:      "http",
		Confidence: "high",
	}
}

func TestStatsProveThePragmas(t *testing.T) {
	t.Parallel()

	s, path := openTest(t)
	stats, err := s.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	if !strings.EqualFold(stats.JournalMode, "wal") {
		t.Errorf("journal_mode = %q, want wal", stats.JournalMode)
	}
	if !stats.ForeignKeys {
		t.Error("foreign_keys is off")
	}
	if stats.SchemaVersion != 1 {
		t.Errorf("schema version = %d, want 1", stats.SchemaVersion)
	}
	if stats.Path != path {
		t.Errorf("path = %q", stats.Path)
	}
}

func TestReopenIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reopen.db")

	first, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer func() { _ = second.Close() }()

	version, err := second.SchemaVersion(ctx)
	if err != nil || version != 1 {
		t.Fatalf("version = %d, err = %v", version, err)
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "future.db")

	s, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE schema_meta SET value = '99' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	if _, err := store.Open(ctx, path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("error = %v, want a newer-schema refusal", err)
	}
}

func TestRecordTracksStateAndTransitions(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	t0 := time.Date(2026, 11, 17, 12, 0, 1, 0, time.UTC)

	// First observation: state is created, but there is no transition to record.
	if err := s.Record(ctx, []store.Probe{probeAt(t0, state.Up)}); err != nil {
		t.Fatal(err)
	}
	states, err := s.States(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if got := states["t1"].State; got != state.Up {
		t.Fatalf("state = %q, want up", got)
	}
	if n, _ := s.StateChangeCount(ctx, "t1", "local"); n != 0 {
		t.Fatalf("state changes = %d, want 0 for the first observation", n)
	}

	// Transition to DOWN carries the reason and the detail.
	down := probeAt(t0.Add(time.Minute), state.Down)
	down.Reason = "certificate expired"
	down.Detail = "certificate expired 2h ago"
	if err := s.Record(ctx, []store.Probe{down}); err != nil {
		t.Fatal(err)
	}
	states, _ = s.States(ctx, "local")
	got := states["t1"]
	if got.State != state.Down || got.FailStreak != 1 || got.LastError != down.Detail {
		t.Fatalf("after down: %+v", got)
	}
	if got.PrevState != string(state.Up) {
		t.Fatalf("prev_state = %q", got.PrevState)
	}
	if n, _ := s.StateChangeCount(ctx, "t1", "local"); n != 1 {
		t.Fatalf("state changes = %d, want 1", n)
	}

	// A second DOWN advances the streak without another transition row.
	if err := s.Record(ctx, []store.Probe{probeAt(t0.Add(2*time.Minute), state.Down)}); err != nil {
		t.Fatal(err)
	}
	states, _ = s.States(ctx, "local")
	if states["t1"].FailStreak != 2 {
		t.Fatalf("fail streak = %d", states["t1"].FailStreak)
	}
	if n, _ := s.StateChangeCount(ctx, "t1", "local"); n != 1 {
		t.Fatalf("state changes = %d, want 1", n)
	}

	// Recovery.
	if err := s.Record(ctx, []store.Probe{probeAt(t0.Add(3*time.Minute), state.Up)}); err != nil {
		t.Fatal(err)
	}
	states, _ = s.States(ctx, "local")
	if states["t1"].OKStreak != 1 || states["t1"].FailStreak != 0 {
		t.Fatalf("streaks after recovery: %+v", states["t1"])
	}
	if n, _ := s.StateChangeCount(ctx, "t1", "local"); n != 2 {
		t.Fatalf("state changes = %d, want 2", n)
	}
}

func TestRollupIncrementsPerState(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	sequence := []state.State{
		state.Up, state.Degraded, state.Down, state.Blocked, state.ConfigError,
	}
	for i, st := range sequence {
		p := probeAt(base.Add(time.Duration(i)*time.Minute), st)
		if err := s.Record(ctx, []store.Probe{p}); err != nil {
			t.Fatal(err)
		}
	}

	roll, err := s.RollupAt(ctx, "t1", "local", base)
	if err != nil {
		t.Fatal(err)
	}
	if roll.N != 5 || roll.Up != 1 || roll.Degraded != 1 || roll.Down != 1 || roll.Blocked != 1 || roll.Unknown != 1 {
		t.Fatalf("rollup = %+v", roll)
	}

	// A bucket that was never written is a zero, not an error.
	empty, err := s.RollupAt(ctx, "t1", "local", base.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if empty.N != 0 {
		t.Fatalf("empty bucket = %+v", empty)
	}
}

func TestDetailIsSanitisedAndTruncated(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	dirty := strings.Repeat("x", 300) + "\x00\x01\tend"
	p := probeAt(time.Now().UTC(), state.Down)
	p.Detail = dirty
	if err := s.Record(ctx, []store.Probe{p}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ProbeDetail(ctx, "t1", "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > 200 {
		t.Fatalf("detail length = %d, want <= 200", len(got))
	}
	if strings.ContainsAny(got, "\x00\x01\t") {
		t.Fatalf("control characters survived: %q", got)
	}
}

func TestForeignKeyRejectsUnknownTarget(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	ctx := context.Background()
	if err := s.EnsureVantage(ctx, "local", "direct", "primary"); err != nil {
		t.Fatal(err)
	}

	if err := s.Record(ctx, []store.Probe{probeAt(time.Now().UTC(), state.Up)}); err == nil {
		t.Fatal("a probe for an unknown target should violate the foreign key")
	}
}

func TestStatesIsEmptyOnAFreshDatabase(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	states, err := s.States(context.Background(), "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 0 {
		t.Fatalf("states = %v", states)
	}
}
