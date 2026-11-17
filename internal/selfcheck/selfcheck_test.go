package selfcheck_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/probe"
	"github.com/Greg-2600/netweather/internal/selfcheck"
	"github.com/Greg-2600/netweather/internal/store"
	"golang.org/x/net/dns/dnsmessage"
)

func TestRunnerRunsEveryCheck(t *testing.T) {
	t.Parallel()

	runner := &selfcheck.Runner{
		Now: func() time.Time { return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC) },
		Checks: []selfcheck.Check{
			{Name: "ok", Run: func(context.Context) (bool, string, string, error) {
				return true, "v", "d", nil
			}},
			{Name: "bad", Run: func(context.Context) (bool, string, string, error) {
				return false, "", "broke", nil
			}},
			{Name: "err", Run: func(context.Context) (bool, string, string, error) {
				return true, "", "", errors.New("boom")
			}},
		},
	}

	results := runner.Run(context.Background())
	if len(results) != 3 {
		t.Fatalf("results = %d", len(results))
	}
	if !results[0].OK || results[0].Value != "v" {
		t.Fatalf("first = %+v", results[0])
	}
	if results[1].OK || results[1].Detail != "broke" {
		t.Fatalf("second = %+v", results[1])
	}
	// An error always wins over the check's own ok flag.
	if results[2].OK || results[2].Detail != "boom" {
		t.Fatalf("third = %+v", results[2])
	}
	if results[0].At.IsZero() {
		t.Fatal("timestamp not set")
	}
}

type fakeSink struct {
	mu      []store.SelfCheck
	callErr error
}

func (f *fakeSink) RecordSelfCheck(_ context.Context, c store.SelfCheck) error {
	if f.callErr != nil {
		return f.callErr
	}
	f.mu = append(f.mu, c)
	return nil
}

func TestRunnerRecords(t *testing.T) {
	t.Parallel()

	sink := &fakeSink{}
	runner := &selfcheck.Runner{
		Checks: []selfcheck.Check{
			{Name: "a", Run: func(context.Context) (bool, string, string, error) {
				return true, "value", "", nil
			}},
		},
	}

	results, err := runner.Record(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || len(sink.mu) != 1 {
		t.Fatalf("results = %d, recorded = %d", len(results), len(sink.mu))
	}
	if sink.mu[0].Check != "a" || !sink.mu[0].OK || sink.mu[0].Value != "value" {
		t.Fatalf("recorded = %+v", sink.mu[0])
	}
}

func TestRunnerRecordPropagatesSinkFailure(t *testing.T) {
	t.Parallel()

	sink := &fakeSink{callErr: errors.New("disk full")}
	runner := &selfcheck.Runner{
		Checks: []selfcheck.Check{
			{Name: "a", Run: func(context.Context) (bool, string, string, error) { return true, "", "", nil }},
		},
	}

	if _, err := runner.Record(context.Background(), sink); err == nil {
		t.Fatal("a sink failure must surface")
	}
}

func TestGateOnlyTrustsFreshFailures(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	gate := selfcheck.NewGate()
	gate.Now = func() time.Time { return now }

	// An empty gate is not blocked: a missing self-check must not silence the
	// board.
	if gate.Blocked("port53") {
		t.Fatal("an empty gate reported blocked")
	}

	gate.Observe([]selfcheck.Result{
		{Check: "port53", OK: true, At: now},
		{Check: "clock", OK: false, At: now},
	})
	if gate.Blocked("port53") {
		t.Fatal("a passing check reported blocked")
	}
	if !gate.Blocked("clock") {
		t.Fatal("a failing clock check was not reported")
	}

	// A failure inside the window is trusted; one outside it is stale, and a
	// stale fact must not keep overriding the classifier.
	gate.Observe([]selfcheck.Result{{Check: "port53", OK: false, At: now}})
	if !gate.Blocked("port53") {
		t.Fatal("a fresh failure was not reported")
	}
	now = now.Add(selfcheck.DefaultGateFreshness + time.Minute)
	if gate.Blocked("port53") {
		t.Fatal("a stale failure was still reported")
	}

	// A newer passing result clears an older failure.
	now = now.Add(time.Minute)
	gate.Observe([]selfcheck.Result{{Check: "port53", OK: true, At: now}})
	if gate.Blocked("port53") {
		t.Fatal("a passing result did not clear the failure")
	}
}

func TestDiskCheckWritesAndCleansUp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	check := selfcheck.DiskCheck(dir)
	ok, value, detail, err := check.Run(context.Background())
	if err != nil || !ok {
		t.Fatalf("disk check = (%v, %q, %q, %v)", ok, value, detail, err)
	}
	if !strings.Contains(value, "64 KiB written and synced") || !strings.Contains(value, "% free") {
		t.Fatalf("value = %q, want the write and the free share", value)
	}
	if _, err := os.Stat(filepath.Join(dir, ".selfcheck")); !os.IsNotExist(err) {
		t.Fatalf("temporary file was not removed: %v", err)
	}

	// The free-share measurement is what makes the warning mean something.
	share, err := selfcheck.DiskFreeShare(dir)
	if err != nil {
		t.Fatalf("DiskFreeShare: %v", err)
	}
	if share <= 0 || share > 1 {
		t.Fatalf("free share = %v, want (0,1]", share)
	}
}

