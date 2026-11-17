package verdict_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
	"github.com/Greg-2600/netweather/internal/verdict"
)

func base() time.Time {
	return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
}

// healthy is the input set for a monitor that has been running for an hour,
// probing, with a full baseline and nothing wrong.
func healthy() verdict.Inputs {
	now := base()
	return verdict.Inputs{
		Now: now, StartedAt: now.Add(-time.Hour),
		LastProbe: now.Add(-10 * time.Second),
		Probed:    10, ReferenceTotal: 3,
		BaselineSamples: 28,
	}
}

func TestEvaluateRules(t *testing.T) {
	t.Parallel()

	now := base()

	tests := []struct {
		name   string
		mutate func(*verdict.Inputs)
		want   verdict.Verdict
	}{
		{"ok", func(*verdict.Inputs) {}, verdict.OK},
		{"starting", func(in *verdict.Inputs) {
			in.Probed = 0
			in.LastProbe = time.Time{}
			in.StartedAt = now.Add(-time.Second)
		}, verdict.Starting},
		{"stale with no probe", func(in *verdict.Inputs) {
			in.LastProbe = time.Time{}
		}, verdict.Stale},
		{"stale past three reference intervals", func(in *verdict.Inputs) {
			in.LastProbe = now.Add(-91 * time.Second)
		}, verdict.Stale},
		{"tamper", func(in *verdict.Inputs) {
			in.TamperAt = now.Add(-5 * time.Minute)
		}, verdict.Tamper},
		{"local", func(in *verdict.Inputs) {
			in.ReferenceTotal = 3
			in.ReferenceFailing = 2
		}, verdict.Local},
		{"regional", func(in *verdict.Inputs) {
			in.CrossVantage = true
		}, verdict.Regional},
		{"incident", func(in *verdict.Inputs) {
			in.OpenIncidents = 2
		}, verdict.Incident},
		{"learning", func(in *verdict.Inputs) {
			in.BaselineSamples = 12
		}, verdict.Learning},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in := healthy()
			tc.mutate(&in)
			got := verdict.Evaluate(in)
			if got.Verdict != tc.want {
				t.Fatalf("verdict = %s (%s), want %s", got.Verdict, got.Detail, tc.want)
			}
			if got.Detail == "" {
				t.Fatal("a verdict without a detail line is not explainable")
			}
		})
	}
}

func TestEvaluatePriority(t *testing.T) {
	t.Parallel()

	// A blind monitor outranks every interpretation.
	in := healthy()
	in.LastProbe = time.Time{}
	in.TamperAt = base().Add(-time.Minute)
	in.OpenIncidents = 1
	if got := verdict.Evaluate(in); got.Verdict != verdict.Stale {
		t.Fatalf("verdict = %s, want STALE to outrank the rest", got.Verdict)
	}

	// A tamper outranks a plain outage and the local-path claim.
	in = healthy()
	in.TamperAt = base().Add(-time.Minute)
	in.OpenIncidents = 1
	in.ReferenceTotal, in.ReferenceFailing = 3, 3
	if got := verdict.Evaluate(in); got.Verdict != verdict.Tamper {
		t.Fatalf("verdict = %s, want TAMPER", got.Verdict)
	}

	// The local path outranks the cluster it may be casting.
	in = healthy()
	in.ReferenceTotal, in.ReferenceFailing = 3, 2
	in.OpenIncidents = 1
	if got := verdict.Evaluate(in); got.Verdict != verdict.Local {
		t.Fatalf("verdict = %s, want LOCAL", got.Verdict)
	}

	// An incident outranks learning: a cluster is real even on a young baseline.
	in = healthy()
	in.OpenIncidents = 1
	in.BaselineSamples = 0
	if got := verdict.Evaluate(in); got.Verdict != verdict.Incident {
		t.Fatalf("verdict = %s, want INCIDENT", got.Verdict)
	}
}

func TestEvaluateIgnoresAnOldTamperAndASmallReferenceSet(t *testing.T) {
	t.Parallel()

	in := healthy()
	in.TamperAt = base().Add(-2 * time.Hour)
	if got := verdict.Evaluate(in); got.Verdict != verdict.OK {
		t.Fatalf("verdict = %s, want OK for a tamper outside the window", got.Verdict)
	}

	// Two reference targets is too small a set to call "is it me?".
	in = healthy()
	in.ReferenceTotal = 2
	in.ReferenceFailing = 2
	if got := verdict.Evaluate(in); got.Verdict != verdict.OK {
		t.Fatalf("verdict = %s, want OK for a two-target reference set", got.Verdict)
	}
}

type fakeStore struct {
	last        time.Time
	counts      map[state.State]int
	reference   map[state.State]int
	sectors     int
	asns        int
	open        int
	divergences []store.DNSDivergenceRecord
	cross       bool
	history     []float64
	err         error
}

