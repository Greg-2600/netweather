package netx

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"time"
)

// Resolver is the subset of *net.Resolver netx needs. It is an interface so a
// test can simulate rebinding, split answers, and resolver failures.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// DialFunc is the last hop: it receives an IP literal, never a hostname.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Dialer is the only outbound dial path in the application.
//
// The sequence is always: validate port -> resolve -> validate every resolved
// address -> dial the validated IP literal. The hostname is never passed to the
// network stack, so a resolver that changes its answer between check and connect
// (DNS rebinding) cannot redirect the connection.
type Dialer struct {
	Policy   Policy
	Resolver Resolver
	Dial     DialFunc
}

// New returns a Dialer with the production policy, the system resolver, and a
// bounded TCP dialer.
func New() *Dialer {
	return &Dialer{
		Policy:   DefaultPolicy(),
		Resolver: net.DefaultResolver,
		Dial: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
}

// DialContext resolves and validates address, then dials a permitted IP.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("netx: address %q: %w", address, err)
	}

	port, err := strconv.Atoi(portText)
	if err != nil {
		return nil, fmt.Errorf("netx: port %q: %w", portText, err)
	}
	if !d.Policy.PortAllowed(port) {
		return nil, &BlockedPortError{Port: port}
	}

	addrs, err := d.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("netx: %q resolved to no addresses", host)
	}

	dial := d.Dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	}

	var (
		blocked error
		dialErr error
	)
	for _, addr := range addrs {
		if err := d.Policy.CheckAddr(addr); err != nil {
			if blocked == nil {
				blocked = err
			}
			continue
		}

		conn, err := dial(ctx, network, net.JoinHostPort(addr.Unmap().String(), portText))
		if err == nil {
			return conn, nil
		}
		dialErr = err
	}

	// A mixed answer where the permitted addresses failed is a real network
	// error, not a policy refusal; reporting it as blocked would hide outages.
	if dialErr != nil {
		return nil, dialErr
	}
	return nil, blocked
}

// CheckHost resolves host and validates every answer without dialing. Probes use
// it when they need the address for a DNS observation, and the dialer uses it
// internally.
func (d *Dialer) CheckHost(ctx context.Context, host string) ([]netip.Addr, error) {
	addrs, err := d.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("netx: %q resolved to no addresses", host)
	}

	allowed := make([]netip.Addr, 0, len(addrs))
	for _, addr := range addrs {
		if err := d.Policy.CheckAddr(addr); err != nil {
			continue
		}
		allowed = append(allowed, addr.Unmap())
	}
	if len(allowed) == 0 {
		return nil, &BlockedError{Addr: addrs[0], Reason: "all answers blocked"}
	}
	return allowed, nil
}

// resolve is deliberately uncached: every dial re-resolves, because a cache is
// the other half of the rebinding attack.
func (d *Dialer) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr}, nil
	}

	resolver := d.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	addrs, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("netx: resolve %q: %w", host, err)
	}
	return addrs, nil
}
