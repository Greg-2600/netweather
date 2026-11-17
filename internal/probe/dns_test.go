package probe_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/probe"
)

// questionEnd returns the offset just past the question section of a query.
func questionEnd(msg []byte) int {
	i := 12
	for i < len(msg) && msg[i] != 0 {
		i += int(msg[i]) + 1
	}
	return i + 1 + 4 // root label + qtype + qclass
}

// dnsResponse builds a minimal response to a query. An empty ip means NOERROR
// with no answers; ipAnswer sets one A record; rcode is passed through.
func dnsResponse(query []byte, rcode int, ipAnswer string) []byte {
	end := questionEnd(query)
	out := make([]byte, end, end+16)
	copy(out, query[:end])

	out[2] = 0x81               // QR=1, RD=1
	out[3] = 0x80 | byte(rcode) // RA=1 plus the rcode
	out[4], out[5] = 0, 1       // QDCOUNT
	if ipAnswer == "" {
		out[6], out[7] = 0, 0
		return out
	}
	out[6], out[7] = 0, 1 // ANCOUNT

	answer := []byte{0xC0, 0x0C, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4}
	answer = append(answer, net.ParseIP(ipAnswer).To4()...)
	return append(out, answer...)
}

// startDNS runs a one-answer UDP responder on loopback.
func startDNS(t *testing.T, rcode int, ipAnswer string) string {
	t.Helper()

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(dnsResponse(buf[:n], rcode, ipAnswer), addr)
		}
	}()

	return pc.LocalAddr().String()
}

func TestDNSResolvesAnARecord(t *testing.T) {
	t.Parallel()

	server := startDNS(t, 0, "192.0.2.10")
	p := probe.NewDNS(nil)

	res := p.Do(context.Background(), server, "example.com")
	if res.Failed() {
		t.Fatalf("Failed: %s: %v", res.ErrorClass, res.Err)
	}
	if len(res.Addresses) != 1 || res.Addresses[0] != netip.MustParseAddr("192.0.2.10") {
		t.Fatalf("addresses = %v", res.Addresses)
	}
	if res.Latency <= 0 {
		t.Fatalf("latency = %v", res.Latency)
	}
}

func TestDNSReportsNXDomain(t *testing.T) {
	t.Parallel()

	server := startDNS(t, 3, "") // NXDOMAIN
	res := probe.NewDNS(nil).Do(context.Background(), server, "missing.example")

	if !res.Failed() {
		t.Fatal("NXDOMAIN should fail")
	}
	if res.ErrorClass != "dns_nxdomain" {
		t.Fatalf("class = %q, want dns_nxdomain (%v)", res.ErrorClass, res.Err)
	}
}

func TestDNSEmptyAnswerIsNotSuccess(t *testing.T) {
	t.Parallel()

	server := startDNS(t, 0, "") // NOERROR, no records
	res := probe.NewDNS(nil).Do(context.Background(), server, "empty.example")

	if !res.Failed() {
		t.Fatalf("a NOERROR/NODATA answer must not count as success: %+v", res)
	}
	// Go's resolver reports NODATA as not-found in some configurations and as a
	// bare empty result in others; either way it is a dns_ failure, never up.
	if res.ErrorClass != "dns_nxdomain" && res.ErrorClass != "dns_nodata" {
		t.Fatalf("class = %q, want dns_nxdomain or dns_nodata", res.ErrorClass)
	}
}

func TestDNSTimesOutOnASilentServer(t *testing.T) {
	t.Parallel()

	// A UDP listener that never answers: the lookup must obey the timeout.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()

	p := probe.NewDNS(nil)
	p.Timeout = 100 * time.Millisecond

	start := time.Now()
	res := p.Do(context.Background(), pc.LocalAddr().String(), "example.com")
	if res.ErrorClass != "dns_timeout" {
		t.Fatalf("class = %q, want dns_timeout (%v)", res.ErrorClass, res.Err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("lookup took %v despite a 100ms timeout", elapsed)
	}
}

func TestDNSRequiresServerAndName(t *testing.T) {
	t.Parallel()

	p := probe.NewDNS(nil)
	for _, tc := range []struct{ server, name string }{
		{"", "example.com"},
		{"127.0.0.1:53", ""},
	} {
		res := p.Do(context.Background(), tc.server, tc.name)
		if res.ErrorClass != "dns_config" {
			t.Errorf("(%q, %q) class = %q, want dns_config", tc.server, tc.name, res.ErrorClass)
		}
	}
}

func TestClassifyDNSError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"deadline", context.DeadlineExceeded, "dns_timeout"},
		{"cancelled", context.Canceled, "dns_timeout"},
		{"nxdomain", &net.DNSError{IsNotFound: true}, "dns_nxdomain"},
		{"timeout", &net.DNSError{IsTimeout: true}, "dns_timeout"},
		{"servfail", &net.DNSError{Err: "servfail"}, "dns_error"},
		{"other", errors.New("boom"), "dns_error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := probe.ClassifyDNSError(tc.err); got != tc.want {
				t.Fatalf("ClassifyDNSError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestDNSConstructorUsesDefaults(t *testing.T) {
	t.Parallel()

	p := probe.NewDNS(nil)
	if p.Dial == nil {
		t.Fatal("NewDNS left Dial nil without installing a default")
	}
	if p.Timeout != 5*time.Second {
		t.Fatalf("timeout = %v", p.Timeout)
	}
}

func TestDNSDoesNotUseTheSystemResolver(t *testing.T) {
	t.Parallel()

	// An unroutable server must fail even though the host can resolve names
	// through its own resolver. Falling back would hide an unreachable anchor.
	p := probe.NewDNS(nil)
	p.Timeout = 50 * time.Millisecond

	res := p.Do(context.Background(), "127.0.0.1:1", "example.com")
	if !res.Failed() {
		t.Fatal("an unreachable DNS server reported success")
	}
	if !strings.HasPrefix(res.ErrorClass, "dns_") {
		t.Fatalf("class = %q, want a dns_ class", res.ErrorClass)
	}
}
