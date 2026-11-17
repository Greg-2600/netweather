package ihi_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/ihi"
	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
)

func at(minutes int) time.Time {
	return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC).Add(time.Duration(minutes) * time.Minute)
}

func TestComputeWeightsAndRenormalises(t *testing.T) {
	t.Parallel()

	now := at(0)
	index := ihi.Compute([]ihi.Component{
		{Name: "Own health", Weight: 0.75, Score: 80, At: now, OK: true, Coverage: 1},
		{Name: "External", Weight: 0.25, Score: 40, At: now, OK: true, Coverage: 1},
	}, ihi.Options{Now: now})

	if index.Score != 70 {
		t.Fatalf("score = %v, want 70", index.Score)
	}
	if len(index.Parts) != 2 || len(index.Dropped) != 0 {
		t.Fatalf("parts = %+v dropped = %+v", index.Parts, index.Dropped)
	}
	if index.Parts[0].Name != "Own health" || index.Parts[0].Weight != 0.75 {
		t.Fatalf("first part = %+v, want the heavier one first", index.Parts[0])
	}

	// A dropped component must not drag the score toward zero: the rest is
	// renormalised over its own weights.
	stale := ihi.Compute([]ihi.Component{
		{Name: "Own health", Weight: 0.75, Score: 80, At: now, OK: true, Coverage: 1},
		{Name: "External", Weight: 0.25, Score: 40, At: now.Add(-2 * time.Hour), OK: true, Coverage: 1},
	}, ihi.Options{Now: now})
	if stale.Score != 80 {
		t.Fatalf("score with a stale component = %v, want 80", stale.Score)
	}
	if len(stale.Dropped) != 1 || stale.Dropped[0].Reason != "stale" {
		t.Fatalf("dropped = %+v, want one stale", stale.Dropped)
	}
}

func TestComputeDropsUnavailableComponents(t *testing.T) {
	t.Parallel()

	now := at(0)
	index := ihi.Compute([]ihi.Component{
		{Name: "a", Weight: 0.2, Score: 100, At: now, OK: false, Reason: "read failed"},
		{Name: "b", Weight: 0, Score: 100, At: now, OK: true},
		{Name: "c", Weight: 0.2, Score: 100, OK: true},
		{Name: "d", Weight: 0.2, Score: -1, At: now, OK: true},
		{Name: "e", Weight: 0.2, Score: 100, At: now, OK: true, Coverage: 1},
	}, ihi.Options{Now: now})

	if index.Score != 100 {
		t.Fatalf("score = %v, want only the usable component", index.Score)
	}
	reasons := map[string]string{}
	for _, d := range index.Dropped {
		reasons[d.Name] = d.Reason
	}
	for name, want := range map[string]string{
		"a": "read failed",
		"b": "no weight",
		"c": "no timestamp",
		"d": "no score",
	} {
		if reasons[name] != want {
			t.Errorf("drop %q = %q, want %q", name, reasons[name], want)
		}
	}
}

func TestOwnHealthCoverageRule(t *testing.T) {
	t.Parallel()

	now := at(0)

	// 60% measurable contributes 60% of its weight: 0.35 * 0.6 = 0.21, with the
	// other component at 0.25. Score = (0.21*100 + 0.25*0)/0.46.
	index := ihi.Compute([]ihi.Component{
		{Name: "Own health", Weight: 0.35, Score: 100, At: now, OK: true, Coverage: 0.6},
		{Name: "External", Weight: 0.25, Score: 0, At: now, OK: true, Coverage: 1},
	}, ihi.Options{Now: now})
	want := 100 * 0.21 / 0.46
	if diff := index.Score - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("score = %v, want %v", index.Score, want)
	}

	// Below 50% the component is dropped entirely, because the board cannot
	// see enough of the scope to speak for it.
	blind := ihi.Compute([]ihi.Component{
		{Name: "Own health", Weight: 0.35, Score: 100, At: now, OK: true, Coverage: 0.49},
		{Name: "External", Weight: 0.25, Score: 40, At: now, OK: true, Coverage: 1},
	}, ihi.Options{Now: now})
	if blind.Score != 40 {
		t.Fatalf("score = %v, want the external component alone", blind.Score)
	}
	if len(blind.Dropped) != 1 || blind.Dropped[0].Reason != "coverage below 50%" {
		t.Fatalf("dropped = %+v", blind.Dropped)
	}
}

func TestComputeWithNothingScoreable(t *testing.T) {
	t.Parallel()

	index := ihi.Compute(nil, ihi.Options{Now: at(0)})
	if index.Score != 0 || len(index.Parts) != 0 {
		t.Fatalf("empty index = %+v", index)
	}
}

