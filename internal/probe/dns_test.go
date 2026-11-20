package probe_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/probe"
	"golang.org/x/net/dns/dnsmessage"
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
	// Latency is measured on loopback, where a coarse platform timer can read
	// zero. The assertion is that it is not negative, not that it is non-zero.
	if res.Latency < 0 {
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
	// The wire client can tell NODATA from NXDOMAIN, so the class is exact
	// rather than "one of two".
	if res.ErrorClass != "dns_nodata" {
		t.Fatalf("class = %q, want dns_nodata", res.ErrorClass)
	}
}

// authoritativeResponse builds a NOERROR answer with the Authoritative bit set
// and no A record. nsInAnswer puts an NS delegation in the answer section (the
// root-server case); otherwise an SOA goes in the authority section (a ccTLD
// apex negative answer).
func authoritativeResponse(query []byte, nsInAnswer bool) []byte {
	var msg dnsmessage.Message
	if err := msg.Unpack(query); err != nil {
		panic(err)
	}
	msg.Response = true
	msg.Authoritative = true
	msg.RecursionDesired = true
	msg.RecursionAvailable = true
	msg.RCode = dnsmessage.RCodeSuccess
	owner := msg.Questions[0].Name
	if nsInAnswer {
		msg.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: owner, Type: dnsmessage.TypeNS, Class: dnsmessage.ClassINET, TTL: 60},
			Body:   &dnsmessage.NSResource{NS: dnsmessage.MustNewName("ns.example.")},
		}}
	} else {
		msg.Authorities = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: owner, Type: dnsmessage.TypeSOA, Class: dnsmessage.ClassINET, TTL: 60},
			Body: &dnsmessage.SOAResource{
				NS:   dnsmessage.MustNewName("ns.example."),
				MBox: dnsmessage.MustNewName("hostmaster.example."),
			},
		}}
	}
	packed, err := msg.Pack()
	if err != nil {
		panic(err)
	}
	return packed
}

// nonAuthoritativeNODATA is the recursive-resolver case: an SOA in authority,
// but no Authoritative bit, so it says nothing about the zone's health.
func nonAuthoritativeNODATA(query []byte) []byte {
	resp := authoritativeResponse(query, false)
	var msg dnsmessage.Message
	if err := msg.Unpack(resp); err != nil {
		panic(err)
	}
	msg.Authoritative = false
	out, err := msg.Pack()
	if err != nil {
		panic(err)
	}
	return out
}

func TestDNSAuthoritativeApexIsReachable(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		nsInAnswer bool
	}{
		{"soa_in_authority", false},
		{"ns_in_answer", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := startDNSResponder(t, func(q []byte) []byte {
				return authoritativeResponse(q, tc.nsInAnswer)
			})
			res := probe.NewDNS(nil).Do(context.Background(), server, "ac")
			if res.Failed() {
				t.Fatalf("authoritative apex should be reachable: %s: %v", res.ErrorClass, res.Err)
			}
			if !res.Authoritative {
				t.Fatal("Authoritative flag not set on an apex delegation answer")
			}
			if len(res.Addresses) != 0 {
				t.Fatalf("addresses = %v, want none", res.Addresses)
			}
		})
	}
}

func TestDNSAuthoritativeNODATAWithoutDelegationStillFails(t *testing.T) {
	t.Parallel()

	server := startDNSResponder(t, func(query []byte) []byte {
		msg, ok := decodeQuery(query)
		if !ok || len(msg.Questions) != 1 {
			return nil
		}
		msg.Response = true
		msg.Authoritative = true
		msg.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{
				Name: msg.Questions[0].Name, Type: dnsmessage.TypeCNAME,
				Class: dnsmessage.ClassINET, TTL: 60,
			},
			Body: &dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName("alias.example.")},
		}}
		return packMessage(msg)
	})

	res := probe.NewDNS(nil).Do(context.Background(), server, "empty.example")
	if res.ErrorClass != "dns_nodata" || res.Authoritative {
		t.Fatalf("class = %q, authoritative = %v", res.ErrorClass, res.Authoritative)
	}
}

