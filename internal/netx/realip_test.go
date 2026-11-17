package netx_test

import (
	"net/netip"
	"testing"

	"github.com/Greg-2600/netweather/internal/netx"
)

func prefixes(t *testing.T, raw ...string) []netip.Prefix {
	t.Helper()

	out := make([]netip.Prefix, 0, len(raw))
	for _, entry := range raw {
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			t.Fatalf("ParsePrefix(%q): %v", entry, err)
		}
		out = append(out, prefix.Masked())
	}
	return out
}

func TestClientIP(t *testing.T) {
	t.Parallel()

	// A proxy that appends the address it saw is 10.0.0.2; the client is
	// 198.51.100.7. A caller can prepend forged entries, so the walk starts at
	// the right.
	trusted := prefixes(t, "10.0.0.0/8", "2001:db8::/32")

	cases := []struct {
		name    string
		remote  string
		forward string
		trusted []netip.Prefix
		want    string
	}{
		{
			name:    "no trusted proxies ignores the header",
			remote:  "203.0.113.9:443",
			forward: "198.51.100.7",
			want:    "203.0.113.9",
		},
		{
			name:    "an untrusted peer ignores the header",
			remote:  "203.0.113.9:443",
			forward: "198.51.100.7",
			trusted: trusted,
			want:    "203.0.113.9",
		},
		{
			name:    "a trusted peer yields the client",
			remote:  "10.0.0.2:443",
			forward: "198.51.100.7",
			trusted: trusted,
			want:    "198.51.100.7",
		},
		{
			name:    "a spoofed prefix is ignored by the right-to-left walk",
			remote:  "10.0.0.2:443",
			forward: "1.2.3.4, 198.51.100.7",
			trusted: trusted,
			want:    "198.51.100.7",
		},
		{
			name:    "trusted hops are skipped until the first untrusted address",
			remote:  "10.0.0.2:443",
			forward: "198.51.100.7, 10.0.0.3",
			trusted: trusted,
			want:    "198.51.100.7",
		},
		{
			name:    "an IPv6 client is unparenthesized",
			remote:  "[10.0.0.2]:443",
			forward: "2001:db8::5, 2001:db9::9",
			trusted: trusted,
			want:    "2001:db9::9",
		},
		{
			name:    "a malformed entry the walk reaches falls back to the peer",
			remote:  "10.0.0.2:443",
			forward: "198.51.100.7, not-an-ip",
			trusted: trusted,
			want:    "10.0.0.2",
		},
		{
			name:    "a malformed forged entry left of the client is never read",
			remote:  "10.0.0.2:443",
			forward: "not-an-ip, 198.51.100.7",
			trusted: trusted,
			want:    "198.51.100.7",
		},
		{
			name:    "an empty header falls back to the peer",
			remote:  "10.0.0.2:443",
			forward: "",
			trusted: trusted,
			want:    "10.0.0.2",
		},
		{
			name:   "a remote address without a port is returned as-is",
			remote: "203.0.113.9",
			want:   "203.0.113.9",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := netx.ClientIP(tc.remote, tc.forward, tc.trusted); got != tc.want {
				t.Fatalf("ClientIP(%q, %q) = %q, want %q", tc.remote, tc.forward, got, tc.want)
			}
		})
	}
}

func TestTrustedPeer(t *testing.T) {
	t.Parallel()

	trusted := prefixes(t, "10.0.0.0/8")
	if !netx.TrustedPeer("10.0.0.2:443", trusted) {
		t.Error("a configured peer was not trusted")
	}
	if netx.TrustedPeer("203.0.113.9:443", trusted) {
		t.Error("an unconfigured peer was trusted")
	}
	if netx.TrustedPeer("10.0.0.2:443", nil) {
		t.Error("a peer was trusted with no configuration")
	}
	if netx.TrustedPeer("[2001:db8::1]:443", trusted) {
		t.Error("an IPv6 peer matched an IPv4 prefix")
	}
}
