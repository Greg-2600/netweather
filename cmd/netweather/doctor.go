package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Greg-2600/netweather/internal/doctor"
	"github.com/Greg-2600/netweather/internal/netx"
	"github.com/Greg-2600/netweather/internal/probe"
	"github.com/Greg-2600/netweather/internal/store"
)

// doctorCommand reports which instruments work on this host.
//
// Exit is 0 when every fatal check passes, even if optional capabilities are
// missing: a host without CAP_NET_RAW can still monitor HTTP, TLS, and DNS.
// The table is data on stdout; the summary error is what main reports on stderr.
func doctorCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "emit the API envelope")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("doctor: %w", err)
	}
	if len(fs.Args()) > 0 {
		return fmt.Errorf("doctor: unexpected argument %q", fs.Args()[0])
	}

	dataDir := strings.TrimSpace(os.Getenv("NETWEATHER_DATA"))
	if dataDir == "" {
		dataDir = "./data"
	}

	dialer := netx.New()
	instruments := newInstruments()

	// Traceroute is off unless the operator opted in. When it is off, the raw
	// socket is not a missing capability but a deliberate choice, so the doctor
	// reports the configuration rather than a warning that suggests a bug.
	traceCheck := doctor.Check{Name: "traceroute (optional)", Run: func(context.Context) (string, error) {
		return "disabled by NETWEATHER_TRACE_ENABLED (opt-in, with CAP_NET_RAW)", nil
	}}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("NETWEATHER_TRACE_ENABLED")), "true") {
		traceCheck.Run = capabilityCheck("raw ICMP socket", func() (io.Closer, error) {
			return probe.NewTrace().Open()
		})
	}

	checks := []doctor.Check{
		{Name: "data dir", Fatal: true, Run: diskCheck(dataDir)},
		{Name: "resolver", Fatal: true, Run: dnsCheck(instruments.DNS)},
		{Name: "tcp egress", Fatal: true, Run: tcpCheck(instruments.TCP)},
		{Name: "tls", Run: tlsCheck(instruments.TLS)},
		{Name: "http", Run: httpCheck(instruments.HTTP)},
		{Name: "ntp", Run: ntpCheck(instruments.NTP)},
		{Name: "icmp (optional)", Run: capabilityCheck("ICMP socket", func() (io.Closer, error) {
			transport, err := instruments.ICMP.Open()
			if err != nil {
				return nil, err
			}
			return transport, nil
		})},
		traceCheck,
		{Name: "egress policy", Fatal: true, Run: func(context.Context) (string, error) {
			if !dialer.Policy.PortAllowed(443) || dialer.Policy.PortAllowed(22) {
				return "", fmt.Errorf("outbound allowlist is not the documented set")
			}
			return "443 allowed, 22 refused", nil
		}},
	}

	// The Tor sidecar is optional, so its check is only added when the vantage
	// is enabled, and a missing proxy is a warning rather than a startup
	// failure. The exit policy itself is guaranteed by the baked-in torrc in
	// Dockerfile.tor; the app container cannot inspect the sidecar's config,
	// and claiming to would be the kind of assurance this project avoids.
	if strings.EqualFold(strings.TrimSpace(os.Getenv("NETWEATHER_TOR")), "true") ||
		strings.TrimSpace(os.Getenv("NETWEATHER_TOR_SOCKS")) != "" {
		proxy := strings.TrimSpace(os.Getenv("NETWEATHER_TOR_SOCKS"))
		if proxy == "" {
			proxy = "tor:9050"
		}
		checks = append(checks, doctor.Check{Name: "tor socks (optional)", Run: torSocksCheck(proxy)})
	}

	runner := &doctor.Runner{Checks: checks}
	results := runner.Run(context.Background())

	if *asJSON {
		type item struct {
			Name     string `json:"name"`
			Status   string `json:"status"`
			Detail   string `json:"detail,omitempty"`
			Fatal    bool   `json:"fatal"`
			Duration string `json:"duration"`
		}
		items := make([]item, 0, len(results))
		for _, res := range results {
			items = append(items, item{
				Name: res.Name, Status: string(res.Status), Detail: res.Detail,
				Fatal: res.Fatal, Duration: res.Dur.Round(time.Millisecond).String(),
			})
		}
		if err := writeJSON(out, items); err != nil {
			return fmt.Errorf("doctor: write report: %w", err)
		}
	} else if err := doctor.Write(out, results); err != nil {
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
	if warnings > 0 && !*asJSON {
		// A summary line belongs on stdout with the table, not stderr: this is
		// the command's output, not a log record.
		_, _ = fmt.Fprintf(out, "\n%d optional capability(ies) unavailable; probes continue with reduced fidelity\n", warnings)
	}

	// DESIGN.md §6.4: the fleet view exists to judge an interval choice. A
	// fresh host has no database yet, which is stated rather than printed as an
	// error.
	if !*asJSON {
		_, _ = fmt.Fprint(out, "\n"+fleetReport(dataDir))
	}
	return nil
}

// fleetReport reads the aggregate view without migrating or writing the
// database, so a diagnostic never changes the file it inspects.
func fleetReport(dataDir string) string {
	path := filepath.Join(dataDir, "netweather.db")
	if _, err := os.Stat(path); err != nil { // #nosec G703 -- operator-supplied data directory.
		return "fleet: no database yet; start the service once to populate it\n"
	}
	ctx := context.Background()
	st, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		return fmt.Sprintf("fleet: unavailable: %v\n", err)
	}
	defer func() { _ = st.Close() }()

	stats, err := st.FleetStats(ctx, "local")
	if err != nil {
		return fmt.Sprintf("fleet: unavailable: %v\n", err)
	}
	return formatFleet(stats)
}

// formatFleet renders the block from DESIGN.md §6.4.
func formatFleet(s store.FleetStats) string {
	classes := make([]string, 0, len(s.ByClass))
	for class := range s.ByClass {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	parts := make([]string, 0, len(classes))
	for _, class := range classes {
		name := class
		if name == "" {
			name = "unclassified"
		}
		parts = append(parts, fmt.Sprintf("%s %d", name, s.ByClass[class]))
	}

	blockedShare := 0.0
	if s.Total > 0 {
		blockedShare = 100 * float64(s.Blocked) / float64(s.Total)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "fleet: %d target(s)", s.Total)
	if len(parts) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(parts, ", "))
	}
	b.WriteByte('\n')
	fmt.Fprintf(&b, "       effective rate %.1f probes/s, blocked %.1f%%, unmeasurable %d\n",
		s.EffectivePerSecond, blockedShare, s.Unmeasurable)
	fmt.Fprintf(&b, "       ASN coverage %d, countries %d", s.ASNCount, s.CountryCount)
	if s.TopASN > 0 {
		fmt.Fprintf(&b, ", top ASN AS%d", s.TopASN)
		if s.TopASNName != "" {
			fmt.Fprintf(&b, " (%s)", s.TopASNName)
		}
		fmt.Fprintf(&b, " %.1f%%", 100*s.TopASNShare)
	}
	b.WriteByte('\n')
	return b.String()
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

// torSocksCheck proves the sidecar's SOCKS port answers. It does not audit the
// exit policy: that is guaranteed by the torrc baked into Dockerfile.tor, and
// the app container has no way to read the sidecar's effective config.
func torSocksCheck(proxy string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		d := net.Dialer{Timeout: 3 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", proxy)
		if err != nil {
			return "", fmt.Errorf("SOCKS proxy %s: %w", proxy, err)
		}
		_ = conn.Close()
		return proxy + " answered", nil
	}
}
