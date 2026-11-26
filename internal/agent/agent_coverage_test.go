package agent_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/agent"
	"github.com/Greg-2600/netweather/internal/dnsdiv"
	"github.com/Greg-2600/netweather/internal/probe"
	"github.com/Greg-2600/netweather/internal/state"
)

func refusedDial(context.Context, string, string) (net.Conn, error) {
	return nil, &net.OpError{Op: "dial", Err: errors.New("refused")}
}

func TestInstrumentsDispatchFamiliesAndEncryptedDNS(t *testing.T) {
	t.Parallel()

	inst := agent.Instruments{
		DNS:  probe.NewDNS(refusedDial),
		DoT:  probe.NewDoT(refusedDial),
		QUIC: probe.NewQUIC(refusedDial),
	}

	cases := []struct {
		name      string
		target    agent.Target
		wantClass string
	}{
		{"dns4", agent.Target{Instrument: "dns", Address: "x.example:53", Query: "example.com"}, "dns_error"},
		{"dns6", agent.Target{Instrument: "dns", Address: "x.example:53", Query: "example.com", Family: 6}, "dns_error"},
		{"dot4", agent.Target{Instrument: "dot", Address: "x.example:853", Query: "example.com"}, "dot_error"},
		{"dot6", agent.Target{Instrument: "dot", Address: "x.example:853", Query: "example.com", Family: 6}, "dot_error"},
		{"quic", agent.Target{Instrument: "quic", Address: "x.example:443"}, "quic_blocked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := inst.Run(context.Background(), tc.target)
			if got.ErrorClass != tc.wantClass {
				t.Fatalf("class = %q, want %q", got.ErrorClass, tc.wantClass)
			}
		})
	}

	for _, instrument := range []string{"doh", "dot", "quic"} {
		if got := (agent.Instruments{}).Run(context.Background(), agent.Target{Instrument: instrument}); got.Err == nil {
			t.Errorf("%s: a missing instrument must be an error, not a silent success", instrument)
		}
	}
}

func TestInstrumentsDispatchDoHFamily6(t *testing.T) {
	t.Parallel()

	var gotQuery, gotType string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("name")
		gotType = r.URL.Query().Get("type")
		_, _ = io.WriteString(w, `{"Status":0,"Answer":[{"type":28,"data":"2001:db8::9"}]}`)
	}))
	t.Cleanup(srv.Close)

	inst := agent.Instruments{DoH: probe.NewDoHWithClient(srv.Client())}
	got := inst.Run(context.Background(), agent.Target{
		ID: "doh6", URL: srv.URL + "/dns-query", Query: "example.com", Instrument: "doh", Family: 6,
	})
	if got.Failed() {
		t.Fatalf("doh6 dispatch failed: %s", got.ErrorClass)
	}
	if gotQuery != "example.com" || gotType != "AAAA" {
		t.Fatalf("query = %q type = %q, want example.com/AAAA", gotQuery, gotType)
	}
	if len(got.Addresses) != 1 || got.Addresses[0].String() != "2001:db8::9" {
		t.Fatalf("addresses = %v", got.Addresses)
	}
}

func TestDNSAuthoritativeAnswerIsReachability(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	tg := target("dns1", "198.41.0.4")
	tg.Instrument = "dns"
	tg.Address = "198.41.0.4:53"
	tg.Query = "example.com"
	tg.URL = ""

	p := newProber()
	p.set("", probe.Result{Authoritative: true, Latency: 3 * time.Millisecond})

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
	if got.Reason != "authoritative answer (zone served)" {
		t.Fatalf("reason = %q", got.Reason)
	}
	if got.Stage != "dns" {
		t.Fatalf("stage = %q, want dns", got.Stage)
	}
}

func TestNTPFailureStaysSelfHealth(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		res  probe.Result
	}{
		{"class", probe.Result{ErrorClass: "ntp_unreachable", Err: errors.New("no reply")}},
		{"error only", probe.Result{Err: errors.New("no reply")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
			tg := target("ntp1", "time.example")
			tg.Instrument = "ntp"
			tg.Address = "time.example:123"
			tg.URL = ""

			p := newProber()
			p.set("", tc.res)

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
			if got.Stage != "ntp" {
				t.Fatalf("stage = %q, want ntp", got.Stage)
			}
		})
	}
}

