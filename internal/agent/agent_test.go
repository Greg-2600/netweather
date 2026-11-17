package agent_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/agent"
	"github.com/Greg-2600/netweather/internal/classify"
	"github.com/Greg-2600/netweather/internal/probe"
	"github.com/Greg-2600/netweather/internal/schedule"
	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
)

type fakeProber struct {
	mu      sync.Mutex
	results map[string]probe.Result
	calls   map[string]int
}

func newProber() *fakeProber {
	return &fakeProber{results: make(map[string]probe.Result), calls: make(map[string]int)}
}

func (f *fakeProber) set(url string, r probe.Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[url] = r
}

func (f *fakeProber) Run(_ context.Context, t agent.Target) probe.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[t.URL]++
	return f.results[t.URL]
}

func (f *fakeProber) count(url string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[url]
}

type fakeSink struct {
	mu     sync.Mutex
	probes []store.Probe
	err    error
}

func (f *fakeSink) Record(_ context.Context, probes []store.Probe) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.probes = append(f.probes, probes...)
	return nil
}

func (f *fakeSink) batch() []store.Probe {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Probe(nil), f.probes...)
}

func target(id, host string) agent.Target {
	return agent.Target{
		ID:       id,
		Name:     id,
		Host:     host,
		URL:      "https://" + host + "/",
		Vantage:  "local",
		Tier:     schedule.TierService,
		Interval: 5 * time.Minute,
	}
}

func at(t time.Time) func() time.Time { return func() time.Time { return t } }

func zeroRand() float64 { return 0 }

func TestStepRunsDueTargetsAndRecords(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	p := newProber()
	p.set("https://chase.com/", probe.Result{Status: http.StatusOK, Latency: 42 * time.Millisecond})
	p.set("https://united.com/", probe.Result{Status: http.StatusInternalServerError})

	sink := &fakeSink{}
	a := agent.New([]agent.Target{
		target("t1", "chase.com"),
		target("t2", "united.com"),
	}, p, sink)
	a.Now = at(now)
	a.Rand = zeroRand

	ran, err := a.Step(context.Background())
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if ran != 2 {
		t.Fatalf("ran = %d, want 2", ran)
	}

	batch := sink.batch()
	if len(batch) != 2 {
		t.Fatalf("recorded %d probes", len(batch))
	}

	byID := map[string]store.Probe{}
	for _, pr := range batch {
		byID[pr.TargetID] = pr
	}
	if got := byID["t1"].State; got != state.UpReachability {
		t.Errorf("chase state = %s, want up_reachability", got)
	}
	if got := byID["t1"].LatencyMS; got != 42 {
		t.Errorf("latency = %d, want 42", got)
	}
	if got := byID["t2"].State; got != state.Down {
		t.Errorf("united state = %s, want down", got)
	}
	if got := byID["t1"].VantageID; got != "local" {
		t.Errorf("vantage = %q", got)
	}
	if got := byID["t1"].Reason; got == "" {
		t.Error("empty reason")
	}
}

func TestStepRespectsTheInterval(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	clock := now
	p := newProber()
	sink := &fakeSink{}

	a := agent.New([]agent.Target{target("t1", "chase.com")}, p, sink)
	a.Now = func() time.Time { return clock }
	a.Rand = zeroRand

	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.count("https://chase.com/") != 1 {
		t.Fatal("first step did not probe")
	}

	clock = now.Add(30 * time.Second)
	if ran, _ := a.Step(context.Background()); ran != 0 {
		t.Fatalf("ran = %d inside the interval", ran)
	}

	clock = now.Add(5*time.Minute + time.Second)
	if ran, _ := a.Step(context.Background()); ran != 1 {
		t.Fatalf("ran = %d after the interval", ran)
	}
}

func TestStepSkipsBusyHost(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	p := newProber()
	sink := &fakeSink{}

	a := agent.New([]agent.Target{target("t1", "www.chase.com")}, p, sink)
	a.Now = at(now)
	a.Rand = zeroRand

	release, ok := a.Limiter.TryAcquire("chase.com")
	if !ok {
		t.Fatal("could not hold the host slot")
	}
	defer release()

	ran, err := a.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ran != 0 || p.count("https://www.chase.com/") != 0 {
		t.Fatalf("ran = %d while the registrable domain was busy", ran)
	}
}

func TestStepMapsConfigErrorsAndTransportFailures(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	p := newProber()
	p.set("https://a.example/", probe.Result{Status: http.StatusNotFound})
	p.set("https://b.example/", probe.Result{ErrorClass: "tcp_refused", Err: errors.New("connection refused")})

	sink := &fakeSink{}
	a := agent.New([]agent.Target{
		target("a", "a.example"),
		target("b", "b.example"),
	}, p, sink)
	a.Now = at(now)
	a.Rand = zeroRand

	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}

	byID := map[string]store.Probe{}
	for _, pr := range sink.batch() {
		byID[pr.TargetID] = pr
	}
	if got := byID["a"].State; got != state.ConfigError {
		t.Errorf("404 -> %s, want config_error", got)
	}
	if got := byID["b"].State; got != state.Down {
		t.Errorf("refused -> %s, want down", got)
	}
	if got := byID["b"].Stage; got != "tcp" {
		t.Errorf("stage = %q, want tcp", got)
	}
}

func TestStepPropagatesSinkFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	sink := &fakeSink{err: errors.New("disk full")}
	a := agent.New([]agent.Target{target("t1", "chase.com")}, newProber(), sink)
	a.Now = at(now)
	a.Rand = zeroRand

	if _, err := a.Step(context.Background()); err == nil {
		t.Fatal("a sink failure must surface")
	}
}

func TestColdStartSpreadsFirstRun(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	p := newProber()
	sink := &fakeSink{}

	a := agent.New([]agent.Target{target("t1", "chase.com")}, p, sink)
	a.Now = at(now)
	a.Rand = func() float64 { return 0.5 } // half of the first interval

	if ran, _ := a.Step(context.Background()); ran != 0 {
		t.Fatalf("ran = %d at t=0; cold start should not fire immediately", ran)
	}
	if p.count("https://chase.com/") != 0 {
		t.Fatal("probe ran before its first scheduled time")
	}
	if next := a.NextRun("t1"); !next.After(now) {
		t.Fatalf("next run = %v, want after %v", next, now)
	}
}

func TestStepWithNoTargets(t *testing.T) {
	t.Parallel()

	a := agent.New(nil, newProber(), &fakeSink{})
	a.Now = at(time.Now())
	ran, err := a.Step(context.Background())
	if err != nil || ran != 0 {
		t.Fatalf("Step = (%d, %v)", ran, err)
	}
}

func TestStepPacesProbes(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	p := newProber()
	sink := &fakeSink{}

	a := agent.New([]agent.Target{
		target("a", "a.example"),
		target("b", "b.example"),
		target("c", "c.example"),
	}, p, sink)
	a.Now = at(now)
	a.Rand = zeroRand
	a.Pace = 20 * time.Millisecond

	start := time.Now()
	ran, err := a.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	if ran != 3 {
		t.Fatalf("ran = %d, want 3", ran)
	}
	// Three probes means at least two pace gaps between them.
	if elapsed < 40*time.Millisecond {
		t.Fatalf("elapsed = %v, want the 20ms pace applied between probes", elapsed)
	}
}

func TestStepPaceHonoursCancellation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	a := agent.New([]agent.Target{
		target("a", "a.example"),
		target("b", "b.example"),
	}, newProber(), &fakeSink{})
	a.Now = at(now)
	a.Rand = zeroRand
	a.Pace = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := a.Step(ctx); err == nil {
		t.Fatal("a cancelled pace must stop the step")
	}
}

func TestClassifierExpectationsAreCarried(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	p := newProber()
	p.set("https://a.example/", probe.Result{Status: http.StatusOK, Body: []byte("service is down")})

	tg := target("a", "a.example")
	tg.Expect = classify.Expected{BodyContains: "healthy"}

	sink := &fakeSink{}
	a := agent.New([]agent.Target{tg}, p, sink)
	a.Now = at(now)
	a.Rand = zeroRand

	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sink.batch()[0].State; got != state.Degraded {
		t.Fatalf("state = %s, want degraded", got)
	}
}

func TestNonHTTPSuccessIsReachability(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	p := newProber()
	p.set("", probe.Result{Latency: 5 * time.Millisecond})

	tg := target("tls1", "example.com")
	tg.Instrument = "tls"
	tg.Address = "example.com:443"
	tg.URL = ""
	tg.Tier = schedule.TierAnchor

	sink := &fakeSink{}
	a := agent.New([]agent.Target{tg}, p, sink)
	a.Now = at(now)
	a.Rand = zeroRand

	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := sink.batch()[0]
	if got.State != state.UpReachability {
		t.Fatalf("state = %s, want up_reachability", got.State)
	}
	if got.Stage != "tls" {
		t.Fatalf("stage = %q, want tls", got.Stage)
	}
}

func TestNonHTTPFailureUsesTheTransportClass(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	p := newProber()
	p.set("", probe.Result{ErrorClass: "tls_cert_expired", Err: errors.New("expired")})

	tg := target("tls1", "example.com")
	tg.Instrument = "tls"
	tg.Address = "example.com:443"
	tg.URL = ""

	sink := &fakeSink{}
	a := agent.New([]agent.Target{tg}, p, sink)
	a.Now = at(now)
	a.Rand = zeroRand

	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := sink.batch()[0]
	if got.State != state.Down {
		t.Fatalf("state = %s, want down", got.State)
	}
	if got.ErrorClass != "tls_cert_expired" {
		t.Fatalf("class = %q", got.ErrorClass)
	}
}

