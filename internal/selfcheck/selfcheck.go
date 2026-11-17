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
	"os"
	"path/filepath"
	"time"

	"github.com/Greg-2600/netweather/internal/probe"
	"github.com/Greg-2600/netweather/internal/store"
)

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
