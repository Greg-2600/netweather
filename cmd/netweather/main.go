// Command netweather is the single binary: probe scheduler, classifier, store,
// and dashboard in one process. See docs/DESIGN.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	neturl "net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Greg-2600/netweather/internal/agent"
	"github.com/Greg-2600/netweather/internal/anomaly"
	"github.com/Greg-2600/netweather/internal/api"
	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/config"
	"github.com/Greg-2600/netweather/internal/corridor"
	"github.com/Greg-2600/netweather/internal/enrich"
	"github.com/Greg-2600/netweather/internal/feeds"
	"github.com/Greg-2600/netweather/internal/incidents"
	"github.com/Greg-2600/netweather/internal/logging"
	"github.com/Greg-2600/netweather/internal/metrics"
	"github.com/Greg-2600/netweather/internal/netx"
	"github.com/Greg-2600/netweather/internal/probe"
	"github.com/Greg-2600/netweather/internal/schedule"
	"github.com/Greg-2600/netweather/internal/seed"
	"github.com/Greg-2600/netweather/internal/selfcheck"
	"github.com/Greg-2600/netweather/internal/startup"
	"github.com/Greg-2600/netweather/internal/store"
	"github.com/Greg-2600/netweather/internal/torcap"
	"github.com/Greg-2600/netweather/internal/version"
	"github.com/Greg-2600/netweather/internal/web"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "netweather:", err)
		os.Exit(1)
	}
}

// run is separated from main so every branch is testable without spawning a process.
func run(args []string, stdout io.Writer) error {
	cfg, err := logging.FromEnv(os.Getenv)
	if err != nil {
		return fmt.Errorf("logging: %w", err)
	}
	lg, err := logging.New(cfg, stdout)
	if err != nil {
		return fmt.Errorf("logging: %w", err)
	}
	defer func() { _ = lg.Close() }()

	command := "serve"
	if len(args) > 0 {
		command = args[0]
	}

	switch command {
	case "version", "-v", "--version":
		return versionCommand(args[1:], stdout)

	case "healthcheck":
		return healthcheckCommand(args[1:], stdout)

	case "seed":
		return seedCommand(stdout)

	case "export":
		return exportCommand(args[1:], stdout)

	case "debug":
		return debugCommand(args[1:], stdout)

	case "doctor":
		return doctorCommand(args[1:], stdout)

	case "serve":
		lg.Info("starting",
			"event", "startup",
			"version", version.Version,
			"commit", version.Commit,
			"log_level", logging.LevelName(cfg.Level),
			"log_format", cfg.Format,
			"log_file", cfg.File,
		)

		if err := startup.RefuseIfRoot(startup.EUID()); err != nil {
			lg.Error("refusing to start", "event", "startup_refused", "error", err)
			return err
		}

		if cfg.Level == logging.LevelTrace {
			lg.Warn("global TRACE is enabled",
				"event", "log_volume_warning",
				"estimate", "0.5-1 GB/day at 750 targets",
				"advice", "scope it instead: netweather debug enable --target <host> --level trace --for 10m (docs/LOGGING.md §5)",
			)
		}

		return serve(lg)

	default:
		return fmt.Errorf("unknown command %q (try: serve, version, healthcheck)", command)
	}
}

