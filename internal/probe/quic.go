package probe

import (
	"context"
	"net"
	"time"
)

// QUIC is a UDP/443 middlebox probe (ROADMAP "Later"). It sends a small
// datagram to port 443 and reports whether the path accepts UDP traffic at all.
// A full QUIC handshake needs a QUIC/TLS stack; this probe deliberately stays
// dependency-free and only tests whether UDP/443 is blocked by a middlebox, which
// is the signal the roadmap item asks for. A timeout is treated as "not
// blocked but no responder" (UNKNOWN), while an ICMP port-unreachable or
// permission error is "blocked" (a middlebox or firewall dropped it).
type QUIC struct {
	Dial    DialFunc
	Timeout time.Duration
}

// NewQUIC builds the production instrument through the guarded dialer.
func NewQUIC(d DialFunc) *QUIC {
	if d == nil {
		d = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	}
	return &QUIC{Dial: d, Timeout: 5 * time.Second}
}

// Do probes address ("host:443") over UDP.
func (q *QUIC) Do(ctx context.Context, address string) Result {
	start := time.Now()
	res := Result{}

	dial := q.Dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	}
	timeout := q.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := dial(lookupCtx, "udp", address)
	if err != nil {
		res.Latency = time.Since(start)
		res.ErrorClass = "quic_blocked"
		res.Err = err
		return res
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := lookupCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	// A minimal QUIC Initial-like probe: a few bytes are enough to elicit an
	// ICMP port-unreachable (blocked) or a response from a QUIC server.
	probe := []byte{0xc0, 0x00, 0x00, 0x00, 0x01}
	if _, err := conn.Write(probe); err != nil {
		res.Latency = time.Since(start)
		res.ErrorClass = "quic_blocked"
		res.Err = err
		return res
	}

	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	res.Latency = time.Since(start)
	if err != nil {
		// A timeout means the packet was not answered but not refused: the path
		// is open (no middlebox block), so report UNKNOWN rather than blocked.
		if isTimeout(err) {
			res.ErrorClass = "quic_no_response"
			res.Err = err
			return res
		}
		res.ErrorClass = "quic_blocked"
		res.Err = err
		return res
	}
	res.Latency = time.Since(start)
	if n > 0 {
		res.Status = 200
	}
	return res
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	// net.Error Timeout()
	type timeoutErr interface{ Timeout() bool }
	if te, ok := err.(timeoutErr); ok {
		return te.Timeout()
	}
	return false
}
