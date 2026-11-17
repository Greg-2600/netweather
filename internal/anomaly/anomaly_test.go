package anomaly_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/anomaly"
	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
)

type fakeSource struct {
	scopes      []store.ScopeRef
	counts      map[string]map[state.State]int
	judgeable   map[string]int
	history     map[string][]float64
	coverage    map[string][]float64
	err         error
	statesErr   error
	historyErr  error
	coverageErr error
}

func (f *fakeSource) Scopes(context.Context) ([]store.ScopeRef, error) {
	return f.scopes, f.err
}

func (f *fakeSource) ScopeStatesFresh(_ context.Context, _, scope, value string, _ time.Time) (map[state.State]int, int, error) {
	if f.statesErr != nil {
		return nil, 0, f.statesErr
	}
	key := scope + "|" + value
	return f.counts[key], f.judgeable[key], nil
}

func (f *fakeSource) ScopeHistory(_ context.Context, scope, value string, _ time.Time) ([]float64, error) {
	if f.historyErr != nil {
		return nil, f.historyErr
	}
	return f.history[scope+"|"+value], nil
}

func (f *fakeSource) ScopeCoverageHistory(_ context.Context, scope, value string, _ time.Time) ([]float64, error) {
	if f.coverageErr != nil {
		return nil, f.coverageErr
	}
	return f.coverage[scope+"|"+value], nil
}

type fakeSink struct {
	mu        sync.Mutex
	anomalies []store.Anomaly
	err       error
}

func (f *fakeSink) RecordAnomaly(_ context.Context, a store.Anomaly) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.anomalies = append(f.anomalies, a)
	return nil
}

func normalHistory() []float64 {
	h := make([]float64, 28)
	for i := range h {
		h[i] = 0.9
	}
	return h
}

func healthy(up int) map[state.State]int { return map[state.State]int{state.Up: up} }

func newSource(counts map[state.State]int) *fakeSource {
	return &fakeSource{
		scopes: []store.ScopeRef{{Scope: "global", Value: ""}},
		counts: map[string]map[state.State]int{
			"global|": counts,
		},
		judgeable: map[string]int{"global|": 50},
		history:   map[string][]float64{"global|": normalHistory()},
		coverage:  map[string][]float64{"global|": normalCoverage()},
	}
}

// normalCoverage is a quiet, fully measurable history: 95% of the scope.
func normalCoverage() []float64 {
	h := make([]float64, 28)
	for i := range h {
		h[i] = 0.95
	}
	return h
}

func TestEvaluateTriggersAfterTwoBuckets(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	src := newSource(map[state.State]int{state.Up: 40, state.Down: 10}) // health 0.8
	sink := &fakeSink{}

	ev := anomaly.New(src, sink, "local")
	ev.Now = func() time.Time { return now }

	if n, err := ev.Evaluate(context.Background()); err != nil || n != 0 {
		t.Fatalf("first evaluation = (%d, %v)", n, err)
	}

	now = now.Add(5 * time.Minute)
	n, err := ev.Evaluate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(sink.anomalies) != 1 {
		t.Fatalf("second evaluation = (%d, %d anomalies)", n, len(sink.anomalies))
	}
	if sink.anomalies[0].Kind != "health_drop" || sink.anomalies[0].Buckets != 2 {
		t.Fatalf("anomaly = %+v", sink.anomalies[0])
	}
}

func TestEvaluateStaysSilentWhileLearning(t *testing.T) {
	t.Parallel()

	// Above the global absolute floor: a young baseline must not invent an
	// incident from an ordinary wobble (DESIGN.md §7.2).
	src := newSource(map[state.State]int{state.Up: 47, state.Down: 3}) // health 0.94
	src.history["global|"] = normalHistory()[:10]
	sink := &fakeSink{}

	ev := anomaly.New(src, sink, "local")
	ev.Now = func() time.Time { return time.Now() }

	for i := 0; i < 3; i++ {
		if n, _ := ev.Evaluate(context.Background()); n != 0 {
			t.Fatalf("learning phase emitted %d anomalies", n)
		}
	}
}

func TestEvaluateSkipsHealthDetectionBelowTheCoverageFloors(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	src := newSource(map[state.State]int{state.Up: 5, state.Unknown: 95}) // measured 5 < 8
	src.judgeable["global|"] = 100
	sink := &fakeSink{}

	ev := anomaly.New(src, sink, "local")
	ev.Now = func() time.Time { return now }

	// The health detector must stay silent: no numerator, no denominator, no
	// claim. The coverage advisory is the one that may speak, and it does.
	if n, _ := ev.Evaluate(context.Background()); n != 0 {
		t.Fatalf("the first pass emitted %d anomalies", n)
	}
	now = now.Add(5 * time.Minute)
	if n, _ := ev.Evaluate(context.Background()); n != 1 {
		t.Fatalf("the second pass emitted %d anomalies, want the coverage advisory", n)
	}
	if len(sink.anomalies) != 1 || sink.anomalies[0].Kind != "coverage_drop" {
		t.Fatalf("anomalies = %+v, want one coverage_drop", sink.anomalies)
	}
}