func TestDNSNonAuthoritativeNODATAStillFails(t *testing.T) {
	t.Parallel()

	server := startDNSResponder(t, nonAuthoritativeNODATA)
	res := probe.NewDNS(nil).Do(context.Background(), server, "empty.example")
	if !res.Failed() || res.ErrorClass != "dns_nodata" {
		t.Fatalf("class = %q, want dns_nodata (authoritative bit must be required)", res.ErrorClass)
	}
}

func TestDNSReportsRCODEs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		rcode int
		want  string
	}{
		{2, "dns_servfail"},
		{5, "dns_refused"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()

			server := startDNS(t, tc.rcode, "")
			res := probe.NewDNS(nil).Do(context.Background(), server, "example.com")
			if !res.Failed() {
				t.Fatalf("rcode %d reported success", tc.rcode)
			}
			if res.ErrorClass != tc.want {
				t.Fatalf("class = %q, want %q (%v)", res.ErrorClass, tc.want, res.Err)
			}
		})
	}
}

func TestDNSRetriesATruncatedAnswerOverTCP(t *testing.T) {
	t.Parallel()

	// DNS over TCP uses the same port, so the TCP listener is created first and
	// the UDP socket is bound to its port. Windows keeps separate ephemeral
	// ranges for TCP and UDP and excludes some ranges entirely, so a port TCP
	// hands out can be unbindable for UDP; retry the pair rather than failing
	// the suite on the first choice.
	var (
		tcpLn net.Listener
		udpPC net.PacketConn
	)
	for attempt := 0; attempt < 20; attempt++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().(*net.TCPAddr)
		pc, err := net.ListenPacket("udp", addr.String())
		if err != nil {
			_ = ln.Close()
			continue
		}
		tcpLn, udpPC = ln, pc
		break
	}
	if tcpLn == nil || udpPC == nil {
		t.Fatal("could not bind a TCP/UDP pair on one loopback port after 20 attempts")
	}
	t.Cleanup(func() { _ = tcpLn.Close() })
	t.Cleanup(func() { _ = udpPC.Close() })

	go func() {
		for {
			conn, err := tcpLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				var size [2]byte
				if _, err := readFully(c, size[:]); err != nil {
					return
				}
				query := make([]byte, int(size[0])<<8|int(size[1]))
				if _, err := readFully(c, query); err != nil {
					return
				}
				answer := dnsResponse(query, 0, "192.0.2.44")
				framed := append([]byte{byte(len(answer) >> 8), byte(len(answer))}, answer...)
				_, _ = c.Write(framed)
			}(conn)
		}
	}()

	go func() {
		buf := make([]byte, 512)
		for {
			n, peer, err := udpPC.ReadFrom(buf)
			if err != nil {
				return
			}
			truncated := dnsResponse(buf[:n], 0, "")
			truncated[2] |= 0x02 // TC: ask again over TCP
			_, _ = udpPC.WriteTo(truncated, peer)
		}
	}()

	res := probe.NewDNS(nil).Do(context.Background(), tcpLn.Addr().String(), "example.com")
	if res.Failed() {
		t.Fatalf("truncated retry failed: %s: %v", res.ErrorClass, res.Err)
	}
	if len(res.Addresses) != 1 || res.Addresses[0] != netip.MustParseAddr("192.0.2.44") {
		t.Fatalf("addresses = %v, want the TCP answer", res.Addresses)
	}
}

func readFully(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// startDNSResponder runs a UDP responder that builds its reply from the query.
// A nil reply sends nothing, which lets a test assert a timeout.
func startDNSResponder(t *testing.T, reply func(query []byte) []byte) string {
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
			if out := reply(buf[:n]); out != nil {
				_, _ = pc.WriteTo(out, addr)
			}
		}
	}()

	return pc.LocalAddr().String()
}

// decodeQuery parses a query so a responder can echo its ID and question.
func decodeQuery(query []byte) (dnsmessage.Message, bool) {
	var msg dnsmessage.Message
	if err := msg.Unpack(query); err != nil {
		return dnsmessage.Message{}, false
	}
	return msg, true
}

func packMessage(msg dnsmessage.Message) []byte {
	packed, err := msg.Pack()
	if err != nil {
		return nil
	}
	return packed
}

