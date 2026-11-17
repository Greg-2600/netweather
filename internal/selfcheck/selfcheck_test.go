package selfcheck_test

import (
	"context"
	"errors"
	"net"
	"net/http"
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

func TestDiskCheckWritesAndCleansUp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	check := selfcheck.DiskCheck(dir)
	ok, value, detail, err := check.Run(context.Background())
	if err != nil || !ok {
		t.Fatalf("disk check = (%v, %q, %q, %v)", ok, value, detail, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".selfcheck")); !os.IsNotExist(err) {
		t.Fatalf("temporary file was not removed: %v", err)
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
	date string
}

func (d dateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	h := make(http.Header)
	if d.date != "" {
		h.Set("Date", d.date)
	}
	return &http.Response{
		StatusCode: 200,
		Header:     h,
		Body:       http.NoBody,
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