func TestEvaluateCooldown(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	src := newSource(map[state.State]int{state.Up: 40, state.Down: 10})
	sink := &fakeSink{}

	ev := anomaly.New(src, sink, "local")
	ev.Now = func() time.Time { return now }

	_, _ = ev.Evaluate(context.Background())
	now = now.Add(5 * time.Minute)
	_, _ = ev.Evaluate(context.Background()) // fires
	now = now.Add(5 * time.Minute)
	if n, _ := ev.Evaluate(context.Background()); n != 0 {
		t.Fatal("cooldown did not suppress the repeat")
	}

	// After the cooldown, one fresh low pair fires again.
	now = now.Add(31 * time.Minute)
	if n, _ := ev.Evaluate(context.Background()); n != 1 {
		t.Fatalf("post-cooldown trigger = %d, want 1", n)
	}
	now = now.Add(5 * time.Minute)
	if n, _ := ev.Evaluate(context.Background()); n != 0 {
		t.Fatal("a second trigger inside the new cooldown was recorded")
	}
}

func TestEvaluatePropagatesSinkFailure(t *testing.T) {
	t.Parallel()

	src := newSource(map[state.State]int{state.Up: 40, state.Down: 10})
	sink := &fakeSink{err: errors.New("disk full")}

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	ev := anomaly.New(src, sink, "local")
	ev.Now = func() time.Time { return now }

	_, _ = ev.Evaluate(context.Background())
	now = now.Add(5 * time.Minute)
	if _, err := ev.Evaluate(context.Background()); err == nil {
		t.Fatal("a sink failure must surface")
	}
}

func TestEvaluatePropagatesSourceFailure(t *testing.T) {
	t.Parallel()

	src := &fakeSource{err: errors.New("db down")}
	ev := anomaly.New(src, &fakeSink{}, "local")

	if _, err := ev.Evaluate(context.Background()); err == nil {
		t.Fatal("a source failure must surface")
	}
}

func TestEvaluatePropagatesStateAndHistoryFailures(t *testing.T) {
	t.Parallel()

	src := newSource(map[state.State]int{state.Up: 40, state.Down: 10})
	src.statesErr = errors.New("states down")
	if _, err := anomaly.New(src, &fakeSink{}, "local").Evaluate(context.Background()); err == nil {
		t.Fatal("a state read failure must surface")
	}

	src = newSource(map[state.State]int{state.Up: 40, state.Down: 10})
	src.historyErr = errors.New("history down")
	if _, err := anomaly.New(src, &fakeSink{}, "local").Evaluate(context.Background()); err == nil {
		t.Fatal("a history read failure must surface")
	}
}

func TestEvaluateSkipsEmptyScopes(t *testing.T) {
	t.Parallel()

	src := newSource(map[state.State]int{state.Up: 1})
	src.judgeable["global|"] = 0
	src.counts["global|"] = nil
	sink := &fakeSink{}

	ev := anomaly.New(src, sink, "local")
	if n, err := ev.Evaluate(context.Background()); err != nil || n != 0 {
		t.Fatalf("empty scope = (%d, %v)", n, err)
	}
}

func TestEvaluatorDefaults(t *testing.T) {
	t.Parallel()

	ev := &anomaly.Evaluator{Cooldown: 0, Now: nil}
	if ev.Cooldown != 0 {
		t.Fatal("test setup")
	}
	// A zero cooldown and nil Now must not panic when evaluated.
	src := newSource(map[state.State]int{state.Up: 40, state.Down: 10})
	ev2 := anomaly.New(src, &fakeSink{}, "local")
	ev2.Cooldown = 0
	ev2.Now = nil
	if _, err := ev2.Evaluate(context.Background()); err != nil {
		t.Fatalf("defaults: %v", err)
	}
}

func TestLoopStopsOnCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		anomaly.Loop(ctx, newSource(healthy(50)), &fakeSink{}, "local", time.Millisecond, nil)
		close(done)
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Loop did not stop after cancel")
	}
}

