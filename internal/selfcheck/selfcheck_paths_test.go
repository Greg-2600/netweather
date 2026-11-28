package selfcheck_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/probe"
	"github.com/Greg-2600/netweather/internal/selfcheck"
)

type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

type captureTransport struct {
	req  *http.Request
	body string
}

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.req = req
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(c.body)),
		Request:    req,
	}, nil
}

func TestResolverServersReadsAndReportsErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "resolv.conf")
	body := "# comment\nnameserver 192.0.2.54\nnameserver 192.0.2.53\nnameserver 192.0.2.54\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := selfcheck.ResolverServers(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "192.0.2.53,192.0.2.54"; strings.Join(got, ",") != want {
		t.Fatalf("servers = %v, want %s", got, want)
	}

	if _, err := selfcheck.ResolverServers(filepath.Join(dir, "missing.conf")); err == nil {
		t.Fatal("a missing resolver file did not report an error")
	}
}

func TestDNSCheckReportsASuccessfulAnswer(t *testing.T) {
	t.Parallel()

	addr := startFakeDNS(t)
	dns := probe.NewDNS(func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	})

	ok, value, _, err := selfcheck.DNSCheck(dns, "127.0.0.1:53", "example.test").
		Run(context.Background())
	if err != nil || !ok {
		t.Fatalf("dns check = (%v, %q, %v)", ok, value, err)
	}
	if value != "203.0.113.9" {
		t.Fatalf("value = %q, want the first address", value)
	}
}

func TestDNSCheckNamesADeadline(t *testing.T) {
	t.Parallel()

	dns := probe.NewDNS(func(context.Context, string, string) (net.Conn, error) {
		return nil, context.DeadlineExceeded
	})
	ok, _, detail, _ := selfcheck.DNSCheck(dns, "127.0.0.1:53", "example.test").
		Run(context.Background())
	if ok || !strings.Contains(detail, "deadline exceeded") {
		t.Fatalf("deadline failure = (%v, %q)", ok, detail)
	}
}

func TestEgressCheckReportsASuccessfulHandshake(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	tcp := &probe.TCP{Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, ln.Addr().String())
	}}

	ok, value, _, err := selfcheck.EgressCheck(tcp, "203.0.113.9:443").
		Run(context.Background())
	if err != nil || !ok {
		t.Fatalf("egress check = (%v, %q, %v)", ok, value, err)
	}
	if value != "203.0.113.9:443" {
		t.Fatalf("value = %q, want the probed address", value)
	}
}

func TestResolverIdentityCheckRequiresAReader(t *testing.T) {
	t.Parallel()

	if ok, _, _, _ := selfcheck.ResolverIdentityCheck(nil).Run(context.Background()); ok {
		t.Fatal("a missing config reader reported success")
	}
}

func TestEgressIPCheckUsesTheDefaultTraceURL(t *testing.T) {
	t.Parallel()

	capture := &captureTransport{body: "ip=203.0.113.9\n"}
	check := selfcheck.EgressIPCheck(probe.NewHTTPWithTransport(capture), "", nil)

	ok, value, detail, err := check.Run(context.Background())
	if err != nil || !ok {
		t.Fatalf("check = (%v, %q, %q, %v)", ok, value, detail, err)
	}
	if value != "203.0.113.9" {
		t.Fatalf("value = %q", value)
	}
	const want = "https://1.1.1.1/cdn-cgi/trace"
	if capture.req == nil || capture.req.URL.String() != want {
		t.Fatalf("requested %v, want %s", capture.req, want)
	}
	if !strings.Contains(detail, want) {
		t.Fatalf("detail = %q, want the default URL named", detail)
	}
}

func TestEgressIPCheckReportsTransportFailure(t *testing.T) {
	t.Parallel()

	check := selfcheck.EgressIPCheck(
		probe.NewHTTPWithTransport(failingTransport{err: errors.New("boom")}),
		"https://trace.example/", nil)
	ok, _, detail, _ := check.Run(context.Background())
	if ok || !strings.Contains(detail, "boom") {
		t.Fatalf("failed transport = (%v, %q)", ok, detail)
	}
}

func TestGateZeroFreshnessUsesTheDefaultWindow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	gate := selfcheck.NewGate()
	gate.Fresh = 0
	gate.Now = func() time.Time { return now }

	gate.Observe([]selfcheck.Result{{Check: "clock", OK: false, At: now}})
	if !gate.Blocked("clock") {
		t.Fatal("a fresh failure was not blocked with zero freshness")
	}

	now = now.Add(selfcheck.DefaultGateFreshness + time.Minute)
	if gate.Blocked("clock") {
		t.Fatal("a stale failure was still blocked with zero freshness")
	}
}

func TestValueChangeIgnoresUnknownValues(t *testing.T) {
	t.Parallel()

	base := selfcheck.Result{Check: "resolver-identity", OK: true, Value: "192.0.2.53"}
	moved := selfcheck.Result{Check: "resolver-identity", OK: true, Value: "198.51.100.53"}
	failed := selfcheck.Result{Check: "resolver-identity", OK: false, Value: "x"}

	cases := []struct {
		name     string
		previous selfcheck.Result
		current  selfcheck.Result
		want     bool
	}{
		{"empty previous", selfcheck.Result{Check: "resolver-identity", OK: true}, base, false},
		{"empty current", base, selfcheck.Result{Check: "resolver-identity", OK: true}, false},
		{"failed previous", failed, base, false},
		{"failed current", base, failed, false},
		{"same value", base, base, false},
		{"moved value", base, moved, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, _ := selfcheck.ValueChange(tc.previous, tc.current); got != tc.want {
				t.Fatalf("ValueChange = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClockCheckEdgeCases(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	cases := []struct {
		name       string
		transport  http.RoundTripper
		maxSkew    time.Duration
		wantOK     bool
		wantDetail string
	}{
		{
			name:      "non-positive threshold uses the default",
			transport: dateTransport{date: now.Format(http.TimeFormat)},
			maxSkew:   0,
			wantOK:    true,
		},
		{
			name:       "future date is an absolute skew",
			transport:  dateTransport{date: now.Add(5 * time.Minute).Format(http.TimeFormat)},
			maxSkew:    time.Minute,
			wantOK:     false,
			wantDetail: "skew",
		},
		{
			name:       "unparseable date",
			transport:  dateTransport{date: "not-a-date"},
			maxSkew:    time.Minute,
			wantOK:     false,
			wantDetail: "unparseable",
		},
		{
			name:       "transport failure",
			transport:  failingTransport{err: errors.New("boom")},
			maxSkew:    time.Minute,
			wantOK:     false,
			wantDetail: "boom",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			check := selfcheck.ClockCheck(
				probe.NewHTTPWithTransport(tc.transport), "https://example.com/", tc.maxSkew)
			ok, _, detail, _ := check.Run(context.Background())
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (detail %q)", ok, tc.wantOK, detail)
			}
			if tc.wantDetail != "" && !strings.Contains(detail, tc.wantDetail) {
				t.Fatalf("detail = %q, want %q", detail, tc.wantDetail)
			}
		})
	}
}

func TestDiskFreeShareReportsErrors(t *testing.T) {
	t.Parallel()

	if _, err := selfcheck.DiskFreeShare(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing path did not report an error")
	}
}