func TestDNSNormalisesTheQName(t *testing.T) {
	t.Parallel()

	seen := make(chan string, 1)
	server := startDNSResponder(t, func(query []byte) []byte {
		msg, ok := decodeQuery(query)
		if !ok || len(msg.Questions) != 1 {
			return nil
		}
		seen <- msg.Questions[0].Name.String()
		return dnsResponse(query, 0, "192.0.2.7")
	})

	res := probe.NewDNS(nil).Do(context.Background(), server, "  Example.COM.  ")
	if res.Failed() {
		t.Fatalf("lookup failed: %s: %v", res.ErrorClass, res.Err)
	}

	select {
	case got := <-seen:
		if got != "example.com." {
			t.Fatalf("question = %q, want a lower-case fully-qualified name", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the responder never saw a question")
	}
}

func TestDNSCollectsAAAAAnswers(t *testing.T) {
	t.Parallel()

	server := startDNSResponder(t, func(query []byte) []byte {
		msg, ok := decodeQuery(query)
		if !ok {
			return nil
		}
		msg.Response = true
		msg.RecursionAvailable = true
		msg.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{
				Name: msg.Questions[0].Name, Type: dnsmessage.TypeAAAA,
				Class: dnsmessage.ClassINET, TTL: 60,
			},
			Body: &dnsmessage.AAAAResource{AAAA: [16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}},
		}}
		return packMessage(msg)
	})

	res := probe.NewDNS(nil).Do(context.Background(), server, "v6.example")
	if res.Failed() {
		t.Fatalf("lookup failed: %s: %v", res.ErrorClass, res.Err)
	}
	if len(res.Addresses) != 1 || res.Addresses[0] != netip.MustParseAddr("2001:db8::1") {
		t.Fatalf("addresses = %v", res.Addresses)
	}
}

func TestDNSRejectsBadResponses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		reply func(dnsmessage.Message) []byte
		want  string
	}{
		{
			name: "mismatched id",
			reply: func(msg dnsmessage.Message) []byte {
				msg.Response = true
				msg.ID++
				return packMessage(msg)
			},
			want: "does not match",
		},
		{
			name: "not a response",
			reply: func(msg dnsmessage.Message) []byte {
				msg.Response = false
				return packMessage(msg)
			},
			want: "not a response",
		},
		{
			name: "garbage",
			reply: func(dnsmessage.Message) []byte {
				return []byte{0x01, 0x02, 0x03}
			},
			want: "unpack",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := startDNSResponder(t, func(query []byte) []byte {
				msg, ok := decodeQuery(query)
				if !ok {
					return nil
				}
				return tc.reply(msg)
			})

			res := probe.NewDNS(nil).Do(context.Background(), server, "example.com")
			if res.ErrorClass != "dns_error" {
				t.Fatalf("class = %q, want dns_error (%v)", res.ErrorClass, res.Err)
			}
			if !strings.Contains(res.Err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", res.Err, tc.want)
			}
		})
	}
}

func TestDNSReportsAnUnknownRCODE(t *testing.T) {
	t.Parallel()

	server := startDNSResponder(t, func(query []byte) []byte {
		msg, ok := decodeQuery(query)
		if !ok {
			return nil
		}
		msg.Response = true
		msg.RCode = dnsmessage.RCodeNotImplemented
		return packMessage(msg)
	})

	res := probe.NewDNS(nil).Do(context.Background(), server, "example.com")
	if res.ErrorClass != "dns_error" {
		t.Fatalf("class = %q, want dns_error", res.ErrorClass)
	}
	if !strings.Contains(res.Err.Error(), "answered") {
		t.Fatalf("error = %q, want the rcode named", res.Err)
	}
}

func TestDNSTruncatedRetryFailureIsReported(t *testing.T) {
	t.Parallel()

	// The UDP answer says "truncated", and the TCP path closes without a full
	// frame: the retry failure must surface as a dns_ class, not as success.
	// The pair is bound through the shared helper because Windows refuses the
	// UDP/TCP co-bind for some ephemeral ports.
	udpPC, tcpLn := listenUDPAndTCP(t)
	t.Cleanup(func() {
		_ = udpPC.Close()
		_ = tcpLn.Close()
	})

	go func() {
		for {
			conn, err := tcpLn.Accept()
			if err != nil {
				return
			}
			// Claim a 40-byte frame and then close: readFull must report EOF.
			_, _ = conn.Write([]byte{0, 40})
			_ = conn.Close()
		}
	}()

	go func() {
		buf := make([]byte, 512)
		for {
			n, peer, err := udpPC.ReadFrom(buf)
			if err != nil {
				return
			}
			truncated := dnsResponse(buf[:n], 0, "")
			truncated[2] |= 0x02
			_, _ = udpPC.WriteTo(truncated, peer)
		}
	}()

	res := probe.NewDNS(nil).Do(context.Background(), tcpLn.Addr().String(), "example.com")
	if !res.Failed() {
		t.Fatal("a broken TCP retry reported success")
	}
	if !strings.HasPrefix(res.ErrorClass, "dns_") {
		t.Fatalf("class = %q, want a dns_ class", res.ErrorClass)
	}
}

