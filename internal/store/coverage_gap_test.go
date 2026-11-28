package store_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/dnsdiv"
	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
)

// setMeasurability writes the calibration column directly. It is normally set
// by RefreshMeasurability, but a test needs a deterministic value without
// manufacturing a week of blocked rollups.
func setMeasurability(t *testing.T, path, id, value string) {
	t.Helper()

	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()

	if _, err := raw.ExecContext(context.Background(),
		`UPDATE target SET measurability = ? WHERE id = ?`, value, id); err != nil {
		t.Fatalf("set measurability for %s: %v", id, err)
	}
}

func TestScopeStatesFreshFiltersStaleTargets(t *testing.T) {
	t.Parallel()

	s, path := openTest(t)
	ctx := context.Background()
	if err := s.EnsureVantage(ctx, "local", "direct", "primary"); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	targets := []struct {
		id       string
		interval int
		probeAgo time.Duration
		probed   bool
		state    state.State
		blind    bool
	}{
		{id: "fresh", interval: 300, probeAgo: 10 * time.Minute, probed: true, state: state.Up},
		{id: "stale", interval: 300, probeAgo: 11 * time.Minute, probed: true, state: state.Down},
		{id: "long-fresh", interval: 1200, probeAgo: 14 * time.Minute, probed: true, state: state.Degraded},
		{id: "long-stale", interval: 1200, probeAgo: 16 * time.Minute, probed: true, state: state.Down},
		{id: "never", interval: 300},
		{id: "blind", interval: 300, probeAgo: 0, probed: true, state: state.Up, blind: true},
	}

	var probes []store.Probe
	for _, tt := range targets {
		if err := s.UpsertTarget(ctx, store.Target{
			ID: tt.id, Host: tt.id + ".example", Port: 443, Probe: "http",
			Name: tt.id, Class: "service", Tier: 2, IntervalS: tt.interval,
		}); err != nil {
			t.Fatal(err)
		}
		if tt.probed {
			probes = append(probes, store.Probe{
				TargetID: tt.id, VantageID: "local", TS: now.Add(-tt.probeAgo),
				State: tt.state, Stage: "http", Confidence: "high",
			})
		}
		if tt.blind {
			setMeasurability(t, path, tt.id, "unmeasurable")
		}
	}
	if err := s.Record(ctx, probes); err != nil {
		t.Fatal(err)
	}

	// The detection variant keeps only targets inside their own freshness
	// window. The blind target is excluded from both views.
	fresh, judgeable, err := s.ScopeStatesFresh(ctx, "local", "global", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if judgeable != 2 {
		t.Fatalf("fresh judgeable = %d, want 2", judgeable)
	}
	if fresh[state.Up] != 1 || fresh[state.Degraded] != 1 || fresh[state.Down] != 0 {
		t.Fatalf("fresh counts = %v", fresh)
	}

	// Coverage's unfiltered variant still counts the stale and never-probed
	// targets, because a missing probe is exactly the blind spot it reports.
	all, judgeable, err := s.ScopeStates(ctx, "local", "global", "")
	if err != nil {
		t.Fatal(err)
	}
	if judgeable != 5 {
		t.Fatalf("unfiltered judgeable = %d, want 5", judgeable)
	}
	if all[state.Up] != 1 || all[state.Degraded] != 1 || all[state.Down] != 2 {
		t.Fatalf("unfiltered counts = %v", all)
	}

	// Invalid inputs are rejected before the query runs.
	if _, _, err := s.ScopeStatesFresh(ctx, "local", "asn", "not-a-number", now); err == nil {
		t.Fatal("a non-numeric ASN was accepted")
	}
	if _, _, err := s.ScopeStatesFresh(ctx, "local", "planet", "", now); err == nil {
		t.Fatal("an unknown scope was accepted")
	}
}

func TestCoverageSkipsCoveredBucketsAndSortsBlindSpots(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	ctx := context.Background()
	if err := s.EnsureVantage(ctx, "local", "direct", "primary"); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	type target struct {
		id      string
		country string
		asn     int
		probed  bool
	}
	var targets []target
	for i := 0; i < 5; i++ {
		targets = append(targets, target{id: "aa-" + string(rune('0'+i)), country: "AA", asn: 64501, probed: true})
	}
	for i := 0; i < 5; i++ {
		targets = append(targets, target{id: "bb-" + string(rune('0'+i)), country: "BB", asn: 64500})
	}
	for i := 0; i < 6; i++ {
		targets = append(targets, target{id: "cc-" + string(rune('0'+i)), country: "CC"})
	}
	for i := 0; i < 5; i++ {
		targets = append(targets, target{id: "dd-" + string(rune('0'+i)), country: "DD"})
	}

	var probes []store.Probe
	for _, tt := range targets {
		if err := s.UpsertTarget(ctx, store.Target{
			ID: tt.id, Host: tt.id + ".example", Port: 443, Probe: "http",
			Name: tt.id, Class: "service", Country: tt.country, Sector: "banking",
			Tier: 2, IntervalS: 300,
		}); err != nil {
			t.Fatal(err)
		}
		if tt.asn > 0 {
			if err := s.UpdateEnrichment(ctx, tt.id, store.Enrichment{ASN: tt.asn}); err != nil {
				t.Fatal(err)
			}
		}
		if tt.probed {
			probes = append(probes, store.Probe{
				TargetID: tt.id, VantageID: "local", TS: now,
				State: state.Up, Stage: "http", Confidence: "high",
			})
		}
	}
	if err := s.Record(ctx, probes); err != nil {
		t.Fatal(err)
	}

	cov, err := s.Coverage(ctx, "local", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if cov.Totals.Total != 21 || cov.Totals.Measurable != 5 {
		t.Fatalf("totals = %+v, want 21 targets with 5 measured", cov.Totals)
	}

	// AA is fully measured and must not be called a blind spot. The rest are
	// ordered biggest first, then by kind and value so the panel is stable.
	want := []struct {
		kind  string
		value string
		total int
	}{
		{kind: "country", value: "CC", total: 6},
		{kind: "asn", value: "64500", total: 5},
		{kind: "country", value: "BB", total: 5},
		{kind: "country", value: "DD", total: 5},
	}
	if len(cov.BlindSpots) != len(want) {
		t.Fatalf("blind spots = %+v, want %d entries", cov.BlindSpots, len(want))
	}
	for i, w := range want {
		got := cov.BlindSpots[i]
		if got.Kind != w.kind || got.Value != w.value || got.Total != w.total {
			t.Fatalf("blind spot %d = %+v, want %s=%s total %d", i, got, w.kind, w.value, w.total)
		}
	}
}

func TestCoverageOnAnEmptyDatabase(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	cov, err := s.Coverage(context.Background(), "local", now)
	if err != nil {
		t.Fatal(err)
	}
	if cov.Totals != (store.CoverageTotals{}) {
		t.Fatalf("totals = %+v, want zero", cov.Totals)
	}
	if len(cov.Classes) != 0 || len(cov.Sectors) != 0 || len(cov.BlindSpots) != 0 || len(cov.Stale) != 0 {
		t.Fatalf("empty coverage = %+v", cov)
	}
	if cov.Concentration != (store.CoverageConcentration{}) {
		t.Fatalf("concentration = %+v, want zero", cov.Concentration)
	}
}

func TestSnapshotCoverageCountsUnmeasurableScopes(t *testing.T) {
	t.Parallel()

	s, path := openTest(t)
	ctx := context.Background()
	if err := s.EnsureVantage(ctx, "local", "direct", "primary"); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	targets := []store.Target{
		{ID: "t1", Host: "t1.example", Port: 443, Probe: "http", Name: "t1", Class: "service", Sector: "energy", Country: "US", Tier: 2, IntervalS: 300},
		{ID: "t2", Host: "t2.example", Port: 443, Probe: "http", Name: "t2", Class: "service", Sector: "energy", Country: "US", Tier: 2, IntervalS: 300},
		{ID: "t3", Host: "t3.example", Port: 443, Probe: "http", Name: "t3", Class: "service", Sector: "blind", Country: "US", Tier: 2, IntervalS: 300},
	}
	for _, target := range targets {
		if err := s.UpsertTarget(ctx, target); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Record(ctx, []store.Probe{probeAt(now, state.Up)}); err != nil {
		t.Fatal(err)
	}
	setMeasurability(t, path, "t2", "unmeasurable")
	setMeasurability(t, path, "t3", "unmeasurable")

	if err := s.SnapshotCoverage(ctx, "local", now); err != nil {
		t.Fatal(err)
	}

	global, err := s.CoverageHistory(ctx, "global", "", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(global) != 1 || global[0].Total != 3 || global[0].Measurable != 1 || global[0].Unmeasurable != 2 {
		t.Fatalf("global = %+v, want total 3, measurable 1, unmeasurable 2", global)
	}

	energy, err := s.CoverageHistory(ctx, "sector", "energy", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(energy) != 1 || energy[0].Total != 2 || energy[0].Measurable != 1 || energy[0].Unmeasurable != 1 {
		t.Fatalf("energy = %+v", energy)
	}

	// A scope made only of unmeasurable targets has nothing judgeable, so it is
	// skipped rather than written as a zero-total snapshot.
	blind, err := s.CoverageHistory(ctx, "sector", "blind", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(blind) != 0 {
		t.Fatalf("blind-only sector = %+v, want no snapshot", blind)
	}

	country, err := s.CoverageHistory(ctx, "country", "US", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(country) != 1 || country[0].Total != 3 || country[0].Unmeasurable != 2 {
		t.Fatalf("country = %+v", country)
	}
}

func TestCoverageHistoryDayClamping(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	for _, at := range []time.Time{now.AddDate(0, 0, -2), now.AddDate(0, 0, -1), now} {
		if err := s.SnapshotCoverage(ctx, "local", at); err != nil {
			t.Fatal(err)
		}
	}

	for _, days := range []int{0, -5, 400} {
		points, err := s.CoverageHistory(ctx, "global", "", days)
		if err != nil {
			t.Fatalf("days=%d: %v", days, err)
		}
		if len(points) != 3 {
			t.Fatalf("days=%d returned %d points, want all 3", days, len(points))
		}
	}

	day := now.UTC().Truncate(24 * time.Hour)
	points, err := s.CoverageHistory(ctx, "global", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 2 || !points[0].Bucket.Equal(day.AddDate(0, 0, -1)) ||
		!points[1].Bucket.Equal(day) {
		t.Fatalf("days=2 = %+v", points)
	}

	empty, err := s.CoverageHistory(ctx, "sector", "missing", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("missing scope = %+v, want empty", empty)
	}
}

func TestExportProbesStopsOnCallbackError(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	if err := s.Record(ctx, []store.Probe{probeAt(now, state.Up)}); err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("stop exporting")
	err := s.ExportProbes(ctx, now.Add(-time.Minute), now.Add(time.Minute), func(store.ExportRow) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("ExportProbes error = %v, want the callback sentinel", err)
	}
}

func TestUpsertCountryLevelsEmptyIsANoOp(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	ctx := context.Background()

	if err := s.UpsertCountryLevels(ctx, nil); err != nil {
		t.Fatalf("nil batch: %v", err)
	}
	if err := s.UpsertCountryLevels(ctx, []store.CountryLevel{}); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
	levels, err := s.CountryLevels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(levels) != 0 {
		t.Fatalf("levels = %+v, want empty", levels)
	}
}

func TestOpenIncidentRejectsBadBatchesAtomically(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	base := store.Incident{
		OpenedAt: now, Verdict: "INCIDENT", Headline: "x", Confidence: "high",
	}
	open := func(id, scope, value string, targets []string, attrs []store.Attribution) error {
		inc := base
		inc.ID, inc.Scope, inc.ScopeValue = id, scope, value
		return s.OpenIncident(ctx, inc, targets, attrs)
	}

	if err := open("inc-dup", "sector", "dup", []string{"t1"}, nil); err != nil {
		t.Fatal(err)
	}
	// A duplicate primary key fails the incident insert and rolls the whole
	// transaction back.
	if err := open("inc-dup", "sector", "dup-again", []string{"t1"}, nil); err == nil {
		t.Fatal("a duplicate incident id was accepted")
	}
	if err := open("inc-fk", "sector", "fk", []string{"missing-target"}, nil); err == nil {
		t.Fatal("an unknown target was accepted")
	}
	// Two attributions at the same rank violate the primary key.
	if err := open("inc-rank", "sector", "rank", []string{"t1"}, []store.Attribution{
		{Rank: 1, Kind: "sector", Value: "airline", Failing: 1, Total: 1},
		{Rank: 1, Kind: "asn", Value: "64500", Failing: 1, Total: 1},
	}); err == nil {
		t.Fatal("duplicate attribution ranks were accepted")
	}

	// Neither failed open may leave an incident or its children behind.
	for _, tc := range []struct{ scope, value string }{
		{"sector", "fk"},
		{"sector", "rank"},
	} {
		found, err := s.HasOpenIncident(ctx, tc.scope, tc.value)
		if err != nil {
			t.Fatal(err)
		}
		if found {
			t.Fatalf("%s=%s survived a failed transaction", tc.scope, tc.value)
		}
	}
	if openCount, err := s.OpenIncidentCount(ctx); err != nil || openCount != 1 {
		t.Fatalf("open incidents = %d, %v; want only the first", openCount, err)
	}
}

func TestStoreErrorsAfterClose(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		call func() error
	}{
		{"Anomalies", func() error { _, err := s.Anomalies(ctx, 10); return err }},
		{"AnnotateIncident", func() error { return s.AnnotateIncident(ctx, "i1", "x", now) }},
		{"AtypicalDayScopes", func() error { _, err := s.AtypicalDayScopes(ctx, "2026-11-17"); return err }},
		{"Backup", func() error { return s.Backup(ctx, filepath.Join(t.TempDir(), "backup.db")) }},
		{"CertPinKnown", func() error { _, err := s.CertPinKnown(ctx, "t1"); return err }},
		{"CloseCorridorEvent", func() error { return s.CloseCorridorEvent(ctx, "c1", now) }},
		{"CloseIncident", func() error { return s.CloseIncident(ctx, "i1", "reason", now) }},
		{"ColdStartAt", func() error { _, err := s.ColdStartAt(ctx); return err }},
		{"CorridorEvents", func() error { _, err := s.CorridorEvents(ctx, 10); return err }},
		{"CorridorSamples", func() error { _, err := s.CorridorSamples(ctx, "c1", now); return err }},
		{"CorridorSeries", func() error { _, err := s.CorridorSeries(ctx, now); return err }},
		{"CorridorStatus", func() error { _, err := s.CorridorStatus(ctx); return err }},
		{"Corridors", func() error { _, err := s.Corridors(ctx); return err }},
		{"Corroboration", func() error {
			_, err := s.Corroboration(ctx, store.Incident{OpenedAt: now}, now, 10)
			return err
		}},
		{"CountProbes", func() error { _, err := s.CountProbes(ctx); return err }},
		{"CountryLevels", func() error { _, err := s.CountryLevels(ctx); return err }},
		{"Coverage", func() error { _, err := s.Coverage(ctx, "local", now); return err }},
		{"CoverageHistory", func() error { _, err := s.CoverageHistory(ctx, "global", "", 30); return err }},
		{"CrossVantageDisagreement", func() error { _, err := s.CrossVantageDisagreement(ctx); return err }},
		{"DNSBaselines", func() error { _, err := s.DNSBaselines(ctx, "t1", now); return err }},
		{"DisableTarget", func() error { return s.DisableTarget(ctx, "t1") }},
		{"EnsureVantage", func() error { return s.EnsureVantage(ctx, "local", "direct", "primary") }},
		{"ExportProbes", func() error {
			return s.ExportProbes(ctx, now.Add(-time.Hour), now, func(store.ExportRow) error { return nil })
		}},
		{"ExternalEvents", func() error { _, err := s.ExternalEvents(ctx, 10); return err }},
		{"FailureSpread", func() error { _, _, err := s.FailureSpread(ctx, "local"); return err }},
		{"FeedStates", func() error { _, err := s.FeedStates(ctx); return err }},
		{"FleetHealth", func() error { _, err := s.FleetHealth(ctx, "local", now); return err }},
		{"FleetStats", func() error { _, err := s.FleetStats(ctx, "local"); return err }},
		{"Fold", func() error { _, err := s.Fold(ctx, now); return err }},
		{"HasOpenIncident", func() error { _, err := s.HasOpenIncident(ctx, "sector", "airline"); return err }},
		{"IncidentByID", func() error { _, _, _, err := s.IncidentByID(ctx, "i1"); return err }},
		{"IncidentProbes", func() error { _, err := s.IncidentProbes(ctx, "i1", 10); return err }},
		{"Incidents", func() error { _, err := s.Incidents(ctx, false, 10); return err }},
		{"IncidentsPage", func() error { _, err := s.IncidentsPage(ctx, false, now, "", 10); return err }},
		{"LastProbeAt", func() error { _, err := s.LastProbeAt(ctx, "local"); return err }},
		{"LatestSelfChecks", func() error { _, err := s.LatestSelfChecks(ctx); return err }},
		{"LatestVantageChange", func() error { _, _, err := s.LatestVantageChange(ctx); return err }},
		{"CreateMaintenanceWindow", func() error {
			_, err := s.CreateMaintenanceWindow(ctx, "dns", "work", now, now.Add(time.Hour))
			return err
		}},
		{"CompleteMaintenanceWindow", func() error { return s.CompleteMaintenanceWindow(ctx, 1, now) }},
		{"MaintenanceWindows", func() error { _, err := s.MaintenanceWindows(ctx, 10); return err }},
		{"MarkAtypicalDay", func() error {
			_, err := s.MarkAtypicalDay(ctx, "2026-11-17", "", "", "calendar", now)
			return err
		}},
		{"Observations", func() error { _, err := s.Observations(ctx, "local"); return err }},
		{"ObserveCert", func() error {
			_, _, err := s.ObserveCert(ctx, store.CertObservation{TargetID: "t1", Issuer: "CN=x"}, now)
			return err
		}},
		{"ObserveLeaf", func() error { _, err := s.ObserveLeaf(ctx, "t1", "fingerprint", now); return err }},
		{"OpenCorridorEvent", func() error {
			return s.OpenCorridorEvent(ctx, store.CorridorEvent{
				ID: "e1", CorridorID: "c1", Kind: "rtt_step", StartedAt: now,
			})
		}},
		{"OpenIncident", func() error {
			return s.OpenIncident(ctx, store.Incident{
				ID: "i1", OpenedAt: now, Verdict: "INCIDENT", Scope: "global",
				Headline: "x", Confidence: "high",
			}, nil, nil)
		}},
		{"OpenIncidentCount", func() error { _, err := s.OpenIncidentCount(ctx); return err }},
		{"OpenIncidents", func() error { _, err := s.OpenIncidents(ctx); return err }},
		{"ProbeDetail", func() error { _, err := s.ProbeDetail(ctx, "t1", "local"); return err }},
		{"ProviderCorroboration", func() error {
			_, err := s.ProviderCorroboration(ctx, "https://status.example", now, 10)
			return err
		}},
		{"Prune", func() error { _, err := s.Prune(ctx, now); return err }},
		{"PruneDNSObservations", func() error { _, err := s.PruneDNSObservations(ctx, now); return err }},
		{"RecentCountryEvents", func() error { _, err := s.RecentCountryEvents(ctx, now); return err }},
		{"RecentDNSDivergences", func() error { _, err := s.RecentDNSDivergences(ctx, 10); return err }},
		{"RecentProbes", func() error { _, err := s.RecentProbes(ctx, "t1", "local", 10); return err }},
		{"Record", func() error {
			return s.Record(ctx, []store.Probe{probeAt(now, state.Up)})
		}},
		{"RecordAnomaly", func() error {
			return s.RecordAnomaly(ctx, store.Anomaly{TS: now, Scope: "global", Kind: "coverage"})
		}},
		{"RecordCorridorSample", func() error {
			return s.RecordCorridorSample(ctx, store.CorridorSample{CorridorID: "c1", TS: now})
		}},
		{"RecordDNSAnswer", func() error {
			return s.RecordDNSAnswer(ctx, "t1", dnsdiv.Answer{
				Resolver: "a:53", At: now, Addresses: []string{"192.0.2.1"},
			})
		}},
		{"RecordDNSDivergence", func() error {
			return s.RecordDNSDivergence(ctx, "t1", dnsdiv.Verdict{Kind: "DNS_ANOMALY"}, now)
		}},
		{"RecordFeedFailure", func() error { return s.RecordFeedFailure(ctx, "feed", "detail", now) }},
		{"RecordFeedSuccess", func() error { return s.RecordFeedSuccess(ctx, "feed", now) }},
		{"RecordSelfCheck", func() error { return s.RecordSelfCheck(ctx, store.SelfCheck{TS: now, Check: "disk"}) }},
		{"RecordTargetSource", func() error {
			return s.RecordTargetSource(ctx, "t1", "config", "sha", now)
		}},
		{"RecordVantageChange", func() error { return s.RecordVantageChange(ctx, "a", "b", now) }},
		{"ReferenceStates", func() error { _, err := s.ReferenceStates(ctx, "local"); return err }},
		{"RefreshMeasurability", func() error { _, err := s.RefreshMeasurability(ctx, "local", now); return err }},
		{"ResetDNSBaselines", func() error { _, err := s.ResetDNSBaselines(ctx); return err }},
		{"RollupAt", func() error { _, err := s.RollupAt(ctx, "t1", "local", now); return err }},
		{"RollupFor", func() error { _, err := s.RollupFor(ctx, "5m", "t1", "local", now); return err }},
		{"SampleScopes", func() error { return s.SampleScopes(ctx, "local", now) }},
		{"SchemaVersion", func() error { _, err := s.SchemaVersion(ctx); return err }},
		{"ScopeCorroboration", func() error { _, err := s.ScopeCorroboration(ctx, "US", 0, now, 10); return err }},
		{"ScopeCoverageHistory", func() error { _, err := s.ScopeCoverageHistory(ctx, "global", "", now); return err }},
		{"ScopeHistory", func() error { _, err := s.ScopeHistory(ctx, "global", "", now); return err }},
		{"ScopeStates", func() error { _, _, err := s.ScopeStates(ctx, "local", "global", ""); return err }},
		{"ScopeStatesFresh", func() error { _, _, err := s.ScopeStatesFresh(ctx, "local", "global", "", now); return err }},
		{"Scopes", func() error { _, err := s.Scopes(ctx); return err }},
		{"Series", func() error { _, err := s.Series(ctx, "t1", "local", "5m", now, now); return err }},
		{"SeriesForTargets", func() error {
			_, err := s.SeriesForTargets(ctx, []string{"t1"}, "local", "5m", now, now)
			return err
		}},
		{"SetFeedNextPoll", func() error { return s.SetFeedNextPoll(ctx, "feed", now) }},
		{"SnapshotCoverage", func() error { return s.SnapshotCoverage(ctx, "local", now) }},
		{"StateChangeCount", func() error { _, err := s.StateChangeCount(ctx, "t1", "local"); return err }},
		{"StateChanges", func() error { _, err := s.StateChanges(ctx, "t1", "local", 10); return err }},
		{"StateCounts", func() error { _, err := s.StateCounts(ctx, "local"); return err }},
		{"States", func() error { _, err := s.States(ctx, "local"); return err }},
		{"Stats", func() error { _, err := s.Stats(ctx); return err }},
		{"TargetByID", func() error { _, err := s.TargetByID(ctx, "local", "t1"); return err }},
		{"TargetExists", func() error { _, err := s.TargetExists(ctx, "t1"); return err }},
		{"TargetIDs", func() error { _, err := s.TargetIDs(ctx); return err }},
		{"TargetSources", func() error { _, err := s.TargetSources(ctx, "t1"); return err }},
		{"TargetVantages", func() error { _, err := s.TargetVantages(ctx, "t1"); return err }},
		{"TargetsNeedingEnrichment", func() error { _, err := s.TargetsNeedingEnrichment(ctx, 10); return err }},
		{"TargetsNeedingRefresh", func() error { _, err := s.TargetsNeedingRefresh(ctx, now, 10); return err }},
		{"TargetsWithState", func() error { _, err := s.TargetsWithState(ctx, "local"); return err }},
		{"TouchEnrichment", func() error { return s.TouchEnrichment(ctx, "t1") }},
		{"UpdateEnrichment", func() error { return s.UpdateEnrichment(ctx, "t1", store.Enrichment{ASN: 64500}) }},
		{"UpsertCorridors", func() error {
			return s.UpsertCorridors(ctx, []store.Corridor{{ID: "c1", From: "a", To: "b"}})
		}},
		{"UpsertCountryLevels", func() error {
			return s.UpsertCountryLevels(ctx, []store.CountryLevel{{Country: "US", ObservedAt: now}})
		}},
		{"UpsertExternalEvents", func() error {
			return s.UpsertExternalEvents(ctx, []store.ExternalEvent{{
				Feed: "feed", ExternalID: "e1", Kind: "outage", StartedAt: now, ObservedAt: now,
			}})
		}},
		{"UpsertTarget", func() error {
			return s.UpsertTarget(ctx, store.Target{
				ID: "t1", Host: "chase.com", Port: 443, Probe: "http",
				Name: "Chase", Class: "service", Tier: 2, IntervalS: 300,
			})
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err == nil {
				t.Fatalf("%s returned nil after Close", tc.name)
			}
		})
	}
}

func TestOpenRejectsUnusablePathsAndReportsItsPath(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	s, path := openTest(t)
	if got := s.Path(); got != path {
		t.Fatalf("Path() = %q, want %q", got, path)
	}

	// A directory is not a database file: the ping in openPool fails.
	dir := t.TempDir()
	asDir := filepath.Join(dir, "database")
	if err := os.Mkdir(asDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(ctx, asDir); err == nil {
		t.Fatal("Open accepted a directory as the database")
	}
	if _, err := store.OpenReadOnly(ctx, asDir); err == nil {
		t.Fatal("OpenReadOnly accepted a directory as the database")
	}

	// A regular file cannot be the parent directory.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(ctx, filepath.Join(blocker, "netweather.db")); err == nil {
		t.Fatal("Open accepted a file as the parent directory")
	}
}
