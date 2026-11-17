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

	"github.com/Greg-2600/netweather/internal/dnsdiv"
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
	if stats.SchemaVersion != 19 {
		t.Errorf("schema version = %d, want 19", stats.SchemaVersion)
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
	if err != nil || version != 19 {
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
		if o.LastProbe.IsZero() {
			t.Errorf("%s observation has no last probe time", o.TargetID)
		}
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

func TestEnrichmentRoundTrip(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	needs, err := s.TargetsNeedingEnrichment(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(needs) != 1 || needs[0].ID != "t1" || needs[0].Host != "chase.com" {
		t.Fatalf("needs = %+v", needs)
	}

	// A zero limit falls back to the documented batch size.
	if all, err := s.TargetsNeedingEnrichment(ctx, 0); err != nil || len(all) != 1 {
		t.Fatalf("zero-limit needs = %+v, %v", all, err)
	}

	if err := s.UpdateEnrichment(ctx, "t1", store.Enrichment{
		ASN: 64500, Prefix: "203.0.113.0/24", CDN: "akamai",
		ASCountry: "US", ASName: "EXAMPLE-AS, US",
		IXNames: []string{"AMS-IX", "DE-CIX Frankfurt"},
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.TargetsWithState(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].ASN != 64500 || rows[0].Prefix != "203.0.113.0/24" || rows[0].CDN != "akamai" {
		t.Fatalf("row = %+v", rows[0])
	}
	if rows[0].ASCountry != "US" || rows[0].ASName != "EXAMPLE-AS, US" {
		t.Fatalf("AS labels = %+v", rows[0])
	}
	if rows[0].IXNames != "AMS-IX, DE-CIX Frankfurt" {
		t.Fatalf("ix_names = %q", rows[0].IXNames)
	}

	// The detail query carries the same labels, not only the board query.
	detail, err := s.TargetByID(ctx, "local", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if detail.IXNames != rows[0].IXNames {
		t.Fatalf("detail ix_names = %q, board = %q", detail.IXNames, rows[0].IXNames)
	}

	// A learned target is no longer in the queue.
	needs, err = s.TargetsNeedingEnrichment(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(needs) != 0 {
		t.Fatalf("learned target still queued: %+v", needs)
	}

	// Updating an unknown target is a no-op, not an error.
	if err := s.UpdateEnrichment(ctx, "nope", store.Enrichment{ASN: 1, Prefix: "x", CDN: "y"}); err != nil {
		t.Fatalf("unknown target: %v", err)
	}
}

func TestEnrichmentRefreshAndTouch(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	if err := s.UpdateEnrichment(ctx, "t1", store.Enrichment{
		ASN: 64500, Prefix: "203.0.113.0/24", CDN: "akamai",
		ASCountry: "US", ASName: "EXAMPLE-AS, US",
	}); err != nil {
		t.Fatal(err)
	}

	// A recent enrichment is not due for a recheck.
	recent, err := s.TargetsNeedingRefresh(ctx, time.Now().Add(-time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 0 {
		t.Fatalf("recent target was queued for refresh: %+v", recent)
	}

	// A zero limit falls back to the documented batch size.
	if _, err := s.TargetsNeedingRefresh(ctx, time.Now().Add(time.Minute), 0); err != nil {
		t.Fatalf("zero-limit refresh: %v", err)
	}

	// A cutoff in the future makes it due, with the learned prefix attached.
	due, err := s.TargetsNeedingRefresh(ctx, time.Now().Add(time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != "t1" || due[0].Prefix != "203.0.113.0/24" || due[0].ASN != 64500 {
		t.Fatalf("due = %+v", due)
	}

	// Touching must not blank the labels it did not change.
	if err := s.TouchEnrichment(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.TargetsWithState(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].ASN != 64500 || rows[0].CDN != "akamai" || rows[0].ASName != "EXAMPLE-AS, US" {
		t.Fatalf("touch erased enrichment: %+v", rows[0])
	}

	// Touching an unknown target is a no-op.
	if err := s.TouchEnrichment(ctx, "nope"); err != nil {
		t.Fatalf("unknown target: %v", err)
	}
}

func TestCorroborationMatchesScopeAndWindow(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	inc := store.Incident{
		ID: "inc-ir", OpenedAt: base, Verdict: "INCIDENT",
		Scope: "country", ScopeValue: "IR", Headline: "x", Confidence: "high",
	}
	if err := s.OpenIncident(ctx, inc, nil, nil); err != nil {
		t.Fatal(err)
	}

	events := []store.ExternalEvent{
		{
			Feed: "ioda", ExternalID: "ir-overlap", Kind: "outage", Severity: 0.9,
			Country: "IR", StartedAt: base.Add(-time.Hour), ObservedAt: base,
		},
		{
			Feed: "ioda", ExternalID: "us-overlap", Kind: "outage", Severity: 0.95,
			Country: "US", StartedAt: base.Add(-time.Hour), ObservedAt: base,
		},
		{
			Feed: "ioda", ExternalID: "ir-old", Kind: "outage", Severity: 0.8,
			Country: "IR", StartedAt: base.Add(-48 * time.Hour),
			EndedAt: base.Add(-47 * time.Hour), ObservedAt: base,
		},
	}
	if err := s.UpsertExternalEvents(ctx, events); err != nil {
		t.Fatal(err)
	}

	got, err := s.Corroboration(ctx, inc, base.Add(time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ExternalID != "ir-overlap" {
		t.Fatalf("country corroboration = %+v", got)
	}

	// A sector incident has no country or ASN, so the window is the only
	// filter and severity orders the result.
	inc.Scope = "sector"
	inc.ScopeValue = "airline"
	got, err = s.Corroboration(ctx, inc, base.Add(time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("sector corroboration = %d, want the two overlapping events", len(got))
	}
	if got[0].ExternalID != "us-overlap" {
		t.Fatalf("ordering = %+v, want severity first", got)
	}

	// A closed incident's window ends at closed_at, so a later event does not
	// leak in.
	closed := inc
	closed.ClosedAt = base.Add(10 * time.Minute)
	if err := s.CloseIncident(ctx, closed.ID, "cleared", closed.ClosedAt); err != nil {
		t.Fatal(err)
	}
	late := store.ExternalEvent{
		Feed: "ioda", ExternalID: "late", Kind: "outage", Severity: 1,
		Country: "IR", StartedAt: base.Add(time.Hour), ObservedAt: base,
	}
	if err := s.UpsertExternalEvents(ctx, []store.ExternalEvent{late}); err != nil {
		t.Fatal(err)
	}
	got, err = s.Corroboration(ctx, closed, base.Add(2*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range got {
		if e.ExternalID == "late" {
			t.Fatalf("an event after the close leaked in: %+v", got)
		}
	}
}

func TestCertificateObservationsRoundTrip(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	tlsProbe := probeAt(now, state.Up)
	tlsProbe.Stage = "tls"
	tlsProbe.CertIssuer = "CN=R11,O=Let's Encrypt,C=US"
	tlsProbe.CertNotAfter = now.Add(60 * 24 * time.Hour)

	if err := s.Record(ctx, []store.Probe{tlsProbe}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.TargetsWithState(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	got := rows[0]
	if got.CertIssuer != tlsProbe.CertIssuer {
		t.Fatalf("issuer = %q", got.CertIssuer)
	}
	if !got.CertNotAfter.Equal(tlsProbe.CertNotAfter) {
		t.Fatalf("not_after = %v, want %v", got.CertNotAfter, tlsProbe.CertNotAfter)
	}

	// The detail query carries the same observation, and the stage.
	row, err := s.TargetByID(ctx, "local", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if row.CertIssuer != tlsProbe.CertIssuer || !row.CertNotAfter.Equal(tlsProbe.CertNotAfter) {
		t.Fatalf("detail cert = %q %v", row.CertIssuer, row.CertNotAfter)
	}
	if row.Stage != "tls" {
		t.Fatalf("detail stage = %q", row.Stage)
	}

	// A later non-TLS probe must not resurrect the old certificate.
	plain := probeAt(now.Add(time.Minute), state.Up)
	if err := s.Record(ctx, []store.Probe{plain}); err != nil {
		t.Fatal(err)
	}
	rows, err = s.TargetsWithState(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].CertIssuer != "" || !rows[0].CertNotAfter.IsZero() {
		t.Fatalf("a non-TLS probe carried a certificate: %+v", rows[0])
	}
}

func TestAnnotateIncident(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	ctx := context.Background()

	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	inc := store.Incident{
		ID: "inc-a", OpenedAt: base, Verdict: "INCIDENT",
		Scope: "sector", ScopeValue: "airline", Headline: "x", Confidence: "high",
	}
	if err := s.OpenIncident(ctx, inc, nil, nil); err != nil {
		t.Fatal(err)
	}

	at := base.Add(time.Hour)
	if err := s.AnnotateIncident(ctx, "inc-a", "AWS us-east-1, confirmed", at); err != nil {
		t.Fatal(err)
	}

	got, _, _, err := s.IncidentByID(ctx, "inc-a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Annotation != "AWS us-east-1, confirmed" {
		t.Fatalf("annotation = %q", got.Annotation)
	}
	if !got.AnnotatedAt.Equal(at) {
		t.Fatalf("annotated_at = %v, want %v", got.AnnotatedAt, at)
	}

	// A second annotation replaces the first; the incident keeps the latest
	// ground truth rather than a history.
	at2 := at.Add(time.Hour)
	if err := s.AnnotateIncident(ctx, "inc-a", "actually the ISP", at2); err != nil {
		t.Fatal(err)
	}
	got, _, _, err = s.IncidentByID(ctx, "inc-a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Annotation != "actually the ISP" || !got.AnnotatedAt.Equal(at2) {
		t.Fatalf("updated incident = %+v", got)
	}

	// An unknown id is an error, not a silent no-op.
	if err := s.AnnotateIncident(ctx, "nope", "x", at); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown id error = %v, want sql.ErrNoRows", err)
	}
}

func TestCertPinLearnsAndFlagsOnce(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	const first = "CN=R11,O=Let's Encrypt,C=US"
	const rotated = "CN=GTS CA 1P5,O=Google Trust Services LLC,C=US"

	if known, err := s.CertPinKnown(ctx, "t1"); err != nil || known {
		t.Fatalf("known before any observation = %v, %v", known, err)
	}

	// The first issuer is a baseline, not a change.
	changed, err := s.ObserveCertIssuer(ctx, "t1", first, now)
	if err != nil || changed {
		t.Fatalf("first observation = (%v, %v)", changed, err)
	}
	if known, err := s.CertPinKnown(ctx, "t1"); err != nil || !known {
		t.Fatalf("known after baseline = %v, %v", known, err)
	}

	// The same issuer again is still not a change, and it refreshes last_seen.
	changed, err = s.ObserveCertIssuer(ctx, "t1", first, now.Add(time.Hour))
	if err != nil || changed {
		t.Fatalf("repeat observation = (%v, %v)", changed, err)
	}

	// A different issuer is accepted on first sight and flagged exactly once.
	changed, err = s.ObserveCertIssuer(ctx, "t1", rotated, now.Add(2*time.Hour))
	if err != nil || !changed {
		t.Fatalf("new issuer = (%v, %v), want a one-time flag", changed, err)
	}
	changed, err = s.ObserveCertIssuer(ctx, "t1", rotated, now.Add(3*time.Hour))
	if err != nil || changed {
		t.Fatalf("accepted issuer re-flagged = (%v, %v)", changed, err)
	}

	// A different target starts its own baseline: one target's issuer set is
	// not evidence about another's.
	if err := s.UpsertTarget(ctx, store.Target{
		ID: "t2", Host: "united.com", Port: 443, Probe: "http",
		Name: "United", Class: "service", Tier: 2, IntervalS: 300,
	}); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.ObserveCertIssuer(ctx, "t2", rotated, now); err != nil || changed {
		t.Fatalf("second target baseline = (%v, %v)", changed, err)
	}

	// Missing arguments are errors, not silent no-ops.
	if _, err := s.ObserveCertIssuer(ctx, "", first, now); err == nil {
		t.Fatal("an empty target was accepted")
	}
	if _, err := s.ObserveCertIssuer(ctx, "t1", "   ", now); err == nil {
		t.Fatal("an empty issuer was accepted")
	}
}

func TestScopeCorroborationMatchesCountryOrASN(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	events := []store.ExternalEvent{
		{
			Feed: "ioda", ExternalID: "ir", Kind: "outage", Severity: 0.9,
			Country: "IR", StartedAt: base.Add(-time.Hour), ObservedAt: base,
		},
		{
			Feed: "radar", ExternalID: "asn", Kind: "hijack", Severity: 0.8,
			ASN: 64500, StartedAt: base.Add(-2 * time.Hour), ObservedAt: base,
		},
		{
			Feed: "ioda", ExternalID: "both", Kind: "outage", Severity: 0.95,
			Country: "US", ASN: 13335, StartedAt: base.Add(-time.Hour), ObservedAt: base,
		},
		{
			Feed: "ioda", ExternalID: "stale", Kind: "outage", Severity: 1,
			Country: "IR", StartedAt: base.Add(-48 * time.Hour), ObservedAt: base,
		},
	}
	if err := s.UpsertExternalEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	since := base.Add(-24 * time.Hour)

	// Country only: only the in-window IR event, not the 48-hour-old one.
	got, err := s.ScopeCorroboration(ctx, "IR", 0, since, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ExternalID != "ir" {
		t.Fatalf("country corroboration = %+v", got)
	}

	// ASN only: only the event that names it.
	got, err = s.ScopeCorroboration(ctx, "", 64500, since, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ExternalID != "asn" {
		t.Fatalf("asn corroboration = %+v", got)
	}

	// Both: a union, highest severity first.
	got, err = s.ScopeCorroboration(ctx, "US", 64500, since, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ExternalID != "both" {
		t.Fatalf("union corroboration = %+v", got)
	}

	// No scope is not an error and not a query.
	got, err = s.ScopeCorroboration(ctx, "", 0, since, 10)
	if err != nil || got != nil {
		t.Fatalf("empty scope = %+v, %v", got, err)
	}
}

func TestRecentCountryEventsKeepsTheWorstPerCountry(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	events := []store.ExternalEvent{
		{
			Feed: "ioda", ExternalID: "ir-low", Kind: "outage", Severity: 0.4,
			Country: "IR", StartedAt: base.Add(-time.Hour), ObservedAt: base,
		},
		{
			Feed: "radar", ExternalID: "ir-high", Kind: "hijack", Severity: 0.9,
			Country: "IR", StartedAt: base.Add(-2 * time.Hour), ObservedAt: base,
		},
		{
			Feed: "ioda", ExternalID: "de", Kind: "outage", Severity: 0.7,
			Country: "DE", StartedAt: base.Add(-time.Hour), ObservedAt: base,
		},
		{
			Feed: "ioda", ExternalID: "no-country", Kind: "outage", Severity: 1.0,
			StartedAt: base.Add(-time.Hour), ObservedAt: base,
		},
		{
			Feed: "ioda", ExternalID: "stale", Kind: "outage", Severity: 1.0,
			Country: "US", StartedAt: base.Add(-48 * time.Hour), ObservedAt: base,
		},
	}
	if err := s.UpsertExternalEvents(ctx, events); err != nil {
		t.Fatal(err)
	}

	got, err := s.RecentCountryEvents(ctx, base.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("countries = %d, want 2 (no-country and stale are excluded): %+v", len(got), got)
	}
	if got["IR"].ExternalID != "ir-high" {
		t.Fatalf("IR = %+v, want the highest-severity event", got["IR"])
	}
	if got["DE"].ExternalID != "de" {
		t.Fatalf("DE = %+v", got["DE"])
	}
	if _, ok := got["US"]; ok {
		t.Fatal("an event outside the window was included")
	}
}

func TestIncidentsPageUsesTheKeyNotAnOffset(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	open := func(id string, at time.Time) {
		t.Helper()
		inc := store.Incident{
			ID: id, OpenedAt: at, Verdict: "INCIDENT", Scope: "sector",
			ScopeValue: "airline", Headline: id, Confidence: "high",
		}
		if err := s.OpenIncident(ctx, inc, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	open("inc-a", base)
	open("inc-b", base.Add(time.Minute))
	open("inc-c", base.Add(2*time.Minute))

	// Newest first, and one extra row is the caller's job: the store returns
	// exactly what it was asked for.
	page, err := s.IncidentsPage(ctx, false, time.Time{}, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].ID != "inc-c" || page[1].ID != "inc-b" {
		t.Fatalf("first page = %+v", page)
	}

	// Resuming after inc-b excludes the boundary and everything newer, even
	// though inc-a is now the only older row.
	page, err = s.IncidentsPage(ctx, false, base.Add(time.Minute), "inc-b", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].ID != "inc-a" {
		t.Fatalf("second page = %+v", page)
	}

	// The tie-break is id descending, so a shared timestamp cannot repeat or
	// skip a row between pages.
	open("inc-d", base.Add(3*time.Minute))
	open("inc-e", base.Add(3*time.Minute))
	page, err = s.IncidentsPage(ctx, false, time.Time{}, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].ID != "inc-e" {
		t.Fatalf("tie first = %+v", page)
	}
	page, err = s.IncidentsPage(ctx, false, base.Add(3*time.Minute), "inc-e", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].ID != "inc-d" {
		t.Fatalf("tie second = %+v", page)
	}

	// openOnly filters closed incidents out of both the page and the cursor
	// walk.
	if err := s.CloseIncident(ctx, "inc-e", "cleared", base.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	page, err = s.IncidentsPage(ctx, true, time.Time{}, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, inc := range page {
		if inc.ID == "inc-e" {
			t.Fatalf("a closed incident appeared in the open page: %+v", page)
		}
	}

	// A non-positive limit falls back to the documented page size.
	page, err = s.IncidentsPage(ctx, true, time.Time{}, "", 0)
	if err != nil || len(page) == 0 {
		t.Fatalf("default limit = %d rows, %v", len(page), err)
	}
}

func TestCorridorsUpsertSamplesAndEvents(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	if err := s.UpsertCorridors(ctx, []store.Corridor{
		{ID: "nyc-lon", From: "NYC", To: "LON"},
		{ID: "lon-sin", From: "LON", To: "SIN"},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.Corridors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "lon-sin" || got[1].ID != "nyc-lon" {
		t.Fatalf("corridors = %+v", got)
	}

	// Two samples, oldest first, with the failure preserved as loss.
	if err := s.RecordCorridorSample(ctx, store.CorridorSample{
		CorridorID: "nyc-lon", TS: base, RTTMS: 80, Loss: 0, OK: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCorridorSample(ctx, store.CorridorSample{
		CorridorID: "nyc-lon", TS: base.Add(5 * time.Minute), RTTMS: 120, Loss: 20, OK: true,
	}); err != nil {
		t.Fatal(err)
	}

	samples, err := s.CorridorSamples(ctx, "nyc-lon", base.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 || !samples[0].TS.Equal(base) || samples[1].RTTMS != 120 || samples[1].Loss != 20 {
		t.Fatalf("samples = %+v", samples)
	}

	// A sample outside the window is not returned.
	if samples, err := s.CorridorSamples(ctx, "nyc-lon", base.Add(time.Minute)); err != nil || len(samples) != 1 {
		t.Fatalf("windowed samples = %+v, %v", samples, err)
	}

	// The series query returns every corridor at once, oldest first, which is
	// what the page's 24-hour shapes read.
	series, err := s.CorridorSeries(ctx, base.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(series["nyc-lon"]) != 2 || !series["nyc-lon"][0].TS.Equal(base) {
		t.Fatalf("series = %+v", series)
	}
	if len(series["lon-sin"]) != 0 {
		t.Fatalf("a corridor with no samples appeared in the series: %+v", series["lon-sin"])
	}

	// Status reports the newest sample and no open event yet.
	status, err := s.CorridorStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(status) != 2 {
		t.Fatalf("status = %+v", status)
	}
	lonSin, nycLon := status[0], status[1]
	if nycLon.Latest == nil || nycLon.Latest.RTTMS != 120 || nycLon.Open != nil {
		t.Fatalf("nyc-lon status = %+v", nycLon)
	}
	if lonSin.Latest != nil {
		t.Fatalf("lon-sin has a sample it never recorded: %+v", lonSin.Latest)
	}

	// Opening an event is visible and closing it clears the open flag.
	if err := s.OpenCorridorEvent(ctx, store.CorridorEvent{
		ID: "ev-1", CorridorID: "nyc-lon", Kind: "rtt_step",
		StartedAt: base, Detail: "rtt 120ms against 80ms",
		Trace: " 1  192.0.2.1  4ms\ndestination 192.0.2.9 reached=true\n",
	}); err != nil {
		t.Fatal(err)
	}
	status, err = s.CorridorStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status[1].Open == nil || status[1].Open.Kind != "rtt_step" {
		t.Fatalf("open event not reported: %+v", status[1])
	}
	if !strings.Contains(status[1].Open.Trace, "reached=true") {
		t.Fatalf("open event lost its trace: %+v", status[1].Open)
	}

	if err := s.CloseCorridorEvent(ctx, "nyc-lon", base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	status, err = s.CorridorStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status[1].Open != nil {
		t.Fatalf("closed event still open: %+v", status[1].Open)
	}

	events, err := s.CorridorEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || !events[0].EndedAt.Equal(base.Add(time.Hour)) {
		t.Fatalf("events = %+v", events)
	}

	// A corridor removed from the config is disabled, not deleted: its history
	// survives, but it is no longer in the enabled list.
	if err := s.UpsertCorridors(ctx, []store.Corridor{{ID: "nyc-lon", From: "NYC", To: "LON"}}); err != nil {
		t.Fatal(err)
	}
	got, err = s.Corridors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "nyc-lon" {
		t.Fatalf("after removal = %+v", got)
	}
}

func TestSeriesForTargetsReadsManyTargetsInOneQuery(t *testing.T) {
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

	now := time.Date(2026, 11, 17, 12, 30, 0, 0, time.UTC)
	base := now.Add(-3 * time.Hour)
	record := func(id string, at time.Time, st state.State) {
		t.Helper()
		p := probeAt(at, st)
		p.TargetID = id
		if err := s.Record(ctx, []store.Probe{p}); err != nil {
			t.Fatal(err)
		}
	}
	record("t1", base, state.Up)
	record("t1", base.Add(5*time.Minute), state.Down)
	record("t2", base, state.Up)

	if _, err := s.Fold(ctx, now); err != nil {
		t.Fatal(err)
	}

	got, err := s.SeriesForTargets(ctx, []string{"t1", "t2", "missing"}, "local", "1h",
		base.Add(-time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got["t1"]) != 1 || got["t1"][0].N != 2 || got["t1"][0].Up != 1 {
		t.Fatalf("t1 = %+v", got["t1"])
	}
	if len(got["t2"]) != 1 || got["t2"][0].Up != 1 {
		t.Fatalf("t2 = %+v", got["t2"])
	}
	if _, ok := got["missing"]; ok {
		t.Fatal("a target with no rollups appeared in the result")
	}

	// No ids is an empty map, not an error.
	if empty, err := s.SeriesForTargets(ctx, nil, "local", "1h", base, now); err != nil || len(empty) != 0 {
		t.Fatalf("empty = %+v, %v", empty, err)
	}

	// An unknown resolution is refused rather than guessed.
	if _, err := s.SeriesForTargets(ctx, []string{"t1"}, "local", "2h", base, now); err == nil {
		t.Fatal("an unknown resolution was accepted")
	}
}

func TestCertPinReportsChainChangesInformational(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	const issuer = "CN=R11,O=Let's Encrypt,C=US"

	// The first observation records the chain as the baseline.
	changed, advisory, err := s.ObserveCert(ctx, store.CertObservation{
		TargetID: "t1", Issuer: issuer, ChainLength: 2, IntermediateSPKI: "aaaa",
	}, now)
	if err != nil || changed || advisory != "" {
		t.Fatalf("baseline = (%v, %q, %v)", changed, advisory, err)
	}

	// Same issuer, different chain length: informational, not a change.
	changed, advisory, err = s.ObserveCert(ctx, store.CertObservation{
		TargetID: "t1", Issuer: issuer, ChainLength: 3, IntermediateSPKI: "bbbb",
	}, now.Add(time.Hour))
	if err != nil || changed {
		t.Fatalf("chain change = (%v, %q, %v)", changed, advisory, err)
	}
	if !strings.Contains(advisory, "chain length changed") {
		t.Fatalf("advisory = %q", advisory)
	}

	// The new shape becomes the baseline, so repeating it is quiet.
	changed, advisory, err = s.ObserveCert(ctx, store.CertObservation{
		TargetID: "t1", Issuer: issuer, ChainLength: 3, IntermediateSPKI: "bbbb",
	}, now.Add(2*time.Hour))
	if err != nil || changed || advisory != "" {
		t.Fatalf("repeat = (%v, %q, %v)", changed, advisory, err)
	}

	// A different intermediate at the same length is also informational.
	changed, advisory, err = s.ObserveCert(ctx, store.CertObservation{
		TargetID: "t1", Issuer: issuer, ChainLength: 3, IntermediateSPKI: "cccc",
	}, now.Add(3*time.Hour))
	if err != nil || changed {
		t.Fatalf("intermediate change = (%v, %q, %v)", changed, advisory, err)
	}
	if !strings.Contains(advisory, "intermediate certificate changed") {
		t.Fatalf("advisory = %q", advisory)
	}

	// A new issuer is still the one-time flag, chain info notwithstanding.
	changed, advisory, err = s.ObserveCert(ctx, store.CertObservation{
		TargetID: "t1", Issuer: "CN=GTS CA 1P5,O=Google Trust Services LLC,C=US",
		ChainLength: 3, IntermediateSPKI: "cccc",
	}, now.Add(4*time.Hour))
	if err != nil || !changed || advisory != "" {
		t.Fatalf("new issuer = (%v, %q, %v)", changed, advisory, err)
	}

	// The read path exposes the learned shape: the longest chain and the most
	// recent intermediate.
	row, err := s.TargetByID(ctx, "local", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if row.CertChainLen != 3 || row.CertIntermediateSPKI != "cccc" {
		t.Fatalf("row chain = (%d, %q)", row.CertChainLen, row.CertIntermediateSPKI)
	}
}

func TestLeafFingerprintRecordsBaselineAndChange(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	// The first fingerprint is a baseline, not a change.
	changed, err := s.ObserveLeaf(ctx, "t1", "aaaa", now)
	if err != nil || changed {
		t.Fatalf("baseline = (%v, %v)", changed, err)
	}

	// Repeating it is quiet.
	changed, err = s.ObserveLeaf(ctx, "t1", "aaaa", now.Add(time.Hour))
	if err != nil || changed {
		t.Fatalf("repeat = (%v, %v)", changed, err)
	}

	// A rotation is reported once and becomes the new baseline.
	changed, err = s.ObserveLeaf(ctx, "t1", "bbbb", now.Add(2*time.Hour))
	if err != nil || !changed {
		t.Fatalf("rotation = (%v, %v)", changed, err)
	}
	changed, err = s.ObserveLeaf(ctx, "t1", "bbbb", now.Add(3*time.Hour))
	if err != nil || changed {
		t.Fatalf("repeat after rotation = (%v, %v)", changed, err)
	}

	// The read path exposes the learned fingerprint.
	row, err := s.TargetByID(ctx, "local", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if row.CertLeafFingerprint != "bbbb" {
		t.Fatalf("leaf fingerprint = %q", row.CertLeafFingerprint)
	}

	// Missing arguments are errors, not silent no-ops.
	if _, err := s.ObserveLeaf(ctx, "", "aaaa", now); err == nil {
		t.Fatal("an empty target was accepted")
	}
	if _, err := s.ObserveLeaf(ctx, "t1", "  ", now); err == nil {
		t.Fatal("an empty fingerprint was accepted")
	}
}

func TestDNSBaselinesAndDivergence(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	record := func(resolver string, at time.Time, addrs ...string) {
		t.Helper()
		if err := s.RecordDNSAnswer(ctx, "t1", dnsdiv.Answer{
			Resolver: resolver, At: at, Addresses: addrs,
		}); err != nil {
			t.Fatal(err)
		}
	}

	record("a:53", base, "192.0.2.1", "192.0.2.2")
	record("a:53", base.Add(time.Hour), "192.0.2.1")
	record("b:53", base, "198.51.100.7")

	baselines, err := s.DNSBaselines(ctx, "t1", base.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(baselines) != 2 {
		t.Fatalf("baselines = %+v", baselines)
	}
	if len(baselines["a:53"].Addresses) != 2 || baselines["a:53"].Samples != 2 {
		t.Fatalf("a baseline = %+v", baselines["a:53"])
	}
	if _, ok := baselines["a:53"].Addresses["192.0.2.2"]; !ok {
		t.Fatal("the address set lost an entry")
	}
	if _, ok := baselines["b:53"].Addresses["198.51.100.7"]; !ok {
		t.Fatalf("b baseline = %+v", baselines["b:53"])
	}

	// The window excludes an old answer, which is what stops an ancient
	// baseline from being treated as current.
	future, err := s.DNSBaselines(ctx, "t1", base.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(future["a:53"].Addresses) != 1 {
		t.Fatalf("windowed baseline = %+v", future["a:53"])
	}

	// Origin ASNs, hosting suffixes, and the delegation live in the same
	// baseline, keyed by kind, and are read back into the sets the engine
	// compares.
	if err := s.RecordDNSAnswer(ctx, "t1", dnsdiv.Answer{
		Resolver: "a:53", At: base,
		Addresses:     []string{"192.0.2.9"},
		ASNs:          []int{13335, 0},
		CNAMESuffixes: []string{"cloudflare.net", "cloudflare.net", ""},
		Delegations:   []string{"ns1.example.net", "ns1.example.net", "ns2.example.net"},
	}); err != nil {
		t.Fatal(err)
	}
	withLabels, err := s.DNSBaselines(ctx, "t1", base.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := withLabels["a:53"].ASNs[13335]; !ok {
		t.Fatalf("ASN set = %+v", withLabels["a:53"].ASNs)
	}
	if len(withLabels["a:53"].CNAMESuffixes) != 1 {
		t.Fatalf("suffix set = %+v", withLabels["a:53"].CNAMESuffixes)
	}
	if _, dup := withLabels["a:53"].CNAMESuffixes[""]; dup {
		t.Fatal("an empty suffix was stored")
	}
	if len(withLabels["a:53"].Delegations) != 2 {
		t.Fatalf("delegation set = %+v", withLabels["a:53"].Delegations)
	}
	if _, ok := withLabels["a:53"].Delegations["ns2.example.net"]; !ok {
		t.Fatalf("delegation set lost an entry: %+v", withLabels["a:53"].Delegations)
	}
	if len(withLabels["a:53"].Addresses) != 3 {
		t.Fatalf("address set = %+v", withLabels["a:53"].Addresses)
	}

	// An answer without a resolver is a programming error; no addresses is a
	// legitimate no-op (a failure says nothing about the baseline).
	if err := s.RecordDNSAnswer(ctx, "t1", dnsdiv.Answer{Addresses: []string{"192.0.2.9"}}); err == nil {
		t.Fatal("an answer without a resolver was accepted")
	}
	if err := s.RecordDNSAnswer(ctx, "t1", dnsdiv.Answer{Resolver: "a:53", At: base}); err != nil {
		t.Fatalf("an empty answer errored: %v", err)
	}

	// An empty verdict is not stored; a real one is visible on the read path.
	if err := s.RecordDNSDivergence(ctx, "t1", dnsdiv.Verdict{}, base); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDNSDivergence(ctx, "t1", dnsdiv.Verdict{
		Kind: "DNS_ANOMALY", Confidence: "medium",
		Detail: "one resolver disagrees with its own baseline",
	}, base); err != nil {
		t.Fatal(err)
	}

	row, err := s.TargetByID(ctx, "local", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if row.DNSVerdict != "DNS_ANOMALY" || row.DNSConfidence != "medium" {
		t.Fatalf("divergence row = %+v", row)
	}
	if !row.DNSAt.Equal(base) {
		t.Fatalf("divergence at = %v, want %v", row.DNSAt, base)
	}

	// Pruning drops baselines older than the cutoff, and nothing else.
	dropped, err := s.PruneDNSObservations(ctx, base.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if dropped == 0 {
		t.Fatal("prune removed nothing")
	}
	remaining, err := s.DNSBaselines(ctx, "t1", base.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for resolver, base := range remaining {
		for addr := range base.Addresses {
			if addr != "192.0.2.1" {
				t.Fatalf("%s still has a pruned address %q", resolver, addr)
			}
		}
	}
}

func TestRecentDNSDivergencesIsNewestFirst(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	record := func(kind, confidence, detail string, at time.Time) {
		t.Helper()
		if err := s.RecordDNSDivergence(ctx, "t1", dnsdiv.Verdict{
			Kind: kind, Confidence: confidence, Detail: detail,
		}, at); err != nil {
			t.Fatal(err)
		}
	}
	record("DNS_ANOMALY", "medium", "first", base)
	record("TAMPER", "high", "second", base.Add(time.Minute))

	got, err := s.RecentDNSDivergences(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("records = %+v", got)
	}
	if got[0].Detail != "second" || got[0].Verdict != "TAMPER" {
		t.Fatalf("newest = %+v", got[0])
	}
	if got[0].Host != "chase.com" || got[0].TargetID != "t1" {
		t.Fatalf("record target = %+v", got[0])
	}
	if !got[1].TS.Equal(base) {
		t.Fatalf("oldest ts = %v", got[1].TS)
	}

	if one, err := s.RecentDNSDivergences(ctx, 1); err != nil || len(one) != 1 {
		t.Fatalf("limit = %+v, %v", one, err)
	}
	if none, err := s.RecentDNSDivergences(ctx, 0); err != nil || len(none) != 2 {
		t.Fatalf("zero limit = %+v, %v", none, err)
	}
}

func TestProviderCorroborationMatchesTheDeclaredPage(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	events := []store.ExternalEvent{
		{
			Feed: "statuspage", ExternalID: "status.example.com:inc-1", Kind: "statuspage",
			Severity: 0.8, StartedAt: base, ObservedAt: base,
		},
		{
			Feed: "statuspage", ExternalID: "other.example:inc-2", Kind: "statuspage",
			Severity: 0.9, StartedAt: base, ObservedAt: base,
		},
		{
			Feed: "statuspage", ExternalID: "status.example.com:old", Kind: "statuspage",
			Severity: 1.0, StartedAt: base.Add(-48 * time.Hour), ObservedAt: base,
		},
	}
	if err := s.UpsertExternalEvents(ctx, events); err != nil {
		t.Fatal(err)
	}

	got, err := s.ProviderCorroboration(ctx, "https://status.example.com",
		base.Add(-24*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ExternalID != "status.example.com:inc-1" {
		t.Fatalf("provider corroboration = %+v", got)
	}

	// A full summary URL still matches by host, which is the key the
	// statuspage adapter stores.
	got, err = s.ProviderCorroboration(ctx, "https://status.example.com/api/v2/summary.json",
		base.Add(-24*time.Hour), 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("normalised URL = %+v, %v", got, err)
	}

	// No host is a programming error, not an empty result.
	if _, err := s.ProviderCorroboration(ctx, "", base, 10); err == nil {
		t.Fatal("an empty status URL was accepted")
	}
}

func TestResetDNSBaselinesClearsAnswersAndKeepsVerdicts(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()
	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	if err := s.RecordDNSAnswer(ctx, "t1", dnsdiv.Answer{
		Resolver: "a:53", At: base,
		Addresses: []string{"192.0.2.1"}, ASNs: []int{13335},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDNSDivergence(ctx, "t1", dnsdiv.Verdict{
		Kind: "DNS_ANOMALY", Confidence: "medium", Detail: "before the move",
	}, base); err != nil {
		t.Fatal(err)
	}

	dropped, err := s.ResetDNSBaselines(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dropped == 0 {
		t.Fatal("the reset removed nothing")
	}

	baselines, err := s.DNSBaselines(ctx, "t1", base.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(baselines) != 0 {
		t.Fatalf("baselines after reset = %+v", baselines)
	}

	// Verdicts are history, not a baseline, so they survive the reset.
	records, err := s.RecentDNSDivergences(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Detail != "before the move" {
		t.Fatalf("verdicts after reset = %+v", records)
	}

	// A second reset is a no-op rather than an error.
	if dropped, err := s.ResetDNSBaselines(ctx); err != nil || dropped != 0 {
		t.Fatalf("second reset = %d, %v", dropped, err)
	}
}
