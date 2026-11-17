package probe

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// DNSResult is one resolver observation.
type DNSResult struct {
	Latency    time.Duration
	Addresses  []netip.Addr
	ErrorClass string
	Err        error
}

// Failed reports whether the query produced a transport-class failure.
func (r DNSResult) Failed() bool { return r.Err != nil || r.ErrorClass != "" }

// DNS is the resolver instrument. It queries a specific server rather than the
// host's configured resolver, so a country or ASN anchor measures that vantage
// path and not whatever /etc/resolv.conf happens to say.
//
// It speaks the wire protocol itself instead of using net.Resolver. That is a
// deliberate exception to "use the standard library": net.Resolver collapses
// SERVFAIL, REFUSED, and NOERROR-with-no-answers into one or two opaque errors,
// and those RCODEs are exactly the difference between "the name is gone" and
// "the resolver refused to answer".
type DNS struct {
	Dial    DialFunc
	Timeout time.Duration
}

// NewDNS returns a DNS instrument over the guarded dialer. UDP is used because
// that is what ordinary resolvers answer on; the dialer still applies the same
// address and port policy.
func NewDNS(d DialFunc) *DNS {
	if d == nil {
		d = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	}
	return &DNS{Dial: d, Timeout: 5 * time.Second}
}

// Do queries name's A records against server ("host:53"). It never falls back
// to the system resolver: a fallback would make an unreachable anchor look
// healthy.
func (p *DNS) Do(ctx context.Context, server, name string) DNSResult {
	start := time.Now()
	out := DNSResult{}

	if server == "" || name == "" {
		out.Latency = time.Since(start)
		out.ErrorClass = "dns_config"
		out.Err = errors.New("probe: dns needs a server and a qname")
		return out
	}

	dial := p.Dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	}

	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	qname, err := dnsmessage.NewName(dnsName(name))
	if err != nil {
		out.Latency = time.Since(start)
		out.ErrorClass = "dns_config"
		out.Err = fmt.Errorf("probe: qname %q: %w", name, err)
		return out
	}

	query, err := packQuery(qname)
	if err != nil {
		out.Latency = time.Since(start)
		out.ErrorClass = "dns_config"
		out.Err = err
		return out
	}

	raw, err := exchange(lookupCtx, dial, "udp", server, query)
	out.Latency = time.Since(start)
	if err != nil {
		out.ErrorClass = classifyExchangeError(err)
		out.Err = err
		return out
	}

	msg, err := unpackResponse(raw, queryID(query))
	if err != nil {
		out.ErrorClass = "dns_error"
		out.Err = err
		return out
	}

	// A truncated UDP answer is the resolver saying "ask again over TCP". Not
	// retrying would turn a large answer into a false NODATA.
	if msg.Truncated {
		raw, err = exchange(lookupCtx, dial, "tcp", server, query)
		out.Latency = time.Since(start)
		if err != nil {
			out.ErrorClass = classifyExchangeError(err)
			out.Err = err
			return out
		}
		msg, err = unpackResponse(raw, queryID(query))
		if err != nil {
			out.ErrorClass = "dns_error"
			out.Err = err
			return out
		}
	}

	switch msg.RCode {
	case dnsmessage.RCodeSuccess:
	case dnsmessage.RCodeNameError:
		out.ErrorClass = "dns_nxdomain"
		out.Err = fmt.Errorf("probe: %q does not exist", name)
		return out
	case dnsmessage.RCodeServerFailure:
		out.ErrorClass = "dns_servfail"
		out.Err = fmt.Errorf("probe: %s refused to answer for %q", server, name)
		return out
	case dnsmessage.RCodeRefused:
		out.ErrorClass = "dns_refused"
		out.Err = fmt.Errorf("probe: %s refused the query for %q", server, name)
		return out
	default:
		out.ErrorClass = "dns_error"
		out.Err = fmt.Errorf("probe: %s answered %s for %q", server, msg.RCode, name)
		return out
	}

	addrs := answerAddresses(msg)
	if len(addrs) == 0 {
		// NOERROR with no records is a real answer, but not a usable one.
		out.ErrorClass = "dns_nodata"
		out.Err = fmt.Errorf("probe: %q resolved to no A records", name)
		return out
	}

	out.Addresses = addrs
	return out
}

// dnsName qualifies a name for the wire: lower-case, one trailing dot.
func dnsName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	return strings.TrimSuffix(name, ".") + "."
}

