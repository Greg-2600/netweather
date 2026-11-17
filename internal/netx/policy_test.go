package netx_test

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/Greg-2600/netweather/internal/netx"
)

func TestCheckAddrBlocksSpecialUseRanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		addr   string
		reason string
	}{
		{"0.0.0.0", "unspecified"},
		{"10.1.2.3", "private"},
		{"100.64.0.1", "cgnat"},
		{"127.0.0.1", "loopback"},
		{"169.254.169.254", "link-local"},
		{"172.16.0.1", "private"},
		{"192.0.0.1", "reserved"},
		{"192.0.2.1", "documentation"},
		{"192.168.1.1", "private"},
		{"198.18.0.1", "benchmark"},
		{"198.51.100.1", "documentation"},
		{"203.0.113.1", "documentation"},
		{"224.0.0.1", "multicast"},
		{"240.0.0.1", "reserved"},
		{"255.255.255.255", "broadcast"},
		{"::", "unspecified"},
		{"::1", "loopback"},
		{"64:ff9b::1", "nat64"},
		{"2001:db8::1", "documentation"},
		{"2002::1", "6to4"},
		{"fc00::1", "private"},
		{"fe80::1", "link-local"},
		{"ff02::1", "multicast"},
		{"::ffff:127.0.0.1", "loopback"}, // IPv4-mapped must not bypass the table
	}

	for _, tc := range tests {
		t.Run(tc.addr, func(t *testing.T) {
			t.Parallel()

			addr := netip.MustParseAddr(tc.addr)
			err := netx.DefaultPolicy().CheckAddr(addr)
			if err == nil {
				t.Fatalf("%s was allowed", tc.addr)
			}
			if !errors.Is(err, netx.ErrBlocked) {
				t.Fatalf("%s: error %v does not match ErrBlocked", tc.addr, err)
			}

			var blocked *netx.BlockedError
			if !errors.As(err, &blocked) {
				t.Fatalf("%s: error is not a BlockedError", tc.addr)
			}
			if blocked.Reason != tc.reason {
				t.Fatalf("%s: reason = %q, want %q", tc.addr, blocked.Reason, tc.reason)
			}
			if !strings.Contains(err.Error(), blocked.Reason) {
				t.Fatalf("%s: message %q omits the reason", tc.addr, err.Error())
			}
		})
	}
}

func TestCheckAddrAllowsPublicAddresses(t *testing.T) {
	t.Parallel()

	for _, s := range []string{
		"1.1.1.1",
		"8.8.8.8",
		"93.184.216.34",
		"2606:4700:4700::1111",
		"2001:4860:4860::8888",
	} {
		if err := netx.DefaultPolicy().CheckAddr(netip.MustParseAddr(s)); err != nil {
			t.Errorf("%s was blocked: %v", s, err)
		}
	}
}

func TestCheckAddrInvalid(t *testing.T) {
	t.Parallel()

	if err := netx.DefaultPolicy().CheckAddr(netip.Addr{}); err == nil {
		t.Fatal("zero address should be rejected")
	}
}

func TestPortAllowed(t *testing.T) {
	t.Parallel()

	policy := netx.DefaultPolicy()
	for _, port := range []int{80, 443, 53, 123, 853} {
		if !policy.PortAllowed(port) {
			t.Errorf("port %d should be allowed", port)
		}
	}
	for _, port := range []int{0, 21, 22, 25, 110, 3306, 8080, 8443} {
		if policy.PortAllowed(port) {
			t.Errorf("port %d should be blocked", port)
		}
	}
}

func TestPortAllowedFailClosed(t *testing.T) {
	t.Parallel()

	// A nil map means defaults; an explicitly empty map allows nothing.
	if !(netx.Policy{}).PortAllowed(443) {
		t.Fatal("nil port map should use the default allowlist")
	}
	if (netx.Policy{Ports: map[int]struct{}{}}).PortAllowed(443) {
		t.Fatal("empty port map must allow nothing")
	}
}
