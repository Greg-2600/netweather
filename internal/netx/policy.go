// Package netx owns every outbound connection netweather makes.
//
// Nothing else in the tree may call net.Dial, http.Get, or a raw resolver. That
// single choke point is what makes the SSRF rules in docs/THREAT_MODEL.md §5.2
// auditable: one policy table, one dial path, one test suite.
package netx

import (
	"errors"
	"fmt"
	"net/netip"
)

// Special-use ranges that must never be dialed. Order matters only for which
// reason is reported first, so more specific ranges come first.
var blockedPrefixes = []struct {
	prefix netip.Prefix
	reason string
}{
	{netip.MustParsePrefix("0.0.0.0/8"), "unspecified"},
	{netip.MustParsePrefix("10.0.0.0/8"), "private"},
	{netip.MustParsePrefix("100.64.0.0/10"), "cgnat"},
	{netip.MustParsePrefix("127.0.0.0/8"), "loopback"},
	{netip.MustParsePrefix("169.254.0.0/16"), "link-local"},
	{netip.MustParsePrefix("172.16.0.0/12"), "private"},
	{netip.MustParsePrefix("192.0.0.0/24"), "reserved"},
	{netip.MustParsePrefix("192.0.2.0/24"), "documentation"},
	{netip.MustParsePrefix("192.168.0.0/16"), "private"},
	{netip.MustParsePrefix("198.18.0.0/15"), "benchmark"},
	{netip.MustParsePrefix("198.51.100.0/24"), "documentation"},
	{netip.MustParsePrefix("203.0.113.0/24"), "documentation"},
	{netip.MustParsePrefix("224.0.0.0/4"), "multicast"},
	{netip.MustParsePrefix("255.255.255.255/32"), "broadcast"},
	{netip.MustParsePrefix("240.0.0.0/4"), "reserved"},

	{netip.MustParsePrefix("::/128"), "unspecified"},
	{netip.MustParsePrefix("::1/128"), "loopback"},
	{netip.MustParsePrefix("64:ff9b::/96"), "nat64"}, // embeds an IPv4 address
	{netip.MustParsePrefix("2001:db8::/32"), "documentation"},
	{netip.MustParsePrefix("2002::/16"), "6to4"}, // embeds an IPv4 address
	{netip.MustParsePrefix("fc00::/7"), "private"},
	{netip.MustParsePrefix("fe80::/10"), "link-local"},
	{netip.MustParsePrefix("ff00::/8"), "multicast"},
}

// DefaultPorts is the complete set of outbound ports netweather may use.
// 22 and everything else are excluded by design, not by configuration.
var DefaultPorts = map[int]struct{}{
	80:  {}, // HTTP
	443: {}, // HTTPS
	53:  {}, // DNS
	123: {}, // NTP
	853: {}, // DoT
}

// Policy is the address and port allowlist.
type Policy struct {
	// Ports defaults to DefaultPorts when nil. A policy with an empty map allows
	// nothing, which is a valid way to fail closed.
	Ports map[int]struct{}
}

// DefaultPolicy returns the production policy.
func DefaultPolicy() Policy { return Policy{Ports: DefaultPorts} }

// PortAllowed reports whether the port is in the allowlist.
func (p Policy) PortAllowed(port int) bool {
	ports := p.Ports
	if ports == nil {
		ports = DefaultPorts
	}
	_, ok := ports[port]
	return ok
}

// CheckAddr rejects special-use addresses. IPv4-mapped IPv6 is unmapped first,
// so ::ffff:127.0.0.1 is loopback here rather than a distinct address.
func (p Policy) CheckAddr(addr netip.Addr) error {
	a := addr.Unmap()
	if !a.IsValid() {
		return &BlockedError{Addr: addr, Reason: "invalid"}
	}
	for _, b := range blockedPrefixes {
		if b.prefix.Contains(a) {
			return &BlockedError{Addr: a, Reason: b.reason}
		}
	}
	return nil
}

// ErrBlocked is matched with errors.Is for every address refusal.
var ErrBlocked = errors.New("destination address blocked")

// BlockedError says why an address was refused. Reason is a stable token safe
// for logs and for the classifier.
type BlockedError struct {
	Addr   netip.Addr
	Reason string
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("blocked address %s (%s)", e.Addr, e.Reason)
}

// Unwrap makes errors.Is(err, ErrBlocked) work through wrapping.
func (e *BlockedError) Unwrap() error { return ErrBlocked }

// ErrBlockedPort is matched with errors.Is for every port refusal.
var ErrBlockedPort = errors.New("destination port blocked")

// BlockedPortError names the refused port.
type BlockedPortError struct {
	Port int
}

func (e *BlockedPortError) Error() string {
	return fmt.Sprintf("blocked port %d", e.Port)
}

// Unwrap makes errors.Is(err, ErrBlockedPort) work.
func (e *BlockedPortError) Unwrap() error { return ErrBlockedPort }
