package fakeinternet_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/classify"
	"github.com/Greg-2600/netweather/internal/fakeinternet"
	"github.com/Greg-2600/netweather/internal/netx"
	"github.com/Greg-2600/netweather/internal/probe"
	"github.com/Greg-2600/netweather/internal/selfcheck"
	"github.com/Greg-2600/netweather/internal/state"
)

// startInternet loads the shipped scenario file and stands it up. Every test
// below runs the real instruments against it, which is the point of the
// harness: no hand-built probe results.
func startInternet(t *testing.T) *fakeinternet.Internet {
	t.Helper()

	scenarios, err := fakeinternet.LoadScenarios("../../testdata/internet/scenarios.yaml")
	if err != nil {
		t.Fatalf("load scenarios: %v", err)
	}
	inet, err := fakeinternet.Start(scenarios)
	if err != nil {
		t.Fatalf("start fake internet: %v", err)
	}
	t.Cleanup(func() { _ = inet.Close() })
	return inet
}

func TestLoadScenariosRejectsBadInput(t *testing.T) {
	t.Parallel()

	write := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "scenarios.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	if _, err := fakeinternet.LoadScenarios(write(t, "[]\n")); err == nil {
		t.Fatal("an empty file was accepted")
	}
	cases := []struct {
		name string
		body string
		want string
	}{
		{"no host", "- tls: valid\n", "no host"},
		{"duplicate", "- host: a.test\n- host: a.test\n", "duplicate"},
		{"unknown tls", "- host: a.test\n  tls: sideways\n", "unknown tls"},
		{"bad delay", "- host: a.test\n  delay: soon\n", "delay"},
		{"bad address", "- host: a.test\n  a: [\"not-an-ip\"]\n", "invalid address"},
		{"unknown field", "- host: a.test\n  tls_mode: valid\n", "field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := fakeinternet.LoadScenarios(write(t, tc.body))
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestHTTPScenariosClassify(t *testing.T) {
	t.Parallel()

	inet := startInternet(t)
	http := probe.NewHTTPWithTransport(inet.Transport())

	tests := []struct {
		name     string
		url      string
		expected classify.Expected
		want     state.State
	}{
		{"challenge page is BLOCKED", "https://chase.test/", classify.Expected{}, state.Blocked},
		{"plain 403 is BLOCKED", "https://akamai.test/", classify.Expected{}, state.Blocked},
		{"200 is reachability", "https://ok.test/", classify.Expected{}, state.UpReachability},
		{"503 is DOWN", "https://failing.test/", classify.Expected{}, state.Down},
		{
			"semantic marker makes it UP",
			"https://semantic.test/",
			classify.Expected{BodyContains: "all systems operational"},
			state.Up,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			res := http.Do(context.Background(), tc.url)
			got := classify.Decide(tc.expected, classify.Response{
				Err: res.Err, ErrorClass: res.ErrorClass,
				Status: res.Status, Header: res.Header,
				Body: res.Body, FinalURL: res.FinalURL,
			})
			if got.State != tc.want {
				t.Fatalf("%s: state = %s (%s), want %s", tc.url, got.State, got.Reason, tc.want)
			}
		})
	}
}

func TestTLSDefectsAreClassified(t *testing.T) {
	t.Parallel()

	inet := startInternet(t)
	tlsProbe := &probe.TLS{Dial: inet.DialContext, Config: inet.TLSConfig()}

	tests := []struct {
		host  string
		want  string
		state state.State
	}{
		{"expired.test:443", "tls_cert_expired", state.Down},
		{"mismatch.test:443", "tls_cert_mismatch", state.Down},
		{"untrusted.test:443", "tls_cert_untrusted", state.Down},
	}

	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			t.Parallel()

			res := tlsProbe.Do(context.Background(), tc.host)
			if !res.Failed() {
				t.Fatalf("%s: a broken certificate verified", tc.host)
			}
			if res.ErrorClass != tc.want {
				t.Fatalf("%s: class = %q, want %q", tc.host, res.ErrorClass, tc.want)
			}
			got := classify.Decide(classify.Expected{}, classify.Response{
				Err: res.Err, ErrorClass: res.ErrorClass,
			})
			if got.State != tc.state {
				t.Fatalf("%s: state = %s, want %s", tc.host, got.State, tc.state)
			}
		})
	}

	// A valid leaf keeps its issuer, which is what the pin baseline stores.
	res := tlsProbe.Do(context.Background(), "ok.test:443")
	if res.Failed() {
		t.Fatalf("valid leaf failed: %s: %v", res.ErrorClass, res.Err)
	}
	if !strings.Contains(res.Issuer, "fake internet CA") {
		t.Fatalf("issuer = %q, want the test CA", res.Issuer)
	}
}

func TestDNSScenarios(t *testing.T) {
	t.Parallel()

	inet := startInternet(t)
	dns := probe.NewDNS(inet.DialContext)

	res := dns.Do(context.Background(), inet.DNSAddr(), "ok.test")
	if res.Failed() {
		t.Fatalf("ok.test: %s: %v", res.ErrorClass, res.Err)
	}
	if len(res.Addresses) != 1 || res.Addresses[0].String() != "127.0.0.1" {
		t.Fatalf("addresses = %v, want the fake listener", res.Addresses)
	}

	if res := dns.Do(context.Background(), inet.DNSAddr(), "absent.test"); res.ErrorClass != "dns_nxdomain" {
		t.Fatalf("absent class = %q, want dns_nxdomain", res.ErrorClass)
	}
	if res := dns.Do(context.Background(), inet.DNSAddr(), "ipv6only.test"); res.ErrorClass != "dns_nodata" {
		t.Fatalf("ipv6only class = %q, want dns_nodata", res.ErrorClass)
	}
}