func TestSetTargetsBeforePrimingAndKeepsExistingSchedules(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	p := newProber()
	for _, host := range []string{"a.example", "b.example", "c.example"} {
		p.set("https://"+host+"/", probe.Result{Status: http.StatusOK})
	}
	sink := &fakeSink{}

	a := agent.New([]agent.Target{target("a", "a.example")}, p, sink)
	a.Now = at(now)
	a.Rand = zeroRand

	// Before the first Step the agent is not primed, so SetTargets must not
	// schedule anything: Step owns the cold start.
	a.SetTargets([]agent.Target{target("a", "a.example"), target("b", "b.example")})
	if !a.NextRun("a").IsZero() || !a.NextRun("b").IsZero() {
		t.Fatal("SetTargets scheduled targets before the agent was primed")
	}

	if ran, err := a.Step(context.Background()); err != nil || ran != 2 {
		t.Fatalf("Step = (%d, %v), want two cold-start probes", ran, err)
	}

	prevA, prevB := a.NextRun("a"), a.NextRun("b")
	a.SetTargets([]agent.Target{
		target("a", "a.example"),
		target("b", "b.example"),
		target("c", "c.example"),
	})
	if !a.NextRun("a").Equal(prevA) || !a.NextRun("b").Equal(prevB) {
		t.Fatal("SetTargets changed an existing target's schedule")
	}
	if a.NextRun("c").IsZero() {
		t.Fatal("SetTargets did not schedule a newly added target")
	}
}

func newDebugLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestCertificateChainAdvisoryAndLeafFailure(t *testing.T) {
	t.Parallel()

	tlsTarget := func() agent.Target {
		tg := target("tls1", "example.com")
		tg.Instrument = "tls"
		tg.Address = "example.com:443"
		tg.URL = ""
		return tg
	}

	cases := []struct {
		name    string
		pins    *fakePins
		result  probe.Result
		wantLog string
	}{
		{
			name:    "chain advisory",
			pins:    &fakePins{advisory: "intermediate rotated"},
			result:  probe.Result{CertIssuer: "CN=R11,O=Let's Encrypt,C=US"},
			wantLog: "cert_chain_changed",
		},
		{
			name:    "leaf write failure",
			pins:    &fakePins{leafErr: errors.New("db down")},
			result:  probe.Result{CertIssuer: "CN=R11,O=Let's Encrypt,C=US", CertLeafFingerprint: "aabbcc"},
			wantLog: "cert_leaf_failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
			p := newProber()
			p.set("", tc.result)

			var out strings.Builder
			logger := newDebugLogger(&out)
			sink := &fakeSink{}
			a := agent.New([]agent.Target{tlsTarget()}, p, sink)
			a.Now = at(now)
			a.Rand = zeroRand
			a.Pins = tc.pins
			a.Logger = logger

			if _, err := a.Step(context.Background()); err != nil {
				t.Fatal(err)
			}

			got := sink.batch()[0]
			if got.State != state.UpReachability || got.ErrorClass != "" {
				t.Fatalf("an informational pin event changed the measurement: %+v", got)
			}
			if !strings.Contains(out.String(), tc.wantLog) {
				t.Fatalf("log missing %q:\n%s", tc.wantLog, out.String())
			}
		})
	}
}

type scriptedDivergence struct {
	baselines   map[string]dnsdiv.Baseline
	answers     []dnsdiv.Answer
	verdicts    []dnsdiv.Verdict
	baselineErr error
	answerErr   error
	verdictErr  error
}

func (f *scriptedDivergence) RecordDNSAnswer(_ context.Context, _ string, ans dnsdiv.Answer) error {
	f.answers = append(f.answers, ans)
	return f.answerErr
}

func (f *scriptedDivergence) DNSBaselines(context.Context, string, time.Time) (map[string]dnsdiv.Baseline, error) {
	return f.baselines, f.baselineErr
}

func (f *scriptedDivergence) RecordDNSDivergence(_ context.Context, _ string, v dnsdiv.Verdict, _ time.Time) error {
	f.verdicts = append(f.verdicts, v)
	return f.verdictErr
}

func TestDNSDivergenceErrorsAreAdvisories(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	tg := dnsTarget("1.1.1.1:53", "9.9.9.9:53")

	cases := []struct {
		name        string
		div         *scriptedDivergence
		nsErr       error
		wantVerdict bool
	}{
		{
			name:        "baseline read fails",
			div:         &scriptedDivergence{baselineErr: errors.New("db down")},
			wantVerdict: false,
		},
		{
			name:        "answer write fails",
			div:         &scriptedDivergence{baselines: knownBaselines(tg.Resolvers...), answerErr: errors.New("db down")},
			wantVerdict: true,
		},
		{
			name:        "verdict write fails",
			div:         &scriptedDivergence{baselines: knownBaselines(tg.Resolvers...), verdictErr: errors.New("db down")},
			wantVerdict: true,
		},
		{
			name:        "delegation query fails",
			div:         &scriptedDivergence{baselines: knownBaselines(tg.Resolvers...)},
			nsErr:       errors.New("ns timeout"),
			wantVerdict: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newProber()
			p.set("", probe.Result{ErrorClass: "dns_nxdomain", Err: errors.New("no such host")})

			sink := &fakeSink{}
			a := agent.New([]agent.Target{tg}, p, sink)
			a.Now = at(now)
			a.Rand = zeroRand
			a.Divergence = tc.div
			a.DNSQuery = func(_ context.Context, server, _ string) probe.DNSResult {
				if server == tg.Resolvers[0] {
					return probe.DNSResult{ErrorClass: "dns_nxdomain", Err: errors.New("no such host")}
				}
				return probe.DNSResult{Addresses: []netip.Addr{netip.MustParseAddr("192.0.2.1")}}
			}
			if tc.nsErr != nil {
				a.DNSQueryNS = func(context.Context, string, string) ([]string, error) {
					return nil, tc.nsErr
				}
			}

			if _, err := a.Step(context.Background()); err != nil {
				t.Fatalf("a store error must not fail the probe: %v", err)
			}

			got := sink.batch()[0]
			if got.State != state.Unknown || got.ErrorClass != "dns_divergence" {
				t.Fatalf("probe = %+v, want the divergence advisory", got)
			}
			if len(tc.div.answers) != len(tg.Resolvers) {
				t.Fatalf("answers = %d, want %d", len(tc.div.answers), len(tg.Resolvers))
			}
			if gotVerdict := len(tc.div.verdicts) > 0; gotVerdict != tc.wantVerdict {
				t.Fatalf("verdicts = %+v, wantVerdict = %v", tc.div.verdicts, tc.wantVerdict)
			}
		})
	}
}