func TestDNSZeroValueUsesDefaults(t *testing.T) {
	t.Parallel()

	// A zero-value instrument must still work: NewDNS is the normal path, but a
	// struct literal with no Dial or Timeout must not silently do nothing.
	server := startDNS(t, 0, "192.0.2.55")
	p := &probe.DNS{}

	res := p.Do(context.Background(), server, "example.com")
	if res.Failed() {
		t.Fatalf("zero-value Do failed: %s: %v", res.ErrorClass, res.Err)
	}
	if len(res.Addresses) != 1 || res.Addresses[0] != netip.MustParseAddr("192.0.2.55") {
		t.Fatalf("addresses = %v", res.Addresses)
	}
}

func TestDNSRejectsAnUnusableQName(t *testing.T) {
	t.Parallel()

	// A label longer than 63 bytes cannot be packed; it is a config error, not
	// a silent empty lookup.
	res := probe.NewDNS(nil).Do(context.Background(), "127.0.0.1:53", strings.Repeat("a", 300)+".example")
	if res.ErrorClass != "dns_config" {
		t.Fatalf("class = %q, want dns_config", res.ErrorClass)
	}
}

func TestDNSTruncatedRetryRejectsABadAnswer(t *testing.T) {
	t.Parallel()

	// The shared helper retries the UDP/TCP co-bind and skips on Windows when
	// the platform refuses it entirely.
	udpPC, tcpLn := listenUDPAndTCP(t)
	t.Cleanup(func() {
		_ = udpPC.Close()
		_ = tcpLn.Close()
	})

	go func() {
		for {
			conn, err := tcpLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				var size [2]byte
				if _, err := readFully(c, size[:]); err != nil {
					return
				}
				query := make([]byte, int(size[0])<<8|int(size[1]))
				if _, err := readFully(c, query); err != nil {
					return
				}
				// A well-framed answer whose id does not match the query.
				answer := dnsResponse(query, 0, "192.0.2.44")
				answer[0], answer[1] = answer[0]^0xff, answer[1]^0xff
				framed := append([]byte{byte(len(answer) >> 8), byte(len(answer))}, answer...)
				_, _ = c.Write(framed)
			}(conn)
		}
	}()

	go func() {
		buf := make([]byte, 512)
		for {
			n, peer, err := udpPC.ReadFrom(buf)
			if err != nil {
				return
			}
			truncated := dnsResponse(buf[:n], 0, "")
			truncated[2] |= 0x02
			_, _ = udpPC.WriteTo(truncated, peer)
		}
	}()

	res := probe.NewDNS(nil).Do(context.Background(), tcpLn.Addr().String(), "example.com")
	if res.ErrorClass != "dns_error" {
		t.Fatalf("class = %q, want dns_error (%v)", res.ErrorClass, res.Err)
	}
	if !strings.Contains(res.Err.Error(), "does not match") {
		t.Fatalf("error = %q, want the id mismatch named", res.Err)
	}
}

func TestDNSContextDeadlineIsATimeout(t *testing.T) {
	t.Parallel()

	p := probe.NewDNS(func(context.Context, string, string) (net.Conn, error) {
		return nil, context.DeadlineExceeded
	})
	if res := p.Do(context.Background(), "127.0.0.1:53", "x"); res.ErrorClass != "dns_timeout" {
		t.Fatalf("class = %q, want dns_timeout", res.ErrorClass)
	}
}