func TestEvaluateEmitsCoverageDropWhileTheHealthDetectorIsSilent(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	// 5% measurable: below the health floors, so only the coverage advisory may
	// speak. This is the bot-wall rollout shape (DESIGN.md §7.2).
	src := newSource(map[state.State]int{state.Up: 5, state.Unknown: 95})
	src.judgeable["global|"] = 100
	sink := &fakeSink{}

	ev := anomaly.New(src, sink, "local")
	ev.Now = func() time.Time { return now }

	if n, err := ev.Evaluate(context.Background()); err != nil || n != 0 {
		t.Fatalf("first evaluation = (%d, %v)", n, err)
	}

	now = now.Add(5 * time.Minute)
	n, err := ev.Evaluate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(sink.anomalies) != 1 {
		t.Fatalf("second evaluation = (%d, %d anomalies)", n, len(sink.anomalies))
	}

	got := sink.anomalies[0]
	if got.Kind != "coverage_drop" || got.Buckets != 2 {
		t.Fatalf("anomaly = %+v, want coverage_drop after two buckets", got)
	}
	if got.Observed > 0.1 || got.Baseline < 0.9 {
		t.Fatalf("coverage anomaly = %+v", got)
	}
	for _, a := range sink.anomalies {
		if a.Kind == "health_drop" {
			t.Fatal("a visibility drop was reported as a health drop")
		}
	}
}

func TestEvaluateStaysSilentOnCoverageWhileLearning(t *testing.T) {
	t.Parallel()

	src := newSource(map[state.State]int{state.Up: 5, state.Unknown: 95})
	src.judgeable["global|"] = 100
	src.coverage["global|"] = normalCoverage()[:10]
	sink := &fakeSink{}

	ev := anomaly.New(src, sink, "local")
	ev.Now = func() time.Time { return time.Now() }

	for i := 0; i < 3; i++ {
		if n, _ := ev.Evaluate(context.Background()); n != 0 {
			t.Fatalf("a short coverage history emitted %d anomalies", n)
		}
	}
}

// varyingHistory gives the detector a real spread, so widening the band has an
// observable effect. All-equal history has zero MAD and widens nothing.
func varyingHistory() []float64 {
	h := make([]float64, 28)
	for i := range h {
		if i%2 == 0 {
			h[i] = 0.85
		} else {
			h[i] = 0.95
		}
	}
	return h
}

func TestLowCoverageWidensTheBand(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	// Coverage 0.6 (30 of 50) and health 0.633: below the normal 3-sigma band,
	// inside the widened 4.5-sigma one. The documented rule says do not fire.
	src := newSource(map[state.State]int{state.Up: 19, state.Down: 11})
	src.judgeable["global|"] = 50
	src.history["global|"] = varyingHistory()
	sink := &fakeSink{}

	ev := anomaly.New(src, sink, "local")
	ev.Now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		_, _ = ev.Evaluate(context.Background())
		now = now.Add(5 * time.Minute)
	}
	for _, a := range sink.anomalies {
		if a.Kind == "health_drop" {
			t.Fatalf("a low-coverage scope fired a health anomaly: %+v", a)
		}
	}
}

func TestFullCoverageUsesTheNormalBand(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	// Coverage 0.8 and health 0.625: the same shape without the widening, so
	// it crosses the normal lower bound and fires.
	src := newSource(map[state.State]int{state.Up: 25, state.Down: 15})
	src.judgeable["global|"] = 50
	src.history["global|"] = varyingHistory()
	sink := &fakeSink{}

	ev := anomaly.New(src, sink, "local")
	ev.Now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		_, _ = ev.Evaluate(context.Background())
		now = now.Add(5 * time.Minute)
	}

	fired := false
	for _, a := range sink.anomalies {
		if a.Kind == "health_drop" {
			fired = true
		}
	}
	if !fired {
		t.Fatalf("a full-coverage drop did not fire: %+v", sink.anomalies)
	}
}

func TestSmallScopeWidensTheBandEvenAtFullCoverage(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	// Ten measurable targets is below the absolute floor, so the ratio is
	// perfect and the information is not. Health 0.6 is below the normal band
	// but inside the widened one.
	src := newSource(map[state.State]int{state.Up: 6, state.Down: 4})
	src.judgeable["global|"] = 10
	src.history["global|"] = varyingHistory()
	sink := &fakeSink{}

	ev := anomaly.New(src, sink, "local")
	ev.Now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		_, _ = ev.Evaluate(context.Background())
		now = now.Add(5 * time.Minute)
	}
	for _, a := range sink.anomalies {
		if a.Kind == "health_drop" {
			t.Fatalf("a ten-target scope fired a health anomaly: %+v", a)
		}
	}
}