func TestLookupNewASNsBranches(t *testing.T) {
	t.Parallel()

	addrs := func(n int) []netip.Addr {
		out := make([]netip.Addr, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, netip.MustParseAddr("192.0.2."+strconv.Itoa(i+1)))
		}
		return out
	}

	cases := []struct {
		name        string
		addrs       []netip.Addr
		lookup      func(netip.Addr) (int, error)
		wantASNs    []int
		wantLookups int
	}{
		{
			name:  "caps lookups per probe",
			addrs: addrs(6),
			lookup: func(a netip.Addr) (int, error) {
				return 64500 + int(a.As4()[3]), nil
			},
			wantASNs:    []int{64501, 64502, 64503, 64504},
			wantLookups: 4,
		},
		{
			name:  "skips errors and zero ASNs",
			addrs: addrs(3),
			lookup: func(a netip.Addr) (int, error) {
				switch a.As4()[3] {
				case 1:
					return 0, errors.New("lookup failed")
				case 2:
					return 0, nil
				default:
					return 64500, nil
				}
			},
			wantASNs:    []int{64500},
			wantLookups: 3,
		},
		{
			name:  "deduplicates ASNs",
			addrs: addrs(3),
			lookup: func(netip.Addr) (int, error) {
				return 64500, nil
			},
			wantASNs:    []int{64500},
			wantLookups: 3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
			tg := dnsTarget("1.1.1.1:53", "9.9.9.9:53")

			p := newProber()
			p.set("", probe.Result{Addresses: tc.addrs})

			baselines := make(map[string]dnsdiv.Baseline, len(tg.Resolvers))
			for _, server := range tg.Resolvers {
				baselines[server] = dnsdiv.Baseline{Samples: 1}
			}
			div := &scriptedDivergence{baselines: baselines}

			a := agent.New([]agent.Target{tg}, p, &fakeSink{})
			a.Now = at(now)
			a.Rand = zeroRand
			a.Divergence = div
			lookups := 0
			a.LookupASN = func(_ context.Context, addr netip.Addr) (int, error) {
				lookups++
				return tc.lookup(addr)
			}
			a.DNSQuery = func(context.Context, string, string) probe.DNSResult {
				return probe.DNSResult{Addresses: tc.addrs}
			}

			if _, err := a.Step(context.Background()); err != nil {
				t.Fatal(err)
			}

			if lookups != tc.wantLookups*len(tg.Resolvers) {
				t.Fatalf("lookups = %d, want %d", lookups, tc.wantLookups*len(tg.Resolvers))
			}
			for _, ans := range div.answers {
				if len(ans.ASNs) != len(tc.wantASNs) {
					t.Fatalf("ASNs = %v, want %v", ans.ASNs, tc.wantASNs)
				}
				for i := range tc.wantASNs {
					if ans.ASNs[i] != tc.wantASNs[i] {
						t.Fatalf("ASNs = %v, want %v", ans.ASNs, tc.wantASNs)
					}
				}
			}
		})
	}
}

func TestStepRateSleepHonoursCancellation(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	targets := []agent.Target{target("a1", "a.example"), target("a2", "b.example")}
	p := newProber()
	for _, tg := range targets {
		p.set(tg.URL, probe.Result{Status: http.StatusOK})
	}

	var waits []time.Duration
	a := agent.New(targets, p, &fakeSink{})
	a.Now = at(base)
	a.Rand = zeroRand
	a.RateCeiling = 2
	a.RateStart = 2
	a.RateSince = base
	a.Sleep = func(d time.Duration) { waits = append(waits, d) }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ran, err := a.Step(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Step error = %v, want context.Canceled", err)
	}
	if ran != 1 {
		t.Fatalf("ran = %d, want the first probe before the cancelled wait", ran)
	}
	if len(waits) != 1 || waits[0] != 500*time.Millisecond {
		t.Fatalf("waits = %v, want one 500ms rate gap", waits)
	}
}