func TestDNSDialErrorsAreClassified(t *testing.T) {
	t.Parallel()

	timeout := probe.NewDNS(func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded}
	})
	timeout.Timeout = 50 * time.Millisecond
	if res := timeout.Do(context.Background(), "127.0.0.1:53", "x"); res.ErrorClass != "dns_timeout" {
		t.Fatalf("timeout class = %q", res.ErrorClass)
	}

	refused := probe.NewDNS(func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("boom")
	})
	if res := refused.Do(context.Background(), "127.0.0.1:53", "x"); res.ErrorClass != "dns_error" {
		t.Fatalf("plain error class = %q", res.ErrorClass)
	}
}

// errConn is a net.Conn whose Write or Read fails, for the exchange error paths
// that a real socket makes awkward to reach deterministically.
type errConn struct {
	writeErr error
	readErr  error
}

func (c errConn) Read([]byte) (int, error)         { return 0, c.readErr }
func (c errConn) Write([]byte) (int, error)        { return 0, c.writeErr }
func (c errConn) Close() error                     { return nil }
func (c errConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c errConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c errConn) SetDeadline(time.Time) error      { return nil }
func (c errConn) SetReadDeadline(time.Time) error  { return nil }
func (c errConn) SetWriteDeadline(time.Time) error { return nil }

