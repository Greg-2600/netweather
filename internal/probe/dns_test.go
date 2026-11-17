package probe_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"strings"
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
	// the UDP socket is bound to its port.
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tcpLn.Close() })

	addr := tcpLn.Addr().(*net.TCPAddr)
	udpPC, err := net.ListenPacket("udp", addr.String())
	if err != nil {
		t.Fatalf("bind udp to the tcp port: %v", err)
	}
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

	res := probe.NewDNS(nil).Do(context.Background(), addr.String(), "example.com")
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
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tcpLn.Close() })

	addr := tcpLn.Addr().(*net.TCPAddr)
	udpPC, err := net.ListenPacket("udp", addr.String())
	if err != nil {
		t.Fatalf("bind udp: %v", err)
	}
	t.Cleanup(func() { _ = udpPC.Close() })

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

	res := probe.NewDNS(nil).Do(context.Background(), addr.String(), "example.com")
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

	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tcpLn.Close() })

	addr := tcpLn.Addr().(*net.TCPAddr)
	udpPC, err := net.ListenPacket("udp", addr.String())
	if err != nil {
		t.Fatalf("bind udp: %v", err)
	}
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

	res := probe.NewDNS(nil).Do(context.Background(), addr.String(), "example.com")
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