func TestDiskCheckFailsOnAnUnwritablePath(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "missing", "nested")
	check := selfcheck.DiskCheck(dir)
	ok, _, detail, err := check.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("writing into a non-existent directory reported success")
	}
	if detail == "" {
		t.Fatal("failure had no detail")
	}

	if ok, _, _, _ := selfcheck.DiskCheck("").Run(context.Background()); ok {
		t.Fatal("an empty data directory reported success")
	}
}

func TestDNSCheck(t *testing.T) {
	t.Parallel()

	// A tiny fake DNS responder would repeat the probe tests; here the seam is
	// the dialer, so a refused connect must produce a failed check.
	failing := func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: errors.New("refused")}
	}
	check := selfcheck.DNSCheck(probe.NewDNS(failing), "127.0.0.1:53", "example.com")
	ok, _, detail, err := check.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a refused resolver reported success")
	}
	if !strings.Contains(detail, "dns_") {
		t.Fatalf("detail = %q, want a dns_ class", detail)
	}

	if ok, _, _, _ := selfcheck.DNSCheck(nil, "", "").Run(context.Background()); ok {
		t.Fatal("a missing instrument reported success")
	}
}

func TestEgressCheck(t *testing.T) {
	t.Parallel()

	failing := func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: errors.New("refused")}
	}
	check := selfcheck.EgressCheck(&probe.TCP{Dial: failing}, "1.1.1.1:443")
	ok, _, detail, _ := check.Run(context.Background())
	if ok || !strings.Contains(detail, "tcp_") {
		t.Fatalf("egress check = (%v, %q)", ok, detail)
	}

	if ok, _, _, _ := selfcheck.EgressCheck(nil, "").Run(context.Background()); ok {
		t.Fatal("a missing instrument reported success")
	}
}

// startFakeDNS answers one A record per query on a local UDP port. It lets the
// port-53 check be exercised end to end without a resolver or a network.
func startFakeDNS(t *testing.T) string {
	t.Helper()

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var query dnsmessage.Message
			if err := query.Unpack(buf[:n]); err != nil {
				continue
			}
			resp := dnsmessage.Message{
				Header:    dnsmessage.Header{ID: query.ID, Response: true},
				Questions: query.Questions,
			}
			if len(query.Questions) > 0 {
				resp.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{
						Name: query.Questions[0].Name, Type: dnsmessage.TypeA,
						Class: dnsmessage.ClassINET, TTL: 60,
					},
					Body: &dnsmessage.AResource{A: [4]byte{203, 0, 113, 9}},
				}}
			}
			packed, err := resp.Pack()
			if err != nil {
				continue
			}
			_, _ = pc.WriteTo(packed, addr)
		}
	}()

	return pc.LocalAddr().String()
}