func (f *fakeStore) LastProbeAt(context.Context, string) (time.Time, error) {
	return f.last, f.err
}

func (f *fakeStore) StateCounts(context.Context, string) (map[state.State]int, error) {
	return f.counts, f.err
}

func (f *fakeStore) ReferenceStates(context.Context, string) (map[state.State]int, error) {
	return f.reference, f.err
}

func (f *fakeStore) FailureSpread(context.Context, string) (int, int, error) {
	return f.sectors, f.asns, f.err
}

func (f *fakeStore) OpenIncidentCount(context.Context) (int, error) { return f.open, f.err }

func (f *fakeStore) RecentDNSDivergences(context.Context, int) ([]store.DNSDivergenceRecord, error) {
	return f.divergences, f.err
}

func (f *fakeStore) CrossVantageDisagreement(context.Context) (bool, error) {
	return f.cross, f.err
}

func (f *fakeStore) ScopeHistory(context.Context, string, string, time.Time) ([]float64, error) {
	return f.history, f.err
}

func TestBuildFromLiveState(t *testing.T) {
	t.Parallel()

	now := base()
	src := &fakeStore{
		last:      now.Add(-10 * time.Second),
		counts:    map[state.State]int{state.Up: 8, state.Down: 2},
		reference: map[state.State]int{state.Up: 3},
		history:   make([]float64, 28),
	}

	got := (&verdict.Builder{
		Store: src, Vantage: "local", StartedAt: now.Add(-time.Hour),
		Now: func() time.Time { return now },
	}).Build(context.Background())
	if got.Verdict != verdict.OK {
		t.Fatalf("verdict = %s (%s), want OK", got.Verdict, got.Detail)
	}

	// A tamper verdict on the newest record wins over the quiet counts.
	src.divergences = []store.DNSDivergenceRecord{
		{TS: now.Add(-time.Hour), Verdict: "DNS_ANOMALY"},
		{TS: now.Add(-2 * time.Minute), Verdict: "TAMPER"},
	}
	got = (&verdict.Builder{
		Store: src, Vantage: "local", StartedAt: now.Add(-time.Hour),
		Now: func() time.Time { return now },
	}).Build(context.Background())
	if got.Verdict != verdict.Tamper {
		t.Fatalf("verdict = %s, want TAMPER", got.Verdict)
	}
}

func TestBuildTreatsReadFailuresAsAbsentSignals(t *testing.T) {
	t.Parallel()

	now := base()
	src := &fakeStore{err: errors.New("disk gone")}

	got := (&verdict.Builder{
		Store: src, Vantage: "local", StartedAt: now.Add(-time.Hour),
		Now: func() time.Time { return now },
	}).Build(context.Background())

	// Every read failed, so nothing can be called healthy or failing. With no
	// probe time the only honest answer is that the monitor cannot speak.
	if got.Verdict != verdict.Stale {
		t.Fatalf("verdict = %s (%s), want STALE", got.Verdict, got.Detail)
	}
}

func TestFailureSpreadIsTheSecondaryLocalHeuristic(t *testing.T) {
	t.Parallel()

	// Five sectors and ten networks at once is the documented secondary
	// heuristic: spread that wide is more likely this path than ten unrelated
	// services.
	in := healthy()
	in.FailureSectors = 5
	in.FailureASNs = 10
	got := verdict.Evaluate(in)
	if got.Verdict != verdict.Local {
		t.Fatalf("verdict = %s, want LOCAL", got.Verdict)
	}
	if !strings.Contains(got.Detail, "possibly your connection") {
		t.Fatalf("detail = %q, want the qualified wording", got.Detail)
	}

	// One short of either bound is a cluster, not a path problem.
	for _, tc := range []struct {
		sectors, asns int
	}{
		{4, 10},
		{5, 9},
	} {
		in := healthy()
		in.FailureSectors, in.FailureASNs = tc.sectors, tc.asns
		if got := verdict.Evaluate(in); got.Verdict == verdict.Local {
			t.Errorf("%d sectors/%d ASNs reported LOCAL", tc.sectors, tc.asns)
		}
	}
}

func TestBuildPassesTheFailureSpread(t *testing.T) {
	t.Parallel()

	now := base()
	src := &fakeStore{
		last:      now.Add(-10 * time.Second),
		counts:    map[state.State]int{state.Up: 20, state.Down: 20},
		reference: map[state.State]int{state.Up: 3},
		sectors:   6,
		asns:      12,
		history:   make([]float64, 28),
	}
	got := (&verdict.Builder{
		Store: src, Vantage: "local", StartedAt: now.Add(-time.Hour),
		Now: func() time.Time { return now },
	}).Build(context.Background())
	if got.Verdict != verdict.Local || !strings.Contains(got.Detail, "6 sectors and 12 ASNs") {
		t.Fatalf("verdict = %s (%s), want the spread-based LOCAL", got.Verdict, got.Detail)
	}
}
