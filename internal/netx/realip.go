package netx

import (
	"net"
	"net/netip"
	"strings"
)

// ClientIP returns the effective client address for per-client limits and log
// lines.
//
// X-Forwarded-For is honored only when the immediate peer is inside one of the
// configured trusted prefixes. The chain is walked from right to left and the
// first address that is not itself a trusted proxy wins: a trusted proxy
// appends the address it saw, so a caller can only prepend forged entries, and
// walking from the right ignores them. With no trusted proxies, or an
// untrusted peer, the peer address is returned unchanged.
func ClientIP(remoteAddr, forwardedFor string, trusted []netip.Prefix) string {
	peer := hostFromRemote(remoteAddr)
	if len(trusted) == 0 || !isTrusted(trusted, peer) {
		return peer
	}

	entries := strings.Split(forwardedFor, ",")
	for i := len(entries) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(entries[i])
		if candidate == "" {
			continue
		}
		addr, err := netip.ParseAddr(candidate)
		if err != nil {
			// A malformed entry makes the rest of the chain untrustworthy;
			// fall back to the peer rather than choosing from it.
			return peer
		}
		host := addr.Unmap().String()
		if isTrusted(trusted, host) {
			continue
		}
		return host
	}
	return peer
}

func hostFromRemote(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

func isTrusted(prefixes []netip.Prefix, host string) bool {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// TrustedPeer reports whether the immediate peer is inside one of the trusted
// prefixes. It is for decisions that depend on a proxy-supplied header (the
// effective scheme) but not on the client address itself.
func TrustedPeer(remoteAddr string, trusted []netip.Prefix) bool {
	return len(trusted) > 0 && isTrusted(trusted, hostFromRemote(remoteAddr))
}