func TestMTUCheckReportsTheSmallestInterface(t *testing.T) {
	t.Parallel()

	interfaces := func() ([]net.Interface, error) {
		return []net.Interface{
			{Name: "lo", MTU: 65536, Flags: net.FlagUp | net.FlagLoopback},
			{Name: "down0", MTU: 900, Flags: 0},
			{Name: "eth0", MTU: 1500, Flags: net.FlagUp},
			{Name: "vpn0", MTU: 1400, Flags: net.FlagUp},
		}, nil
	}

	ok, value, detail, err := selfcheck.MTUCheck(interfaces).Run(context.Background())
	if err != nil || !ok {
		t.Fatalf("check = (%v, %q, %q, %v)", ok, value, detail, err)
	}
	if value != "vpn0 1400" {
		t.Fatalf("value = %q, want the smallest up interface", value)
	}
	if !strings.Contains(detail, "vpn0") {
		t.Fatalf("detail = %q", detail)
	}
}

func TestMTUCheckWarnsBelowTheFloor(t *testing.T) {
	t.Parallel()

	interfaces := func() ([]net.Interface, error) {
		return []net.Interface{{Name: "wg0", MTU: 1280, Flags: net.FlagUp}}, nil
	}

	ok, value, detail, err := selfcheck.MTUCheck(interfaces).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("an MTU below the floor reported healthy")
	}
	if value != "wg0 1280" {
		t.Fatalf("value = %q", value)
	}
	if !strings.Contains(detail, "floor") {
		t.Fatalf("detail = %q, want the floor named", detail)
	}
}

func TestMTUCheckFailuresAreVisible(t *testing.T) {
	t.Parallel()

	failing := func() ([]net.Interface, error) { return nil, errors.New("no interfaces") }
	if ok, _, detail, _ := selfcheck.MTUCheck(failing).Run(context.Background()); ok || detail == "" {
		t.Fatalf("error case = (%v, %q)", ok, detail)
	}

	empty := func() ([]net.Interface, error) {
		return []net.Interface{
			{Name: "lo", MTU: 65536, Flags: net.FlagUp | net.FlagLoopback},
			{Name: "down0", MTU: 1500, Flags: 0},
			{Name: "zero0", MTU: 0, Flags: net.FlagUp},
		}, nil
	}
	if ok, _, detail, _ := selfcheck.MTUCheck(empty).Run(context.Background()); ok || !strings.Contains(detail, "no up") {
		t.Fatalf("empty case = (%v, %q)", ok, detail)
	}
}

func TestPort53CheckRequiresBothTransports(t *testing.T) {
	t.Parallel()

	udpAddr := startFakeDNS(t)
	accepting, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = accepting.Close() })
	go func() {
		for {
			conn, err := accepting.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	dns := probe.NewDNS(func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, udpAddr)
	})
	tcp := &probe.TCP{Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, accepting.Addr().String())
	}}

	ok, value, detail, err := selfcheck.Port53Check(dns, tcp, "127.0.0.1:53", "example.test").
		Run(context.Background())
	if err != nil || !ok {
		t.Fatalf("check = (%v, %q, %q, %v)", ok, value, detail, err)
	}
	if value != "udp+tcp" {
		t.Fatalf("value = %q", value)
	}
}

