//go:build e2e

// Real-endpoint E2E. It is build-tagged and additionally gated on
// NETWEATHER_E2E=1, so it never runs on a pull request: the internet is not
// deterministic and a third party being down must not block a merge.
//
// Six targets, loose assertions on behaviour rather than on those third parties
// being healthy (QUALITY.md §5.4).
package e2e_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/classify"
	"github.com/Greg-2600/netweather/internal/netx"
	"github.com/Greg-2600/netweather/internal/probe"
	"github.com/Greg-2600/netweather/internal/state"
)

func TestRealEndpoints(t *testing.T) {
	if os.Getenv("NETWEATHER_E2E") != "1" {
		t.Skip("set NETWEATHER_E2E=1 to run the real-endpoint suite")
	}

	dialer := netx.New()
	httpProbe := probe.NewHTTP(dialer)
	httpProbe.Client.Timeout = 15 * time.Second

	targets := []string{
		"https://www.cloudflare.com/",
		"https://www.wikipedia.org/",
		"https://github.com/",
		"https://www.chase.com/",
		"https://www.united.com/",
		"https://example.com/",
	}

	known := map[state.State]bool{
		state.Up: true, state.UpReachability: true, state.Degraded: true,
		state.Down: true, state.Blocked: true, state.Unknown: true,
		state.ConfigError: true,
	}

	measured := 0
	for _, url := range targets {
		t.Run(url, func(t *testing.T) {
			var (
				res    probe.Result
				result classify.Result
			)
			// One retry only: a second failure is worth a person, not a loop.
			for attempt := 0; attempt < 2; attempt++ {
				res = httpProbe.Do(context.Background(), url)
				result = classify.Decide(classify.Expected{}, classify.Response{
					Err: res.Err, ErrorClass: res.ErrorClass, Status: res.Status,
					Header: res.Header, Body: res.Body, FinalURL: res.FinalURL,
				})
				if result.State != state.Unknown {
					break
				}
			}

			if !known[result.State] {
				t.Fatalf("unexpected state %q for %s", result.State, url)
			}
			if res.ErrorClass != "" {
				prefixes := []string{"dns_", "tcp_", "tls_", "http_", "icmp_"}
				matched := false
				for _, prefix := range prefixes {
					if strings.HasPrefix(res.ErrorClass, prefix) {
						matched = true
						break
					}
				}
				if !matched {
					t.Fatalf("unknown transport class %q", res.ErrorClass)
				}
			}
			if result.State != state.Unknown {
				measured++
			}
		})
	}

	// The suite proves the internet is reachable from this host, not that any
	// particular target is up: a bot wall is a successful measurement.
	if measured < 4 {
		t.Fatalf("only %d of %d endpoints produced a judgeable state", measured, len(targets))
	}

	// The resolver path is part of the monitor; if it is broken, every hostname
	// target is blind and the board is meaningless.
	dns := probe.NewDNS(dialer.DialContext)
	if res := dns.Do(context.Background(), "1.1.1.1:53", "example.com"); res.Failed() {
		t.Fatalf("resolver check failed: %s: %v", res.ErrorClass, res.Err)
	}
}
