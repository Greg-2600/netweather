//go:build linux

package probe_test

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/probe"
)

// TestPingSocketReachesLoopback exercises the production ICMP path on Linux.
// NewICMP prefers the unprivileged ping socket, which is what the container
// uses with all capabilities dropped and net.ipv4.ping_group_range set. A host
// where that sysctl is disabled and no capability is available skips: the
// assertion is about the socket this binary will actually use, not about the
// test machine's privilege posture.
func TestPingSocketReachesLoopback(t *testing.T) {
	t.Parallel()

	ping := probe.NewICMP()
	ping.Count = 1
	ping.Timeout = 2 * time.Second
	ping.Resolve = func(context.Context, string) (netip.Addr, error) {
		return netip.MustParseAddr("127.0.0.1"), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got := ping.Do(ctx, "loopback")
	if got.ErrorClass == "icmp_unavailable" {
		t.Skipf("no ICMP socket on this host: %v", got.Err)
	}
	if got.Replied != 1 {
		t.Fatalf("loopback echo = %+v, want one reply", got)
	}
	if got.RTT <= 0 {
		t.Fatalf("loopback RTT = %v, want a positive duration", got.RTT)
	}
}