func TestNTPNeverProducesATargetState(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	p := newProber()
	p.set("", probe.Result{Latency: 12 * time.Millisecond, Offset: -1500 * time.Millisecond})

	tg := target("ntp1", "time.example")
	tg.Instrument = "ntp"
	tg.Address = "time.example:123"
	tg.URL = ""

	sink := &fakeSink{}
	a := agent.New([]agent.Target{tg}, p, sink)
	a.Now = at(now)
	a.Rand = zeroRand

	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := sink.batch()[0]
	if got.State != state.Unknown {
		t.Fatalf("state = %s, want unknown: NTP is self-health only", got.State)
	}
	if !strings.Contains(got.Reason, "-1.5s") {
		t.Fatalf("reason = %q, want the measured offset", got.Reason)
	}
	if got.Stage != "ntp" {
		t.Fatalf("stage = %q, want ntp", got.Stage)
	}
}

func TestInstrumentsDispatch(t *testing.T) {
	t.Parallel()

	boom := func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: errors.New("refused")}
	}

	inst := agent.Instruments{
		TLS: &probe.TLS{Dial: boom},
		TCP: &probe.TCP{Dial: boom},
		DNS: probe.NewDNS(boom),
		NTP: probe.NewNTPWithDial(boom),
	}

	if got := inst.Run(context.Background(), agent.Target{Instrument: "tcp", Address: "x.example:443"}); got.ErrorClass != "tcp_refused" {
		t.Errorf("tcp dispatch: class = %q", got.ErrorClass)
	}
	if got := inst.Run(context.Background(), agent.Target{Instrument: "tls", Address: "x.example:443"}); got.ErrorClass != "tcp_refused" {
		t.Errorf("tls dispatch: class = %q", got.ErrorClass)
	}
	if got := inst.Run(context.Background(), agent.Target{
		Instrument: "dns", Address: "x.example:53", Query: "example.com",
	}); got.ErrorClass != "dns_error" {
		t.Errorf("dns dispatch: class = %q", got.ErrorClass)
	}
	if got := inst.Run(context.Background(), agent.Target{
		Instrument: "ntp", Address: "x.example:123",
	}); got.ErrorClass != "ntp_unreachable" {
		t.Errorf("ntp dispatch: class = %q", got.ErrorClass)
	}

	empty := agent.Instruments{}
	for _, instrument := range []string{"tcp", "tls", "dns", "ntp", "http"} {
		if got := empty.Run(context.Background(), agent.Target{Instrument: instrument}); got.Err == nil {
			t.Errorf("%s: a missing instrument must be an error, not a silent success", instrument)
		}
	}
}

func TestSetTargetsSwapsTheLiveSet(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	p := newProber()
	p.set("https://a.example/", probe.Result{Status: http.StatusOK})
	p.set("https://b.example/", probe.Result{Status: http.StatusOK})
	sink := &fakeSink{}

	a := agent.New([]agent.Target{target("a", "a.example")}, p, sink)
	a.Now = at(now)
	a.Rand = zeroRand

	if ran, _ := a.Step(context.Background()); ran != 1 {
		t.Fatalf("initial step ran = %d", ran)
	}

	a.SetTargets([]agent.Target{target("b", "b.example")})
	if a.TargetCount() != 1 {
		t.Fatalf("count = %d", a.TargetCount())
	}
	if ids := a.TargetIDs(); len(ids) != 1 || ids[0] != "b" {
		t.Fatalf("ids = %v", ids)
	}

	if ran, _ := a.Step(context.Background()); ran != 1 {
		t.Fatalf("post-swap step ran = %d", ran)
	}
	if p.count("https://b.example/") != 1 {
		t.Fatal("new target was not probed")
	}
	if p.count("https://a.example/") != 1 {
		t.Fatal("removed target was probed again")
	}
}

func TestPauseStopsProbing(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	p := newProber()
	sink := &fakeSink{}

	a := agent.New([]agent.Target{target("t1", "chase.com")}, p, sink)
	a.Now = at(now)
	a.Rand = zeroRand
	a.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	a.Pause()
	if !a.Paused() {
		t.Fatal("Paused() false after Pause")
	}
	if ran, err := a.Step(context.Background()); err != nil || ran != 0 {
		t.Fatalf("paused Step = (%d, %v)", ran, err)
	}
	if p.count("https://chase.com/") != 0 {
		t.Fatal("a paused agent probed")
	}

	a.Resume()
	if ran, err := a.Step(context.Background()); err != nil || ran != 1 {
		t.Fatalf("resumed Step = (%d, %v)", ran, err)
	}
	if p.count("https://chase.com/") != 1 {
		t.Fatal("resumed agent did not probe")
	}
}

func TestStepLogsTheDecisionRecord(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	p := newProber()
	p.set("https://chase.com/", probe.Result{Status: http.StatusOK, Latency: 7 * time.Millisecond})

	var out bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))

	a := agent.New([]agent.Target{target("t1", "chase.com")}, p, &fakeSink{})
	a.Now = at(now)
	a.Rand = zeroRand
	a.Logger = logger

	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}

	line := out.String()
	for _, want := range []string{
		"msg=probe",
		"target=chase.com",
		"vantage=local",
		"state=up_reachability",
		"stage=http",
		"confidence=low",
		"reason=",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("log line missing %q:\n%s", want, line)
		}
	}
}