// serve wires the pipeline: store -> seed targets -> agent loop -> board, and
// the HTTP surface on top. Configuration and the exposure guardrail are
// validated before the socket is opened.
func serve(lg *logging.Logger) error {
	cfg, err := api.FromEnv(os.Getenv)
	if err != nil {
		lg.Error("invalid ui configuration", "event", "config_invalid", "error", err)
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dataDir := os.Getenv("NETWEATHER_DATA")
	if dataDir == "" {
		dataDir = "./data"
	}

	// Scoped verbose logging is time-boxed and persisted, so a restart cannot
	// silently inherit an old debug session.
	scopes := logging.NewScopes(filepath.Join(dataDir, "debug.json"))
	if expired, err := scopes.Load(); err != nil {
		lg.Warn("debug scopes unavailable", "event", "debug_scopes_load_failed", "error", err)
	} else {
		for _, sc := range expired {
			lg.Info("debug scope expired while stopped",
				"event", "debug_scope_expired", "component", "logging",
				"scope_id", sc.ID, "target", sc.Target, "component_scope", sc.Component)
		}
	}
	lg.UseScopes(scopes)

	st, err := store.Open(ctx, filepath.Join(dataDir, "netweather.db"))
	if err != nil {
		lg.Error("store unavailable", "event", "store_open_failed", "error", err)
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			lg.Warn("store close failed", "event", "store_close_failed", "error", err)
		}
	}()

	if err := st.EnsureVantage(ctx, "local", "direct", "primary"); err != nil {
		return err
	}

	targets, err := loadTargets(ctx, st)
	if err != nil {
		return err
	}

	torAgent, err := newTorAgent(ctx, st, targets, lg)
	if err != nil {
		lg.Error("tor vantage unavailable", "event", "tor_unavailable", "error", err)
		return err
	}

	changes := board.NewBroadcast()
	// One signal stream for the board, one for the incident log: a state change
	// and an incident opening are different events even when they share a tick.
	incidentChanges := board.NewBroadcast()
	source := &board.StoreSource{Store: st, Vantage: "local", Changes: changes}

	// A blocked egress port 53 is a fact about this vantage, not about a
	// target: the agent asks the gate before it trusts a DNS transport failure.
	healthGate := selfcheck.NewGate()
	ag := newAgent(targets, st, lg)
	ag.DNSBlocked = func() bool { return healthGate.Blocked("port53") }

	// The monitor must be able to see before its board is believable, so the
	// self-checks run at startup and with maintenance, not only on request.
	instruments := newInstruments()
	// Egress identity: every baseline is relative to the address this host
	// leaves from, so the address is worth recording and worth noticing when it
	// moves (DESIGN.md §7.5).
	egressLookup := cymruLookup()

	selfRunner := &selfcheck.Runner{Checks: []selfcheck.Check{
		selfcheck.DiskCheck(dataDir),
		selfcheck.DNSCheck(instruments.DNS, "1.1.1.1:53", "example.com"),
		selfcheck.Port53Check(instruments.DNS, instruments.TCP, "1.1.1.1:53", "example.com"),
		selfcheck.MTUCheck(nil),
		selfcheck.EgressCheck(instruments.TCP, "1.1.1.1:443"),
		selfcheck.EgressIPCheck(instruments.HTTP, "https://1.1.1.1/cdn-cgi/trace", egressLookup),
		selfcheck.ClockCheck(instruments.HTTP, "https://www.cloudflare.com/", time.Minute),
	}}

	// Feeds are opt-in: they are outbound calls to someone else's API, and an
	// operator should choose that rather than discover it.
	feedPollers := buildFeedPollers(st, lg)
	for _, p := range feedPollers {
		go feeds.Loop(ctx, p, lg.Logger)
	}

	var metricsHandler http.Handler
	if os.Getenv("NETWEATHER_METRICS") == "true" {
		metricsHandler = metrics.Handler(st, "local")
	}

	ui, err := web.New(web.Deps{
		Logger:              lg.Logger,
		Source:              source,
		Watcher:             source,
		Token:               cfg.Token,
		SecureCookies:       os.Getenv("NETWEATHER_UI_SECURE_COOKIES") == "true",
		Version:             version.Version,
		StartedAt:           time.Now(),
		History:             st,
		Incidents:           st,
		Corroboration:       st,
		Feeds:               st,
		TargetCorroboration: st,
		CountryEvents:       st,
		Corridors:           st,
		DNSObservations:     st,
		IncidentWatcher:     incidentChanges,
		Coverage:            st,
		CoverageTrend:       st,
		Series:              st,
		SeriesBatch:         st,
		Changes:             st,
		Control:             ag,
		Vantage:             "local",
	})
	if err != nil {
		return err
	}

	handler, err := api.New(cfg, api.Deps{
		Logger:          lg.Logger,
		Ring:            lg.Ring,
		Version:         version.Version,
		Commit:          version.Commit,
		StartedAt:       time.Now(),
		Source:          source,
		Watcher:         changes,
		RunContext:      ctx,
		Control:         ag,
		Probes:          st,
		Incidents:       st,
		Corroboration:   st,
		Corridors:       st,
		Scopes:          st,
		Anomalies:       st,
		Targets:         st,
		Coverage:        st,
		CoverageHistory: st,
		SelfChecks:      st,
		Exporter:        st,
		Feeds:           st,
		Debug:           scopes,
		IncidentWatcher: incidentChanges,
		Tracer:          probe.NewTrace(),
		Resolver:        netx.New(),
		Metrics:         metricsHandler,
		Vantage:         "local",
		Fallback:        ui,
	})
	if err != nil {
		return err
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Listen, err)
	}

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// SSE will relax this per connection with http.ResponseController.
		WriteTimeout:   30 * time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 16 << 10,
		ErrorLog:       slog.NewLogLogger(lg.Handler(), slog.LevelWarn),
	}

	go probeLoop(ctx, lg, ag, changes)
	if torAgent != nil {
		go probeLoop(ctx, lg, torAgent, changes)
	}

	incidentInterval := 5 * time.Minute
	if v := os.Getenv("NETWEATHER_INCIDENT_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			incidentInterval = d
		}
	}
	go incidents.Loop(ctx, st, st, st, "local", incidentInterval, lg.Logger, incidentChanges.Notify)
	go anomaly.Loop(ctx, st, st, "local", incidentInterval, lg.Logger)
	go maintenanceLoop(ctx, st, lg, selfRunner, healthGate, scopes)
	go configLoop(ctx, st, ag, lg.Logger)
	go enrichmentLoop(ctx, st, lg)
	go corridorLoop(ctx, st, ag.Paused, lg)

	go func() {
		<-ctx.Done()
		// Shutdown needs its own deadline, but must inherit the parent's values
		// while ignoring its cancellation (which is the reason we are here).
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	lg.Info("listening",
		"event", "startup",
		"addr", cfg.Listen,
		"auth", cfg.AuthMode(),
		"targets", len(targets),
		"db", st.Path(),
	)

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}

	lg.Info("stopped", "event", "shutdown")
	return nil
}