func TestComputeClampsScores(t *testing.T) {
	t.Parallel()

	now := at(0)
	index := ihi.Compute([]ihi.Component{
		{Name: "too high", Weight: 1, Score: 140, At: now, OK: true, Coverage: 1},
	}, ihi.Options{Now: now})
	if index.Score != 100 {
		t.Fatalf("score = %v, want the clamp to 100", index.Score)
	}
}

type fakeStore struct {
	coverage    store.Coverage
	counts      map[state.State]int
	events      []store.ExternalEvent
	corridors   []store.CorridorStatus
	divergences []store.DNSDivergenceRecord
	err         error
}

func (f *fakeStore) Coverage(context.Context, string, time.Time) (store.Coverage, error) {
	return f.coverage, f.err
}

func (f *fakeStore) StateCounts(context.Context, string) (map[state.State]int, error) {
	return f.counts, f.err
}

func (f *fakeStore) ExternalEvents(context.Context, int) ([]store.ExternalEvent, error) {
	return f.events, f.err
}

func (f *fakeStore) CorridorStatus(context.Context) ([]store.CorridorStatus, error) {
	return f.corridors, f.err
}

func (f *fakeStore) RecentDNSDivergences(context.Context, int) ([]store.DNSDivergenceRecord, error) {
	return f.divergences, f.err
}

func TestBuildFromLiveState(t *testing.T) {
	t.Parallel()

	now := at(0)
	src := &fakeStore{
		coverage: store.Coverage{Totals: store.CoverageTotals{Total: 10, Measurable: 9, Unmeasurable: 1}},
		counts: map[state.State]int{
			state.Up: 6, state.UpReachability: 1, state.Degraded: 1, state.Down: 1,
			state.Blocked: 1,
		},
		events: []store.ExternalEvent{
			{Kind: "outage", Severity: 0.4, StartedAt: now.Add(-time.Hour)},
			{Kind: "hijack", Severity: 0.2, StartedAt: now.Add(-2 * time.Hour)},
		},
		corridors: []store.CorridorStatus{
			{Latest: &store.CorridorSample{TS: now.Add(-10 * time.Minute)}, Open: nil},
			{Latest: &store.CorridorSample{TS: now.Add(-5 * time.Minute)}, Open: &store.CorridorEvent{ID: "e1"}},
		},
		divergences: []store.DNSDivergenceRecord{
			{TS: now.Add(-time.Hour), Verdict: "TAMPER"},
		},
	}

	index := (&ihi.Builder{Store: src, Vantage: "local", Now: func() time.Time { return now }}).Build(context.Background())
	if len(index.Dropped) != 0 {
		t.Fatalf("dropped = %+v", index.Dropped)
	}
	if len(index.Parts) != 5 {
		t.Fatalf("parts = %+v, want five", index.Parts)
	}

	scores := map[string]float64{}
	for _, part := range index.Parts {
		scores[part.Name] = part.Score
	}
	// Own health: (6 + 1 + 0.5) / 9 = 83.3; coverage 9/10 weighs it to 0.315.
	if got := scores["Own health"]; got < 83.3 || got > 83.4 {
		t.Errorf("own health = %v, want 83.3", got)
	}
	if got := scores["External outage severity"]; got != 60 {
		t.Errorf("external = %v, want 60", got)
	}
	if got := scores["BGP stability"]; got != 80 {
		t.Errorf("bgp = %v, want 80", got)
	}
	if got := scores["Corridor stability"]; got != 50 {
		t.Errorf("corridors = %v, want 50", got)
	}
	if got := scores["Tamper signals"]; got != 80 {
		t.Errorf("tamper = %v, want 80", got)
	}
}

func TestBuildDropsWhatItCannotSee(t *testing.T) {
	t.Parallel()

	now := at(0)
	src := &fakeStore{
		coverage: store.Coverage{Totals: store.CoverageTotals{Total: 4, Measurable: 1}},
		counts:   map[state.State]int{state.Down: 1},
		// No corridors and no divergences: the corridor component must say so,
		// and tamper is a quiet 100 rather than a missing panel.
	}
	index := (&ihi.Builder{Store: src, Vantage: "local", Now: func() time.Time { return now }}).Build(context.Background())

	reasons := map[string]string{}
	for _, d := range index.Dropped {
		reasons[d.Name] = d.Reason
	}
	if reasons["Corridor stability"] != "no corridors configured" {
		t.Errorf("corridor drop = %q", reasons["Corridor stability"])
	}
	// 25% measurable is below the floor, so own health is dropped too.
	if reasons["Own health"] != "coverage below 50%" {
		t.Errorf("own health drop = %q", reasons["Own health"])
	}

	// A read error drops the component with its reason and leaves the rest.
	src.err = errors.New("disk gone")
	broken := (&ihi.Builder{Store: src, Vantage: "local", Now: func() time.Time { return now }}).Build(context.Background())
	if len(broken.Parts) != 0 || broken.Score != 0 {
		t.Fatalf("broken index = %+v, want an empty, stated answer", broken)
	}
	found := false
	for _, d := range broken.Dropped {
		if d.Name == "Own health" && d.Reason == "unavailable: disk gone" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a read error was not named: %+v", broken.Dropped)
	}
}

