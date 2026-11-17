//go:build e2e

// The Tor-vantage real test. It is separate from the general real-endpoint
// suite because it needs a running SOCKS5 proxy, which a CI runner does not
// have by default (QUALITY.md §5.4).
//
// It is double-gated: the e2e build tag and NETWEATHER_TOR_E2E=1. A missing
// proxy skips with a message rather than failing, because "Tor is not running
// here" is not a product defect.
package e2e_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/netx"
	"github.com/Greg-2600/netweather/internal/probe"
)

func TestTorVantageReachesTheInternet(t *testing.T) {
	if os.Getenv("NETWEATHER_TOR_E2E") != "1" {
		t.Skip("set NETWEATHER_TOR_E2E=1 (and run a SOCKS5 proxy) to test the Tor vantage")
	}

	proxy := strings.TrimSpace(os.Getenv("NETWEATHER_TOR_SOCKS"))
	if proxy == "" {
		proxy = "127.0.0.1:9050"
	}

	dial, err := netx.Socks5(proxy)
	if err != nil {
		t.Fatalf("netx.Socks5(%q): %v", proxy, err)
	}

	// Prove the proxy answers at all before spending a probe through it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dial(ctx, "tcp", "example.com:443")
	if err != nil {
		t.Skipf("SOCKS5 proxy %s is not reachable: %v", proxy, err)
	}
	_ = conn.Close()

	tcp := probe.NewTCPWithDial(probe.DialFunc(dial))

	// The dial context bounds the attempt; the TCP instrument has no timeout of
	// its own because the scheduler already owns the target's budget.
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer probeCancel()
	res := tcp.Do(probeCtx, "example.com:443")
	if res.Failed() {
		// A Tor exit may legitimately be blocked or slow; the assertion is that
		// the failure is classified, not that a third party is healthy.
		if !strings.HasPrefix(res.ErrorClass, "tcp_") {
			t.Fatalf("unclassified Tor transport failure: %q (%v)", res.ErrorClass, res.Err)
		}
		t.Logf("Tor exit refused or timed out, which is a normal Tor result: %s", res.ErrorClass)
		return
	}
	if res.Latency <= 0 {
		t.Fatalf("latency = %v", res.Latency)
	}
}
