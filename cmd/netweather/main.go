// Command netweather is the single binary: probe scheduler, classifier, store,
// and dashboard in one process. See docs/DESIGN.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
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
		_, _ = fmt.Fprintln(stdout, version.String())
		return nil

	case "healthcheck":
		return healthcheck()

	case "seed":
		return seedCommand(stdout)

	case "export":
		return exportCommand(args[1:], stdout)

	case "debug":
		return debugCommand(args[1:], stdout)

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

	ag := newAgent(targets, st, lg)

	// The monitor must be able to see before its board is believable, so the
	// self-checks run at startup and with maintenance, not only on request.
	instruments := newInstruments()
	selfRunner := &selfcheck.Runner{Checks: []selfcheck.Check{
		selfcheck.DiskCheck(dataDir),
		selfcheck.DNSCheck(instruments.DNS, "1.1.1.1:53", "example.com"),
		selfcheck.EgressCheck(instruments.TCP, "1.1.1.1:443"),
		selfcheck.ClockCheck(instruments.HTTP, "https://www.cloudflare.com/", time.Minute),
	}}

	// Feeds are opt-in: they are outbound calls to someone else's API, and an
	// operator should choose that rather than discover it.
	feedPollers := buildFeedPollers(st)
	for _, p := range feedPollers {
		go feeds.Loop(ctx, p, lg.Logger)
	}

	var metricsHandler http.Handler
	if os.Getenv("NETWEATHER_METRICS") == "true" {
		metricsHandler = metrics.Handler(st, "local")
	}

	ui, err := web.New(web.Deps{
		Logger:          lg.Logger,
		Source:          source,
		Watcher:         source,
		Token:           cfg.Token,
		SecureCookies:   os.Getenv("NETWEATHER_UI_SECURE_COOKIES") == "true",
		Version:         version.Version,
		StartedAt:       time.Now(),
		History:         st,
		Incidents:       st,
		IncidentWatcher: incidentChanges,
		Coverage:        st,
		CoverageTrend:   st,
		Series:          st,
		Changes:         st,
		Control:         ag,
		Vantage:         "local",
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
		Control:         ag,
		Probes:          st,
		Incidents:       st,
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
	go maintenanceLoop(ctx, st, lg, selfRunner, scopes)
	go configLoop(ctx, st, ag, lg.Logger)
	go enrichmentLoop(ctx, st, lg)

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
			url     string
			address string
		)
		switch t.Probe {
		case "http":
			url = targetURL(t.Host, t.Port)
		case "tls", "tcp":
			address = net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
		case "dns", "ntp":
			address = net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
		default:
			return nil, fmt.Errorf("target %s: probe %q is not implemented yet", t.Host, t.Probe)
		}

		expect, err := t.Expect.Classify()
		if err != nil {
			return nil, fmt.Errorf("target %s: %w", t.Host, err)
		}
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
	return agent.Instruments{
		HTTP: probe.NewHTTP(dialer),
		TLS:  probe.NewTLS(dialer),
		TCP:  probe.NewTCP(dialer),
		DNS:  probe.NewDNS(dialer.DialContext),
		NTP:  probe.NewNTP(dialer),
	}
}

func newAgent(targets []agent.Target, st *store.Store, lg *logging.Logger) *agent.Agent {
	ag := agent.New(targets, newInstruments(), st)
	ag.Logger = lg.Logger
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
			if err := st.UpdateEnrichment(ctx, t.ID, res.ASN, res.Prefix, res.CDN); err != nil {
				lg.Warn("enrichment write failed",
					"event", "enrichment_write_failed", "component", "enrich",
					"target", t.Host, "error", err)
				continue
			}
			lg.Info("target enriched",
				"event", "target_enriched", "component", "enrich",
				"target", t.Host, "asn", res.ASN, "prefix", res.Prefix, "cdn", res.CDN)

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

// Tor caps from ACCEPTABLE_USE.md §8: at most 25 targets, at least 2s between
// probes, and a slower interval than the direct vantage.
const (
	defaultTorMax   = 25
	minTorInterval  = 10 * time.Minute
	defaultTorProxy = "127.0.0.1:9050"
	torPace         = 2 * time.Second
)

// newTorAgent builds the optional corroboration vantage. It returns nil when
// NETWEATHER_TOR is not "true": Tor is off by default, because it is a
// different trust domain and a different volume budget.
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

	if err := st.EnsureVantage(ctx, "tor", "socks5", "corroboration"); err != nil {
		return nil, err
	}

	runner := agent.Instruments{
		HTTP: probe.NewHTTPWithDial(probe.DialFunc(dial)),
		TLS:  probe.NewTLSWithDial(probe.DialFunc(dial)),
		TCP:  probe.NewTCPWithDial(probe.DialFunc(dial)),
	}
	ag := agent.New(selected, runner, st)
	ag.Logger = lg.Logger
	ag.Pace = torPace

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
func buildFeedPollers(st *store.Store) []*feeds.Poller {
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
		}
	}
	return out
}

// maintenanceLoop folds closed rollups, applies retention, and runs the
// self-checks. It runs once at startup so a long downtime self-heals, then every
// ten minutes.
func maintenanceLoop(ctx context.Context, st *store.Store, lg *logging.Logger, self *selfcheck.Runner, scopes *logging.Scopes) {
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
		results, err := self.Record(ctx, st)
		if err != nil {
			lg.Warn("self-check record failed",
				"event", "selfcheck_record_failed", "component", "maintenance", "error", err)
		}
		for _, res := range results {
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