func TestDNSExchangeFailuresAreClassified(t *testing.T) {
	t.Parallel()

	writeFail := probe.NewDNS(func(context.Context, string, string) (net.Conn, error) {
		return errConn{writeErr: errors.New("write failed")}, nil
	})
	if res := writeFail.Do(context.Background(), "127.0.0.1:53", "x"); res.ErrorClass != "dns_error" {
		t.Fatalf("write failure class = %q", res.ErrorClass)
	}

	readFail := probe.NewDNS(func(context.Context, string, string) (net.Conn, error) {
		return errConn{readErr: errors.New("read failed")}, nil
	})
	if res := readFail.Do(context.Background(), "127.0.0.1:53", "x"); res.ErrorClass != "dns_error" {
		t.Fatalf("read failure class = %q", res.ErrorClass)
	}

	readTimeout := probe.NewDNS(func(context.Context, string, string) (net.Conn, error) {
		return errConn{readErr: os.ErrDeadlineExceeded}, nil
	})
	if res := readTimeout.Do(context.Background(), "127.0.0.1:53", "x"); res.ErrorClass != "dns_timeout" {
		t.Fatalf("read timeout class = %q", res.ErrorClass)
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

func TestDNSDoNSReadsTheDelegation(t *testing.T) {
	t.Parallel()

	server := startDNSResponder(t, func(query []byte) []byte {
		msg, ok := decodeQuery(query)
		if !ok || len(msg.Questions) != 1 {
			return nil
		}
		if msg.Questions[0].Type != dnsmessage.TypeNS {
			t.Errorf("question type = %v, want NS", msg.Questions[0].Type)
		}
		msg.Response = true
		one, err := dnsmessage.NewName("ns2.example.net.")
		if err != nil {
			return nil
		}
		two, err := dnsmessage.NewName("ns1.example.net.")
		if err != nil {
			return nil
		}
		msg.Answers = []dnsmessage.Resource{
			{
				Header: dnsmessage.ResourceHeader{
					Name: msg.Questions[0].Name, Type: dnsmessage.TypeNS,
					Class: dnsmessage.ClassINET, TTL: 60,
				},
				Body: &dnsmessage.NSResource{NS: one},
			},
			{
				Header: dnsmessage.ResourceHeader{
					Name: msg.Questions[0].Name, Type: dnsmessage.TypeNS,
					Class: dnsmessage.ClassINET, TTL: 60,
				},
				Body: &dnsmessage.NSResource{NS: two},
			},
		}
		return packMessage(msg)
	})

	res := probe.NewDNS(nil).DoNS(context.Background(), server, "example.com")
	if res.ErrorClass != "" || res.Err != nil {
		t.Fatalf("DoNS = %s: %v", res.ErrorClass, res.Err)
	}
	want := []string{"ns1.example.net", "ns2.example.net"}
	if len(res.Names) != len(want) || res.Names[0] != want[0] || res.Names[1] != want[1] {
		t.Fatalf("names = %v, want %v sorted and de-duplicated", res.Names, want)
	}

	// A NOERROR answer with no NS records is NODATA, not an empty success.
	empty := startDNSResponder(t, func(query []byte) []byte {
		msg, ok := decodeQuery(query)
		if !ok {
			return nil
		}
		msg.Response = true
		return packMessage(msg)
	})
	res = probe.NewDNS(nil).DoNS(context.Background(), empty, "example.com")
	if res.ErrorClass != "dns_nodata" {
		t.Fatalf("class = %q, want dns_nodata", res.ErrorClass)
	}
}

func TestDNSDoNSDeduplicatesAndIgnoresEmptyTargets(t *testing.T) {
	t.Parallel()

	server := startDNSResponder(t, func(query []byte) []byte {
		msg, ok := decodeQuery(query)
		if !ok || len(msg.Questions) != 1 {
			return nil
		}
		ns := dnsmessage.MustNewName("ns.example.")
		authority := dnsmessage.MustNewName("auth.example.")
		root := dnsmessage.MustNewName(".")
		header := dnsmessage.ResourceHeader{
			Name: msg.Questions[0].Name, Type: dnsmessage.TypeNS,
			Class: dnsmessage.ClassINET, TTL: 60,
		}
		msg.Response = true
		msg.Answers = []dnsmessage.Resource{
			{Header: header, Body: &dnsmessage.NSResource{NS: ns}},
			{Header: header, Body: &dnsmessage.NSResource{NS: ns}},
			{Header: header, Body: &dnsmessage.NSResource{NS: root}},
		}
		msg.Authorities = []dnsmessage.Resource{
			{Header: header, Body: &dnsmessage.NSResource{NS: authority}},
		}
		return packMessage(msg)
	})

	res := probe.NewDNS(nil).DoNS(context.Background(), server, "example.com")
	if res.Failed() {
		t.Fatalf("DoNS failed: %s: %v", res.ErrorClass, res.Err)
	}
	want := []string{"auth.example", "ns.example"}
	if len(res.Names) != len(want) || res.Names[0] != want[0] || res.Names[1] != want[1] {
		t.Fatalf("names = %v, want %v from answers and authority", res.Names, want)
	}
}

// nsResponse answers an NS query with one NS record, so the TCP retry path and
// the delegation parser share one shape.
func nsResponse(query []byte, ns string) []byte {
	msg, ok := decodeQuery(query)
	if !ok || len(msg.Questions) != 1 {
		return nil
	}
	name, err := dnsmessage.NewName(ns)
	if err != nil {
		return nil
	}
	msg.Response = true
	msg.Answers = []dnsmessage.Resource{{
		Header: dnsmessage.ResourceHeader{
			Name: msg.Questions[0].Name, Type: dnsmessage.TypeNS,
			Class: dnsmessage.ClassINET, TTL: 60,
		},
		Body: &dnsmessage.NSResource{NS: name},
	}}
	return packMessage(msg)
}

func TestDNSDoNSReportsRCODEs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		rcode int
		want  string
	}{
		{"nxdomain", 3, "dns_nxdomain"},
		{"servfail", 2, "dns_servfail"},
		{"refused", 5, "dns_refused"},
		{"not implemented", 4, "dns_error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := startDNSResponder(t, func(query []byte) []byte {
				return dnsResponse(query, tc.rcode, "")
			})
			res := probe.NewDNS(nil).DoNS(context.Background(), server, "example.com")
			if res.ErrorClass != tc.want {
				t.Fatalf("class = %q, want %q (%v)", res.ErrorClass, tc.want, res.Err)
			}
		})
	}
}

