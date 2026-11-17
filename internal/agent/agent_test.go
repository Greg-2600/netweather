package agent_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/agent"
	"github.com/Greg-2600/netweather/internal/classify"
	"github.com/Greg-2600/netweather/internal/dnsdiv"
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

func TestDNSFailuresUnderABlockedPortAreUnmeasurable(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	p := newProber()
	p.set("", probe.Result{ErrorClass: "dns_timeout", Err: errors.New("timeout")})

	tg := target("dns1", "198.41.0.4")
	tg.Instrument = "dns"
	tg.Address = "198.41.0.4:53"
	tg.Query = "example.com"
	tg.URL = ""

	sink := &fakeSink{}
	a := agent.New([]agent.Target{tg}, p, sink)
	a.Now = at(now)
	a.Rand = zeroRand
	a.DNSBlocked = func() bool { return true }

	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := sink.batch()[0]
	if got.State != state.Unmeasurable {
		t.Fatalf("state = %s, want unmeasurable", got.State)
	}
	if !strings.Contains(got.Reason, "port 53") {
		t.Fatalf("reason = %q, want the blocked port named", got.Reason)
	}

	// Without the gate the same observation is a DOWN, which is the whole
	// difference this wiring exists to make.
	p.set("", probe.Result{ErrorClass: "dns_timeout", Err: errors.New("timeout")})
	sink2 := &fakeSink{}
	a2 := agent.New([]agent.Target{tg}, p, sink2)
	a2.Now = at(now)
	a2.Rand = zeroRand
	a2.DNSBlocked = func() bool { return false }

	if _, err := a2.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sink2.batch()[0]; got.State != state.Down {
		t.Fatalf("state without the gate = %s, want down", got.State)
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

type refusedEcho struct{}

func (refusedEcho) SendEcho(netip.Addr, int, int) error { return errors.New("network unreachable") }
func (refusedEcho) Recv(time.Time) (netip.Addr, probe.TraceMessage, error) {
	return netip.Addr{}, probe.TraceMessage{}, errors.New("no reply")
}
func (refusedEcho) Close() error { return nil }

func TestInstrumentsDispatch(t *testing.T) {
	t.Parallel()

	boom := func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: errors.New("refused")}
	}

	ping := probe.NewICMP()
	ping.Timeout = 20 * time.Millisecond
	ping.Count = 1
	ping.Open = func() (probe.EchoTransport, error) { return refusedEcho{}, nil }
	ping.Resolve = func(context.Context, string) (netip.Addr, error) {
		return netip.MustParseAddr("192.0.2.1"), nil
	}

	inst := agent.Instruments{
		TLS:  &probe.TLS{Dial: boom},
		TCP:  &probe.TCP{Dial: boom},
		DNS:  probe.NewDNS(boom),
		NTP:  probe.NewNTPWithDial(boom),
		ICMP: ping,
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
	if got := inst.Run(context.Background(), agent.Target{
		Instrument: "icmp", Address: "192.0.2.1",
	}); got.ErrorClass != "icmp_unreachable" {
		t.Errorf("icmp dispatch: class = %q", got.ErrorClass)
	}

	empty := agent.Instruments{}
	for _, instrument := range []string{"tcp", "tcpping", "tls", "dns", "ntp", "icmp", "http"} {
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

func TestSuspendStopsProbingWithoutPausing(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	p := newProber()
	sink := &fakeSink{}

	a := agent.New([]agent.Target{target("t1", "chase.com")}, p, sink)
	a.Now = at(now)
	a.Rand = zeroRand
	a.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	suspended := true
	a.Suspend = func() bool { return suspended }

	if ran, err := a.Step(context.Background()); err != nil || ran != 0 {
		t.Fatalf("suspended Step = (%d, %v)", ran, err)
	}
	if p.count("https://chase.com/") != 0 {
		t.Fatal("a suspended agent probed")
	}
	if a.Paused() {
		t.Fatal("a budget suspension must not look like the operator's pause")
	}

	suspended = false
	if ran, err := a.Step(context.Background()); err != nil || ran != 1 {
		t.Fatalf("resumed Step = (%d, %v)", ran, err)
	}
	if p.count("https://chase.com/") != 1 {
		t.Fatal("the agent did not resume probing")
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

type fakePins struct {
	mu        sync.Mutex
	changed   bool
	advisory  string
	leafMoved bool
	err       error
	issuers   []string
	leaves    []string
}

func (f *fakePins) ObserveCert(_ context.Context, obs store.CertObservation, _ time.Time) (bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issuers = append(f.issuers, obs.Issuer)
	return f.changed, f.advisory, f.err
}

func (f *fakePins) ObserveLeaf(_ context.Context, _, fingerprint string, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leaves = append(f.leaves, fingerprint)
	return f.leafMoved, f.err
}

func TestCertificatePinFlagsANewIssuer(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	newTLS := func() agent.Target {
		tg := target("tls1", "example.com")
		tg.Instrument = "tls"
		tg.Address = "example.com:443"
		tg.URL = ""
		return tg
	}

	// A changed issuer is a one-time flag, not a verdict: the classifier keeps
	// it at UNKNOWN with the reason naming the learned set.
	p := newProber()
	p.set("", probe.Result{CertIssuer: "CN=GTS CA 1P5,O=Google Trust Services LLC,C=US"})
	sink := &fakeSink{}
	pins := &fakePins{changed: true}
	a := agent.New([]agent.Target{newTLS()}, p, sink)
	a.Now = at(now)
	a.Rand = zeroRand
	a.Pins = pins

	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := sink.batch()[0]
	if got.ErrorClass != "tls_issuer_unexpected" {
		t.Fatalf("class = %q, want the pin class", got.ErrorClass)
	}
	if got.State != state.Unknown {
		t.Fatalf("state = %s, want unknown", got.State)
	}
	if !strings.Contains(got.Reason, "issuer outside learned set") {
		t.Fatalf("reason = %q", got.Reason)
	}
	if len(pins.issuers) != 1 || !strings.Contains(pins.issuers[0], "Google Trust") {
		t.Fatalf("observed issuers = %+v", pins.issuers)
	}

	// An unchanged issuer leaves the TLS success as reachability.
	p = newProber()
	p.set("", probe.Result{CertIssuer: "CN=R11,O=Let's Encrypt,C=US"})
	sink = &fakeSink{}
	a = agent.New([]agent.Target{newTLS()}, p, sink)
	a.Now = at(now)
	a.Rand = zeroRand
	a.Pins = &fakePins{changed: false}

	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sink.batch()[0]; got.State != state.UpReachability || got.ErrorClass != "" {
		t.Fatalf("unchanged issuer = %+v", got)
	}

	// A pin-store failure must not turn a good measurement into a bad one.
	p = newProber()
	p.set("", probe.Result{CertIssuer: "CN=R11,O=Let's Encrypt,C=US"})
	sink = &fakeSink{}
	a = agent.New([]agent.Target{newTLS()}, p, sink)
	a.Now = at(now)
	a.Rand = zeroRand
	a.Pins = &fakePins{err: errors.New("db down")}

	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sink.batch()[0]; got.State != state.UpReachability {
		t.Fatalf("pin failure changed the state: %+v", got)
	}
}

func TestCertificatePinSkippedWithoutIssuerOrPins(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	tg := target("tls1", "example.com")
	tg.Instrument = "tls"
	tg.Address = "example.com:443"
	tg.URL = ""

	// No issuer on the result: nothing to learn.
	p := newProber()
	p.set("", probe.Result{})
	sink := &fakeSink{}
	pins := &fakePins{changed: true}
	a := agent.New([]agent.Target{tg}, p, sink)
	a.Now = at(now)
	a.Rand = zeroRand
	a.Pins = pins
	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pins.issuers) != 0 {
		t.Fatalf("observed issuers without one = %+v", pins.issuers)
	}

	// No pin store: the probe is unaffected.
	p = newProber()
	p.set("", probe.Result{CertIssuer: "CN=R11"})
	sink = &fakeSink{}
	a = agent.New([]agent.Target{tg}, p, sink)
	a.Now = at(now)
	a.Rand = zeroRand
	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sink.batch()[0]; got.State != state.UpReachability {
		t.Fatalf("state without pins = %+v", got)
	}
}

func TestLeafChangeIsInformational(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	tg := target("tls1", "example.com")
	tg.Instrument = "tls"
	tg.Address = "example.com:443"
	tg.URL = ""

	p := newProber()
	p.set("", probe.Result{
		CertIssuer:          "CN=R11,O=Let's Encrypt,C=US",
		CertLeafFingerprint: "aabbcc",
	})
	sink := &fakeSink{}
	pins := &fakePins{leafMoved: true}
	a := agent.New([]agent.Target{tg}, p, sink)
	a.Now = at(now)
	a.Rand = zeroRand
	a.Pins = pins

	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A rotated leaf is expected, so it must not change the state or the class.
	got := sink.batch()[0]
	if got.State != state.UpReachability || got.ErrorClass != "" {
		t.Fatalf("leaf change altered the measurement: %+v", got)
	}
	if len(pins.leaves) != 1 || pins.leaves[0] != "aabbcc" {
		t.Fatalf("leaves = %+v", pins.leaves)
	}
}

type fakeDivergence struct {
	mu        sync.Mutex
	baselines map[string]dnsdiv.Baseline
	answers   []dnsdiv.Answer
	verdicts  []dnsdiv.Verdict
	err       error
}

func (f *fakeDivergence) RecordDNSAnswer(_ context.Context, _ string, ans dnsdiv.Answer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, ans)
	return f.err
}

func (f *fakeDivergence) DNSBaselines(context.Context, string, time.Time) (map[string]dnsdiv.Baseline, error) {
	return f.baselines, f.err
}

func (f *fakeDivergence) RecordDNSDivergence(_ context.Context, _ string, v dnsdiv.Verdict, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verdicts = append(f.verdicts, v)
	return f.err
}

func dnsTarget(resolvers ...string) agent.Target {
	tg := target("dns1", "1.1.1.1")
	tg.Instrument = "dns"
	tg.Address = "1.1.1.1:53"
	tg.Query = "example.com"
	tg.URL = ""
	tg.Resolvers = resolvers
	return tg
}

func knownBaselines(resolvers ...string) map[string]dnsdiv.Baseline {
	out := make(map[string]dnsdiv.Baseline, len(resolvers))
	for _, resolver := range resolvers {
		out[resolver] = dnsdiv.Baseline{
			Addresses: map[string]struct{}{"192.0.2.1": {}},
			Samples:   1,
		}
	}
	return out
}

func TestDNSDivergenceDowngradesALocalFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	tg := dnsTarget("1.1.1.1:53", "9.9.9.9:53")

	p := newProber()
	p.set("", probe.Result{ErrorClass: "dns_nxdomain", Err: errors.New("no such host")})

	div := &fakeDivergence{baselines: knownBaselines(tg.Resolvers...)}
	sink := &fakeSink{}
	a := agent.New([]agent.Target{tg}, p, sink)
	a.Now = at(now)
	a.Rand = zeroRand
	a.Divergence = div
	a.DNSQuery = func(_ context.Context, server, _ string) probe.DNSResult {
		if server == tg.Resolvers[0] {
			return probe.DNSResult{ErrorClass: "dns_nxdomain", Err: errors.New("no such host")}
		}
		return probe.DNSResult{Addresses: []netip.Addr{netip.MustParseAddr("192.0.2.1")}}
	}

	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The primary failed, but a second resolver answered: not DOWN.
	got := sink.batch()[0]
	if got.State != state.Unknown {
		t.Fatalf("state = %s, want unknown (%s)", got.State, got.Reason)
	}
	if got.ErrorClass != "dns_divergence" {
		t.Fatalf("class = %q, want dns_divergence", got.ErrorClass)
	}
	if len(div.answers) != 2 {
		t.Fatalf("recorded answers = %d, want one per resolver", len(div.answers))
	}
	if len(div.verdicts) != 1 || div.verdicts[0].Kind != "DNS_ANOMALY" {
		t.Fatalf("verdicts = %+v, want one advisory", div.verdicts)
	}
}

func TestDNSDivergenceKeepsAGenuineOutageDown(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	tg := dnsTarget("1.1.1.1:53", "9.9.9.9:53")

	p := newProber()
	p.set("", probe.Result{ErrorClass: "dns_nxdomain", Err: errors.New("no such host")})

	div := &fakeDivergence{baselines: knownBaselines(tg.Resolvers...)}
	sink := &fakeSink{}
	a := agent.New([]agent.Target{tg}, p, sink)
	a.Now = at(now)
	a.Rand = zeroRand
	a.Divergence = div
	a.DNSQuery = func(context.Context, string, string) probe.DNSResult {
		return probe.DNSResult{ErrorClass: "dns_nxdomain", Err: errors.New("no such host")}
	}

	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Every resolver says NXDOMAIN where it used to answer: that is the ladder's
	// high-confidence TAMPER, and the recorded state is still a real DOWN.
	if got := sink.batch()[0]; got.State != state.Down || got.ErrorClass != "dns_nxdomain" {
		t.Fatalf("probe = %+v, want down with the DNS class", got)
	}
	if len(div.verdicts) != 1 || div.verdicts[0].Kind != "TAMPER" || div.verdicts[0].Confidence != "high" {
		t.Fatalf("verdicts = %+v, want a high-confidence TAMPER", div.verdicts)
	}
}

func TestDNSDivergenceNeedsMoreThanOneResolver(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	tg := dnsTarget("1.1.1.1:53")

	p := newProber()
	p.set("", probe.Result{Addresses: []netip.Addr{netip.MustParseAddr("192.0.2.1")}})

	div := &fakeDivergence{}
	sink := &fakeSink{}
	a := agent.New([]agent.Target{tg}, p, sink)
	a.Now = at(now)
	a.Rand = zeroRand
	a.Divergence = div
	a.DNSQuery = func(context.Context, string, string) probe.DNSResult {
		return probe.DNSResult{Addresses: []netip.Addr{netip.MustParseAddr("192.0.2.1")}}
	}

	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(div.answers) != 0 || len(div.verdicts) != 0 {
		t.Fatalf("a single resolver ran divergence: %+v %+v", div.answers, div.verdicts)
	}
}

func TestDNSDivergenceLearnsNewAddressLabels(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	tg := dnsTarget("1.1.1.1:53", "9.9.9.9:53")

	p := newProber()
	p.set("", probe.Result{
		Addresses: []netip.Addr{netip.MustParseAddr("192.0.2.9")},
		CNAMEs:    []string{"edge.foo.cloudflare.net."},
	})

	// Both resolvers know an address in AS13335 behind cloudflare.net, so the
	// new address is edge rotation, not a signal.
	baselines := make(map[string]dnsdiv.Baseline, len(tg.Resolvers))
	for _, server := range tg.Resolvers {
		baselines[server] = dnsdiv.Baseline{
			Addresses:     map[string]struct{}{"192.0.2.1": {}},
			ASNs:          map[int]struct{}{13335: {}},
			CNAMESuffixes: map[string]struct{}{"cloudflare.net": {}},
			Samples:       1,
		}
	}

	div := &fakeDivergence{baselines: baselines}
	sink := &fakeSink{}
	a := agent.New([]agent.Target{tg}, p, sink)
	a.Now = at(now)
	a.Rand = zeroRand
	a.Divergence = div
	looked := 0
	a.LookupASN = func(context.Context, netip.Addr) (int, error) {
		looked++
		return 13335, nil
	}
	askedNS := 0
	a.DNSQueryNS = func(context.Context, string, string) ([]string, error) {
		askedNS++
		return []string{"ns1.example.net", "ns2.example.net"}, nil
	}
	a.DNSQuery = func(context.Context, string, string) probe.DNSResult {
		return probe.DNSResult{
			Addresses: []netip.Addr{netip.MustParseAddr("192.0.2.9")},
			CNAMEs:    []string{"edge.foo.cloudflare.net."},
		}
	}

	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}

	// One lookup per resolver, because the address is new to each of them.
	if looked != len(tg.Resolvers) {
		t.Fatalf("lookups = %d, want %d", looked, len(tg.Resolvers))
	}
	if got := sink.batch()[0]; got.State != state.UpReachability {
		t.Fatalf("state = %s, want reachability", got.State)
	}
	if len(div.verdicts) != 0 {
		t.Fatalf("verdicts = %+v, want none for a known host", div.verdicts)
	}
	for _, ans := range div.answers {
		if len(ans.ASNs) != 1 || ans.ASNs[0] != 13335 {
			t.Fatalf("answer ASNs = %+v", ans.ASNs)
		}
		if len(ans.CNAMESuffixes) != 1 || ans.CNAMESuffixes[0] != "cloudflare.net" {
			t.Fatalf("answer suffixes = %+v", ans.CNAMESuffixes)
		}
		if len(ans.Delegations) != 2 {
			t.Fatalf("answer delegations = %+v", ans.Delegations)
		}
	}
	if askedNS != len(tg.Resolvers) {
		t.Fatalf("delegation queries = %d, want %d", askedNS, len(tg.Resolvers))
	}

	// A known address does not trigger a lookup at all.
	div2 := &fakeDivergence{baselines: map[string]dnsdiv.Baseline{
		tg.Resolvers[0]: {
			Addresses: map[string]struct{}{"192.0.2.9": {}},
			ASNs:      map[int]struct{}{13335: {}},
			Samples:   1,
		},
		tg.Resolvers[1]: {
			Addresses: map[string]struct{}{"192.0.2.9": {}},
			ASNs:      map[int]struct{}{13335: {}},
			Samples:   1,
		},
	}}
	looked = 0
	a2 := agent.New([]agent.Target{tg}, p, &fakeSink{})
	a2.Now = at(now)
	a2.Rand = zeroRand
	a2.Divergence = div2
	a2.LookupASN = func(context.Context, netip.Addr) (int, error) {
		looked++
		return 64500, nil
	}
	a2.DNSQuery = func(context.Context, string, string) probe.DNSResult {
		return probe.DNSResult{Addresses: []netip.Addr{netip.MustParseAddr("192.0.2.9")}}
	}
	if _, err := a2.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if looked != 0 {
		t.Fatalf("lookups for a known address = %d, want 0", looked)
	}
}

func TestLowMTUGateReachesTheClassifier(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	tg := target("web1", "example.com")

	// The handler accepts the connection and never answers: a timeout with no
	// transport error is exactly what a black-holed handshake looks like.
	p := newProber()
	p.set(tg.URL, probe.Result{ErrorClass: "http_timeout", Err: errors.New("timeout")})

	sink := &fakeSink{}
	a := agent.New([]agent.Target{tg}, p, sink)
	a.Now = at(now)
	a.Rand = zeroRand
	a.MTUBlocked = func() bool { return true }

	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := sink.batch()[0]
	if got.State != state.Unknown {
		t.Fatalf("state = %s, want unknown under a low MTU (%s)", got.State, got.Reason)
	}
	if !strings.Contains(got.Reason, "MTU") {
		t.Fatalf("reason = %q, want the MTU named", got.Reason)
	}

	// With the gate saying the path is fine, the same timeout is a DOWN.
	clearSink := &fakeSink{}
	b := agent.New([]agent.Target{tg}, p, clearSink)
	b.Now = at(now)
	b.Rand = zeroRand
	b.MTUBlocked = func() bool { return false }

	if _, err := b.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := clearSink.batch()[0]; got.State != state.Down {
		t.Fatalf("state = %s without the gate, want down", got.State)
	}
}

func TestGlobalRateCeilingPacesProbes(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	targets := []agent.Target{
		target("a1", "a.example"),
		target("a2", "b.example"),
		target("a3", "c.example"),
	}
	p := newProber()
	for _, tg := range targets {
		p.set(tg.URL, probe.Result{Status: 200})
	}

	now := base
	var waits []time.Duration
	a := agent.New(targets, p, &fakeSink{})
	a.Now = func() time.Time { return now }
	a.Rand = zeroRand
	a.RateCeiling = 2
	a.RateStart = 2
	a.RateSince = base
	a.Sleep = func(d time.Duration) {
		waits = append(waits, d)
		now = now.Add(d)
	}

	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Three probes at 2 req/s: the first starts immediately, the next two wait
	// half a second each.
	if len(waits) != 2 {
		t.Fatalf("waits = %v, want two gaps", waits)
	}
	for _, wait := range waits {
		if wait != 500*time.Millisecond {
			t.Fatalf("wait = %v, want 500ms at 2 req/s", wait)
		}
	}
}

func TestGlobalRateCeilingRampsToTheCeiling(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	targets := []agent.Target{
		target("a1", "a.example"),
		target("a2", "b.example"),
		target("a3", "c.example"),
	}
	p := newProber()
	for _, tg := range targets {
		p.set(tg.URL, probe.Result{Status: 200})
	}

	// Ten minutes after the first target the ramp is complete, so the gap is
	// the steady-state 50ms rather than the cold-start 500ms.
	now := base.Add(10 * time.Minute)
	var waits []time.Duration
	a := agent.New(targets, p, &fakeSink{})
	a.Now = func() time.Time { return now }
	a.Rand = zeroRand
	a.RateCeiling = 20
	a.RateStart = 2
	a.RateRamp = 10 * time.Minute
	a.RateSince = base
	a.Sleep = func(d time.Duration) {
		waits = append(waits, d)
		now = now.Add(d)
	}

	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 2 {
		t.Fatalf("waits = %v, want two gaps", waits)
	}
	for _, wait := range waits {
		if wait != 50*time.Millisecond {
			t.Fatalf("wait = %v, want 50ms at 20 req/s", wait)
		}
	}
}

func TestNoRatePacingWithoutACeiling(t *testing.T) {
	t.Parallel()

	targets := []agent.Target{
		target("a1", "a.example"),
		target("a2", "b.example"),
	}
	p := newProber()
	for _, tg := range targets {
		p.set(tg.URL, probe.Result{Status: 200})
	}

	a := agent.New(targets, p, &fakeSink{})
	a.Rand = zeroRand
	a.RateCeiling = 0
	a.Sleep = func(time.Duration) {
		t.Fatal("paced without a ceiling")
	}

	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBlockedBackoffDoublesUpToEight(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	tg := target("b1", "blocked.example")
	p := newProber()
	p.set(tg.URL, probe.Result{Status: 403})

	a := agent.New([]agent.Target{tg}, p, &fakeSink{})
	a.Rand = zeroRand
	a.Now = func() time.Time { return now }

	// The first block keeps the configured interval; only a repeat is a hint
	// that the monitor is being ignored.
	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := a.NextRun(tg.ID).Sub(now); got != tg.Interval {
		t.Fatalf("first block next in %v, want %v", got, tg.Interval)
	}

	for _, factor := range []time.Duration{2, 4, 8, 8} {
		now = a.NextRun(tg.ID)
		if _, err := a.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := tg.Interval * factor
		if got := a.NextRun(tg.ID).Sub(now); got != want {
			t.Fatalf("consecutive block next in %v, want %v", got, want)
		}
	}

	// Any other result resets the streak.
	p.set(tg.URL, probe.Result{Status: 200})
	now = a.NextRun(tg.ID)
	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := a.NextRun(tg.ID).Sub(now); got != tg.Interval {
		t.Fatalf("after reset next in %v, want %v", got, tg.Interval)
	}
}

func TestDownSchedulesRecoveryWatchForFiveMinutes(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	now := start
	tg := target("d1", "down.example")
	p := newProber()
	p.set(tg.URL, probe.Result{Status: 503})

	a := agent.New([]agent.Target{tg}, p, &fakeSink{})
	a.Rand = zeroRand
	a.Now = func() time.Time { return now }

	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := a.NextRun(tg.ID).Sub(now); got != 30*time.Second {
		t.Fatalf("down next in %v, want the 30s recovery watch", got)
	}

	// Still inside the five-minute window.
	now = a.NextRun(tg.ID)
	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := a.NextRun(tg.ID).Sub(now); got != 30*time.Second {
		t.Fatalf("still down next in %v, want 30s", got)
	}

	// Past it, the configured interval returns.
	now = start.Add(5*time.Minute + time.Second)
	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := a.NextRun(tg.ID).Sub(now); got != tg.Interval {
		t.Fatalf("past the watch next in %v, want %v", got, tg.Interval)
	}
}

func TestUnmeasurableUsesAnchorCadence(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	tg := target("u1", "blocked.example")
	tg.Interval = 10 * time.Minute // deliberately slower than the anchor cadence
	p := newProber()
	p.set(tg.URL, probe.Result{
		ErrorClass: "policy_blocked", Err: errors.New("destination refused"),
	})

	a := agent.New([]agent.Target{tg}, p, &fakeSink{})
	a.Rand = zeroRand
	a.Now = func() time.Time { return now }

	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := schedule.DefaultInterval(schedule.TierAnchor)
	if got := a.NextRun(tg.ID).Sub(now); got != want {
		t.Fatalf("unmeasurable next in %v, want the anchor cadence %v", got, want)
	}
}

func TestRetryAfterIsObeyedExactly(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	tg := target("r1", "limited.example")
	header := http.Header{}
	header.Set("Retry-After", "7")
	p := newProber()
	p.set(tg.URL, probe.Result{Status: 429, Header: header})

	a := agent.New([]agent.Target{tg}, p, &fakeSink{})
	a.Rand = zeroRand
	a.Now = func() time.Time { return now }

	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := a.NextRun(tg.ID).Sub(now); got != 7*time.Second {
		t.Fatalf("retry-after seconds next in %v, want 7s", got)
	}

	// An HTTP date is honoured too, relative to the agent's clock.
	now = a.NextRun(tg.ID)
	header.Set("Retry-After", now.Add(45*time.Second).UTC().Format(http.TimeFormat))
	if _, err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := a.NextRun(tg.ID).Sub(now); got != 45*time.Second {
		t.Fatalf("retry-after date next in %v, want 45s", got)
	}
}

func TestRemoveTargetStopsScheduling(t *testing.T) {
	t.Parallel()

	targets := []agent.Target{
		target("a1", "a.example"),
		target("a2", "b.example"),
	}
	a := agent.New(targets, newProber(), &fakeSink{})
	a.Rand = zeroRand

	if got := a.TargetCount(); got != 2 {
		t.Fatalf("target count = %d, want 2", got)
	}
	if !a.RemoveTarget("a1") {
		t.Fatal("removing a live target reported no removal")
	}
	if got := a.TargetCount(); got != 1 {
		t.Fatalf("target count after removal = %d, want 1", got)
	}
	if !a.NextRun("a1").IsZero() {
		t.Fatalf("a removed target is still scheduled at %v", a.NextRun("a1"))
	}
	if a.RemoveTarget("a1") {
		t.Fatal("removing the same target twice reported a removal")
	}
	if a.RemoveTarget("missing") {
		t.Fatal("removing an unknown target reported a removal")
	}
}

func TestInstrumentsHonourThePerTargetRedirectBudget(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/done", http.StatusFound)
	})
	mux.HandleFunc("/done", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "done")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	inst := agent.Instruments{HTTP: probe.NewHTTPWithTransport(http.DefaultTransport)}
	ctx := context.Background()

	// A one-hop target stops at the redirect; the same instrument still
	// follows the default budget for a target that did not set one.
	limited := agent.Target{ID: "a", URL: srv.URL + "/a", Instrument: "http", MaxRedirects: 1}
	if got := inst.Run(ctx, limited); got.Status != http.StatusFound {
		t.Fatalf("limited status = %d, want 302", got.Status)
	}
	byDefault := agent.Target{ID: "b", URL: srv.URL + "/a", Instrument: "http"}
	if got := inst.Run(ctx, byDefault); got.Status != http.StatusOK {
		t.Fatalf("default status = %d, want 200", got.Status)
	}
	if inst.HTTP.MaxRedirects != probe.DefaultMaxRedirects {
		t.Fatalf("the shared instrument budget changed to %d", inst.HTTP.MaxRedirects)
	}
}
