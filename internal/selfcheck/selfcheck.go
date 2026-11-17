// Package selfcheck answers "is the monitor itself healthy?".
//
// A monitoring system that silently loses the ability to resolve, connect, or
// write must say so before its board is believed. Each check is a function so
// the runner is testable without a network, a clock, or a disk.
package selfcheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Greg-2600/netweather/internal/probe"
	"github.com/Greg-2600/netweather/internal/store"
)

// Gate holds the latest self-check outcome so a probe path can consult it.
//
// A failed check is a fact about the vantage, not about a target, so the agent
// asks the gate before it trusts a path-level failure. The freshness window
// matters: a gate that has not been updated for hours must not keep overriding
// the classifier, because that would hide a real outage behind a stale file.
type Gate struct {
	// Now and Fresh are injectable so the window is testable without sleeping.
	Now   func() time.Time
	Fresh time.Duration

	mu   sync.Mutex
	last map[string]Result
}

// DefaultGateFreshness is twice the maintenance interval (ten minutes), so a
// gate survives one missed cycle but not a stalled one.
const DefaultGateFreshness = 20 * time.Minute

// NewGate returns an empty gate with the default freshness window.
func NewGate() *Gate {
	return &Gate{
		last:  make(map[string]Result),
		Now:   time.Now,
		Fresh: DefaultGateFreshness,
	}
}

// Observe records one finished run. Later results replace earlier ones, which
// is what "latest outcome" means.
func (g *Gate) Observe(results []Result) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, res := range results {
		g.last[res.Check] = res
	}
}

// Blocked reports whether check's latest result failed and is still fresh. An
// unknown or stale check is not blocked: failing open keeps a missing
// self-check from silencing the board.
func (g *Gate) Blocked(check string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	res, ok := g.last[check]
	if !ok || res.OK {
		return false
	}

	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	fresh := g.Fresh
	if fresh == 0 {
		fresh = DefaultGateFreshness
	}
	if fresh > 0 && now().Sub(res.At) > fresh {
		return false
	}
	return true
}

// Check is one named self-test.
type Check struct {
	Name string
	Run  func(ctx context.Context) (ok bool, value, detail string, err error)
}

// Runner executes a fixed set of checks.
type Runner struct {
	Checks []Check
	Now    func() time.Time
}

// Result is one finished check.
type Result struct {
	Check    string
	OK       bool
	Value    string
	Detail   string
	At       time.Time
	Duration time.Duration
}

// Run executes every check once. A check that returns an error is recorded as
// failed with the error in Detail; it never aborts the other checks.
func (r *Runner) Run(ctx context.Context) []Result {
	out := make([]Result, 0, len(r.Checks))
	for _, c := range r.Checks {
		start := r.now()
		ok, value, detail, err := c.Run(ctx)
		if err != nil {
			ok = false
			if detail == "" {
				detail = err.Error()
			}
		}
		out = append(out, Result{
			Check: c.Name, OK: ok, Value: value, Detail: detail,
			At: start, Duration: time.Since(start),
		})
	}
	return out
}

// Record runs every check and persists the results through sink.
func (r *Runner) Record(ctx context.Context, sink interface {
	RecordSelfCheck(ctx context.Context, c store.SelfCheck) error
},
) ([]Result, error) {
	results := r.Run(ctx)
	for _, res := range results {
		if err := sink.RecordSelfCheck(ctx, store.SelfCheck{
			TS: res.At, Check: res.Check, OK: res.OK,
			Value: res.Value, Detail: res.Detail,
		}); err != nil {
			return results, err
		}
	}
	return results, nil
}

func (r *Runner) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

// DiskCheck writes, syncs, and removes a file under dir. It proves the data
// volume is writable, which is the failure that silently stops all history.
func DiskCheck(dir string) Check {
	return Check{
		Name: "disk",
		Run: func(context.Context) (bool, string, string, error) {
			if dir == "" {
				return false, "", "no data directory configured", nil
			}
			path := filepath.Join(dir, ".selfcheck")
			// #nosec G304 -- path is the operator-configured data directory plus a fixed name.
			f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if err != nil {
				return false, "", err.Error(), nil
			}
			defer func() { _ = os.Remove(path) }()
			defer func() { _ = f.Close() }()

			payload := make([]byte, 64<<10)
			if _, err := f.Write(payload); err != nil {
				return false, "", err.Error(), nil
			}
			if err := f.Sync(); err != nil {
				return false, "", err.Error(), nil
			}
			return true, "64 KiB written and synced", "", nil
		},
	}
}

// DNSCheck proves the resolver path works by asking a well-known server.
func DNSCheck(dns *probe.DNS, server, name string) Check {
	return Check{
		Name: "resolver",
		Run: func(ctx context.Context) (bool, string, string, error) {
			if dns == nil {
				return false, "", "dns instrument is not configured", nil
			}
			res := dns.Do(ctx, server, name)
			if res.Failed() {
				return false, "", res.ErrorClass + ": " + errText(res.Err), nil
			}
			return true, res.Addresses[0].String(), res.Latency.Round(time.Millisecond).String(), nil
		},
	}
}