// loadTargets prefers NETWEATHER_CONFIG, then the shipped seed list. Only the
// HTTP instrument exists so far; a config that asks for another probe fails
// loudly instead of silently dropping targets.
func loadTargets(ctx context.Context, st *store.Store) ([]agent.Target, error) {
	path := os.Getenv("NETWEATHER_CONFIG")
	if path == "" {
		return seedTargets(ctx, st)
	}

	f, err := config.Load(path)
	if err != nil {
		return nil, err
	}

	out := make([]agent.Target, 0, len(f.Targets))
	for _, t := range f.Targets {
		var (
			url       string
			address   string
			resolvers []string
		)
		switch t.Probe {
		case "http":
			url = targetURL(t.Host, t.Port)
		case "tls", "tcp", "tcpping", "dns", "ntp":
			address = net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
		case "icmp":
			// ICMP has no port; config validation already forced port 0.
			address = t.Host
		default:
			return nil, fmt.Errorf("target %s: probe %q is not implemented yet", t.Host, t.Probe)
		}

		// A DNS target always has at least one resolver: its own address. The
		// divergence path only wakes when a second one is configured.
		if t.Probe == "dns" {
			resolvers = append(resolvers, address)
			for _, extra := range t.Resolvers {
				if extra != address {
					resolvers = append(resolvers, extra)
				}
			}
		}

		expect, err := t.Expect.Classify()
		if err != nil {
			return nil, fmt.Errorf("target %s: %w", t.Host, err)
		}
		// Only an ICMP anchor may be classified DOWN from an ICMP timeout; on a
		// service target the classifier refuses to make that claim.
		expect.ICMPAnchor = t.ICMPAnchor
		if err := st.UpsertTarget(ctx, store.Target{
			ID:        t.ID,
			Host:      t.Host,
			Port:      t.Port,
			Probe:     t.Probe,
			Name:      t.Name,
			Class:     t.Class,
			Sector:    t.Sector,
			Country:   t.Country,
			Tier:      t.Tier,
			IntervalS: t.IntervalS,
		}); err != nil {
			return nil, err
		}

		out = append(out, agent.Target{
			ID:         t.ID,
			Name:       t.Name,
			Host:       t.Host,
			URL:        url,
			Address:    address,
			Query:      t.QName,
			Resolvers:  resolvers,
			Instrument: t.Probe,
			Vantage:    "local",
			Tier:       schedule.Tier(t.Tier),
			Interval:   t.Interval(),
			Expect:     expect,
			ICMPAnchor: t.ICMPAnchor,
		})
	}
	return out, nil
}

func targetURL(host string, port int) string {
	scheme := "http"
	if port == 443 {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/"
}

// seedTargets reconciles the shipped list into the store and returns the agent's
// view. NETWEATHER_SEED=none disables seeding (tests, air-gapped installs).
func seedTargets(ctx context.Context, st *store.Store) ([]agent.Target, error) {
	if os.Getenv("NETWEATHER_SEED") == "none" {
		return nil, nil
	}

	entries := seed.Default()
	out := make([]agent.Target, 0, len(entries))
	for _, e := range entries {
		interval := schedule.DefaultInterval(e.Tier)

		if err := st.UpsertTarget(ctx, store.Target{
			ID:        e.ID,
			Host:      e.Host,
			Port:      443,
			Probe:     "http",
			Name:      e.Name,
			Class:     e.Class,
			Sector:    e.Sector,
			Tier:      int(e.Tier),
			IntervalS: int(interval / time.Second),
		}); err != nil {
			return nil, err
		}

		out = append(out, agent.Target{
			ID:       e.ID,
			Name:     e.Name,
			Host:     e.Host,
			URL:      e.URL,
			Vantage:  "local",
			Tier:     e.Tier,
			Interval: interval,
		})
	}
	return out, nil
}

// seedCommand loads NETWEATHER_CONFIG into the database without starting a
// server. It is the populate-and-exit path for images, CI and first installs.
func seedCommand(out io.Writer) error {
	path := strings.TrimSpace(os.Getenv("NETWEATHER_CONFIG"))
	if path == "" {
		return errors.New("seed: NETWEATHER_CONFIG must point at a targets file")
	}

	dataDir := os.Getenv("NETWEATHER_DATA")
	if dataDir == "" {
		dataDir = "./data"
	}

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dataDir, "netweather.db"))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	if err := st.EnsureVantage(ctx, "local", "direct", "primary"); err != nil {
		return err
	}

	targets, err := loadTargets(ctx, st)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(out, "seeded %d targets into %s\n", len(targets), st.Path())
	return err
}

// versionCommand prints build identity. The plain form is one line for a human
// and a log grep; --json is the same data in the API envelope.
func versionCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "emit the API envelope")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("version: %w", err)
	}
	if len(fs.Args()) > 0 {
		return fmt.Errorf("version: unexpected argument %q", fs.Args()[0])
	}

	if !*asJSON {
		_, err := fmt.Fprintln(out, version.String())
		return err
	}
	return writeJSON(out, map[string]string{
		"version": version.Version,
		"commit":  version.Commit,
		"built":   version.Date,
	})
}

// healthcheckCommand is the container liveness probe. Its contract is silence
// on success, which is why --json is explicit rather than the default.
func healthcheckCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "emit the API envelope")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	if len(fs.Args()) > 0 {
		return fmt.Errorf("healthcheck: unexpected argument %q", fs.Args()[0])
	}

	if err := healthcheck(); err != nil {
		if *asJSON {
			_ = writeJSONError(out, "unhealthy", err)
		}
		return err
	}
	if *asJSON {
		return writeJSON(out, map[string]string{"status": "ok"})
	}
	return nil
}

// healthcheck is the container liveness probe. It asks the local listener for
// /healthz rather than inspecting process state, because "the process is alive"
// and "the server answers" are different claims (NETWORKING.md §7.3).
func healthcheck() error {
	url := os.Getenv("NETWEATHER_HEALTHCHECK_URL")
	if url == "" {
		addr := os.Getenv("NETWEATHER_UI_LISTEN")
		if addr == "" {
			addr = "127.0.0.1:2600"
		}
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return fmt.Errorf("healthcheck: %w", err)
		}
		url = "http://127.0.0.1:" + port + "/healthz"
	}

	// A liveness probe must only ever touch the local instance. Validating that
	// here keeps the healthcheck from becoming an SSRF primitive if the
	// environment is attacker-controlled.
	parsed, err := neturl.Parse(url)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("healthcheck: unsupported scheme %q", parsed.Scheme)
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("healthcheck: %q is not a loopback address", host)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// #nosec G704 -- parsed.Scheme and the host have been checked as loopback above.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	// #nosec G704 -- same validated loopback URL.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: %s", resp.Status)
	}
	return nil
}

