package netx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"golang.org/x/net/proxy"
)

// Socks5 returns a dialer that connects through a SOCKS5 proxy and leaves name
// resolution to the proxy (socks5h).
//
// That distinction is the whole point: resolving locally would leak every
// queried hostname to the operator's resolver, which is exactly what a Tor
// vantage exists to avoid. x/net/proxy sends domain names with ATYP=3, so the
// proxy resolves them.
//
// The proxy itself is dialed directly, not through Policy: it is the operator's
// own local endpoint, and applying the target policy to it would reject the
// loopback sidecar.
func Socks5(proxyAddr string) (DialFunc, error) {
	host, portText, err := net.SplitHostPort(proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("netx: socks proxy %q: %w", proxyAddr, err)
	}
	if host == "" {
		return nil, fmt.Errorf("netx: socks proxy %q has no host", proxyAddr)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("netx: socks proxy %q has an invalid port", proxyAddr)
	}

	base := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	dialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, base)
	if err != nil {
		return nil, fmt.Errorf("netx: socks proxy: %w", err)
	}

	return func(ctx context.Context, network, address string) (net.Conn, error) {
		switch network {
		case "tcp", "tcp4", "tcp6":
		default:
			// SOCKS5 UDP associate is not used: the instruments that need UDP
			// (DNS) do not run on a Tor vantage.
			return nil, fmt.Errorf("netx: socks5 supports tcp only, not %q", network)
		}
		if d, ok := dialer.(proxy.ContextDialer); ok {
			return d.DialContext(ctx, network, address)
		}
		return nil, errors.New("netx: socks dialer has no context support")
	}, nil
}
