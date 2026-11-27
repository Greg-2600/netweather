package incidents_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
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
	hasErr    error
	listErr   error
	closeErr  error
}

func newSink() *fakeSink {
	return &fakeSink{open: make(map[string]bool)}
}

func (f *fakeSink) HasOpenIncident(_ context.Context, scope, value string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hasErr != nil {
		return false, f.hasErr
	}
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
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]store.Incident(nil), f.incidents...), nil
}

func (f *fakeSink) CloseIncident(_ context.Context, id, _ string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closeErr != nil {
		return f.closeErr
	}
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
	return failures(failing, total, time.Now())
}

func failures(failing int, total int, now time.Time) []store.Observation {
	out := make([]store.Observation, 0, total)
	for i := 0; i < total; i++ {
		out = append(out, store.Observation{
			TargetID:  string(rune('a' + i)),
			Sector:    "airline",
			Stage:     "tls",
			Failing:   i < failing,
			IntervalS: 300,
			LastProbe: now,
		})
	}
	return out
}

func TestEvaluateOpensAnIncident(t *testing.T) {
	t.Parallel()

	src := &fakeSource{obs: airlineFailures(5, 6)}
	sink := newSink()

	now := time.Now().UTC()
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

func TestEvaluateIgnoresStaleObservations(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	stale := airlineFailures(5, 6)
	for i := range stale {
		stale[i].LastProbe = now.Add(-time.Hour)
	}

	sink := newSink()
	if _, opened, err := incidents.Evaluate(context.Background(), &fakeSource{obs: stale}, sink, "local", now); err != nil {
		t.Fatal(err)
	} else if opened {
		t.Fatal("stale observations produced an incident")
	}

	// One fresh failure among stale ones counts only the fresh set; three
	// stale failures plus one fresh one must not clear the failure gate.
	mixed := append(stale[:3:3], store.Observation{
		TargetID: "fresh", Sector: "airline", Stage: "tls", Failing: true, LastProbe: now,
	})
	if _, opened, err := incidents.Evaluate(context.Background(), &fakeSource{obs: mixed}, sink, "local", now); err != nil {
		t.Fatal(err)
	} else if opened {
		t.Fatal("stale observations counted toward the failure gate")
	}
}

func TestReconcileKeepsIncidentsWhenBlind(t *testing.T) {
	t.Parallel()

	sink := newSink()
	sink.incidents = []store.Incident{{ID: "inc-1", Scope: "sector", ScopeValue: "airline"}}

	now := time.Now().UTC()
	stale := airlineFailures(0, 6)
	for i := range stale {
		stale[i].LastProbe = now.Add(-time.Hour)
	}

	closed, err := incidents.Reconcile(context.Background(), &fakeSource{obs: stale}, sink, "local", now)
	if err != nil {
		t.Fatal(err)
	}
	if closed != 0 {
		t.Fatal("reconciliation closed an incident while every observation was stale")
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

func TestEvaluateFreshnessWindow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		name       string
		intervalS  int
		age        time.Duration
		wantOpened bool
	}{
		{"default window fresh", 0, 9 * time.Minute, true},
		{"default window stale", 0, 11 * time.Minute, false},
		{"configured window fresh", 300, 10 * time.Minute, true},
		{"configured window stale", 300, 11 * time.Minute, false},
		{"capped window fresh", 3600, 14 * time.Minute, true},
		{"capped window stale", 3600, 16 * time.Minute, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			obs := failures(5, 6, now.Add(-tc.age))
			for i := range obs {
				obs[i].IntervalS = tc.intervalS
			}

			_, opened, err := incidents.Evaluate(context.Background(), &fakeSource{obs: obs}, newSink(), "local", now)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if opened != tc.wantOpened {
				t.Fatalf("opened = %v, want %v", opened, tc.wantOpened)
			}
		})
	}
}