func TestBuildCoversWindowAndFilterBranches(t *testing.T) {
	t.Parallel()

	now := at(0)
	src := &fakeStore{
		coverage: store.Coverage{Totals: store.CoverageTotals{Total: 10, Measurable: 10}},
		// Nothing judgeable: only states that are not evidence.
		counts: map[state.State]int{state.Blocked: 2, state.Unmeasurable: 1},
		events: []store.ExternalEvent{
			// Only an end time: the ended-at branch of the window check.
			{Kind: "outage", Severity: 1.7, EndedAt: now.Add(-time.Hour)},
			// Only an observation time.
			{Kind: "leak", Severity: 0.1, ObservedAt: now.Add(-2 * time.Hour)},
			// Outside the window entirely.
			{Kind: "outage", Severity: 0.9, EndedAt: now.Add(-48 * time.Hour)},
		},
		corridors: []store.CorridorStatus{
			// No newest sample: a configured corridor the sampler has not
			// reached must drop the component.
			{Open: nil},
		},
		divergences: []store.DNSDivergenceRecord{
			// Older than the window, and a verdict that is not a signal.
			{TS: now.Add(-48 * time.Hour), Verdict: "TAMPER"},
			{TS: now.Add(-time.Hour), Verdict: "CONSISTENT"},
			{TS: now.Add(-time.Hour), Verdict: "DNS_ANOMALY"},
		},
	}

	index := (&ihi.Builder{Store: src, Vantage: "local", Now: func() time.Time { return now }}).Build(context.Background())

	reasons := map[string]string{}
	for _, d := range index.Dropped {
		reasons[d.Name] = d.Reason
	}
	if reasons["Own health"] != "no judgeable targets" {
		t.Errorf("own health = %q", reasons["Own health"])
	}
	if reasons["Corridor stability"] != "no corridor samples" {
		t.Errorf("corridors = %q", reasons["Corridor stability"])
	}

	scores := map[string]float64{}
	for _, part := range index.Parts {
		scores[part.Name] = part.Score
	}
	// The 1.7 severity is clamped to 1, so the component floors at 0.
	if got := scores["External outage severity"]; got != 0 {
		t.Errorf("external = %v, want the clamped 0", got)
	}
	// A leak observed two hours ago is inside the window.
	if got := scores["BGP stability"]; got != 90 {
		t.Errorf("bgp = %v, want 90", got)
	}
	// One DNS_ANOMALY in the window: 100 - 20.
	if got := scores["Tamper signals"]; got != 80 {
		t.Errorf("tamper = %v, want 80", got)
	}
}

func TestBuildDropsComponentsOnReadErrors(t *testing.T) {
	t.Parallel()

	now := at(0)
	src := &readErrorStore{err: errors.New("read failed")}
	index := (&ihi.Builder{Store: src, Vantage: "local", Now: func() time.Time { return now }}).Build(context.Background())

	reasons := map[string]string{}
	for _, d := range index.Dropped {
		reasons[d.Name] = d.Reason
	}
	for _, name := range []string{"Corridor stability", "Tamper signals"} {
		if !strings.HasPrefix(reasons[name], "unavailable: read failed") {
			t.Errorf("%s reason = %q, want the read error", name, reasons[name])
		}
	}
	// The embedded fake still answers the event reads, so those components
	// keep a quiet score: one failed reader must not take the others down.
	scores := map[string]float64{}
	for _, part := range index.Parts {
		scores[part.Name] = part.Score
	}
	if len(scores) != 2 || scores["External outage severity"] != 100 || scores["BGP stability"] != 100 {
		t.Fatalf("parts = %+v, want the two event components at 100", index.Parts)
	}
}

// readErrorStore fails the corridor and tamper reads specifically, so their
// error branches are reached without hiding the others.
type readErrorStore struct {
	fakeStore
	err error
}

func (r *readErrorStore) CorridorStatus(context.Context) ([]store.CorridorStatus, error) {
	return nil, r.err
}

func (r *readErrorStore) RecentDNSDivergences(context.Context, int) ([]store.DNSDivergenceRecord, error) {
	return nil, r.err
}
