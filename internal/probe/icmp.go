package probe

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// EchoTransport is a raw ICMP echo socket. TraceTransport builds on it, and the
// ping instrument uses it directly.
type EchoTransport interface {
	SendEcho(dst netip.Addr, id, seq int) error
	Recv(deadline time.Time) (from netip.Addr, msg TraceMessage, err error)
	Close() error
}

// ICMP is the ping instrument. It is for anchors chosen because they answer
// ICMP; on a service target a timeout is meaningless (CLASSIFIER.md §3.6).
type ICMP struct {
	// Count is the number of echo requests. Five is the documented default.
	Count int
	// Timeout is per echo request.
	Timeout time.Duration
	// Open builds a transport. Nil uses the raw ICMP implementation.
	Open func() (EchoTransport, error)
	// Resolve turns a hostname into an address. Nil uses the system resolver;
	// production injects the guarded dialer's resolver.
	Resolve func(ctx context.Context, host string) (netip.Addr, error)
}

// ICMPResult is one ping run.
type ICMPResult struct {
	Replied int
	Lost    int
	// RTT is the mean round-trip time of the replies.
	RTT        time.Duration
	ErrorClass string
	Err        error
}

// Failed reports whether the run could not produce a single reply. Partial loss
// is a degraded signal, not a failure.
func (r ICMPResult) Failed() bool { return r.ErrorClass != "" }

const (
	// DefaultPingCount is the documented five echoes.
	DefaultPingCount = 5
	// DefaultPingTimeout is per request. Five seconds allows for a congested
	// path without making a five-echo run take a minute.
	DefaultPingTimeout = 2 * time.Second
)

// NewICMP returns a ping instrument with the production defaults. It prefers an
// unprivileged ping socket and falls back to a raw one, so a host with
// ping_group_range enabled needs no capability and a host without it still works
// when CAP_NET_RAW is granted (THREAT_MODEL.md T53).
func NewICMP() *ICMP {
	return &ICMP{
		Count:   DefaultPingCount,
		Timeout: DefaultPingTimeout,
		Open: func() (EchoTransport, error) {
			ping, pingErr := openICMPPing()
			if pingErr == nil {
				return ping, nil
			}
			raw, rawErr := openICMPTrace()
			if rawErr != nil {
				return nil, fmt.Errorf("probe: no ICMP socket: unprivileged: %w; raw: %w", pingErr, rawErr)
			}
			return raw, nil
		},
	}
}

// Do sends Count echo requests to host and reports replies and mean RTT.
//
// Loss is classified honestly: zero replies is icmp_timeout, some replies is
// icmp_loss_partial, and a missing capability is icmp_unavailable. None of
// those is allowed to move a service target; the classifier enforces that.
func (p *ICMP) Do(ctx context.Context, host string) ICMPResult {
	out := ICMPResult{}

	dst, err := p.resolve(ctx, host)
	if err != nil {
		out.ErrorClass = "icmp_config"
		out.Err = err
		return out
	}
	if dst.Is6() {
		out.ErrorClass = "icmp_unsupported"
		out.Err = fmt.Errorf("probe: ICMP is IPv4 only in this release")
		return out
	}

	open := p.Open
	if open == nil {
		open = func() (EchoTransport, error) { return openICMPTrace() }
	}
	transport, err := open()
	if err != nil {
		out.ErrorClass = "icmp_unavailable"
		out.Err = err
		return out
	}
	defer func() { _ = transport.Close() }()

	count := p.Count
	if count <= 0 {
		count = DefaultPingCount
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultPingTimeout
	}

	id := net.IPv4len*100 + count // arbitrary but stable for the run
	var total time.Duration

	for seq := 1; seq <= count; seq++ {
		if err := ctx.Err(); err != nil {
			out.ErrorClass = "icmp_cancelled"
			out.Err = err
			break
		}

		start := time.Now()
		if err := transport.SendEcho(dst, id, seq); err != nil {
			out.ErrorClass = "icmp_unreachable"
			out.Err = err
			break
		}

		deadline := start.Add(timeout)
		for time.Now().Before(deadline) {
			_, msg, err := transport.Recv(deadline)
			if err != nil {
				break
			}
			if msg.Kind != "echo_reply" || msg.Seq != seq {
				continue
			}
			total += time.Since(start)
			out.Replied++
			break
		}
	}

	out.Lost = count - out.Replied
	if out.Replied > 0 {
		out.RTT = total / time.Duration(out.Replied)
	}

	switch {
	case out.Replied == 0 && out.ErrorClass == "":
		out.ErrorClass = "icmp_timeout"
		out.Err = fmt.Errorf("probe: no reply from %s in %d echoes", dst, count)
	case out.Replied == 0:
		// A send or resolve failure already set a more precise class.
	case out.Lost > 0:
		out.ErrorClass = "icmp_loss_partial"
	}
	return out
}

func (p *ICMP) resolve(ctx context.Context, host string) (netip.Addr, error) {
	if host == "" {
		return netip.Addr{}, fmt.Errorf("probe: ICMP needs a host")
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.Unmap(), nil
	}

	resolve := p.Resolve
	if resolve == nil {
		resolve = func(ctx context.Context, host string) (netip.Addr, error) {
			addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
			if err != nil {
				return netip.Addr{}, err
			}
			if len(addrs) == 0 {
				return netip.Addr{}, fmt.Errorf("probe: %q resolved to no addresses", host)
			}
			return addrs[0].Unmap(), nil
		}
	}
	return resolve(ctx, host)
}