func TestPort53CheckNamesTheBlockedTransport(t *testing.T) {
	t.Parallel()

	refused := func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: errors.New("refused")}
	}

	// Both blocked: UDP is reported first because it is the common case.
	ok, value, detail, _ := selfcheck.Port53Check(
		probe.NewDNS(refused), &probe.TCP{Dial: refused}, "127.0.0.1:53", "example.test").
		Run(context.Background())
	if ok || value != "udp blocked" || !strings.Contains(detail, "udp/53") {
		t.Fatalf("udp failure = (%v, %q, %q)", ok, value, detail)
	}

	// UDP answers, TCP is blocked: a middlebox that allows queries but not
	// fallback is exactly what this half of the check exists to find.
	udpAddr := startFakeDNS(t)
	dns := probe.NewDNS(func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, udpAddr)
	})
	ok, value, detail, _ = selfcheck.Port53Check(dns, &probe.TCP{Dial: refused},
		"127.0.0.1:53", "example.test").Run(context.Background())
	if ok || value != "tcp blocked" || !strings.Contains(detail, "tcp/53") {
		t.Fatalf("tcp failure = (%v, %q, %q)", ok, value, detail)
	}

	// A missing instrument is a configuration error, not a pass.
	if ok, _, _, _ := selfcheck.Port53Check(nil, &probe.TCP{Dial: refused}, "x:53", "x").
		Run(context.Background()); ok {
		t.Fatal("a missing DNS instrument reported success")
	}
	if ok, _, _, _ := selfcheck.Port53Check(probe.NewDNS(refused), nil, "x:53", "x").
		Run(context.Background()); ok {
		t.Fatal("a missing TCP instrument reported success")
	}
}

type dateTransport struct {
	date   string
	status int
	body   string
}

func (d dateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	h := make(http.Header)
	if d.date != "" {
		h.Set("Date", d.date)
	}
	status := d.status
	if status == 0 {
		status = http.StatusOK
	}
	body := io.NopCloser(strings.NewReader(d.body))
	if d.body == "" {
		body = http.NoBody
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       body,
		Request:    req,
	}, nil
}

func TestClockCheckDetectsSkew(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	accurate := now.Format(http.TimeFormat)
	check := selfcheck.ClockCheck(probe.NewHTTPWithTransport(dateTransport{date: accurate}),
		"https://example.com/", time.Minute)
	if ok, _, detail, _ := check.Run(context.Background()); !ok {
		t.Fatalf("an accurate clock failed: %s", detail)
	}

	skewed := now.Add(-10 * time.Minute).Format(http.TimeFormat)
	check = selfcheck.ClockCheck(probe.NewHTTPWithTransport(dateTransport{date: skewed}),
		"https://example.com/", time.Minute)
	ok, value, detail, _ := check.Run(context.Background())
	if ok {
		t.Fatal("a ten-minute skew reported healthy")
	}
	if value == "" || !strings.Contains(detail, "skew") {
		t.Fatalf("skew check = (%q, %q)", value, detail)
	}
}

func TestClockCheckRequiresADateHeader(t *testing.T) {
	t.Parallel()

	check := selfcheck.ClockCheck(probe.NewHTTPWithTransport(dateTransport{}),
		"https://example.com/", time.Minute)
	ok, _, detail, _ := check.Run(context.Background())
	if ok || !strings.Contains(detail, "Date") {
		t.Fatalf("clock check = (%v, %q)", ok, detail)
	}
}

func TestClockCheckMissingInstrument(t *testing.T) {
	t.Parallel()

	if ok, _, _, _ := selfcheck.ClockCheck(nil, "", 0).Run(context.Background()); ok {
		t.Fatal("a missing instrument reported success")
	}
}

func TestEgressIPCheckRecordsTheAddress(t *testing.T) {
	t.Parallel()

	body := "fl=1f2\nh=1.1.1.1\nip=203.0.113.9\nts=1763263200\n"
	check := selfcheck.EgressIPCheck(
		probe.NewHTTPWithTransport(dateTransport{body: body}),
		"https://1.1.1.1/cdn-cgi/trace", nil)

	ok, value, detail, err := check.Run(context.Background())
	if err != nil || !ok {
		t.Fatalf("check = (%v, %q, %q, %v)", ok, value, detail, err)
	}
	if value != "203.0.113.9" {
		t.Fatalf("value = %q", value)
	}
	if !strings.Contains(detail, "1.1.1.1") {
		t.Fatalf("detail = %q", detail)
	}
}