func newInstruments() agent.Instruments {
	dialer := netx.New()

	// ICMP resolves through the guarded dialer like every other instrument, so
	// a hostname anchor cannot leak a lookup to the host resolver.
	ping := probe.NewICMP()
	ping.Resolve = func(ctx context.Context, host string) (netip.Addr, error) {
		addrs, err := dialer.CheckHost(ctx, host)
		if err != nil {
			return netip.Addr{}, err
		}
		if len(addrs) == 0 {
			return netip.Addr{}, fmt.Errorf("icmp: %q resolved to no addresses", host)
		}
		return addrs[0], nil
	}

	return agent.Instruments{
		HTTP: probe.NewHTTP(dialer),
		TLS:  probe.NewTLS(dialer),
		TCP:  probe.NewTCP(dialer),
		DNS:  probe.NewDNS(dialer.DialContext),
		NTP:  probe.NewNTP(dialer),
		ICMP: ping,
	}
}

func newAgent(targets []agent.Target, st *store.Store, lg *logging.Logger) *agent.Agent {
	instruments := newInstruments()
	ag := agent.New(targets, instruments, st)
	ag.Logger = lg.Logger
	// The direct vantage owns the issuer baseline; the Tor vantage is
	// corroboration and does not get to learn a different one.
	ag.Pins = st
	// Per-resolver DNS baselines: each target with more than one resolver is
	// compared against its own history (CLASSIFIER.md §3.2).
	ag.Divergence = st
	ag.DNSQuery = func(ctx context.Context, server, name string) probe.DNSResult {
		return instruments.DNS.Do(ctx, server, name)
	}
	return ag
}

// enrichmentLoop learns ASN, prefix and CDN for hostname targets. It runs at
// startup and every six hours; NETWEATHER_ENRICH=false disables it.
func enrichmentLoop(ctx context.Context, st *store.Store, lg *logging.Logger) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("NETWEATHER_ENRICH")), "false") {
		return
	}

	dialer := netx.New()
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			// Every query goes to one well-known resolver over the guarded
			// dialer; the host's resolver is not part of the trust boundary.
			return dialer.DialContext(ctx, "udp", "1.1.1.1:53")
		},
	}

	// IX membership is a label, never a signal, and one ASN can sit behind many
	// targets: the client caches per ASN so a run costs one request per network.
	// NETWEATHER_PEERINGDB=false disables it without disabling enrichment.
	var peerdb *enrich.PeerDB
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("NETWEATHER_PEERINGDB")), "false") {
		peerdb = enrich.NewPeerDB(&http.Client{
			Transport: probe.Transport(netx.New()),
			Timeout:   20 * time.Second,
		})
		if base := strings.TrimSpace(os.Getenv("NETWEATHER_PEERINGDB_URL")); base != "" {
			peerdb.BaseURL = base
		}
	}

	ixFor := func(ctx context.Context, asn int) []string {
		if peerdb == nil || asn <= 0 {
			return nil
		}
		names, err := peerdb.IXNames(ctx, asn)
		if err != nil {
			lg.Debug("peeringdb lookup failed",
				"event", "peeringdb_lookup_failed", "component", "enrich",
				"asn", asn, "error", err)
			return nil
		}
		return names
	}

	run := func() {
		targets, err := st.TargetsNeedingEnrichment(ctx, 50)
		if err != nil {
			lg.Warn("enrichment scan failed",
				"event", "enrichment_scan_failed", "component", "enrich", "error", err)
			return
		}
		for _, t := range targets {
			if ctx.Err() != nil {
				return
			}

			addrs, err := dialer.CheckHost(ctx, t.Host)
			if err != nil {
				lg.Debug("enrichment resolve failed",
					"event", "enrichment_resolve_failed", "component", "enrich",
					"target", t.Host, "error", err)
				continue
			}
			res, err := enrich.Host(ctx, resolver, t.Host, addrs)
			if err != nil {
				lg.Debug("enrichment lookup failed",
					"event", "enrichment_lookup_failed", "component", "enrich",
					"target", t.Host, "error", err)
				continue
			}
			if err := st.UpdateEnrichment(ctx, t.ID, store.Enrichment{
				ASN: res.ASN, Prefix: res.Prefix, CDN: res.CDN,
				ASCountry: res.ASCountry, ASName: res.ASName,
				IXNames: ixFor(ctx, res.ASN),
			}); err != nil {
				lg.Warn("enrichment write failed",
					"event", "enrichment_write_failed", "component", "enrich",
					"target", t.Host, "error", err)
				continue
			}
			lg.Info("target enriched",
				"event", "target_enriched", "component", "enrich",
				"target", t.Host, "asn", res.ASN, "prefix", res.Prefix, "cdn", res.CDN,
				"as_name", res.ASName, "as_country", res.ASCountry)

			select {
			case <-ctx.Done():
				return
			case <-time.After(150 * time.Millisecond):
			}
		}

		// A hostname's address can move. Recheck a bounded batch daily: if the
		// address is still inside the learned prefix the row is only touched, so
		// CDN and AS labels are not blanked by a no-op update.
		refresh, err := st.TargetsNeedingRefresh(ctx, time.Now().Add(-24*time.Hour), 50)
		if err != nil {
			lg.Warn("enrichment refresh scan failed",
				"event", "enrichment_refresh_scan_failed", "component", "enrich", "error", err)
			return
		}
		for _, t := range refresh {
			if ctx.Err() != nil {
				return
			}

			addrs, err := dialer.CheckHost(ctx, t.Host)
			if err != nil {
				lg.Debug("enrichment refresh resolve failed",
					"event", "enrichment_refresh_resolve_failed", "component", "enrich",
					"target", t.Host, "error", err)
				continue
			}
			if enrich.PrefixContains(t.Prefix, addrs[0]) {
				if err := st.TouchEnrichment(ctx, t.ID); err != nil {
					lg.Warn("enrichment touch failed",
						"event", "enrichment_touch_failed", "component", "enrich",
						"target", t.Host, "error", err)
				}
				continue
			}

			res, err := enrich.Host(ctx, resolver, t.Host, addrs)
			if err != nil {
				lg.Debug("enrichment refresh lookup failed",
					"event", "enrichment_refresh_lookup_failed", "component", "enrich",
					"target", t.Host, "error", err)
				continue
			}
			if err := st.UpdateEnrichment(ctx, t.ID, store.Enrichment{
				ASN: res.ASN, Prefix: res.Prefix, CDN: res.CDN,
				ASCountry: res.ASCountry, ASName: res.ASName,
				IXNames: ixFor(ctx, res.ASN),
			}); err != nil {
				lg.Warn("enrichment refresh write failed",
					"event", "enrichment_refresh_write_failed", "component", "enrich",
					"target", t.Host, "error", err)
				continue
			}
			lg.Info("target re-enriched after address change",
				"event", "target_reenriched", "component", "enrich",
				"target", t.Host, "asn", res.ASN, "prefix", res.Prefix,
				"as_name", res.ASName, "as_country", res.ASCountry)

			select {
			case <-ctx.Done():
				return
			case <-time.After(150 * time.Millisecond):
			}
		}
	}

	run()

	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// corridorLoop samples the configured corridors every five minutes