func TestDNSDoNSErrors(t *testing.T) {
	t.Parallel()

	if res := probe.NewDNS(nil).DoNS(context.Background(), "", "example.com"); res.ErrorClass != "dns_config" {
		t.Fatalf("empty server class = %q", res.ErrorClass)
	}
	if res := probe.NewDNS(nil).DoNS(context.Background(), "127.0.0.1:53", ""); res.ErrorClass != "dns_config" {
		t.Fatalf("empty name class = %q", res.ErrorClass)
	}
	badName := probe.NewDNS(nil).DoNS(context.Background(), "127.0.0.1:53", strings.Repeat("a", 300)+".example")
	if badName.ErrorClass != "dns_config" {
		t.Fatalf("bad qname class = %q", badName.ErrorClass)
	}

	timeout := probe.NewDNS(func(context.Context, string, string) (net.Conn, error) {
		return nil, context.DeadlineExceeded
	})
	if res := timeout.DoNS(context.Background(), "127.0.0.1:53", "example.com"); res.ErrorClass != "dns_timeout" {
		t.Fatalf("timeout class = %q", res.ErrorClass)
	}

	refused := probe.NewDNS(func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("boom")
	})
	if res := refused.DoNS(context.Background(), "127.0.0.1:53", "example.com"); res.ErrorClass != "dns_error" {
		t.Fatalf("plain error class = %q", res.ErrorClass)
	}

	writeFail := probe.NewDNS(func(context.Context, string, string) (net.Conn, error) {
		return errConn{writeErr: errors.New("write failed")}, nil
	})
	if res := writeFail.DoNS(context.Background(), "127.0.0.1:53", "example.com"); res.ErrorClass != "dns_error" {
		t.Fatalf("write failure class = %q", res.ErrorClass)
	}

	// A responder that answers with a mismatched id is not a usable delegation.
	badAnswer := probe.NewDNS(func(context.Context, string, string) (net.Conn, error) {
		return errConn{readErr: errors.New("garbage")}, nil
	})
	if res := badAnswer.DoNS(context.Background(), "127.0.0.1:53", "example.com"); res.ErrorClass != "dns_error" {
		t.Fatalf("bad answer class = %q", res.ErrorClass)
	}
}

// listenUDPAndTCP binds a UDP socket and a TCP listener on the same loopback
// port, which is what a DNS server does. Windows will not reliably let one
// process co-bind a UDP socket to a TCP ephemeral port, so exhausting the
// retries there is a platform limit, not a failure of the code under test; the
// TCP retry path is covered on Linux CI.
func listenUDPAndTCP(t *testing.T) (net.PacketConn, net.Listener) {
	t.Helper()

	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			lastErr = err
			continue
		}
		pc, err := net.ListenPacket("udp", tcpLn.Addr().String())
		if err != nil {
			lastErr = err
			_ = tcpLn.Close()
			continue
		}
		return pc, tcpLn
	}
	if runtime.GOOS == "windows" {
		t.Skipf("Windows refuses the UDP/TCP co-bind this test needs: %v", lastErr)
	}
	t.Fatalf("bind a UDP/TCP pair: %v", lastErr)
	return nil, nil
}

func TestDNSDoNSRetriesATruncatedAnswerOverTCP(t *testing.T) {
	t.Parallel()

	udpPC, tcpLn := listenUDPAndTCP(t)
	t.Cleanup(func() {
		_ = udpPC.Close()
		_ = tcpLn.Close()
	})

	addr := tcpLn.Addr().(*net.TCPAddr)

	go func() {
		for {
			conn, err := tcpLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				var size [2]byte
				if _, err := readFully(c, size[:]); err != nil {
					return
				}
				query := make([]byte, int(size[0])<<8|int(size[1]))
				if _, err := readFully(c, query); err != nil {
					return
				}
				answer := nsResponse(query, "ns1.example.net.")
				if answer == nil {
					return
				}
				framed := append([]byte{byte(len(answer) >> 8), byte(len(answer))}, answer...)
				_, _ = c.Write(framed)
			}(conn)
		}
	}()

	go func() {
		buf := make([]byte, 512)
		for {
			n, peer, err := udpPC.ReadFrom(buf)
			if err != nil {
				return
			}
			truncated := dnsResponse(buf[:n], 0, "")
			truncated[2] |= 0x02 // TC: ask again over TCP
			_, _ = udpPC.WriteTo(truncated, peer)
		}
	}()

	res := probe.NewDNS(nil).DoNS(context.Background(), addr.String(), "example.com")
	if res.ErrorClass != "" || res.Err != nil {
		t.Fatalf("retry failed: %s: %v", res.ErrorClass, res.Err)
	}
	if len(res.Names) != 1 || res.Names[0] != "ns1.example.net" {
		t.Fatalf("names = %v", res.Names)
	}
}

func TestDNSDoNSTruncatedRetryFailureIsReported(t *testing.T) {
	t.Parallel()

	// UDP says truncated and there is no TCP listener on that port.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	go func() {
		buf := make([]byte, 512)
		for {
			n, peer, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			truncated := dnsResponse(buf[:n], 0, "")
			truncated[2] |= 0x02
			_, _ = pc.WriteTo(truncated, peer)
		}
	}()

	res := probe.NewDNS(nil).DoNS(context.Background(), pc.LocalAddr().String(), "example.com")
	if !res.Failed() {
		t.Fatal("a failed TCP retry reported success")
	}
	if !strings.HasPrefix(res.ErrorClass, "dns_") {
		t.Fatalf("class = %q, want a dns_ class", res.ErrorClass)
	}
}

