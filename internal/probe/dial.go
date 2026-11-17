package probe

import (
	"context"
	"errors"
	"net"

	"github.com/Greg-2600/netweather/internal/netx"
)

// DialFunc is the connect seam shared by the instruments. It matches
// net.Dialer.DialContext and netx.Dialer.DialContext.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// ClassifyDialError maps connect-phase failures. It differs from ClassifyError
// in one place that matters: a deadline during connect is tcp_timeout, not
// http_timeout.
func ClassifyDialError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "tcp_timeout"
	}

	// A destination this monitor's own policy refuses is not a target outage:
	// the probe never reached the network. Naming it lets the classifier say
	// "unmeasurable" rather than DOWN (THREAT_MODEL.md T16).
	switch {
	case errors.Is(err, netx.ErrBlockedPort):
		return "policy_blocked_port"
	case errors.Is(err, netx.ErrBlocked):
		return "policy_blocked"
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

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "tcp_timeout"
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) {
		switch opErr.Op {
		case "dial":
			return "tcp_refused"
		case "read", "write":
			return "tcp_reset"
		}
	}

	return "tcp_error"
}