// (DESIGN.md §9). Corridors are configuration-driven: without a `corridors:`
// section nothing runs, so the feature cannot invent endpoints.
//
// A step change opens one event; recovery closes it. Traces are deliberately
// not automated here: a walk touches ~30 third-party routers and the manual
// trace action is the documented way to localise a step.
func corridorLoop(ctx context.Context, st *store.Store, paused func() bool, lg *logging.Logger) {
	provider := func() ([]config.Corridor, error) {
		path := strings.TrimSpace(os.Getenv("NETWEATHER_CONFIG"))
		if path == "" {
			return nil, nil
		}
		f, err := config.Load(path)
		if err != nil {
			return nil, err
		}
		return f.Corridors, nil
	}

	prober := &tcpProber{tcp: probe.NewTCP(netx.New())}

	run := func() {
		if paused != nil && paused() {
			return
		}

		corridors, err := provider()
		if err != nil {
			lg.Warn("corridor config unavailable",
				"event", "corridor_config_failed", "component", "corridor", "error", err)
			return
		}

		stored := make([]store.Corridor, 0, len(corridors))
		for _, c := range corridors {
			stored = append(stored, store.Corridor{ID: c.ID, From: c.From, To: c.To})
		}
		if err := st.UpsertCorridors(ctx, stored); err != nil {
			lg.Warn("corridor sync failed",
				"event", "corridor_sync_failed", "component", "corridor", "error", err)
			return
		}
		if len(corridors) == 0 {
			return
		}

		statuses, err := st.CorridorStatus(ctx)
		if err != nil {
			lg.Warn("corridor status failed",
				"event", "corridor_status_failed", "component", "corridor", "error", err)
			return
		}
		open := make(map[string]store.CorridorEvent, len(statuses))
		for _, status := range statuses {
			if status.Open != nil {
				open[status.Corridor.ID] = *status.Open
			}
		}

		now := time.Now().UTC()
		for _, c := range corridors {
			if ctx.Err() != nil {
				return
			}

			sample := corridor.Measure(ctx, c.Endpoints, prober)
			if err := st.RecordCorridorSample(ctx, store.CorridorSample{
				CorridorID: c.ID, TS: sample.TS, RTTMS: sample.RTTMS,
				Loss: sample.Loss, OK: sample.OK,
			}); err != nil {
				lg.Warn("corridor sample write failed",
					"event", "corridor_sample_failed", "component", "corridor",
					"corridor", c.ID, "error", err)
				continue
			}

			stored, err := st.CorridorSamples(ctx, c.ID, now.Add(-corridor.Window))
			if err != nil {
				lg.Warn("corridor history read failed",
					"event", "corridor_history_failed", "component", "corridor",
					"corridor", c.ID, "error", err)
				continue
			}
			history := make([]corridor.Sample, 0, len(stored))
			for _, s := range stored {
				history = append(history, corridor.Sample{
					TS: s.TS, RTTMS: s.RTTMS, Loss: s.Loss, OK: s.OK,
				})
			}

			verdict := corridor.Evaluate(history)
			openEvent, wasOpen := open[c.ID]

			switch {
			case verdict.Step && !wasOpen:
				event := store.CorridorEvent{
					ID:         fmt.Sprintf("ce-%d-%s", now.UnixMilli(), c.ID),
					CorridorID: c.ID, Kind: verdict.Kind,
					StartedAt: now, Detail: verdict.Detail,
				}
				if err := st.OpenCorridorEvent(ctx, event); err != nil {
					lg.Warn("corridor event write failed",
						"event", "corridor_event_failed", "component", "corridor",
						"corridor", c.ID, "error", err)
					continue
				}
				open[c.ID] = event
				lg.Warn("corridor step change",
					"event", "corridor_step", "component", "corridor",
					"corridor", c.ID, "from", c.From, "to", c.To,
					"kind", verdict.Kind, "detail", verdict.Detail)
			case !verdict.Step && wasOpen:
				if err := st.CloseCorridorEvent(ctx, c.ID, now); err != nil {
					lg.Warn("corridor event close failed",
						"event", "corridor_event_close_failed", "component", "corridor",
						"corridor", c.ID, "error", err)
					continue
				}
				delete(open, c.ID)
				lg.Info("corridor recovered",
					"event", "corridor_recovered", "component", "corridor",
					"corridor", c.ID, "kind", openEvent.Kind)
			}
		}
	}

	run()

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// tcpProber adapts the TCP instrument to the corridor Prober seam, so the
// detection package needs no probe dependency.
type tcpProber struct {
	tcp *probe.TCP
}

func (p *tcpProber) Probe(ctx context.Context, address string) (time.Duration, bool) {
	if p == nil || p.tcp == nil {
		return 0, false
	}
	res := p.tcp.Do(ctx, address)
	if res.Failed() {
		return 0, false
	}
	return res.Latency, true
}

// Tor caps from ACCEPTABLE_USE.md §8: at most 25 targets, at least 2s between
// probes, and a slower interval than the direct vantage.
const (
	defaultTorMax   = 25
	minTorInterval  = 10 * time.Minute
	defaultTorProxy = "127.0.0.1:9050"
	torPace         = 2 * time.Second
	// torDailyMBDefault keeps the corroboration vantage honest: 25 targets at a
	// ten-minute floor is a few hundred KB a day, so 50 MB is generous headroom
	// rather than a target.
	torDailyMBDefault = 50
)

// newTorAgent builds the optional corroboration vantage. It returns nil when
// NETWEATHER_TOR is not "true": Tor is off by default, because it is a
// different trust domain and a different volume budget.
// cymruLookup returns an ASN and country resolver for an egress address. The
// query goes to one well-known resolver over the guarded dialer, so the host's
// resolver is not part of the measurement.
func cymruLookup() func(context.Context, netip.Addr) (int, string) {
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return netx.New().DialContext(ctx, "udp", "1.1.1.1:53")
		},
	}
	return func(ctx context.Context, addr netip.Addr) (int, string) {
		asn, _, err := enrich.ASN(ctx, resolver, addr)
		if err != nil {
			return 0, ""
		}
		_, country, err := enrich.Org(ctx, resolver, asn)
		if err != nil {
			return asn, ""
		}
		return asn, country
	}
}

