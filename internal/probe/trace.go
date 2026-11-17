package probe

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// TraceHop is one TTL step. A hop with a zero Addr is a timeout: routers
// rate-limit TTL-exceeded, so this is a hint, never proof (DESIGN.md §5.4).
type TraceHop struct {
	TTL  int
	Addr netip.Addr
	RTT  time.Duration
}

// TraceResult is one TTL walk.
type TraceResult struct {
	Hops        []TraceHop
	Destination netip.Addr
	Reached     bool
	ErrorClass  string
	Err         error
}

// Failed reports whether the walk could not run at all. An unreachable
// destination is a result, not a failure.
func (r TraceResult) Failed() bool { return r.Err != nil || r.ErrorClass != "" }

// TraceMessage is the protocol-neutral subset of an ICMP reply.
type TraceMessage struct {
	Kind string // "time_exceeded", "echo_reply", "other"
	ID   int
	Seq  int
}

// TraceTransport is the socket seam. Production uses a raw ICMP socket; tests
// script replies without needing CAP_NET_RAW.
type TraceTransport interface {
	SetTTL(ttl int) error
	SendEcho(dst netip.Addr, id, seq int) error
	Recv(deadline time.Time) (from netip.Addr, msg TraceMessage, err error)
	Close() error
}

const (
	// DefaultTraceHops is the usual ceiling before a path is called dark.
	DefaultTraceHops = 30
	// DefaultTraceTimeout is per hop. Routers deprioritise TTL-exceeded, so a
	// short deadline would turn ordinary rate limiting into missing hops.
	DefaultTraceTimeout = 2 * time.Second
	// traceStopAfter is how many consecutive silent hops end a walk early.
	traceStopAfter = 3
)

// Trace performs an IPv4 ICMP TTL walk. It is on-demand only: the API caps it
// at ten runs per hour across the whole instance.
type Trace struct {
	MaxHops int
	Timeout time.Duration
	// Open builds a transport. Nil uses the raw ICMP implementation.
	Open func() (TraceTransport, error)
}

// NewTrace returns a trace with the production defaults.
func NewTrace() *Trace {
	return &Trace{
		MaxHops: DefaultTraceHops,
		Timeout: DefaultTraceTimeout,
		Open:    openICMPTrace,
	}
}

// Run walks the path to dst. A destination that never answers is a completed
// walk with Reached=false, not an error.
func (t *Trace) Run(ctx context.Context, dst netip.Addr) TraceResult {
	out := TraceResult{Destination: dst}

	dst = dst.Unmap()
	if !dst.IsValid() {
		out.ErrorClass = "trace_config"
		out.Err = fmt.Errorf("probe: trace needs a destination address")
		return out
	}
	if dst.Is6() {
		out.ErrorClass = "trace_unsupported"
		out.Err = fmt.Errorf("probe: traceroute is IPv4 only in this release")
		return out
	}

	open := t.Open
	if open == nil {
		open = openICMPTrace
	}
	transport, err := open()
	if err != nil {
		out.ErrorClass = "trace_unavailable"
		out.Err = err
		return out
	}
	defer func() { _ = transport.Close() }()

	maxHops := t.MaxHops
	if maxHops <= 0 {
		maxHops = DefaultTraceHops
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = DefaultTraceTimeout
	}

	id := os.Getpid() & 0xffff
	silent := 0

	for ttl := 1; ttl <= maxHops; ttl++ {
		if err := ctx.Err(); err != nil {
			out.ErrorClass = "trace_cancelled"
			out.Err = err
			return out
		}

		if err := transport.SetTTL(ttl); err != nil {
			out.ErrorClass = "trace_unavailable"
			out.Err = err
			return out
		}

		start := time.Now()
		if err := transport.SendEcho(dst, id, ttl); err != nil {
			out.ErrorClass = "trace_send"
			out.Err = err
			return out
		}

		deadline := start.Add(timeout)
		hop := TraceHop{TTL: ttl}
		answered := false

		for time.Now().Before(deadline) {
			from, msg, err := transport.Recv(deadline)
			if err != nil {
				break
			}

			switch msg.Kind {
			case "echo_reply":
				if msg.Seq != ttl {
					continue
				}
				hop.Addr = from
				hop.RTT = time.Since(start)
				out.Hops = append(out.Hops, hop)
				out.Reached = true
				return out
			case "time_exceeded":
				hop.Addr = from
				hop.RTT = time.Since(start)
				answered = true
			}
			if answered {
				break
			}
		}

		out.Hops = append(out.Hops, hop)
		if answered {
			silent = 0
			continue
		}
		silent++
		if silent >= traceStopAfter {
			// Three silent hops in a row: the rest of the walk would be noise.
			return out
		}
	}
	return out
}

// icmpTrace is the raw-socket transport. It needs CAP_NET_RAW, which is why
// traceroute ships as a separate compose profile instead of in the default one.
type icmpTrace struct {
	conn *icmp.PacketConn
}

func openICMPTrace() (TraceTransport, error) {
	conn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return nil, fmt.Errorf("probe: raw ICMP socket unavailable (CAP_NET_RAW is required): %w", err)
	}
	return &icmpTrace{conn: conn}, nil
}

func (t *icmpTrace) SetTTL(ttl int) error {
	if err := t.conn.IPv4PacketConn().SetTTL(ttl); err != nil {
		return fmt.Errorf("probe: trace set ttl %d: %w", ttl, err)
	}
	return nil
}

func (t *icmpTrace) SendEcho(dst netip.Addr, id, seq int) error {
	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{ID: id, Seq: seq, Data: []byte("netweather")},
	}
	payload, err := msg.Marshal(nil)
	if err != nil {
		return fmt.Errorf("probe: trace marshal echo: %w", err)
	}
	addr := &net.IPAddr{IP: net.IP(dst.AsSlice())}
	if _, err := t.conn.WriteTo(payload, addr); err != nil {
		return fmt.Errorf("probe: trace send echo: %w", err)
	}
	return nil
}

func (t *icmpTrace) Recv(deadline time.Time) (netip.Addr, TraceMessage, error) {
	if err := t.conn.SetReadDeadline(deadline); err != nil {
		return netip.Addr{}, TraceMessage{}, err
	}

	buf := make([]byte, 1500)
	n, peer, err := t.conn.ReadFrom(buf)
	if err != nil {
		return netip.Addr{}, TraceMessage{}, err
	}

	parsed, err := icmp.ParseMessage(1, buf[:n])
	if err != nil {
		return netip.Addr{}, TraceMessage{Kind: "other"}, nil //nolint:nilerr // ignore unparseable noise
	}

	var from netip.Addr
	if ipPeer, ok := peer.(*net.IPAddr); ok {
		if addr, ok := netip.AddrFromSlice(ipPeer.IP); ok {
			from = addr.Unmap()
		}
	}

	switch parsed.Type {
	case ipv4.ICMPTypeTimeExceeded:
		return from, TraceMessage{Kind: "time_exceeded"}, nil
	case ipv4.ICMPTypeEchoReply:
		msg := TraceMessage{Kind: "echo_reply"}
		if echo, ok := parsed.Body.(*icmp.Echo); ok {
			msg.ID, msg.Seq = echo.ID, echo.Seq
		}
		return from, msg, nil
	default:
		return from, TraceMessage{Kind: "other"}, nil
	}
}

func (t *icmpTrace) Close() error { return t.conn.Close() }