func TestDNSDoNSTruncatedRetryRejectsABadAnswer(t *testing.T) {
	t.Parallel()

	udpPC, tcpLn := listenUDPAndTCP(t)
	t.Cleanup(func() {
		_ = udpPC.Close()
		_ = tcpLn.Close()
	})
	addr := tcpLn.Addr().(*net.TCPAddr)

	go func() {
		for {
			conn, err := tcpLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				var size [2]byte
				if _, err := readFully(c, size[:]); err != nil {
					return
				}
				query := make([]byte, int(size[0])<<8|int(size[1]))
				if _, err := readFully(c, query); err != nil {
					return
				}
				_, _ = c.Write([]byte{0, 3, 'a', 'b', 'c'})
			}(conn)
		}
	}()

	go func() {
		buf := make([]byte, 512)
		for {
			n, peer, err := udpPC.ReadFrom(buf)
			if err != nil {
				return
			}
			truncated := dnsResponse(buf[:n], 0, "")
			truncated[2] |= 0x02
			_, _ = udpPC.WriteTo(truncated, peer)
		}
	}()

	res := probe.NewDNS(nil).DoNS(context.Background(), addr.String(), "example.com")
	if res.ErrorClass != "dns_error" || res.Err == nil {
		t.Fatalf("class = %q, want dns_error (%v)", res.ErrorClass, res.Err)
	}
}

func TestDNSCollectsTheCNAMEChain(t *testing.T) {
	t.Parallel()

	server := startDNSResponder(t, func(query []byte) []byte {
		msg, ok := decodeQuery(query)
		if !ok || len(msg.Questions) != 1 {
			return nil
		}
		target, err := dnsmessage.NewName("edge.example.net.")
		if err != nil {
			return nil
		}
		msg.Response = true
		msg.Answers = []dnsmessage.Resource{
			{
				Header: dnsmessage.ResourceHeader{
					Name: msg.Questions[0].Name, Type: dnsmessage.TypeCNAME,
					Class: dnsmessage.ClassINET, TTL: 60,
				},
				Body: &dnsmessage.CNAMEResource{CNAME: target},
			},
			{
				Header: dnsmessage.ResourceHeader{
					Name: target, Type: dnsmessage.TypeA,
					Class: dnsmessage.ClassINET, TTL: 60,
				},
				Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 10}},
			},
		}
		return packMessage(msg)
	})

	res := probe.NewDNS(nil).Do(context.Background(), server, "www.example.com")
	if res.Failed() {
		t.Fatalf("lookup failed: %s: %v", res.ErrorClass, res.Err)
	}
	if len(res.CNAMEs) != 1 || res.CNAMEs[0] != "edge.example.net." {
		t.Fatalf("CNAMEs = %v", res.CNAMEs)
	}
	if len(res.Addresses) != 1 || res.Addresses[0].String() != "192.0.2.10" {
		t.Fatalf("addresses = %v", res.Addresses)
	}
}

// A configured resolver that cannot be reached must not make the instrument
// fall back to the host resolver: a fallback would silently answer from a
// different server and hide the very failure the probe exists to report.
func TestDNSNeverFallsBackToTheHostResolver(t *testing.T) {
	t.Parallel()

	var (
		mu      sync.Mutex
		dialled []string
	)
	dial := func(_ context.Context, _, address string) (net.Conn, error) {
		mu.Lock()
		dialled = append(dialled, address)
		mu.Unlock()
		return nil, errors.New("unreachable")
	}

	dns := probe.NewDNS(dial)
	res := dns.Do(context.Background(), "203.0.113.5:53", "example.com")
	if !res.Failed() {
		t.Fatal("an unreachable resolver reported success")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(dialled) == 0 {
		t.Fatal("the instrument never tried the configured resolver")
	}
	for _, address := range dialled {
		if address != "203.0.113.5:53" {
			t.Fatalf("instrument dialled %q, want only the configured resolver", address)
		}
	}
}