// torExitCountry verifies a declared exit country through the Tor proxy.
//
// A country-labelled vantage is a country measurement only when the exit is
// actually in that country, so the identity is fetched through the same SOCKS
// hop the probes use, enriched with Cymru, and compared. An unverifiable or
// mismatched exit is an error: a mislabelled country measurement is worse than
// none (ACCEPTABLE_USE.md §8).
func torExitCountry(ctx context.Context, httpProbe *probe.HTTP, lookup func(context.Context, netip.Addr) (int, string), want string) (string, error) {
	want = strings.ToUpper(strings.TrimSpace(want))
	if len(want) != 2 {
		return "", fmt.Errorf("exit country %q is not a two-letter code", want)
	}

	check := selfcheck.EgressIPCheck(httpProbe, "https://1.1.1.1/cdn-cgi/trace", lookup)
	ok, value, detail, err := check.Run(ctx)
	if err != nil {
		return "", fmt.Errorf("tor exit check: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("tor exit identity unavailable: %s", detail)
	}

	// The value is "ip,ASn,country" when enrichment succeeded.
	parts := strings.Split(value, ",")
	if len(parts) < 3 || strings.TrimSpace(parts[2]) == "" {
		return "", errors.New("tor exit country could not be learned")
	}
	if got := strings.ToUpper(strings.TrimSpace(parts[2])); got != want {
		return "", fmt.Errorf("tor exit country is %s, want %s", got, want)
	}
	return value, nil
}

func newTorAgent(ctx context.Context, st *store.Store, direct []agent.Target, lg *logging.Logger) (*agent.Agent, error) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("NETWEATHER_TOR")), "true") {
		return nil, nil
	}

	proxyAddr := strings.TrimSpace(os.Getenv("NETWEATHER_TOR_SOCKS"))
	if proxyAddr == "" {
		proxyAddr = defaultTorProxy
	}
	dial, err := netx.Socks5(proxyAddr)
	if err != nil {
		return nil, err
	}

	// ACCEPTABLE_USE.md §8: the Tor vantage carries a daily volume budget. It
	// is counted on the SOCKS connection itself, not estimated from response
	// sizes, so a large body or a retry cannot slip past it.
	dailyMB := torDailyMBDefault
	if raw := strings.TrimSpace(os.Getenv("NETWEATHER_TOR_DAILY_MB")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("NETWEATHER_TOR_DAILY_MB %q is not a non-negative integer", raw)
		}
		dailyMB = n
	}
	budget := torcap.New(int64(dailyMB) << 20)
	budget.OnExhausted = func(used int64) {
		lg.Warn("tor daily volume budget exhausted; the tor vantage stops until UTC midnight",
			"event", "tor_budget_exhausted", "component", "agent",
			"used_bytes", used, "limit_bytes", budget.Limit, "proxy", proxyAddr)
	}
	torDial := probe.DialFunc(budget.Wrap(dial))

	maxTargets := defaultTorMax
	if raw := strings.TrimSpace(os.Getenv("NETWEATHER_TOR_MAX")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("NETWEATHER_TOR_MAX %q is not a positive integer", raw)
		}
		maxTargets = n
	}

	selected := selectTorTargets(direct, maxTargets)
	if len(selected) == 0 {
		return nil, errors.New("tor vantage enabled but no tcp-based targets are configured")
	}

	// A country label is only honest when the exit is actually there. The
	// identity is fetched through the same SOCKS hop the probes will use, so a
	// mismatched or unverifiable exit refuses to start rather than fabricating
	// a country measurement (ACCEPTABLE_USE.md §8).
	vantageID := "tor"
	if want := strings.TrimSpace(os.Getenv("NETWEATHER_TOR_EXIT_COUNTRY")); want != "" {
		exitProbe := probe.NewHTTPWithDial(torDial)
		value, err := torExitCountry(ctx, exitProbe, cymruLookup(), want)
		if err != nil {
			return nil, err
		}
		vantageID = "tor-" + strings.ToLower(strings.TrimSpace(want))
		lg.Info("tor exit country verified",
			"event", "tor_exit_verified", "component", "agent",
			"exit", value, "country", strings.ToUpper(strings.TrimSpace(want)))
	}
	for i := range selected {
		selected[i].Vantage = vantageID
	}

	if err := st.EnsureVantage(ctx, vantageID, "socks5", "corroboration"); err != nil {
		return nil, err
	}

	runner := agent.Instruments{
		HTTP: probe.NewHTTPWithDial(torDial),
		TLS:  probe.NewTLSWithDial(torDial),
		TCP:  probe.NewTCPWithDial(torDial),
	}
	ag := agent.New(selected, runner, st)
	ag.Logger = lg.Logger
	ag.Pace = torPace
	ag.Suspend = budget.Exhausted

	lg.Info("tor vantage enabled",
		"event", "tor_enabled", "component", "agent",
		"proxy", proxyAddr, "targets", len(selected), "pace", torPace.String())
	return ag, nil
}

