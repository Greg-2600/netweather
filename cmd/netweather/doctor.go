package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Greg-2600/netweather/internal/doctor"
	"github.com/Greg-2600/netweather/internal/netx"
	"github.com/Greg-2600/netweather/internal/probe"
)

// doctorCommand reports which instruments work on this host.
//
// Exit is 0 when every fatal check passes, even if optional capabilities are
// missing: a host without CAP_NET_RAW can still monitor HTTP, TLS, and DNS.
// The table is data on stdout; the summary error is what main reports on stderr.
func doctorCommand(args []string, out io.Writer) error {
	if len(args) > 0 {
		return fmt.Errorf("doctor: takes no arguments")
	}

	dataDir := strings.TrimSpace(os.Getenv("NETWEATHER_DATA"))
	if dataDir == "" {
		dataDir = "./data"
	}

	dialer := netx.New()
	instruments := newInstruments()

	checks := []doctor.Check{
		{Name: "data dir", Fatal: true, Run: diskCheck(dataDir)},
		{Name: "resolver", Fatal: true, Run: dnsCheck(instruments.DNS)},
		{Name: "tcp egress", Fatal: true, Run: tcpCheck(instruments.TCP)},
		{Name: "tls", Run: tlsCheck(instruments.TLS)},
		{Name: "http", Run: httpCheck(instruments.HTTP)},
		{Name: "ntp", Run: ntpCheck(instruments.NTP)},
		{Name: "icmp (optional)", Run: capabilityCheck("raw ICMP socket", func() (io.Closer, error) {
			transport, err := instruments.ICMP.Open()
			if err != nil {
				return nil, err
			}
			return transport, nil
		})},
		{Name: "traceroute (optional)", Run: capabilityCheck("raw ICMP socket", func() (io.Closer, error) {
			return probe.NewTrace().Open()
		})},
		{Name: "egress policy", Fatal: true, Run: func(context.Context) (string, error) {
			if !dialer.Policy.PortAllowed(443) || dialer.Policy.PortAllowed(22) {
				return "", fmt.Errorf("outbound allowlist is not the documented set")
			}
			return "443 allowed, 22 refused", nil
		}},
	}

	runner := &doctor.Runner{Checks: checks}
	results := runner.Run(context.Background())

	if err := doctor.Write(out, results); err != nil {
		return fmt.Errorf("doctor: write report: %w", err)
	}

	warnings := 0
	for _, res := range results {
		if res.Status == doctor.WARN {
			warnings++
		}
	}
	if doctor.Failed(results) {
		return fmt.Errorf("doctor: %d fatal check(s) failed", countFailed(results))
	}
	if warnings > 0 {
		// A summary line belongs on stdout with the table, not stderr: this is
		// the command's output, not a log record.
		_, _ = fmt.Fprintf(out, "\n%d optional capability(ies) unavailable; probes continue with reduced fidelity\n", warnings)
	}
	return nil
}

func countFailed(results []doctor.Result) int {
	n := 0
	for _, res := range results {
		if res.Status == doctor.FAIL {
			n++
		}
	}
	return n
}

func diskCheck(dir string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		info, err := os.Stat(dir) // #nosec G703 -- operator-configured data directory.
		if err != nil {
			return "", fmt.Errorf("data directory %s is not usable: %w", dir, err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("data path %s is not a directory", dir)
		}

		path := filepath.Join(dir, ".doctor")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) // #nosec G304,G703 -- operator-configured data directory plus a fixed name.
		if err != nil {
			return "", fmt.Errorf("data directory is not writable: %w", err)
		}
		defer func() { _ = os.Remove(path) }() // #nosec G703 -- fixed name under the data directory.
		defer func() { _ = f.Close() }()

		payload := make([]byte, 64<<10)
		if _, err := f.Write(payload); err != nil {
			return "", fmt.Errorf("write: %w", err)
		}
		if err := f.Sync(); err != nil {
			return "", fmt.Errorf("sync: %w", err)
		}
		return "64 KiB written and synced", nil
	}
}

func dnsCheck(dns *probe.DNS) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		if dns == nil {
			return "", fmt.Errorf("dns instrument is not configured")
		}
		res := dns.Do(ctx, "1.1.1.1:53", "example.com")
		if res.Failed() {
			return "", fmt.Errorf("%s: %w", res.ErrorClass, res.Err)
		}
		return fmt.Sprintf("%s in %s", res.Addresses[0], res.Latency.Round(time.Millisecond)), nil
	}
}

func tcpCheck(tcp *probe.TCP) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		if tcp == nil {
			return "", fmt.Errorf("tcp instrument is not configured")
		}
		res := tcp.Do(ctx, "1.1.1.1:443")
		if res.Failed() {
			return "", fmt.Errorf("%s: %w", res.ErrorClass, res.Err)
		}
		return res.Latency.Round(time.Millisecond).String(), nil
	}
}

func tlsCheck(tls *probe.TLS) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		if tls == nil {
			return "", fmt.Errorf("tls instrument is not configured")
		}
		res := tls.Do(ctx, "1.1.1.1:443")
		if res.Failed() {
			return "", fmt.Errorf("%s: %w", res.ErrorClass, res.Err)
		}
		return res.Latency.Round(time.Millisecond).String(), nil
	}
}

func httpCheck(h *probe.HTTP) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		if h == nil {
			return "", fmt.Errorf("http instrument is not configured")
		}
		res := h.Do(ctx, "https://www.cloudflare.com/")
		if res.Failed() {
			return "", fmt.Errorf("%s: %w", res.ErrorClass, res.Err)
		}
		return fmt.Sprintf("%d in %s", res.Status, res.Latency.Round(time.Millisecond)), nil
	}
}

func ntpCheck(ntp *probe.NTP) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		if ntp == nil {
			return "", fmt.Errorf("ntp instrument is not configured")
		}
		res := ntp.Do(ctx, "time.cloudflare.com:123")
		if res.Failed() {
			return "", fmt.Errorf("%s: %w", res.ErrorClass, res.Err)
		}
		detail := fmt.Sprintf("offset %s, stratum %d", res.Offset.Round(time.Millisecond), res.Stratum)
		if res.Offset > 2*time.Second || res.Offset < -2*time.Second {
			return detail, fmt.Errorf("clock offset beyond 2s")
		}
		return detail, nil
	}
}

// capabilityCheck proves a raw socket can be opened and closed, then closes it.
// It never sends traffic: the doctor's job is availability, not measurement.
func capabilityCheck(name string, open func() (io.Closer, error)) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		if open == nil {
			return "", fmt.Errorf("instrument is not configured")
		}
		transport, err := open()
		if err != nil {
			return "", fmt.Errorf("%s unavailable: %w", name, err)
		}
		_ = transport.Close()
		return name + " available", nil
	}
}