func TestEvaluateConfidence(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		name    string
		failing int
		total   int
		want    string
	}{
		{"five of five is high", 5, 5, "high"},
		{"five of six is high", 5, 6, "high"},
		{"four of five is medium", 4, 5, "medium"},
		{"six of eight is medium", 6, 8, "medium"},
		{"three of three is medium", 3, 3, "medium"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			inc, opened, err := incidents.Evaluate(context.Background(),
				&fakeSource{obs: failures(tc.failing, tc.total, now)}, newSink(), "local", now)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if !opened {
				t.Fatal("expected an incident")
			}
			if inc.Confidence != tc.want {
				t.Fatalf("confidence = %q, want %q", inc.Confidence, tc.want)
			}
		})
	}
}

func TestEvaluateSinkErrors(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		name    string
		hasErr  error
		openErr error
	}{
		{"open state lookup fails", errors.New("lookup"), nil},
		{"open write fails", nil, errors.New("write")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sink := newSink()
			sink.hasErr = tc.hasErr
			sink.err = tc.openErr

			_, opened, err := incidents.Evaluate(context.Background(),
				&fakeSource{obs: failures(5, 6, now)}, sink, "local", now)
			if err == nil {
				t.Fatal("a sink failure must surface")
			}
			if opened {
				t.Fatal("a failed sink must not report an opened incident")
			}
		})
	}
}