// selectTorTargets copies the TCP-based targets under the vantage, sorted by id
// so the selected subset is stable across restarts, and slows each interval to
// the Tor floor.
func selectTorTargets(direct []agent.Target, max int) []agent.Target {
	if max <= 0 {
		max = defaultTorMax
	}

	candidates := make([]agent.Target, 0, len(direct))
	for _, t := range direct {
		switch t.Instrument {
		case "http", "tls", "tcp", "":
			candidates = append(candidates, t)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })

	if len(candidates) > max {
		candidates = candidates[:max]
	}

	out := make([]agent.Target, 0, len(candidates))
	for _, t := range candidates {
		t.Vantage = "tor"
		if t.Interval < minTorInterval {
			t.Interval = minTorInterval
		}
		out = append(out, t)
	}
	return out
}

// buildFeedPollers returns a poller per enabled feed. NETWEATHER_FEEDS is a
// comma-separated list; empty means "no external calls", which is the default
// because a feed is someone else's API and an operator should opt in.
func buildFeedPollers(st *store.Store, lg *logging.Logger) []*feeds.Poller {
	enabled := strings.ToLower(strings.TrimSpace(os.Getenv("NETWEATHER_FEEDS")))
	if enabled == "" || enabled == "none" {
		return nil
	}

	client := &http.Client{
		Transport: probe.Transport(netx.New()),
		Timeout:   20 * time.Second,
	}

	var out []*feeds.Poller
	for _, name := range strings.Split(enabled, ",") {
		switch strings.TrimSpace(name) {
		case "ioda":
			ioda := feeds.NewIODA(client)
			if endpoint := strings.TrimSpace(os.Getenv("NETWEATHER_IODA_URL")); endpoint != "" {
				ioda.Endpoint = endpoint
			}
			out = append(out, &feeds.Poller{
				Feed:   "ioda",
				Source: ioda,
				Sink:   st,
				Every:  10 * time.Minute,
			})
		case "radar":
			token := strings.TrimSpace(os.Getenv("NETWEATHER_RADAR_TOKEN"))
			if token == "" {
				// A feed without its credential is a configuration mistake, not
				// a silent no-op: say so at startup and run without it.
				lg.Warn("radar feed requested without NETWEATHER_RADAR_TOKEN",
					"event", "feed_not_configured", "component", "feeds", "feed", "radar")
				continue
			}
			radar := feeds.NewRadar(client, token)
			if base := strings.TrimSpace(os.Getenv("NETWEATHER_RADAR_URL")); base != "" {
				radar.BaseURL = base
			}
			out = append(out, &feeds.Poller{
				Feed:   "radar",
				Source: radar,
				Sink:   st,
				Every:  10 * time.Minute,
			})
		case "statuspage":
			raw := strings.TrimSpace(os.Getenv("NETWEATHER_STATUSPAGE_URLS"))
			if raw == "" {
				lg.Warn("statuspage feed requested without NETWEATHER_STATUSPAGE_URLS",
					"event", "feed_not_configured", "component", "feeds", "feed", "statuspage")
				continue
			}
			urls := splitList(raw)
			out = append(out, &feeds.Poller{
				Feed:   "statuspage",
				Source: feeds.NewStatuspage(client, urls),
				Sink:   st,
				Every:  5 * time.Minute,
			})
		case "aws-status":
			aws := feeds.NewAWSStatus(client)
			if endpoint := strings.TrimSpace(os.Getenv("NETWEATHER_AWS_STATUS_URL")); endpoint != "" {
				aws.URL = endpoint
			}
			out = append(out, &feeds.Poller{
				Feed:   "aws-status",
				Source: aws,
				Sink:   st,
				Every:  5 * time.Minute,
			})
		case "gcp-status":
			gcp := feeds.NewGCPStatus(client)
			if endpoint := strings.TrimSpace(os.Getenv("NETWEATHER_GCP_STATUS_URL")); endpoint != "" {
				gcp.URL = endpoint
			}
			out = append(out, &feeds.Poller{
				Feed:   "gcp-status",
				Source: gcp,
				Sink:   st,
				Every:  5 * time.Minute,
			})
		case "azure-status":
			azure := feeds.NewAzureStatus(client)
			if endpoint := strings.TrimSpace(os.Getenv("NETWEATHER_AZURE_STATUS_URL")); endpoint != "" {
				azure.URL = endpoint
			}
			out = append(out, &feeds.Poller{
				Feed:   "azure-status",
				Source: azure,
				Sink:   st,
				Every:  5 * time.Minute,
			})
		}
	}
	return out
}