func TestSlowHostTimesOut(t *testing.T) {
	t.Parallel()

	inet := startInternet(t)
	http := probe.NewHTTPWithTransport(inet.Transport())
	http.Client.Timeout = 150 * time.Millisecond

	res := http.Do(context.Background(), "https://slow.test/")
	if res.ErrorClass != "http_timeout" {
		t.Fatalf("class = %q, want http_timeout (%v)", res.ErrorClass, res.Err)
	}
	got := classify.Decide(classify.Expected{}, classify.Response{
		Err: res.Err, ErrorClass: res.ErrorClass,
	})
	if got.State != state.Down {
		t.Fatalf("state = %s, want down", got.State)
	}
}

func TestSOCKSPathReachesTheFakeInternet(t *testing.T) {
	t.Parallel()

	inet := startInternet(t)
	dial, err := netx.Socks5(inet.SOCKSAddr())
	if err != nil {
		t.Fatalf("socks client: %v", err)
	}

	// The TCP instrument over the real SOCKS client and server: this is the
	// Tor vantage's last hop, exercised rather than assumed.
	tcp := probe.NewTCPWithDial(probe.DialFunc(dial))
	if res := tcp.Do(context.Background(), "ok.test:443"); res.Failed() {
		t.Fatalf("tcp over socks: %s: %v", res.ErrorClass, res.Err)
	}

	// The same hop with TLS and the test CA, which also proves the SOCKS
	// server forwards bytes instead of only completing its own handshake.
	rt := probe.TransportForDial(probe.DialFunc(dial))
	rt.TLSClientConfig.RootCAs = inet.Roots
	http := probe.NewHTTPWithTransport(rt)
	res := http.Do(context.Background(), "https://ok.test/")
	if res.Failed() {
		t.Fatalf("http over socks: %s: %v", res.ErrorClass, res.Err)
	}
	if res.Status != 200 || string(res.Body) != "ok" {
		t.Fatalf("response = %d %q", res.Status, res.Body)
	}

	// A host the fake internet does not serve is refused, not silently
	// resolved somewhere real.
	if res := tcp.Do(context.Background(), "elsewhere.test:443"); !res.Failed() {
		t.Fatal("an unknown host reached a listener")
	}
}

func TestClockEndpoint(t *testing.T) {
	t.Parallel()

	inet := startInternet(t)
	http := probe.NewHTTPWithTransport(inet.Transport())

	check := selfcheck.ClockCheck(http, inet.ClockURL(), 2*time.Second)
	ok, _, detail, err := check.Run(context.Background())
	if err != nil || !ok {
		t.Fatalf("clock check = (%v, %q, %v)", ok, detail, err)
	}

	// A large skew is exactly what the check exists to catch.
	inet.SetClock(time.Now().Add(90 * time.Second))
	ok, _, detail, _ = check.Run(context.Background())
	if ok {
		t.Fatalf("a 90s skew passed: %q", detail)
	}
}

func TestEgressEndpointFeedsTheIdentity(t *testing.T) {
	t.Parallel()

	inet := startInternet(t)
	http := probe.NewHTTPWithTransport(inet.Transport())

	check := selfcheck.EgressIPCheck(http, inet.EgressURL(), nil)
	ok, value, _, err := check.Run(context.Background())
	if err != nil || !ok {
		t.Fatalf("egress check = (%v, %q, %v)", ok, value, err)
	}
	if value != "203.0.113.9" {
		t.Fatalf("value = %q", value)
	}

	inet.SetEgressIP("198.51.100.4")
	ok, moved, _, err := check.Run(context.Background())
	if err != nil || !ok {
		t.Fatalf("second egress check = (%v, %q, %v)", ok, moved, err)
	}
	changed, detail := selfcheck.VantageChange(
		selfcheck.Result{Check: "egress-ip", OK: true, Value: value},
		selfcheck.Result{Check: "egress-ip", OK: true, Value: moved})
	if !changed || !strings.Contains(detail, "198.51.100.4") {
		t.Fatalf("change = (%v, %q)", changed, detail)
	}
}

func TestControlEndpointsAreLoopbackOnly(t *testing.T) {
	t.Parallel()

	inet := startInternet(t)
	for name, addr := range map[string]string{
		"dns":    inet.DNSAddr(),
		"https":  inet.HTTPSAddr(),
		"socks":  inet.SOCKSAddr(),
		"clock":  strings.TrimPrefix(inet.ClockURL(), "http://"),
		"egress": strings.TrimPrefix(inet.EgressURL(), "http://"),
	} {
		if !strings.HasPrefix(addr, "127.0.0.1:") {
			t.Errorf("%s listens on %q, want loopback", name, addr)
		}
	}

	// Close is idempotent: a cleanup that runs twice must not panic.
	if err := inet.Close(); err != nil {
		t.Fatal(err)
	}
	if err := inet.Close(); err != nil {
		t.Fatal(err)
	}
	// A closed internet refuses new connections rather than hanging.
	if _, err := net.DialTimeout("tcp", inet.SOCKSAddr(), 200*time.Millisecond); err == nil {
		t.Error("a closed listener still accepted a connection")
	}
}