func TestReconcileSinkErrors(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		name     string
		listErr  error
		closeErr error
	}{
		{"listing open incidents fails", errors.New("list"), nil},
		{"closing an incident fails", nil, errors.New("close")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sink := newSink()
			sink.incidents = []store.Incident{{ID: "inc-1", Scope: "sector", ScopeValue: "airline"}}
			sink.listErr = tc.listErr
			sink.closeErr = tc.closeErr

			closed, err := incidents.Reconcile(context.Background(),
				&fakeSource{obs: failures(0, 6, now)}, sink, "local", now)
			if err == nil {
				t.Fatal("a sink failure must surface")
			}
			if closed != 0 {
				t.Fatalf("closed = %d, want 0 on failure", closed)
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
		incidents.Loop(ctx, &fakeSource{obs: airlineFailures(5, 6)}, sink, sink, "local", time.Millisecond, nil, nil, nil)
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

// countingSource counts reads, so a loop that never evaluates is observable
// without relying on a sink's absence.
type countingSource struct {
	mu    sync.Mutex
	calls int
	obs   []store.Observation
}

func (c *countingSource) Observations(context.Context, string) ([]store.Observation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.obs, nil
}

func (c *countingSource) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

type signalingSource struct {
	obs   []store.Observation
	err   error
	once  sync.Once
	ready chan struct{}
}

func (s *signalingSource) Observations(context.Context, string) ([]store.Observation, error) {
	s.once.Do(func() { close(s.ready) })
	return s.obs, s.err
}

type signalingSink struct {
	*fakeSink
	once  sync.Once
	ready chan struct{}
}

func (s *signalingSink) OpenIncidents(ctx context.Context) ([]store.Incident, error) {
	s.once.Do(func() { close(s.ready) })
	return s.fakeSink.OpenIncidents(ctx)
}

func TestLoopBranches(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		every time.Duration
		build func() (incidents.Source, *fakeSink, incidents.Reconciler, func() bool, func(), <-chan struct{}, func(*testing.T, *fakeSink))
	}{
		{
			name:  "zero interval falls back to the default",
			every: 0,
			build: func() (incidents.Source, *fakeSink, incidents.Reconciler, func() bool, func(), <-chan struct{}, func(*testing.T, *fakeSink)) {
				sink := newSink()
				return &fakeSource{}, sink, sink, nil, nil, nil, func(*testing.T, *fakeSink) {}
			},
		},
		{
			name:  "evaluation failure is logged and retried",
			every: time.Millisecond,
			build: func() (incidents.Source, *fakeSink, incidents.Reconciler, func() bool, func(), <-chan struct{}, func(*testing.T, *fakeSink)) {
				src := &signalingSource{err: errors.New("source down"), ready: make(chan struct{})}
				sink := newSink()
				return src, sink, sink, nil, nil, src.ready, func(t *testing.T, s *fakeSink) {
					s.mu.Lock()
					defer s.mu.Unlock()
					if len(s.incidents) != 0 {
						t.Fatalf("a failed evaluation opened %d incident(s)", len(s.incidents))
					}
				}
			},
		},
		{
			name:  "reconciliation failure is logged and retried",
			every: time.Millisecond,
			build: func() (incidents.Source, *fakeSink, incidents.Reconciler, func() bool, func(), <-chan struct{}, func(*testing.T, *fakeSink)) {
				sink := newSink()
				sink.listErr = errors.New("list down")
				rec := &signalingSink{fakeSink: sink, ready: make(chan struct{})}
				return &fakeSource{}, sink, rec, nil, nil, rec.ready, func(*testing.T, *fakeSink) {}
			},
		},
		{
			name:  "closing an incident notifies",
			every: time.Millisecond,
			build: func() (incidents.Source, *fakeSink, incidents.Reconciler, func() bool, func(), <-chan struct{}, func(*testing.T, *fakeSink)) {
				sink := newSink()
				sink.incidents = []store.Incident{{ID: "inc-1", Scope: "sector", ScopeValue: "airline"}}
				notifyCh := make(chan struct{})
				var once sync.Once
				notify := func() { once.Do(func() { close(notifyCh) }) }
				return &fakeSource{obs: failures(0, 6, time.Now())}, sink, sink, nil, notify, notifyCh,
					func(t *testing.T, s *fakeSink) {
						s.mu.Lock()
						defer s.mu.Unlock()
						if len(s.closed) != 1 || s.closed[0] != "inc-1" {
							t.Fatalf("closed = %v, want [inc-1]", s.closed)
						}
					}
			},
		},
		{
			name:  "suppression logs once and resumes",
			every: time.Millisecond,
			build: func() (incidents.Source, *fakeSink, incidents.Reconciler, func() bool, func(), <-chan struct{}, func(*testing.T, *fakeSink)) {
				src := &signalingSource{obs: failures(5, 6, time.Now()), ready: make(chan struct{})}
				sink := newSink()
				var calls atomic.Int32
				suppress := func() bool { return calls.Add(1) == 1 }
				return src, sink, sink, suppress, nil, src.ready, func(t *testing.T, s *fakeSink) {
					s.mu.Lock()
					defer s.mu.Unlock()
					if len(s.incidents) != 1 {
						t.Fatalf("incidents = %d, want the first incident after resume", len(s.incidents))
					}
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			src, sink, rec, suppress, notify, ready, check := tc.build()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			done := make(chan struct{})
			go func() {
				incidents.Loop(ctx, src, sink, rec, "local", tc.every, nil, notify, suppress)
				close(done)
			}()

			if ready != nil {
				select {
				case <-ready:
				case <-time.After(2 * time.Second):
					t.Fatal("Loop did not reach the expected branch")
				}
			}
			cancel()

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("Loop did not stop after cancel")
			}
			check(t, sink)
		})
	}
}

func TestLoopSkipsCorrelationWhileSuppressed(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	src := &countingSource{obs: airlineFailures(5, 6)}
	sink := newSink()
	done := make(chan struct{})
	go func() {
		incidents.Loop(ctx, src, sink, sink, "local", time.Millisecond, nil, nil,
			func() bool { return true })
		close(done)
	}()

	time.Sleep(15 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Loop did not stop after cancel")
	}

	if calls := src.Calls(); calls != 0 {
		t.Fatalf("the source was read %d times while suppressed", calls)
	}
	sink.mu.Lock()
	opened := len(sink.incidents)
	sink.mu.Unlock()
	if opened != 0 {
		t.Fatalf("a skewed clock opened %d incident(s)", opened)
	}
}
