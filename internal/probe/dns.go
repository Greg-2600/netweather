package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
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

	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dial(ctx, "udp", server)
		},
	}

	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	addrs, err := resolver.LookupNetIP(lookupCtx, "ip4", name)
	out.Latency = time.Since(start)
	if err != nil {
		out.ErrorClass = ClassifyDNSError(err)
		out.Err = err
		return out
	}
	if len(addrs) == 0 {
		// NOERROR with no records is a real answer, but not a usable one.
		out.ErrorClass = "dns_nodata"
		out.Err = fmt.Errorf("probe: %q resolved to no A records", name)
		return out
	}

	out.Addresses = addrs
	return out
}

// ClassifyDNSError maps resolver failures to the stable class strings the
// classifier consumes.
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