func TestEgressIPCheckEnrichesWithASNAndCountry(t *testing.T) {
	t.Parallel()

	body := "ip=203.0.113.9\n"
	lookup := func(context.Context, netip.Addr) (int, string) { return 64500, "US" }
	check := selfcheck.EgressIPCheck(
		probe.NewHTTPWithTransport(dateTransport{body: body}), "https://trace.example/", lookup)

	ok, value, _, err := check.Run(context.Background())
	if err != nil || !ok {
		t.Fatalf("check = (%v, %q, %v)", ok, value, err)
	}
	if value != "203.0.113.9,AS64500,US" {
		t.Fatalf("value = %q, want the recorded identity tuple", value)
	}
}

func TestEgressIPCheckFailures(t *testing.T) {
	t.Parallel()

	if ok, _, _, _ := selfcheck.EgressIPCheck(nil, "", nil).Run(context.Background()); ok {
		t.Fatal("a missing instrument reported success")
	}

	noIP := selfcheck.EgressIPCheck(
		probe.NewHTTPWithTransport(dateTransport{body: "fl=1\nh=x\n"}), "https://trace.example/", nil)
	if ok, _, detail, _ := noIP.Run(context.Background()); ok || !strings.Contains(detail, "ip") {
		t.Fatalf("missing ip = (%v, %q)", ok, detail)
	}

	badStatus := selfcheck.EgressIPCheck(
		probe.NewHTTPWithTransport(dateTransport{status: 502, body: "ip=203.0.113.9\n"}),
		"https://trace.example/", nil)
	if ok, _, detail, _ := badStatus.Run(context.Background()); ok || !strings.Contains(detail, "502") {
		t.Fatalf("bad status = (%v, %q)", ok, detail)
	}
}

func TestVantageChange(t *testing.T) {
	t.Parallel()

	first := selfcheck.Result{Check: "egress-ip", OK: true, Value: "203.0.113.9,AS7922,US"}
	same := selfcheck.Result{Check: "egress-ip", OK: true, Value: "203.0.113.9,AS7922,US"}
	moved := selfcheck.Result{Check: "egress-ip", OK: true, Value: "198.51.100.4,AS9009,NL"}

	if changed, _ := selfcheck.VantageChange(first, same); changed {
		t.Fatal("an identical record was reported as a change")
	}
	changed, detail := selfcheck.VantageChange(first, moved)
	if !changed || !strings.Contains(detail, "203.0.113.9") || !strings.Contains(detail, "198.51.100.4") {
		t.Fatalf("change = (%v, %q)", changed, detail)
	}

	// An unknown is not evidence that the address moved.
	failed := selfcheck.Result{Check: "egress-ip", OK: false, Value: ""}
	if changed, _ := selfcheck.VantageChange(first, failed); changed {
		t.Fatal("a failed observation was reported as a change")
	}
	if changed, _ := selfcheck.VantageChange(failed, first); changed {
		t.Fatal("a missing previous observation was reported as a change")
	}
}

func TestClockSyncCheckUsesTheTenSecondSuppressionThreshold(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()

	// Five seconds is inside the warning band but still allows correlation.
	five := now.Add(-5 * time.Second).Format(http.TimeFormat)
	check := selfcheck.ClockSyncCheck(probe.NewHTTPWithTransport(dateTransport{date: five}),
		"https://example.com/")
	if ok, _, detail, _ := check.Run(context.Background()); !ok {
		t.Fatalf("five seconds suppressed correlation: %s", detail)
	}
	if check.Name != "clock-sync" {
		t.Fatalf("name = %q, want clock-sync", check.Name)
	}

	// Twelve seconds is past the suppression threshold.
	twelve := now.Add(-12 * time.Second).Format(http.TimeFormat)
	check = selfcheck.ClockSyncCheck(probe.NewHTTPWithTransport(dateTransport{date: twelve}),
		"https://example.com/")
	ok, _, detail, _ := check.Run(context.Background())
	if ok {
		t.Fatal("a twelve-second skew was allowed to correlate")
	}
	if !strings.Contains(detail, "skew") {
		t.Fatalf("detail = %q", detail)
	}
}