// splitList trims and drops empty entries from a comma-separated operator
// value. A trailing comma is a typo, not a target.
func splitList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if item := strings.TrimSpace(part); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// maintenanceLoop folds closed rollups, applies retention, and runs the
// self-checks. It runs once at startup so a long downtime self-heals, then every
// ten minutes.
func maintenanceLoop(ctx context.Context, st *store.Store, lg *logging.Logger, self *selfcheck.Runner, gate *selfcheck.Gate, scopes *logging.Scopes) {
	runDebugPrune := func() {
		if scopes == nil {
			return
		}
		for _, sc := range scopes.Prune() {
			lg.Info("debug scope expired",
				"event", "debug_scope_expired",
				"component", "logging",
				"scope_id", sc.ID,
				"target", sc.Target,
				"component_scope", sc.Component,
				"level", sc.Level,
			)
		}
	}

	runSelfChecks := func() {
		if self == nil {
			return
		}

		// Read the previous run before recording, so the egress comparison has
		// something to compare against.
		previous := map[string]selfcheck.Result{}
		if rows, err := st.LatestSelfChecks(ctx); err == nil {
			for _, row := range rows {
				previous[row.Check] = selfcheck.Result{
					Check: row.Check, OK: row.OK, Value: row.Value,
				}
			}
		}

		results, err := self.Record(ctx, st)
		if err != nil {
			lg.Warn("self-check record failed",
				"event", "selfcheck_record_failed", "component", "maintenance", "error", err)
		}
		if gate != nil {
			gate.Observe(results)
		}
		for _, res := range results {
			if res.Check == "egress-ip" {
				if changed, detail := selfcheck.VantageChange(previous["egress-ip"], res); changed {
					lg.Warn("vantage changed",
						"event", "vantage_changed", "component", "maintenance", "detail", detail)
				}
			}
			if res.OK {
				lg.Debug("self-check ok",
					"event", "selfcheck", "component", "maintenance",
					"check", res.Check, "value", res.Value, "detail", res.Detail)
				continue
			}
			lg.Warn("self-check failed",
				"event", "selfcheck_failed", "component", "maintenance",
				"check", res.Check, "detail", res.Detail)
		}
	}

	lastCoverageDay := ""
	run := func() {
		runSelfChecks()
		runDebugPrune()

		now := time.Now()
		if day := now.UTC().Format("2006-01-02"); day != lastCoverageDay {
			if err := st.SnapshotCoverage(ctx, "local", now); err != nil {
				lg.Warn("coverage snapshot failed",
					"event", "coverage_snapshot_failed", "component", "maintenance", "error", err)
			} else {
				lastCoverageDay = day
				lg.Info("coverage snapshot written",
					"event", "coverage_snapshot", "component", "maintenance", "day", day)
			}
		}

		if err := st.SampleScopes(ctx, "local", now); err != nil {
			lg.Warn("scope sampling failed",
				"event", "scope_sampling_failed", "component", "maintenance", "error", err)
		}

		folded, err := st.Fold(ctx, time.Now())
		if err != nil {
			lg.Warn("rollup fold failed",
				"event", "rollup_fold_failed", "component", "maintenance", "error", err)
		} else if folded.Hours > 0 || folded.Days > 0 {
			lg.Info("rollups folded",
				"event", "rollup_folded", "component", "maintenance",
				"hours", folded.Hours, "days", folded.Days)
		}

		pruned, err := st.Prune(ctx, time.Now())
		if err != nil {
			lg.Warn("retention prune failed",
				"event", "prune_failed", "component", "maintenance", "error", err)
		} else if pruned.Probes > 0 || pruned.Rollup5m > 0 {
			lg.Info("retention pruned",
				"event", "prune", "component", "maintenance",
				"probes", pruned.Probes,
				"rollup_5m", pruned.Rollup5m,
				"rollup_1h", pruned.Rollup1h,
				"rollup_1d", pruned.Rollup1d)
		}

		// DNS baselines age out on the seven-day window the divergence rule
		// names; an older answer must not be treated as current.
		if dropped, err := st.PruneDNSObservations(ctx, time.Now().Add(-7*24*time.Hour)); err != nil {
			lg.Warn("dns baseline prune failed",
				"event", "dns_prune_failed", "component", "maintenance", "error", err)
		} else if dropped > 0 {
			lg.Info("dns baselines pruned",
				"event", "dns_prune", "component", "maintenance", "rows", dropped)
		}
	}

	run()

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

func probeLoop(ctx context.Context, lg *logging.Logger, ag *agent.Agent, changes *board.Broadcast) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ran, err := ag.Step(ctx)
			if err != nil {
				lg.Warn("probe step failed",
					"event", "probe_step_failed", "component", "agent", "error", err)
				continue
			}
			if ran > 0 {
				changes.Notify()
			}
		}
	}
}