func packQuery(qname dnsmessage.Name) ([]byte, error) {
	id := dnsID()
	msg := dnsmessage.Message{
		Header: dnsmessage.Header{ID: id, RecursionDesired: true},
		Questions: []dnsmessage.Question{{
			Name: qname, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET,
		}},
	}
	packed, err := msg.Pack()
	if err != nil {
		return nil, fmt.Errorf("probe: pack dns query: %w", err)
	}
	return packed, nil
}

var queryID = func(packed []byte) uint16 { return binary.BigEndian.Uint16(packed[:2]) }

// dnsID is random rather than sequential so an off-path attacker cannot guess
// which response the resolver will accept.
func dnsID() uint16 {
	var buf [2]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return uint16(time.Now().UnixNano()) // #nosec G115 -- a truncated clock value is a fine fallback id.
	}
	return binary.BigEndian.Uint16(buf[:])
}

// exchange sends one query and reads one answer. The connection deadline is the
// context deadline, so a silent server obeys the instrument timeout.
//
// DNS over TCP frames every message with a two-byte length prefix; UDP does not.
// Getting that wrong yields an answer that looks like garbage rather than a
// network error, so the framing lives here where both paths can be tested.
func exchange(ctx context.Context, dial DialFunc, network, server string, query []byte) ([]byte, error) {
	conn, err := dial(ctx, network, server)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if network == "tcp" {
		if len(query) > 0xffff {
			return nil, fmt.Errorf("probe: dns query is %d bytes, over the TCP frame limit", len(query))
		}
		framed := make([]byte, 2+len(query))
		binary.BigEndian.PutUint16(framed[:2], uint16(len(query))) // #nosec G115 -- the length check above bounds it.
		copy(framed[2:], query)
		if _, err := conn.Write(framed); err != nil {
			return nil, err
		}

		var size [2]byte
		if _, err := readFull(conn, size[:]); err != nil {
			return nil, err
		}
		length := int(binary.BigEndian.Uint16(size[:]))
		out := make([]byte, length)
		if _, err := readFull(conn, out); err != nil {
			return nil, err
		}
		return out, nil
	}

	if _, err := conn.Write(query); err != nil {
		return nil, err
	}

	// 512 is the classic UDP ceiling; a larger buffer costs little and covers
	// an answer that ignores our lack of EDNS.
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func readFull(conn net.Conn, buf []byte) (int, error) {
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

// unpackResponse parses one response and rejects an answer to someone else's
// question.
func unpackResponse(raw []byte, wantID uint16) (dnsmessage.Message, error) {
	var msg dnsmessage.Message
	if err := msg.Unpack(raw); err != nil {
		return dnsmessage.Message{}, fmt.Errorf("probe: unpack dns response: %w", err)
	}
	if msg.ID != wantID {
		return dnsmessage.Message{}, fmt.Errorf("probe: dns response id %d does not match query %d", msg.ID, wantID)
	}
	if !msg.Response {
		return dnsmessage.Message{}, errors.New("probe: dns message is not a response")
	}
	return msg, nil
}

// answerAddresses collects the A and AAAA records from the answer section. A
// CNAME chain appears in the same section, so the final addresses are found
// without following anything separately.
func answerAddresses(msg dnsmessage.Message) []netip.Addr {
	var out []netip.Addr
	for _, resource := range msg.Answers {
		switch body := resource.Body.(type) {
		case *dnsmessage.AResource:
			addr, ok := netip.AddrFromSlice(body.A[:])
			if ok {
				out = append(out, addr)
			}
		case *dnsmessage.AAAAResource:
			addr, ok := netip.AddrFromSlice(body.AAAA[:])
			if ok {
				out = append(out, addr)
			}
		}
	}
	return out
}

func classifyExchangeError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "dns_timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "dns_timeout"
	}
	return "dns_error"
}

// ClassifyDNSError maps resolver failures to the stable class strings the
// classifier consumes. The instrument no longer returns *net.DNSError, but the
// helper is still used by callers that pass a resolver error (and by the tests
// that pin the mapping).
func ClassifyDNSError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "dns_timeout"
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		switch {
		case dnsErr.IsNotFound:
			return "dns_nxdomain"
		case dnsErr.IsTimeout:
			return "dns_timeout"
		default:
			return "dns_error"
		}
	}
	return "dns_error"
}