func TestParseResolverServers(t *testing.T) {
	t.Parallel()

	body := `# generated by NetworkManager
search example.com
nameserver 192.0.2.53
nameserver 192.0.2.54
; a comment
nameserver 192.0.2.53
options edns0
`
	got := selfcheck.ParseResolverServers(body)
	want := []string{"192.0.2.53", "192.0.2.54"}
	if len(got) != len(want) {
		t.Fatalf("servers = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("servers = %v, want %v (sorted and deduplicated)", got, want)
		}
	}

	if servers := selfcheck.ParseResolverServers("search only.example\n"); len(servers) != 0 {
		t.Fatalf("a file with no nameserver = %v", servers)
	}
}

func TestResolverIdentityCheck(t *testing.T) {
	t.Parallel()

	check := selfcheck.ResolverIdentityCheck(func() ([]string, error) {
		return []string{"192.0.2.53", "192.0.2.54"}, nil
	})
	ok, value, detail, _ := check.Run(context.Background())
	if !ok {
		t.Fatalf("identity failed: %s", detail)
	}
	if check.Name != "resolver-identity" || value != "192.0.2.53,192.0.2.54" {
		t.Fatalf("check = (%q, %q, %q)", check.Name, value, detail)
	}

	empty := selfcheck.ResolverIdentityCheck(func() ([]string, error) { return nil, nil })
	if ok, _, detail, _ := empty.Run(context.Background()); ok || !strings.Contains(detail, "no name servers") {
		t.Fatalf("empty resolver set = (%v, %q)", ok, detail)
	}

	broken := selfcheck.ResolverIdentityCheck(func() ([]string, error) {
		return nil, errors.New("permission denied")
	})
	if ok, _, detail, _ := broken.Run(context.Background()); ok || !strings.Contains(detail, "permission denied") {
		t.Fatalf("unreadable config = (%v, %q)", ok, detail)
	}
}

func TestValueChangeUsesTheCheckName(t *testing.T) {
	t.Parallel()

	previous := selfcheck.Result{Check: "resolver-identity", OK: true, Value: "192.0.2.53,192.0.2.54"}
	same := selfcheck.Result{Check: "resolver-identity", OK: true, Value: "192.0.2.53,192.0.2.54"}
	moved := selfcheck.Result{Check: "resolver-identity", OK: true, Value: "198.51.100.53"}

	if changed, _ := selfcheck.ValueChange(previous, same); changed {
		t.Fatal("an identical resolver set was a change")
	}
	changed, detail := selfcheck.ValueChange(previous, moved)
	if !changed {
		t.Fatal("a moved resolver set was not a change")
	}
	if !strings.Contains(detail, "resolver-identity changed") ||
		!strings.Contains(detail, "192.0.2.53") || !strings.Contains(detail, "198.51.100.53") {
		t.Fatalf("detail = %q", detail)
	}

	// A failed read is not evidence that the set moved.
	failed := selfcheck.Result{Check: "resolver-identity", OK: false}
	if changed, _ := selfcheck.ValueChange(failed, moved); changed {
		t.Fatal("a failed observation was reported as a change")
	}
}

func TestRedactEgressValue(t *testing.T) {
	t.Parallel()

	cases := []struct{ in, want string }{
		{"203.0.113.9,AS7922,US", "[redacted],AS7922,US"},
		{"2001:db8::1,AS64500,DE", "[redacted],AS64500,DE"},
		{"203.0.113.9", "[redacted]"},
		{"", ""},
		{"not-an-address", "not-an-address"},
		{"unavailable: dial tcp: timeout", "unavailable: dial tcp: timeout"},
	}
	for _, tc := range cases {
		if got := selfcheck.RedactEgressValue(tc.in); got != tc.want {
			t.Errorf("RedactEgressValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