// EgressCheck proves outbound TCP works by completing a handshake.
func EgressCheck(tcp *probe.TCP, address string) Check {
	return Check{
		Name: "egress",
		Run: func(ctx context.Context) (bool, string, string, error) {
			if tcp == nil {
				return false, "", "tcp instrument is not configured", nil
			}
			res := tcp.Do(ctx, address)
			if res.Failed() {
				return false, "", res.ErrorClass + ": " + errText(res.Err), nil
			}
			return true, address, res.Latency.Round(time.Millisecond).String(), nil
		},
	}
}

// mtuFloor is the smallest MTU this project treats as safe. The compose file
// deliberately sets 1400 on the egress network, so anything below that is a
// misconfiguration rather than a choice.
const mtuFloor = 1400

// MTUCheck reports the smallest MTU among the host's up, non-loopback
// interfaces.
//
// It cannot measure the *path* MTU: that needs a DF-bit probe and therefore raw
// sockets. What it can do is catch the documented local failure - a bridge or
// VPN interface smaller than the egress floor - before a large handshake
// black-holes and looks like DOWN (DESIGN.md §7.5, NETWORKING.md §4).
func MTUCheck(interfaces func() ([]net.Interface, error)) Check {
	return Check{
		Name: "mtu",
		Run: func(context.Context) (bool, string, string, error) {
			if interfaces == nil {
				interfaces = net.Interfaces
			}
			ifaces, err := interfaces()
			if err != nil {
				return false, "", err.Error(), nil
			}

			smallest, name, checked := 0, "", 0
			for _, iface := range ifaces {
				if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
					continue
				}
				if iface.MTU <= 0 {
					continue
				}
				checked++
				if smallest == 0 || iface.MTU < smallest {
					smallest, name = iface.MTU, iface.Name
				}
			}
			if checked == 0 {
				return false, "", "no up, non-loopback interface found", nil
			}

			detail := fmt.Sprintf("%s has the smallest MTU (%d)", name, smallest)
			if smallest < mtuFloor {
				return false, fmt.Sprintf("%s %d", name, smallest),
					detail + fmt.Sprintf("; below the %d-byte floor, large handshakes may black-hole", mtuFloor), nil
			}
			return true, fmt.Sprintf("%s %d", name, smallest), detail, nil
		},
	}
}

// Port53Check proves both transports to a resolver on port 53 work.
//
// A blocked port 53 is the documented false-red source: every DNS anchor times
// out and the board looks like a global outage while the internet is fine
// (DESIGN.md §7.5). UDP is the common case and TCP catches a middlebox that
// allows queries but blocks fallback, so both are checked.
func Port53Check(dns *probe.DNS, tcp *probe.TCP, server, name string) Check {
	return Check{
		Name: "port53",
		Run: func(ctx context.Context) (bool, string, string, error) {
			if dns == nil || tcp == nil {
				return false, "", "dns or tcp instrument is not configured", nil
			}

			udp := dns.Do(ctx, server, name)
			if udp.Failed() {
				return false, "udp blocked",
					"udp/53: " + udp.ErrorClass + ": " + errText(udp.Err), nil
			}

			tcpRes := tcp.Do(ctx, server)
			if tcpRes.Failed() {
				return false, "tcp blocked",
					"tcp/53: " + tcpRes.ErrorClass + ": " + errText(tcpRes.Err), nil
			}

			return true, "udp+tcp", fmt.Sprintf("udp %s, tcp %s",
				udp.Latency.Round(time.Millisecond),
				tcpRes.Latency.Round(time.Millisecond)), nil
		},
	}
}

// ClockCheck compares the local clock with the Date header of one HTTP response.
// Skew beyond maxSkew is a degraded check, not a process failure: the board
// still renders, but timestamps and baselines become suspect.
func ClockCheck(http *probe.HTTP, url string, maxSkew time.Duration) Check {
	return Check{
		Name: "clock",
		Run: func(ctx context.Context) (bool, string, string, error) {
			if http == nil {
				return false, "", "http instrument is not configured", nil
			}
			if maxSkew <= 0 {
				maxSkew = time.Minute
			}
			res := http.Do(ctx, url)
			if res.Failed() {
				return false, "", res.ErrorClass + ": " + errText(res.Err), nil
			}
			date := res.Header.Get("Date")
			if date == "" {
				return false, "", "response has no Date header", nil
			}
			remote, err := time.Parse(time.RFC3339, date)
			if err != nil {
				// http.TimeFormat is RFC1123 with GMT.
				remote, err = time.Parse(time.RFC1123, date)
				if err != nil {
					return false, "", "unparseable Date header: " + date, nil
				}
			}
			skew := time.Since(remote)
			if skew < 0 {
				skew = -skew
			}
			detail := fmt.Sprintf("%.0fs skew", skew.Seconds())
			if skew > maxSkew {
				return false, date, detail, nil
			}
			return true, date, detail, nil
		},
	}
}

func errText(err error) string {
	if err == nil {
		return "no detail"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline exceeded"
	}
	return err.Error()
}
