package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	if stats.SchemaVersion != 9 {
		t.Errorf("schema version = %d, want 9", stats.SchemaVersion)
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
	if err != nil || version != 9 {
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

func TestTargetsWithState(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	if err := s.Record(ctx, []store.Probe{probeAt(now, state.Up)}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.TargetsWithState(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].State != state.Up || rows[0].Name != "Chase" {
		t.Fatalf("row = %+v", rows[0])
	}
	if rows[0].Since.IsZero() {
		t.Error("since was not populated")
	}
	if rows[0].Stage != "http" {
		t.Errorf("stage = %q, want the last probe's stage", rows[0].Stage)
	}

	// A target that has never been probed is present and UNKNOWN, not absent.
	if err := s.UpsertTarget(ctx, store.Target{
		ID: "t2", Host: "united.com", Port: 443, Probe: "http",
		Name: "United", Class: "service", Tier: 2, IntervalS: 300,
	}); err != nil {
		t.Fatal(err)
	}

	rows, err = s.TargetsWithState(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r.ID == "t2" && r.State != state.Unknown {
			t.Fatalf("unprobed target state = %s, want unknown", r.State)
		}
	}
}

func TestUpsertTargetUpdatesExisting(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)

	if err := s.UpsertTarget(context.Background(), store.Target{
		ID: "t1", Host: "chase.com", Port: 443, Probe: "http",
		Name: "Chase Bank", Class: "service", Tier: 1, IntervalS: 120,
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.TargetsWithState(context.Background(), "local")
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Name != "Chase Bank" || rows[0].Tier != 1 {
		t.Fatalf("row = %+v", rows[0])
	}
}

func TestEnsureVantageIsIdempotent(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	ctx := context.Background()

	if err := s.EnsureVantage(ctx, "local", "direct", "primary"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureVantage(ctx, "local", "socks5", "corroboration"); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
}

func TestRecordEmptyBatch(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	if err := s.Record(context.Background(), nil); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
}

func TestProbeDetailWithoutProbes(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	if _, err := s.ProbeDetail(context.Background(), "t1", "local"); err == nil {
		t.Fatal("a missing probe should be an error, not an empty detail")
	}
}

func TestReadsFailAfterClose(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := s.States(ctx, "local"); err == nil {
		t.Error("States on a closed store should fail")
	}
	if _, err := s.TargetsWithState(ctx, "local"); err == nil {
		t.Error("TargetsWithState on a closed store should fail")
	}
	if _, err := s.RollupAt(ctx, "t1", "local", time.Now()); err == nil {
		t.Error("RollupAt on a closed store should fail")
	}
	if _, err := s.ProbeDetail(ctx, "t1", "local"); err == nil {
		t.Error("ProbeDetail on a closed store should fail")
	}
	if _, err := s.Stats(ctx); err == nil {
		t.Error("Stats on a closed store should fail")
	}
	if _, err := s.SchemaVersion(ctx); err == nil {
		t.Error("SchemaVersion on a closed store should fail")
	}
}

func TestRecentProbesNewestFirstAndLimited(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	if err := s.Record(ctx, []store.Probe{probeAt(base, state.Up)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(ctx, []store.Probe{probeAt(base.Add(time.Minute), state.Down)}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.RecentProbes(ctx, "t1", "local", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0].State != state.Down || rows[1].State != state.Up {
		t.Fatalf("order = %s, %s (want newest first)", rows[0].State, rows[1].State)
	}

	one, err := s.RecentProbes(ctx, "t1", "local", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].State != state.Down {
		t.Fatalf("limit = %+v", one)
	}

	none, err := s.RecentProbes(ctx, "missing", "local", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("unknown target returned %d rows", len(none))
	}
}

func TestTargetExists(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	exists, err := s.TargetExists(ctx, "t1")
	if err != nil || !exists {
		t.Fatalf("t1 exists = %v, err = %v", exists, err)
	}
	exists, err = s.TargetExists(ctx, "nope")
	if err != nil || exists {
		t.Fatalf("nope exists = %v, err = %v", exists, err)
	}
}

func TestTargetIDsAndDisableTarget(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	if err := s.UpsertTarget(ctx, store.Target{
		ID: "t2", Host: "united.com", Port: 443, Probe: "http",
		Name: "United", Class: "service", Tier: 2, IntervalS: 300,
	}); err != nil {
		t.Fatal(err)
	}

	ids, err := s.TargetIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("ids = %v", ids)
	}

	if err := s.DisableTarget(ctx, "t2"); err != nil {
		t.Fatal(err)
	}

	ids, err = s.TargetIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("ids after disable = %v", ids)
	}

	exists, err := s.TargetExists(ctx, "t2")
	if err != nil || exists {
		t.Fatalf("disabled target exists = %v, err = %v", exists, err)
	}

	// Disabling is not deleting: the board stops listing it, but the row stays
	// for history.
	rows, err := s.TargetsWithState(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
}

func TestObservationsIncludeOnlyProbedTargets(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	if err := s.UpsertTarget(ctx, store.Target{
		ID: "t2", Host: "united.com", Port: 443, Probe: "http",
		Name: "United", Class: "service", Tier: 2, IntervalS: 300,
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	down := probeAt(now, state.Down)
	down.Stage = "tls"
	up := probeAt(now, state.Up)
	up.TargetID = "t2"

	if err := s.Record(ctx, []store.Probe{down, up}); err != nil {
		t.Fatal(err)
	}

	obs, err := s.Observations(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 2 {
		t.Fatalf("observations = %d, want 2", len(obs))
	}
	for _, o := range obs {
		switch o.TargetID {
		case "t1":
			if !o.Failing || o.Stage != "tls" {
				t.Errorf("t1 observation = %+v", o)
			}
		case "t2":
			if o.Failing || o.Stage != "http" {
				t.Errorf("t2 observation = %+v", o)
			}
		}
	}
}

func TestOpenIncidentRoundTrip(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	inc := store.Incident{
		ID: "inc-test", OpenedAt: now, Verdict: "INCIDENT",
		Scope: "sector", ScopeValue: "airline",
		Headline: "5 of 6 targets failing on sector=airline", Confidence: "high",
	}
	attrs := []store.Attribution{
		{Rank: 1, Kind: "sector", Value: "airline", Failing: 5, Total: 6, Share: 5.0 / 6.0, Score: 1.2},
	}

	if err := s.OpenIncident(ctx, inc, []string{"t1"}, attrs); err != nil {
		t.Fatal(err)
	}

	open, err := s.HasOpenIncident(ctx, "sector", "airline")
	if err != nil || !open {
		t.Fatalf("open = %v, err = %v", open, err)
	}
	if open, _ := s.HasOpenIncident(ctx, "sector", "cloud"); open {
		t.Fatal("a different cluster reported open")
	}

	list, err := s.Incidents(ctx, true, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "inc-test" {
		t.Fatalf("incidents = %+v", list)
	}

	got, gotAttrs, targets, err := s.IncidentByID(ctx, "inc-test")
	if err != nil {
		t.Fatal(err)
	}
	if got.Scope != "sector" || got.ScopeValue != "airline" {
		t.Fatalf("incident = %+v", got)
	}
	if len(gotAttrs) != 1 || gotAttrs[0].Value != "airline" {
		t.Fatalf("attributions = %+v", gotAttrs)
	}
	if len(targets) != 1 || targets[0] != "t1" {
		t.Fatalf("targets = %v", targets)
	}
}

func TestIncidentByIDMissing(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	if _, _, _, err := s.IncidentByID(context.Background(), "nope"); err == nil {
		t.Fatal("missing incident should be an error")
	}
}

func TestStateCountsAndOpenIncidentCount(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	if err := s.UpsertTarget(ctx, store.Target{
		ID: "t2", Host: "united.com", Port: 443, Probe: "http",
		Name: "United", Class: "service", Tier: 2, IntervalS: 300,
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	down := probeAt(now, state.Down)
	up := probeAt(now, state.Up)
	up.TargetID = "t2"
	if err := s.Record(ctx, []store.Probe{down, up}); err != nil {
		t.Fatal(err)
	}

	counts, err := s.StateCounts(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if counts[state.Down] != 1 || counts[state.Up] != 1 {
		t.Fatalf("counts = %v", counts)
	}

	open, err := s.OpenIncidentCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if open != 0 {
		t.Fatalf("open incidents = %d", open)
	}

	inc := store.Incident{
		ID: "inc-count", OpenedAt: now, Verdict: "INCIDENT",
		Scope: "sector", ScopeValue: "airline", Headline: "x", Confidence: "high",
	}
	if err := s.OpenIncident(ctx, inc, nil, nil); err != nil {
		t.Fatal(err)
	}
	if open, _ := s.OpenIncidentCount(ctx); open != 1 {
		t.Fatalf("open incidents = %d after opening", open)
	}

	if err := s.CloseIncident(ctx, "inc-count", "cluster cleared", now); err != nil {
		t.Fatal(err)
	}
	if open, _ := s.OpenIncidentCount(ctx); open != 0 {
		t.Fatalf("open incidents = %d after closing", open)
	}
}

func TestCloseIncident(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	inc := store.Incident{
		ID: "inc-close", OpenedAt: now, Verdict: "INCIDENT",
		Scope: "sector", ScopeValue: "airline", Headline: "x", Confidence: "high",
	}
	if err := s.OpenIncident(ctx, inc, []string{"t1"}, nil); err != nil {
		t.Fatal(err)
	}

	closedAt := now.Add(10 * time.Minute)
	if err := s.CloseIncident(ctx, "inc-close", "cluster cleared", closedAt); err != nil {
		t.Fatal(err)
	}

	if open, _ := s.HasOpenIncident(ctx, "sector", "airline"); open {
		t.Fatal("incident still reported open")
	}

	openList, err := s.OpenIncidents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(openList) != 0 {
		t.Fatalf("open incidents = %d", len(openList))
	}

	all, err := s.Incidents(ctx, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("incidents = %d", len(all))
	}
	if all[0].ClosedAt.IsZero() || all[0].ClosedReason != "cluster cleared" {
		t.Fatalf("closed incident = %+v", all[0])
	}

	// Closing twice is a no-op, not an error.
	if err := s.CloseIncident(ctx, "inc-close", "again", closedAt); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestExportProbesRange(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	if err := s.Record(ctx, []store.Probe{probeAt(base, state.Up)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(ctx, []store.Probe{probeAt(base.Add(time.Minute), state.Down)}); err != nil {
		t.Fatal(err)
	}

	var rows []store.ExportRow
	err := s.ExportProbes(ctx, base.Add(-time.Second), base.Add(30*time.Second), func(r store.ExportRow) error {
		rows = append(rows, r)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want the one inside the range", len(rows))
	}
	if rows[0].State != state.Up || rows[0].Host != "chase.com" {
		t.Fatalf("row = %+v", rows[0])
	}

	empty := 0
	if err := s.ExportProbes(ctx, base.Add(time.Hour), base.Add(2*time.Hour), func(store.ExportRow) error {
		empty++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if empty != 0 {
		t.Fatalf("empty window returned %d rows", empty)
	}
}

func TestScopeStates(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	if err := s.UpsertTarget(ctx, store.Target{
		ID: "t2", Host: "united.com", Port: 443, Probe: "http",
		Name: "United", Class: "service", Sector: "airline", Tier: 2, IntervalS: 300,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTarget(ctx, store.Target{
		ID: "t3", Host: "delta.com", Port: 443, Probe: "http",
		Name: "Delta", Class: "service", Sector: "airline", Tier: 2, IntervalS: 300,
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	up1 := probeAt(now, state.Up)
	down := probeAt(now, state.Down)
	down.TargetID = "t2"
	up2 := probeAt(now, state.Up)
	up2.TargetID = "t3"
	if err := s.Record(ctx, []store.Probe{up1, down, up2}); err != nil {
		t.Fatal(err)
	}

	counts, judgeable, err := s.ScopeStates(ctx, "local", "global", "")
	if err != nil {
		t.Fatal(err)
	}
	if judgeable != 3 || counts[state.Up] != 2 || counts[state.Down] != 1 {
		t.Fatalf("global = %v judgeable=%d", counts, judgeable)
	}

	counts, judgeable, err = s.ScopeStates(ctx, "local", "sector", "airline")
	if err != nil {
		t.Fatal(err)
	}
	if judgeable != 2 || counts[state.Up] != 1 || counts[state.Down] != 1 {
		t.Fatalf("airline = %v judgeable=%d", counts, judgeable)
	}

	if _, _, err := s.ScopeStates(ctx, "local", "asn", "nope"); err == nil {
		t.Fatal("invalid ASN should be an error")
	}
	if _, _, err := s.ScopeStates(ctx, "local", "planet", ""); err == nil {
		t.Fatal("unknown scope should be an error")
	}
}

func TestSampleScopesAndHistory(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	now := time.Date(2026, 11, 17, 12, 30, 0, 0, time.UTC)
	if err := s.Record(ctx, []store.Probe{probeAt(now, state.Up)}); err != nil {
		t.Fatal(err)
	}

	if err := s.SampleScopes(ctx, "local", now); err != nil {
		t.Fatal(err)
	}

	history, err := s.ScopeHistory(ctx, "global", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0] != 1 {
		t.Fatalf("history = %v", history)
	}

	// Sampling twice in the same hour replaces the snapshot.
	if err := s.SampleScopes(ctx, "local", now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	history, err = s.ScopeHistory(ctx, "global", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Fatalf("same-hour sampling duplicated the bucket: %v", history)
	}

	// A different weekday/hour-of-week does not match.
	other, err := s.ScopeHistory(ctx, "global", "", now.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("wrong hour-of-week returned %v", other)
	}

	refs, err := s.Scopes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) == 0 || refs[0].Scope != "global" {
		t.Fatalf("scopes = %+v", refs)
	}
}

func TestFoldBuildsHourlyAndDaily(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	now := time.Date(2026, 11, 17, 12, 30, 0, 0, time.UTC)
	base := now.Add(-26 * time.Hour) // previous day, still inside the 48h fold window

	if err := s.Record(ctx, []store.Probe{probeAt(base, state.Up)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(ctx, []store.Probe{probeAt(base.Add(5*time.Minute), state.Down)}); err != nil {
		t.Fatal(err)
	}

	stats, err := s.Fold(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Hours == 0 || stats.Days == 0 {
		t.Fatalf("fold stats = %+v, want both resolutions", stats)
	}

	hour, err := s.RollupFor(ctx, "1h", "t1", "local", base)
	if err != nil {
		t.Fatal(err)
	}
	if hour.N != 2 || hour.Up != 1 || hour.Down != 1 {
		t.Fatalf("hourly = %+v", hour)
	}

	day, err := s.RollupFor(ctx, "1d", "t1", "local", base)
	if err != nil {
		t.Fatal(err)
	}
	if day.N != 2 || day.Up != 1 || day.Down != 1 {
		t.Fatalf("daily = %+v", day)
	}
}

func TestPruneAppliesRetention(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	old := now.Add(-200 * time.Hour) // beyond both 72h probes and 7d rollups
	recent := now.Add(-time.Hour)

	if err := s.Record(ctx, []store.Probe{probeAt(old, state.Up)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(ctx, []store.Probe{probeAt(recent, state.Down)}); err != nil {
		t.Fatal(err)
	}

	stats, err := s.Prune(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Probes != 1 {
		t.Fatalf("pruned probes = %d, want 1", stats.Probes)
	}
	if stats.Rollup5m == 0 {
		t.Fatal("old 5-minute rollup was not pruned")
	}

	count, err := s.CountProbes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("probes remaining = %d, want 1", count)
	}

	oldRoll, err := s.RollupFor(ctx, "5m", "t1", "local", old)
	if err != nil {
		t.Fatal(err)
	}
	if oldRoll.N != 0 {
		t.Fatalf("old rollup survived: %+v", oldRoll)
	}

	recentRoll, err := s.RollupFor(ctx, "5m", "t1", "local", recent)
	if err != nil {
		t.Fatal(err)
	}
	if recentRoll.N != 1 {
		t.Fatalf("recent rollup = %+v", recentRoll)
	}
}

func TestRollupForRejectsUnknownResolution(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	if _, err := s.RollupFor(context.Background(), "2w", "t1", "local", time.Now()); err == nil {
		t.Fatal("unknown resolution should be an error")
	}
}

func TestSeriesReadsRollups(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	if err := s.Record(ctx, []store.Probe{probeAt(now.Add(-30*time.Minute), state.Up)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(ctx, []store.Probe{probeAt(now.Add(-20*time.Minute), state.Down)}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.Series(ctx, "t1", "local", "5m", now.Add(-time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("5m series = %d buckets, want 2", len(rows))
	}
	if rows[0].Up != 1 || rows[1].Down != 1 {
		t.Fatalf("series rows = %+v", rows)
	}

	if _, err := s.Fold(ctx, now); err != nil {
		t.Fatal(err)
	}
	hourly, err := s.Series(ctx, "t1", "local", "1h", now.Add(-2*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(hourly) == 0 || hourly[0].N != 2 {
		t.Fatalf("hourly series = %+v", hourly)
	}

	if _, err := s.Series(ctx, "t1", "local", "2w", now.Add(-time.Hour), now); err == nil {
		t.Fatal("unknown resolution should be an error")
	}
}

func TestRecordAnomalyAndList(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	ctx := context.Background()

	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	first := store.Anomaly{
		TS: base, Scope: "sector", ScopeValue: "airline", Kind: "health_drop",
		Observed: 0.62, Baseline: 0.94, MAD: 0.01, Sigma: 0.0148, Buckets: 2,
	}
	if err := s.RecordAnomaly(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.TS = base.Add(5 * time.Minute)
	second.Scope = "global"
	second.ScopeValue = ""
	second.IncidentID = "inc-1"
	if err := s.RecordAnomaly(ctx, second); err != nil {
		t.Fatal(err)
	}

	got, err := s.Anomalies(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("anomalies = %d, want 2", len(got))
	}
	// Newest first.
	if got[0].Scope != "global" || got[0].IncidentID != "inc-1" {
		t.Fatalf("first = %+v", got[0])
	}
	if got[1].Scope != "sector" || got[1].ScopeValue != "airline" {
		t.Fatalf("second = %+v", got[1])
	}
	if got[1].Observed != 0.62 || got[1].Buckets != 2 {
		t.Fatalf("values did not round-trip: %+v", got[1])
	}

	one, err := s.Anomalies(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].Scope != "global" {
		t.Fatalf("limit = %+v", one)
	}

	// A fresh database has no anomalies, which is not an error.
	empty, _ := openTest(t)
	none, err := empty.Anomalies(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("fresh database returned %d anomalies", len(none))
	}
}

func TestTargetByID(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	if err := s.Record(ctx, []store.Probe{probeAt(now, state.Down)}); err != nil {
		t.Fatal(err)
	}

	row, err := s.TargetByID(ctx, "local", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if row.Name != "Chase" || row.State != state.Down || row.Port != 443 {
		t.Fatalf("row = %+v", row)
	}
	if row.Since.IsZero() {
		t.Fatal("since was not populated")
	}

	if _, err := s.TargetByID(ctx, "local", "nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown target error = %v, want sql.ErrNoRows", err)
	}
}

func TestCoverageFindsBlindSpotsAndStaleTargets(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	// A country with five targets, only one measured: a blind spot. A sector
	// with five targets is reported separately, and three-member slices fall
	// under the reporting floor.
	for i := 0; i < 5; i++ {
		if err := s.UpsertTarget(ctx, store.Target{
			ID:        fmt.Sprintf("nl-%d", i),
			Host:      fmt.Sprintf("nl%d.example", i),
			Port:      443,
			Probe:     "http",
			Name:      fmt.Sprintf("NL %d", i),
			Class:     "country",
			Country:   "NL",
			Sector:    "small",
			Tier:      3,
			IntervalS: 120,
		}); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	// One NL target is current; the rest were never probed.
	if err := s.Record(ctx, []store.Probe{{
		TargetID: "nl-0", VantageID: "local", TS: now,
		State: state.Up, Stage: "http", Confidence: "high",
	}}); err != nil {
		t.Fatal(err)
	}

	cov, err := s.Coverage(ctx, "local", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	if cov.Totals.Total != 6 {
		t.Fatalf("totals = %+v, want 6 targets", cov.Totals)
	}
	if cov.Totals.Measurable != 1 { // only nl-0 has been probed
		t.Fatalf("measurable = %d, want 1", cov.Totals.Measurable)
	}

	var nl *store.CoverageBucket
	for i := range cov.BlindSpots {
		if cov.BlindSpots[i].Kind == "country" && cov.BlindSpots[i].Value == "NL" {
			nl = &cov.BlindSpots[i]
		}
	}
	if nl == nil {
		t.Fatalf("NL blind spot missing: %+v", cov.BlindSpots)
	}
	if nl.Total != 5 || nl.Measurable != 1 {
		t.Fatalf("NL bucket = %+v", nl)
	}
	for _, b := range cov.BlindSpots {
		if b.Value == "small" {
			t.Fatalf("a three-member sector should be below the floor: %+v", b)
		}
	}

	// Everything is stale: t1 never ran, and nl-1..4 never ran.
	if len(cov.Stale) != 5 {
		t.Fatalf("stale = %+v, want the five unprobed targets", cov.Stale)
	}
}

func TestSelfCheckRoundTripAndLatest(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	ctx := context.Background()

	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	if err := s.RecordSelfCheck(ctx, store.SelfCheck{
		TS: base, Check: "disk", OK: true, Value: "64 KiB", Detail: "",
	}); err != nil {
		t.Fatal(err)
	}
	// A newer run of the same check replaces it; a different check is separate.
	if err := s.RecordSelfCheck(ctx, store.SelfCheck{
		TS: base.Add(time.Minute), Check: "disk", OK: false, Detail: "read-only file system",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSelfCheck(ctx, store.SelfCheck{
		TS: base, Check: "egress", OK: true, Value: "1.1.1.1:443",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.LatestSelfChecks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("checks = %d, want one row per name", len(got))
	}
	if got[0].Check != "disk" {
		t.Fatalf("first = %+v", got[0])
	}
	if got[0].OK || got[0].Detail != "read-only file system" {
		t.Fatalf("latest disk check = %+v", got[0])
	}
	if got[1].Check != "egress" || !got[1].OK || got[1].Value != "1.1.1.1:443" {
		t.Fatalf("egress = %+v", got[1])
	}

	empty, _ := openTest(t)
	none, err := empty.LatestSelfChecks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("fresh database returned %d self checks", len(none))
	}
}

func TestExternalEventsDeduplicateByFeedAndID(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	ctx := context.Background()

	base := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	event := store.ExternalEvent{
		Feed: "ioda", ExternalID: "evt-1", Kind: "outage", Severity: 0.8,
		Country: "IR", StartedAt: base, ObservedAt: base, Payload: `{"id":"evt-1"}`,
	}
	if err := s.UpsertExternalEvents(ctx, []store.ExternalEvent{event}); err != nil {
		t.Fatal(err)
	}

	// The same feed id with a new severity updates the row, not duplicates it.
	event.Severity = 0.95
	event.ObservedAt = base.Add(time.Minute)
	if err := s.UpsertExternalEvents(ctx, []store.ExternalEvent{event}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ExternalEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("events = %d, want one", len(got))
	}
	if got[0].Severity != 0.95 || got[0].Country != "IR" {
		t.Fatalf("event = %+v", got[0])
	}

	// A different feed may reuse the same external id.
	other := event
	other.Feed = "radar"
	if err := s.UpsertExternalEvents(ctx, []store.ExternalEvent{other}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ExternalEvents(ctx, 10)
	if len(got) != 2 {
		t.Fatalf("events = %d, want two feeds kept apart", len(got))
	}
}

func TestFeedStateTracksFailuresAndRecovery(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	if err := s.RecordFeedSuccess(ctx, "ioda", now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.RecordFeedFailure(ctx, "ioda", "503 from upstream", now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	states, err := s.FeedStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("states = %d", len(states))
	}
	got := states[0]
	if got.Feed != "ioda" || got.ConsecutiveFailures != 2 {
		t.Fatalf("state = %+v", got)
	}
	if !got.LastOKAt.Equal(now) {
		t.Fatalf("last ok = %v, want the earlier success preserved", got.LastOKAt)
	}
	if got.LastErr != "503 from upstream" {
		t.Fatalf("last err = %q", got.LastErr)
	}

	// A success clears the error and the streak.
	if err := s.RecordFeedSuccess(ctx, "ioda", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	states, _ = s.FeedStates(ctx)
	if states[0].ConsecutiveFailures != 0 || states[0].LastErr != "" {
		t.Fatalf("state after recovery = %+v", states[0])
	}

	if err := s.SetFeedNextPoll(ctx, "ioda", now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	states, _ = s.FeedStates(ctx)
	if !states[0].NextPollAt.Equal(now.Add(10 * time.Minute)) {
		t.Fatalf("next poll = %v", states[0].NextPollAt)
	}

	// Upserting an empty batch is a no-op, not an error.
	if err := s.UpsertExternalEvents(ctx, nil); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
}

func TestSnapshotCoverageIsDailyAndIdempotent(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	if err := s.UpsertTarget(ctx, store.Target{
		ID: "t2", Host: "united.com", Port: 443, Probe: "http",
		Name: "United", Class: "service", Sector: "airline",
		Country: "US", Tier: 2, IntervalS: 300,
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	if err := s.Record(ctx, []store.Probe{probeAt(now, state.Up)}); err != nil {
		t.Fatal(err)
	}

	if err := s.SnapshotCoverage(ctx, "local", now); err != nil {
		t.Fatal(err)
	}
	// Later the same day, with the second target still unprobed: the row is
	// updated in place, not duplicated.
	if err := s.SnapshotCoverage(ctx, "local", now.Add(6*time.Hour)); err != nil {
		t.Fatal(err)
	}

	points, err := s.CoverageHistory(ctx, "global", "", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 {
		t.Fatalf("points = %d, want one daily row: %+v", len(points), points)
	}
	if points[0].Total != 2 || points[0].Measurable != 1 {
		t.Fatalf("point = %+v", points[0])
	}
	if !points[0].Bucket.Equal(time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("bucket = %v", points[0].Bucket)
	}

	// A sector slice is recorded separately.
	if err := s.SnapshotCoverage(ctx, "local", now); err != nil {
		t.Fatal(err)
	}
	airline, err := s.CoverageHistory(ctx, "sector", "airline", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(airline) != 1 || airline[0].Total != 1 {
		t.Fatalf("airline history = %+v", airline)
	}

	// The window is capped and ordered oldest first across days.
	if err := s.SnapshotCoverage(ctx, "local", now.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	window, err := s.CoverageHistory(ctx, "global", "", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(window) != 2 || !window[0].Bucket.Before(window[1].Bucket) {
		t.Fatalf("window = %+v", window)
	}
}

func TestStateChangesAreReturnedNewestFirst(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	if err := s.Record(ctx, []store.Probe{probeAt(base, state.Up)}); err != nil {
		t.Fatal(err)
	}

	down := probeAt(base.Add(time.Minute), state.Down)
	down.Reason = "certificate expired"
	if err := s.Record(ctx, []store.Probe{down}); err != nil {
		t.Fatal(err)
	}
	recovered := probeAt(base.Add(2*time.Minute), state.Up)
	recovered.Reason = "semantic ok"
	if err := s.Record(ctx, []store.Probe{recovered}); err != nil {
		t.Fatal(err)
	}

	changes, err := s.StateChanges(ctx, "t1", "local", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("changes = %d, want 2", len(changes))
	}
	if changes[0].From != state.Down || changes[0].To != state.Up || changes[0].Reason != "semantic ok" {
		t.Fatalf("newest = %+v", changes[0])
	}
	if changes[1].From != state.Up || changes[1].To != state.Down || changes[1].Reason != "certificate expired" {
		t.Fatalf("oldest = %+v", changes[1])
	}

	one, err := s.StateChanges(ctx, "t1", "local", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].To != state.Up {
		t.Fatalf("limit = %+v", one)
	}

	none, err := s.StateChanges(ctx, "missing", "local", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("unknown target returned %+v", none)
	}
}

func TestTargetRowsCarryEnrichment(t *testing.T) {
	t.Parallel()

	s, path := openTest(t)
	seed(t, s)
	ctx := context.Background()

	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()

	if _, err := raw.ExecContext(ctx, `
		UPDATE target SET asn = ?, prefix = ?, cdn = ?, measurability = ?
		WHERE id = 't1'`, 64500, "203.0.113.0/24", "akamai", "mostly_blocked"); err != nil {
		t.Fatal(err)
	}

	rows, err := s.TargetsWithState(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	got := rows[0]
	if got.ASN != 64500 || got.Prefix != "203.0.113.0/24" || got.CDN != "akamai" ||
		got.Measurability != "mostly_blocked" {
		t.Fatalf("board row = %+v", got)
	}

	row, err := s.TargetByID(ctx, "local", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if row.ASN != 64500 || row.CDN != "akamai" {
		t.Fatalf("detail row = %+v", row)
	}
}
