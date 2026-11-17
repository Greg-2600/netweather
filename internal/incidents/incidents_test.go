package incidents_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/incidents"
	"github.com/Greg-2600/netweather/internal/store"
)

type fakeSource struct {
	obs []store.Observation
	err error
}

func (f *fakeSource) Observations(context.Context, string) ([]store.Observation, error) {
	return f.obs, f.err
}

type fakeSink struct {
	mu        sync.Mutex
	open      map[string]bool
	incidents []store.Incident
	targets   [][]string
	attrs     [][]store.Attribution
	closed    []string
	err       error
}

func newSink() *fakeSink {
	return &fakeSink{open: make(map[string]bool)}
}

func (f *fakeSink) HasOpenIncident(_ context.Context, scope, value string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open[scope+"|"+value], nil
}

func (f *fakeSink) OpenIncident(_ context.Context, inc store.Incident, targetIDs []string, attrs []store.Attribution) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.incidents = append(f.incidents, inc)
	f.targets = append(f.targets, targetIDs)
	f.attrs = append(f.attrs, attrs)
	f.open[inc.Scope+"|"+inc.ScopeValue] = true
	return nil
}

func (f *fakeSink) OpenIncidents(context.Context) ([]store.Incident, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Incident(nil), f.incidents...), nil
}

func (f *fakeSink) CloseIncident(_ context.Context, id, _ string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, id)
	for i, inc := range f.incidents {
		if inc.ID == id {
			delete(f.open, inc.Scope+"|"+inc.ScopeValue)
			f.incidents = append(f.incidents[:i], f.incidents[i+1:]...)
			break
		}
	}
	return nil
}

func airlineFailures(failing int, total int) []store.Observation {
	out := make([]store.Observation, 0, total)
	for i := 0; i < total; i++ {
		out = append(out, store.Observation{
			TargetID: string(rune('a' + i)),
			Sector:   "airline",
			Stage:    "tls",
			Failing:  i < failing,
		})
	}
	return out
}

func TestEvaluateOpensAnIncident(t *testing.T) {
	t.Parallel()

	src := &fakeSource{obs: airlineFailures(5, 6)}
	sink := newSink()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	inc, opened, err := incidents.Evaluate(context.Background(), src, sink, "local", now)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !opened {
		t.Fatal("expected an incident")
	}

	if inc.Scope != "sector" || inc.ScopeValue != "airline" {
		t.Fatalf("scope = %s=%s", inc.Scope, inc.ScopeValue)
	}
	if inc.Verdict != "INCIDENT" {
		t.Fatalf("verdict = %q", inc.Verdict)
	}
	if inc.Confidence != "high" {
		t.Fatalf("confidence = %q, want high for 5/6", inc.Confidence)
	}
	if inc.ID == "" {
		t.Fatal("empty id")
	}
	if len(sink.targets) != 1 || len(sink.targets[0]) != 5 {
		t.Fatalf("incident targets = %v, want the five failing ids", sink.targets)
	}
	if len(sink.attrs) != 1 || len(sink.attrs[0]) == 0 {
		t.Fatalf("attributions not stored: %v", sink.attrs)
	}
	if sink.attrs[0][0].Kind != "sector" || sink.attrs[0][0].Value != "airline" {
		t.Fatalf("top attribution = %+v", sink.attrs[0][0])
	}
}

func TestEvaluateRespectsBothGates(t *testing.T) {
	t.Parallel()

	cases := map[string][]store.Observation{
		"two failures": airlineFailures(2, 3),
		"low share":    airlineFailures(5, 30),
	}
	for name, obs := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			sink := newSink()
			_, opened, err := incidents.Evaluate(context.Background(), &fakeSource{obs: obs}, sink, "local", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if opened {
				t.Fatalf("opened an incident for %s", name)
			}
		})
	}
}

func TestEvaluateDoesNotReopen(t *testing.T) {
	t.Parallel()

	src := &fakeSource{obs: airlineFailures(5, 6)}
	sink := newSink()

	if _, opened, _ := incidents.Evaluate(context.Background(), src, sink, "local", time.Now()); !opened {
		t.Fatal("first evaluation should open")
	}
	if _, opened, _ := incidents.Evaluate(context.Background(), src, sink, "local", time.Now()); opened {
		t.Fatal("second evaluation reopened the same cluster")
	}
	if len(sink.incidents) != 1 {
		t.Fatalf("incidents = %d", len(sink.incidents))
	}
}

func TestEvaluatePropagatesSourceFailure(t *testing.T) {
	t.Parallel()

	src := &fakeSource{err: errors.New("store down")}
	if _, _, err := incidents.Evaluate(context.Background(), src, newSink(), "local", time.Now()); err == nil {
		t.Fatal("a source failure must surface")
	}
}

func TestEvaluatePropagatesSinkFailure(t *testing.T) {
	t.Parallel()

	sink := newSink()
	sink.err = errors.New("disk full")
	if _, opened, err := incidents.Evaluate(context.Background(), &fakeSource{obs: airlineFailures(5, 6)}, sink, "local", time.Now()); err == nil || opened {
		t.Fatalf("opened=%v err=%v, want a sink error", opened, err)
	}
}

func TestNewIDIsUnique(t *testing.T) {
	t.Parallel()

	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		id := incidents.NewID()
		if id == "" {
			t.Fatal("empty id")
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestReconcileClosesClearedCluster(t *testing.T) {
	t.Parallel()

	sink := newSink()
	sink.incidents = []store.Incident{{ID: "inc-1", Scope: "sector", ScopeValue: "airline"}}

	src := &fakeSource{obs: airlineFailures(0, 6)}
	closed, err := incidents.Reconcile(context.Background(), src, sink, "local", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if closed != 1 || len(sink.closed) != 1 || sink.closed[0] != "inc-1" {
		t.Fatalf("closed = %d, ids = %v", closed, sink.closed)
	}
}

func TestReconcileKeepsAnActiveCluster(t *testing.T) {
	t.Parallel()

	sink := newSink()
	sink.incidents = []store.Incident{{ID: "inc-1", Scope: "sector", ScopeValue: "airline"}}

	src := &fakeSource{obs: airlineFailures(5, 6)}
	closed, err := incidents.Reconcile(context.Background(), src, sink, "local", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if closed != 0 || len(sink.closed) != 0 {
		t.Fatalf("closed = %d, ids = %v", closed, sink.closed)
	}
}

func TestReconcileWithNothingOpen(t *testing.T) {
	t.Parallel()

	closed, err := incidents.Reconcile(context.Background(), &fakeSource{}, newSink(), "local", time.Now())
	if err != nil || closed != 0 {
		t.Fatalf("closed = %d, err = %v", closed, err)
	}
}

func TestReconcilePropagatesSourceFailure(t *testing.T) {
	t.Parallel()

	sink := newSink()
	sink.incidents = []store.Incident{{ID: "inc-1", Scope: "sector", ScopeValue: "airline"}}

	src := &fakeSource{err: errors.New("store down")}
	if _, err := incidents.Reconcile(context.Background(), src, sink, "local", time.Now()); err == nil {
		t.Fatal("a source failure must surface")
	}
}

func TestLoopStopsOnCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	sink := newSink()

	go func() {
		incidents.Loop(ctx, &fakeSource{obs: airlineFailures(5, 6)}, sink, sink, "local", time.Millisecond, nil, nil)
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
