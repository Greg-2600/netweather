// Command netweather is the single binary: probe scheduler, classifier, store,
// and dashboard in one process. See docs/DESIGN.md.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Greg-2600/netweather/internal/agent"
	"github.com/Greg-2600/netweather/internal/anomaly"
	"github.com/Greg-2600/netweather/internal/api"
	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/bytecap"
	"github.com/Greg-2600/netweather/internal/calendar"
	"github.com/Greg-2600/netweather/internal/classify"
	"github.com/Greg-2600/netweather/internal/config"
	"github.com/Greg-2600/netweather/internal/corridor"
	"github.com/Greg-2600/netweather/internal/enrich"
	"github.com/Greg-2600/netweather/internal/feeds"
	"github.com/Greg-2600/netweather/internal/ihi"
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
	"github.com/Greg-2600/netweather/internal/tracebudget"
	"github.com/Greg-2600/netweather/internal/verdict"
	"github.com/Greg-2600/netweather/internal/version"
	"github.com/Greg-2600/netweather/internal/web"
	"github.com/Greg-2600/netweather/internal/webhook"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "netweather:", err)
		// Usage errors are 2, runtime failures are 1: a wrapper can tell "you
		// invoked it wrong" from "it ran and failed" (API.md §9).
		if errors.Is(err, errUnknownCommand) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// errUnknownCommand marks a usage error rather than a runtime failure, so main
// can exit 2 without string matching.
var errUnknownCommand = errors.New("unknown command")

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
		return seedCommand(args[1:], stdout)

	case "export":
		return exportCommand(args[1:], stdout)

	case "debug":
		return debugCommand(args[1:], stdout)

	case "disable":
		return disableCommand(args[1:], stdout)

	case "doctor":
		return doctorCommand(args[1:], stdout)

	case "backup":
		return backupCommand(args[1:], stdout)

	case "agent":
		return agentCommand(args[1:], stdout)

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
		return fmt.Errorf("%w %q (try: serve, agent, version, healthcheck, seed, export, debug, disable, doctor, backup)",
			errUnknownCommand, command)
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
	if cfg.TokenFromEnv {
		// An inline environment value is readable by anything that can inspect
		// the process environment; a mounted file need not be. Accept it, but
		// say so once (THREAT_MODEL.md T14).
		lg.Warn("UI token came from the environment; a token file or secret mount is preferred",
			"event", "auth_token_env", "component", "startup")
	}

	// Per-client limits and log lines are only meaningful behind a correctly
	// configured proxy. An empty list keeps the peer address, which is the
	// only address a client cannot forge (NETWORKING.md §7.3).
	trustedProxies, err := trustedProxiesFromEnv()
	if err != nil {
		lg.Error("invalid proxy configuration", "event", "config_invalid", "error", err)
		return err
	}
	if len(trustedProxies) > 0 {
		lg.Info("trusting X-Forwarded-For from configured proxies",
			"event", "trusted_proxies", "component", "http", "count", len(trustedProxies))
	}

	// There is no TLS in the binary, so the listener is plaintext even behind a
	// proxy that terminates TLS. Say it once at startup; the board also shows a
	// per-request badge when the browser's own connection is plaintext
	// (NETWORKING.md §7.1, THREAT_MODEL.md T8).
	lg.Warn("dashboard listener is plaintext HTTP; terminate TLS at a reverse proxy for anything beyond a trusted network",
		"event", "plaintext_transport", "component", "http", "listen", cfg.Listen)

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

	dbPath := filepath.Join(dataDir, "netweather.db")
	// A non-empty WAL is normal after an unclean shutdown and SQLite recovers
	// it. After a restore, the -wal and -shm siblings belong to the replaced
	// file and must be removed first (SCHEMA.md §14). The binary cannot tell a
	// foreign WAL from a crashed one, so it states what it sees rather than
	// claiming to detect the difference.
	if size, ok := staleWAL(dbPath); ok {
		lg.Info("database has a write-ahead log at startup; SQLite will recover it. After restoring a backup, delete the -wal and -shm siblings first",
			"event", "db_wal_present", "component", "store", "wal_bytes", size)
	}

	st, err := store.Open(ctx, dbPath)
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
	// Remote vantages are registered up front so an agent reporting under one
	// of their ids is accepted. They are corroborating by default: only the
	// primary vantage may declare DOWN (DESIGN.md 7.1).
	for _, id := range splitList(os.Getenv("NETWEATHER_REMOTE_VANTAGES")) {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if err := st.EnsureVantage(ctx, id, "socks5", "corroboration"); err != nil {
			return err
		}
	}

	// One INFO line per confirmed state change (LOGGING.md §3, §4). The store
	// returns the transition after the batch commits rather than logging from
	// inside its transaction: a rolled-back batch must not leave a line claiming
	// a change, and the store stays a pure data layer.
	st.OnTransition = func(t store.Transition) {
		lg.Info("state change",
			"event", "state_change",
			"component", "store",
			"target", t.Host,
			"vantage", t.VantageID,
			"tier", t.Tier,
			"state_from", string(t.From),
			"state_to", string(t.To),
			"stage", t.Stage,
			"error_class", t.ErrorClass,
			"http_status", t.HTTPStatus,
			"latency_ms", t.LatencyMS,
			"confidence", t.Confidence,
			"reason", t.Reason,
		)
	}

	// The atypical-day calendar is local and static (DESIGN.md §7.2). Every
	// listed date is marked once at startup: a future date costs no delete
	// now and is skipped when it arrives, and a past date loses the samples
	// it should never have contributed.
	if path := calendarPathFromEnv(); path != "" {
		entries, err := calendar.Load(path)
		if err != nil {
			lg.Error("calendar invalid", "event", "calendar_invalid", "component", "maintenance", "error", err)
			return err
		}
		for _, entry := range entries {
			for _, scope := range entry.Scopes() {
				if _, err := st.MarkAtypicalDay(ctx, entry.Date, scope[0], scope[1], "calendar", time.Now()); err != nil {
					lg.Warn("calendar day could not be marked",
						"event", "calendar_mark_failed", "component", "maintenance",
						"day", entry.Date, "error", err)
				}
			}
		}
		if len(entries) > 0 {
			lg.Info("atypical-day calendar loaded",
				"event", "calendar_loaded", "component", "maintenance",
				"days", len(entries), "path", path)
		}
	}

	targets, err := loadTargets(ctx, st)
	if err != nil {
		return err
	}

	// Identification: a probe must be attributable. The contact is the
	// operator's to set, and an unattributed monitor is indistinguishable from
	// a scanner (ACCEPTABLE_USE.md §7). The placeholder is kept, visibly, and
	// the warning is at startup so it cannot be missed.
	contact := strings.TrimSpace(os.Getenv("NETWEATHER_CONTACT"))
	if contact == "" {
		lg.Warn("no operator contact configured; probes identify as an unattributed client",
			"event", "contact_missing", "component", "startup",
			"detail", "set NETWEATHER_CONTACT so an abuse desk can tell the monitor from a scanner")
	}
	userAgent := probe.BuildUserAgent(version.Version, contact)

	torAgents, torBudgets, torSpecs, err := newTorAgents(ctx, st, targets, lg, userAgent)
	if err != nil {
		lg.Error("tor vantages unavailable", "event", "tor_unavailable", "error", err)
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

	// The daily egress budget is the backstop for every rate above it
	// (ACCEPTABLE_USE.md §4): a misconfiguration stops at a byte count, not at
	// the operator's bandwidth bill or a target's abuse desk. The direct
	// vantage gets the large allowance; the Tor vantage has its own small one.
	directDialer := netx.New()
	directBudget, err := newDirectBudget(lg)
	if err != nil {
		return err
	}
	if directBudget != nil {
		directDialer.Dial = directBudget.Wrap(directDialer.Dial)
	}

	ag := newAgentWith(targets, st, lg, directDialer, userAgent)
	ag.DNSBlocked = func() bool { return healthGate.Blocked("port53") }
	// The same gate, for the other way a working path can look dead: a low
	// egress MTU black-holes large handshakes (DESIGN.md §7.5).
	ag.MTUBlocked = func() bool { return healthGate.Blocked("mtu") }
	if directBudget != nil {
		ag.Suspend = directBudget.Exhausted
	}

	// The global request ceiling from DESIGN.md §6.2: 20 req/s by default and
	// settable with NETWEATHER_RATE_LIMIT, ramping from 2 req/s over the first
	// ten minutes. The ramp is measured from the database's first target so a
	// restart resumes at the current rate instead of repeating it
	// (DESIGN.md §6.5). A ceiling below the 2 req/s start simply starts at the
	// ceiling.
	rateCeiling, err := rateLimitFromEnv()
	if err != nil {
		return err
	}
	ag.RateCeiling = rateCeiling
	ag.RateStart = 2
	if rateCeiling < ag.RateStart {
		ag.RateStart = rateCeiling
	}
	ag.RateRamp = 10 * time.Minute
	if coldStart, err := st.ColdStartAt(ctx); err == nil && !coldStart.IsZero() {
		ag.RateSince = coldStart
	} else {
		ag.RateSince = time.Now()
	}

	// The monitor must be able to see before its board is believable, so the
	// self-checks run at startup and with maintenance, not only on request.
	// They share the agent's dialer, so their traffic counts against the same
	// budget.
	instruments := newInstrumentsWithDialer(directDialer, userAgent)
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
		// The warning threshold is two seconds (DESIGN.md §7.5); the gate
		// below suppresses correlation above ten.
		selfcheck.ClockCheck(instruments.HTTP, "https://www.cloudflare.com/", 2*time.Second),
		selfcheck.ClockSyncCheck(instruments.HTTP, "https://www.cloudflare.com/"),
	}}

	// The host resolver set is a vantage fact: a DHCP lease that changes it
	// changes every answer without changing anything this monitor measures
	// (DESIGN.md §7.5). resolv.conf is the Linux/Unix form; on a platform
	// without it the check is omitted rather than reported as a failure.
	if _, err := os.Stat("/etc/resolv.conf"); err == nil {
		selfRunner.Checks = append(selfRunner.Checks,
			selfcheck.ResolverIdentityCheck(func() ([]string, error) {
				return selfcheck.ResolverServers("/etc/resolv.conf")
			}))
	}

	// Feeds are opt-in: they are outbound calls to someone else's API, and an
	// operator should choose that rather than discover it.
	feedPollers := buildFeedPollers(st, lg)
	for _, p := range feedPollers {
		go feeds.Loop(ctx, p, lg.Logger)
	}
	// Level feeds are separate bounded polls: their snapshots are read by the
	// world view, which never calls an upstream on a page view.
	for _, p := range buildLevelPollers(st, lg) {
		go feeds.LevelLoop(ctx, p, lg.Logger)
	}
	// RIS Live can also be read as a real-time stream. It is opt-in and additive
	// to the RIPEstat poller: a persistent WebSocket is a different network
	// commitment from a bounded REST read.
	for _, s := range buildRISStreams(st, lg) {
		go feeds.StreamLoop(ctx, s, lg.Logger)
	}

	var metricsHandler http.Handler
	if os.Getenv("NETWEATHER_METRICS") == "true" {
		metricsHandler = metrics.Handler(st, "local")
	}

	// The Internet Health Index is computed per request from live state, so it
	// cannot go stale between ticks (DESIGN.md §11).
	health := &ihi.Builder{Store: st, Vantage: "local"}

	// The global verdict is the same idea in one sentence: read the signals the
	// page already reads and say which one wins (DESIGN.md §3.3).
	globalVerdict := &verdict.Builder{Store: st, Vantage: "local", StartedAt: time.Now()}

	// The last config-reload outcome is shown on the board: a rejected edit
	// keeps the old target set live, and it must not vanish into the log
	// (THREAT_MODEL.md T36).
	reloadState := &config.ReloadState{}

	// Traceroute is the most visible thing this binary can do, so the
	// capability is opt-in and the budget is per instance. The API and the
	// drawer spend from this one governor, which is what makes the documented
	// ten-per-hour a property of the process rather than of each surface
	// (THREAT_MODEL.md T29, DESIGN.md §9.1).
	traceEnabled := strings.EqualFold(strings.TrimSpace(os.Getenv("NETWEATHER_TRACE_ENABLED")), "true")
	if traceEnabled {
		lg.Info("traceroute enabled", "event", "trace_enabled", "component", "startup")
	}
	traceBudget := tracebudget.New(tracebudget.DefaultLimit, tracebudget.DefaultWindow)
	tracer := probe.NewTrace()
	traceResolver := netx.New()

	webDeps := web.Deps{
		Logger:              lg.Logger,
		Source:              source,
		Watcher:             source,
		Token:               cfg.Token,
		SecureCookies:       os.Getenv("NETWEATHER_UI_SECURE_COOKIES") == "true",
		TrustedProxies:      trustedProxies,
		Version:             version.Version,
		StartedAt:           time.Now(),
		History:             st,
		Incidents:           st,
		IncidentProbes:      st,
		Corroboration:       st,
		Feeds:               st,
		TargetCorroboration: st,
		Provider:            st,
		CountryEvents:       st,
		CountryLevels:       st,
		Corridors:           st,
		DNSObservations:     st,
		Health:              health,
		Verdict:             globalVerdict,
		Anomalies:           st,
		Vantages:            st,
		Provenance:          st,
		Fleet:               st,
		VantageChanges:      st,
		ConfigReload:        reloadState,
		TraceEnabled:        traceEnabled,
		Tracer:              tracer,
		Resolver:            traceResolver,
		TraceBudget:         traceBudget,
		// The ring and the measured logging setup back the diagnostics page
		// and the drawer's Why panel (LOGGING.md §9).
		Logs:            lg,
		IncidentWatcher: incidentChanges,
		Coverage:        st,
		CoverageTrend:   st,
		Series:          st,
		SeriesBatch:     st,
		Changes:         st,
		Control:         ag,
		Vantage:         "local",
	}
	if directBudget != nil {
		// A typed nil in the interface would look configured and panic on the
		// first method call, so the field is set only when a budget exists.
		webDeps.Budget = directBudget
	}
	if len(torBudgets) > 0 {
		// The Tor vantages' combined allowance is shown beside the direct one
		// so a paused corroboration source is visible rather than inferred
		// from missing samples (DESIGN.md §5.7). Each exit keeps its own cap;
		// this is only the display total.
		webDeps.TorBudget = combinedBudget(torBudgets)
	}
	ui, err := web.New(webDeps)
	if err != nil {
		return err
	}

	// The applied migration number is not secret and makes a support bundle
	// useful without database access; unknown is reported as absent.
	schemaVersion, _ := st.SchemaVersion(ctx)

	// Stopping one host is the abuse-response path (ACCEPTABLE_USE.md §7).
	// The row and its history stay; the live schedulers drop it immediately,
	// and a config reload can bring it back.
	disableTarget := func(ctx context.Context, id string) (bool, error) {
		exists, err := st.TargetExists(ctx, id)
		if err != nil || !exists {
			return exists, err
		}
		if err := st.DisableTarget(ctx, id); err != nil {
			return false, err
		}
		removed := ag.RemoveTarget(id)
		for _, torAgent := range torAgents {
			torAgent.RemoveTarget(id)
		}
		changes.Notify()
		lg.Warn("target disabled at runtime",
			"event", "target_disabled", "component", "api",
			"target_id", id, "removed_from_agent", removed,
			"request_id", logging.RequestIDFrom(ctx))
		return true, nil
	}
	webDeps.TargetControl = web.TargetControlFunc(disableTarget)

	apiDeps := api.Deps{
		Logger:          lg.Logger,
		Ring:            lg.Ring,
		Version:         version.Version,
		Commit:          version.Commit,
		StartedAt:       time.Now(),
		SchemaVersion:   schemaVersion,
		Source:          source,
		Watcher:         changes,
		RunContext:      ctx,
		TrustedProxies:  trustedProxies,
		Control:         ag,
		TargetControl:   targetDisabler(disableTarget),
		ProbeRecorder:   st,
		Probes:          st,
		Incidents:       st,
		IncidentProbes:  st,
		Corroboration:   st,
		Corridors:       st,
		IHI:             health,
		Verdict:         globalVerdict,
		Scopes:          st,
		Anomalies:       st,
		Vantages:        st,
		Targets:         st,
		Provenance:      st,
		Coverage:        st,
		CoverageHistory: st,
		SelfChecks:      st,
		RedactEgress:    strings.EqualFold(strings.TrimSpace(os.Getenv("NETWEATHER_REDACT_EGRESS")), "true"),
		Exporter:        st,
		Feeds:           st,
		Debug:           scopes,
		IncidentWatcher: incidentChanges,
		Tracer:          tracer,
		TraceEnabled:    traceEnabled,
		Resolver:        traceResolver,
		TraceBudget:     traceBudget,
		Metrics:         metricsHandler,
		Vantage:         "local",
		Fallback:        ui,
	}
	if directBudget != nil {
		apiDeps.Budget = directBudget
	}
	handler, err := api.New(cfg, apiDeps)
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
	for _, torAgent := range torAgents {
		go probeLoop(ctx, lg, torAgent, changes)
	}

	incidentInterval := 5 * time.Minute
	if v := os.Getenv("NETWEATHER_INCIDENT_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			incidentInterval = d
		}
	}
	webhookNotifier := webhook.New(os.Getenv("NETWEATHER_WEBHOOK_URL"), os.Getenv("NETWEATHER_WEBHOOK_SECRET"), lg.Logger)
	incSink := incidents.Sink(st)
	incRecon := incidents.Reconciler(st)
	if webhookNotifier != nil {
		incSink = webhook.WrapSink(st, webhookNotifier)
		incRecon = webhook.WrapReconciler(st, webhookNotifier)
	}
	go incidents.Loop(ctx, st, incSink, incRecon, "local", incidentInterval, lg.Logger, incidentChanges.Notify,
		func() bool { return healthGate.Blocked("clock-sync") })
	go anomaly.Loop(ctx, st, st, "local", incidentInterval, lg.Logger, func(day string) {
		// The both-direction rule marked the day atypical. Its samples are
		// already in scope_sample, so delete them as well as skipping the rest
		// (DESIGN.md §7.2); otherwise the band keeps the morning it learned.
		if _, err := st.MarkAtypicalDay(ctx, day, "", "", "detector", time.Now()); err != nil {
			lg.Warn("atypical day could not be excluded",
				"event", "atypical_day_failed", "component", "anomaly", "day", day, "error", err)
			return
		}
		lg.Info("atypical day marked; its samples are excluded from the baseline",
			"event", "atypical_day", "component", "anomaly", "day", day)
	})
	go maintenanceLoop(ctx, st, lg, selfRunner, healthGate, scopes)
	go configLoop(ctx, st, ag, lg.Logger, reloadState)
	go enrichmentLoop(ctx, st, lg)
	go corridorLoop(ctx, st, ag.Paused, lg, corridorTraceStep(lg))

	go func() {
		<-ctx.Done()
		// Shutdown needs its own deadline, but must inherit the parent's values
		// while ignoring its cancellation (which is the reason we are here).
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	// The startup identity is split across two lines by necessity: the first,
	// before wiring, says which build and sinks are running; this one, when the
	// socket is about to open, says what it will serve and from where
	// (LOGGING.md §4).
	configSource := strings.TrimSpace(os.Getenv("NETWEATHER_CONFIG"))
	if configSource == "" {
		configSource = "builtin seed"
	}
	vantages := []string{"local"}
	for _, spec := range torSpecs {
		vantages = append(vantages, spec.ID)
	}
	lg.Info("listening",
		"event", "startup",
		"addr", cfg.Listen,
		"auth", cfg.AuthMode(),
		"targets", len(targets),
		"db", st.Path(),
		"schema_version", schemaVersion,
		"config", configSource,
		"vantages", vantages,
	)

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}

	lg.Info("stopped", "event", "shutdown")
	return nil
}

// loadTargets prefers NETWEATHER_CONFIG, then the shipped seed list. Each
// instrument has its own branch; a probe this build does not dispatch fails
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

	// Provenance for the whole file or directory (THREAT_MODEL.md T35). One
	// hash for the payload, recorded per target so a poisoned list can be
	// traced back to the input that produced it.
	source := "config:" + path
	payloadSHA, err := configPayloadSHA(path)
	if err != nil {
		return nil, fmt.Errorf("config payload hash: %w", err)
	}
	loadedAt := time.Now().UTC()

	out := make([]agent.Target, 0, len(f.Targets))
	for _, t := range f.Targets {
		var (
			url       string
			address   string
			resolvers []string
		)
		switch t.Probe {
		case "http":
			url = targetURL(t.Host, t.Port, t.Path)
		case "doh":
			url = dohEndpoint(t.Host, t.Port, t.URL)
		case "tls", "tcp", "tcpping", "dns", "ntp", "dot", "quic":
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
			Query:     t.QName,
			Name:      t.Name,
			Class:     t.Class,
			Sector:    t.Sector,
			Country:   t.Country,
			StatusURL: t.StatusURL,
			Tier:      t.Tier,
			IntervalS: t.IntervalS,
		}); err != nil {
			return nil, err
		}
		if err := st.RecordTargetSource(ctx, t.ID, source, payloadSHA, loadedAt); err != nil {
			return nil, err
		}

		out = append(out, agent.Target{
			ID:           t.ID,
			Name:         t.Name,
			Host:         t.Host,
			URL:          url,
			Address:      address,
			Query:        t.QName,
			Resolvers:    resolvers,
			Instrument:   t.Probe,
			Vantage:      "local",
			Tier:         schedule.Tier(t.Tier),
			Interval:     t.Interval(),
			Expect:       expect,
			MaxRedirects: expect.MaxRedirects,
			ICMPAnchor:   t.ICMPAnchor,
			Family:       t.Family,
		})
	}
	return out, nil
}

func targetURL(host string, port int, path string) string {
	scheme := "http"
	if port == 443 {
		scheme = "https"
	}
	if path == "" {
		path = "/"
	}
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(port)) + path
}

// dohEndpoint is the DoH JSON endpoint for a target. An operator can name it
// explicitly because providers disagree on the path (Google uses /resolve,
// Cloudflare /dns-query); otherwise the common shape is used.
func dohEndpoint(host string, port int, explicit string) string {
	if explicit != "" {
		return explicit
	}
	return "https://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/dns-query"
}

// seedTargets reconciles the shipped list into the store and returns the agent's
// view. NETWEATHER_SEED=none disables seeding (tests, air-gapped installs).
func seedTargets(ctx context.Context, st *store.Store) ([]agent.Target, error) {
	if os.Getenv("NETWEATHER_SEED") == "none" {
		return nil, nil
	}
	entries := seed.Default()
	return applySeedEntries(ctx, st, entries, "seed:builtin", seedPayloadSHA(entries))
}

// payloadSHA is the provenance hash for a target payload. It is a hex SHA-256
// of canonical text, never of a map, so the same input always hashes the same
// (THREAT_MODEL.md T35).
func payloadSHA(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// seedPayloadSHA hashes the starter entries in their declared order.
func seedPayloadSHA(entries []seed.Entry) string {
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%s|%s|%d|%s|%s|%s|%d\n",
			e.ID, e.Host, e.Port, e.Probe, e.Class, e.Sector, e.Tier)
	}
	return payloadSHA(b.String())
}

// configPayloadSHA hashes a target file, or a directory's files in a stable
// order with their relative paths, so an edit to any sites file is visible in
// the provenance row.
func configPayloadSHA(path string) (string, error) {
	info, err := os.Stat(path) // #nosec G703 -- operator-supplied NETWEATHER_CONFIG.
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if !info.IsDir() {
		raw, err := os.ReadFile(path) // #nosec G304 G703 -- operator-supplied NETWEATHER_CONFIG.
		if err != nil {
			return "", err
		}
		h.Write(raw)
		return hex.EncodeToString(h.Sum(nil)), nil
	}

	var files []string
	err = filepath.WalkDir(path, func(p string, entry os.DirEntry, err error) error { // #nosec G703 -- operator-supplied NETWEATHER_CONFIG.
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(path, p)
		if err != nil {
			return err
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	for _, rel := range files {
		raw, err := os.ReadFile(filepath.Join(path, rel)) // #nosec G304 G703 -- files discovered under NETWEATHER_CONFIG.
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00", rel, len(raw))
		h.Write(raw)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// applySeedEntries reconciles starter entries into the store and returns the
// agent's view. It is shared by the shipped list and the generated ccTLD
// anchors, so both go through one path: an entry that names a probe and a port
// is stored with them, and an entry that names neither keeps the shipped
// HTTP-on-443 default.
func applySeedEntries(ctx context.Context, st *store.Store, entries []seed.Entry, source, payload string) ([]agent.Target, error) {
	if len(entries) > config.MaxTargets {
		return nil, fmt.Errorf("seed: %d entries exceeds the %d target limit", len(entries), config.MaxTargets)
	}
	// A seed is untrusted input (THREAT_MODEL.md T33): the ccTLD anchors come
	// from a fetched root zone, and a malformed entry must fail the apply
	// rather than become a probe.
	if err := seed.Validate(entries); err != nil {
		return nil, err
	}
	loadedAt := time.Now().UTC()
	out := make([]agent.Target, 0, len(entries))
	for _, e := range entries {
		interval := schedule.DefaultInterval(e.Tier)

		port := e.Port
		if port == 0 {
			port = 443
		}
		probe := e.Probe
		if probe == "" {
			probe = "http"
		}

		var (
			url       string
			address   string
			resolvers []string
		)
		switch probe {
		case "http":
			url = e.URL
			if url == "" {
				url = targetURL(e.Host, port, "")
			}
		case "doh":
			url = dohEndpoint(e.Host, port, e.URL)
		case "tls", "tcp", "tcpping", "dns", "ntp", "dot", "quic":
			address = net.JoinHostPort(e.Host, strconv.Itoa(port))
		case "icmp":
			address = e.Host
		default:
			return nil, fmt.Errorf("seed target %s: probe %q is not implemented yet", e.Host, probe)
		}
		if probe == "dns" {
			// The target server is the first resolver; a second entry enables
			// the divergence check, exactly as a configured target does.
			resolvers = append(resolvers, address)
		}

		// A generated HTTP target has no content assertion, so it is probed
		// for status and headers and a 206 is a success. The shipped seed
		// entries and the ccTLD anchors both come through here.
		expect := classify.Expected{}
		if probe == "http" {
			expect.PartialOK = true
		}

		if err := st.UpsertTarget(ctx, store.Target{
			ID:        e.ID,
			Host:      e.Host,
			Port:      port,
			Probe:     probe,
			Query:     e.Query,
			Name:      e.Name,
			Class:     e.Class,
			Sector:    e.Sector,
			Tier:      int(e.Tier),
			IntervalS: int(interval / time.Second),
		}); err != nil {
			return nil, err
		}
		if err := st.RecordTargetSource(ctx, e.ID, source, payload, loadedAt); err != nil {
			return nil, err
		}

		out = append(out, agent.Target{
			ID:         e.ID,
			Name:       e.Name,
			Host:       e.Host,
			URL:        url,
			Address:    address,
			Query:      e.Query,
			Resolvers:  resolvers,
			Instrument: probe,
			Vantage:    "local",
			Tier:       e.Tier,
			Interval:   interval,
			Expect:     expect,
		})
	}
	return out, nil
}

// seedCommand loads NETWEATHER_CONFIG into the database without starting a
// server. It is the populate-and-exit path for images, CI and first installs.
//
// --country-anchors is the documented ccTLD seed (DESIGN.md §4.2): one fetch of
// the IANA root zone, parsed into one DNS target per country and territory.
// The fetch is explicit rather than automatic at startup, because it is an
// outbound request to a third party and belongs in an operator-visible action.
func seedCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	countryAnchors := fs.Bool("country-anchors", false,
		"fetch the IANA root zone and add one DNS target per ccTLD")
	diff := fs.Bool("diff", false,
		"show what would be added or changed without writing anything")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	if len(fs.Args()) > 0 {
		return fmt.Errorf("seed: unexpected argument %q", fs.Args()[0])
	}

	path := strings.TrimSpace(os.Getenv("NETWEATHER_CONFIG"))
	if path == "" && !*countryAnchors && !*diff {
		return errors.New("seed: NETWEATHER_CONFIG must point at a targets file, or pass --country-anchors")
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

	// --country-anchors needs one fetch either way; a diff and an apply should
	// compare and write the same set. The zone hash travels with the entries so
	// the store records which root-zone payload produced them.
	var anchors []seed.Entry
	var anchorsSHA string
	if *countryAnchors {
		fetchCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()

		zone, err := fetchRootZone(fetchCtx, rootZoneURL())
		if err != nil {
			return fmt.Errorf("seed: country anchors: %w", err)
		}
		anchors = seed.ParseCountryAnchors(zone)
		if len(anchors) == 0 {
			return errors.New("seed: country anchors: the root zone contained no ccTLD delegations")
		}
		anchorsSHA = payloadSHA(zone)
	}

	if *diff {
		planned, err := plannedSeedTargets(path, anchors)
		if err != nil {
			return fmt.Errorf("seed: diff: %w", err)
		}
		current, err := st.TargetsWithState(ctx, "local")
		if err != nil {
			return err
		}
		printSeedDiff(out, planned, current)
		return nil
	}

	if err := st.EnsureVantage(ctx, "local", "direct", "primary"); err != nil {
		return err
	}

	total := 0
	if path != "" {
		targets, err := loadTargets(ctx, st)
		if err != nil {
			return err
		}
		total += len(targets)
	}

	if *countryAnchors {
		targets, err := applySeedEntries(ctx, st, anchors, "seed:root-zone", anchorsSHA)
		if err != nil {
			return err
		}
		total += len(targets)
		if _, err := fmt.Fprintf(out, "added %d ccTLD anchors\n", len(targets)); err != nil {
			return err
		}
	}

	_, err = fmt.Fprintf(out, "seeded %d targets into %s\n", total, st.Path())
	return err
}

// plannedTarget is the identity a seed diff compares. Name and sector are
// deliberately omitted: they can change without changing what is probed.
type plannedTarget struct {
	ID    string
	Host  string
	Probe string
	Port  int
}

// plannedSeedTargets returns what a seed run would apply, without applying it.
// A config file is parsed the same way the apply path parses it; without one,
// the shipped list is the plan. Country anchors arrive already fetched so a
// diff and an apply compare and write the same set.
func plannedSeedTargets(path string, anchors []seed.Entry) ([]plannedTarget, error) {
	var entries []seed.Entry
	if path != "" {
		f, err := config.Load(path)
		if err != nil {
			return nil, err
		}
		for _, t := range f.Targets {
			entries = append(entries, seed.Entry{
				ID: t.ID, Host: t.Host, Port: t.Port, Probe: t.Probe,
				Tier: schedule.Tier(t.Tier),
			})
		}
	} else {
		entries = seed.Default()
	}
	entries = append(entries, anchors...)

	out := make([]plannedTarget, 0, len(entries))
	for _, e := range entries {
		port := e.Port
		if port == 0 {
			port = 443
		}
		probe := e.Probe
		if probe == "" {
			probe = "http"
		}
		out = append(out, plannedTarget{ID: e.ID, Host: e.Host, Probe: probe, Port: port})
	}
	return out, nil
}

// printSeedDiff is the review step THREAT_MODEL.md T35 promises: an operator
// can see what a poisoned seed would add before it reaches the scheduler.
func printSeedDiff(out io.Writer, planned []plannedTarget, current []store.TargetRow) {
	have := make(map[string]plannedTarget, len(current))
	for _, row := range current {
		have[row.ID] = plannedTarget{ID: row.ID, Host: row.Host, Probe: row.Probe, Port: row.Port}
	}

	added, changed := 0, 0
	for _, p := range planned {
		cur, ok := have[p.ID]
		switch {
		case !ok:
			added++
			_, _ = fmt.Fprintf(out, "+ %s %s %s:%d\n", p.ID, p.Host, p.Probe, p.Port)
		case cur != p:
			changed++
			_, _ = fmt.Fprintf(out, "~ %s %s %s:%d (was %s %s:%d)\n",
				p.ID, p.Host, p.Probe, p.Port, cur.Host, cur.Probe, cur.Port)
		}
	}
	_, _ = fmt.Fprintf(out, "%d addition(s), %d change(s); no targets were written\n", added, changed)
}

// rootZoneURL is the one host the country-anchor seed fetches. The override
// exists for tests and for an operator behind a mirror; the default is IANA's
// published zone file.
func rootZoneURL() string {
	if v := strings.TrimSpace(os.Getenv("NETWEATHER_ROOT_ZONE_URL")); v != "" {
		return v
	}
	return seed.RootZoneURL
}

// rootZoneClient builds the HTTP client for the fetch. It is a variable so a
// test can trust its own TLS server; production keeps the timeout and the
// redirect policy that refuses a hop to plaintext.
var rootZoneClient = func() *http.Client {
	return &http.Client{Timeout: 60 * time.Second, CheckRedirect: rootZoneRedirectPolicy}
}

// maxRootZoneBytes bounds the seed payload. The root zone is a couple of
// megabytes; 8 MiB is generous, and a larger payload is refused rather than
// truncated into a silent partial seed.
const maxRootZoneBytes int64 = 8 << 20

// rootZoneRedirectPolicy revalidates every redirect hop. T40 requires the seed
// over HTTPS, and a redirect is just another request: following one to plain
// HTTP would undo the rule on the second hop.
func rootZoneRedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return fmt.Errorf("root zone fetch: too many redirects")
	}
	if !strings.EqualFold(req.URL.Scheme, "https") {
		return fmt.Errorf("root zone fetch: redirect to %q refused, HTTPS is required", req.URL.Redacted())
	}
	return nil
}

// fetchRootZone reads the zone from HTTPS, or from a local file:// mirror. The
// two schemes are the documented ones (THREAT_MODEL.md T40): anything else,
// including plaintext HTTP and a file on another host, is refused. The payload
// is capped and an oversized one is an error, not a truncated seed.
func fetchRootZone(ctx context.Context, rawURL string) (string, error) {
	u, err := neturl.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("root zone URL %q: %w", rawURL, err)
	}
	if u.User != nil {
		// A credential in the URL ends up in logs and in the request's own
		// error strings; the fetch has no use for one.
		return "", fmt.Errorf("root zone URL %q carries userinfo; remove it", rawURL)
	}

	switch strings.ToLower(u.Scheme) {
	case "file":
		if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
			return "", fmt.Errorf("root zone URL %q must name a local file (file:///path)", rawURL)
		}
		path := u.Path
		// A Windows file URL looks like file:///C:/path; url.Parse keeps the
		// leading slash, and os.Open does not want it.
		if runtime.GOOS == "windows" && len(path) >= 3 && path[0] == '/' && path[2] == ':' {
			path = path[1:]
		}
		f, err := os.Open(filepath.FromSlash(path)) // #nosec G304 -- operator-supplied NETWEATHER_ROOT_ZONE_URL.
		if err != nil {
			return "", fmt.Errorf("root zone file: %w", err)
		}
		defer func() { _ = f.Close() }()
		body, err := readRootZone(f)
		if err != nil {
			return "", err
		}
		return string(body), nil

	case "https":
		if u.Host == "" {
			return "", fmt.Errorf("root zone URL %q has no host", rawURL)
		}

	default:
		return "", fmt.Errorf("root zone URL %q must be https, or file:// for a local mirror (THREAT_MODEL.md T40)", rawURL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent",
		probe.BuildUserAgent(version.Version, os.Getenv("NETWEATHER_CONTACT")))

	client := rootZoneClient()
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s returned %s", u.Redacted(), resp.Status)
	}

	body, err := readRootZone(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// readRootZone enforces the cap. Reading limit+1 is what distinguishes "exactly
// at the cap" from "larger": a bare LimitReader would silently return a
// truncated zone, and a truncated zone parses into a smaller anchor list that
// looks like success.
func readRootZone(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxRootZoneBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxRootZoneBytes {
		return nil, fmt.Errorf("root zone is larger than %d bytes", maxRootZoneBytes)
	}
	return body, nil
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
	return newInstrumentsWithDialer(netx.New(),
		probe.BuildUserAgent(version.Version, os.Getenv("NETWEATHER_CONTACT")))
}

// newInstrumentsWithDialer builds the instruments over one guarded dialer, so
// the agent and the self-checks share the same egress path and the same byte
// budget.
func newInstrumentsWithDialer(dialer *netx.Dialer, userAgent string) agent.Instruments {
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

	httpProbe := probe.NewHTTP(dialer)
	httpProbe.UserAgent = userAgent

	dohProbe := probe.NewDoH(dialer)
	dohProbe.UserAgent = userAgent

	dotProbe := probe.NewDoT(dialer.DialContext)

	quicProbe := probe.NewQUIC(dialer.DialContext)

	return agent.Instruments{
		HTTP: httpProbe,
		TLS:  probe.NewTLS(dialer),
		TCP:  probe.NewTCP(dialer),
		DNS:  probe.NewDNS(dialer.DialContext),
		DoH:  dohProbe,
		DoT:  dotProbe,
		QUIC: quicProbe,
		NTP:  probe.NewNTP(dialer),
		ICMP: ping,
	}
}

// newAgentWith builds the direct agent over the given dialer, which run()
// wraps in the daily egress budget (ACCEPTABLE_USE.md 4).
func newAgentWith(targets []agent.Target, st *store.Store, lg *logging.Logger,
	dialer *netx.Dialer, userAgent string,
) *agent.Agent {
	instruments := newInstrumentsWithDialer(dialer, userAgent)
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
	// Origin ASNs for new DNS answers, so a CDN rotation inside a known
	// network is not treated as new hosting (CLASSIFIER.md §3.2).
	asnResolver := cymruResolver()
	ag.LookupASN = func(ctx context.Context, addr netip.Addr) (int, error) {
		asn, _, err := enrich.ASN(ctx, asnResolver, addr)
		return asn, err
	}
	// The parent delegation for the same reason: a changed NS set is the
	// strongest divergence signal (CLASSIFIER.md §3.2).
	ag.DNSQueryNS = func(ctx context.Context, server, name string) ([]string, error) {
		r := instruments.DNS.DoNS(ctx, server, name)
		if r.ErrorClass != "" || r.Err != nil {
			return nil, fmt.Errorf("%s: %w", r.ErrorClass, r.Err)
		}
		return r.Names, nil
	}
	return ag
}

// directDailyMB reads NETWEATHER_DAILY_MB. The default is the documented 2 GB;
// 0 disables the budget. A malformed value is a startup error rather than a
// silent default, because this setting is a safety limit.
func directDailyMB() (int, error) {
	raw := strings.TrimSpace(os.Getenv("NETWEATHER_DAILY_MB"))
	if raw == "" {
		return 2048, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("NETWEATHER_DAILY_MB %q is not a non-negative integer", raw)
	}
	return n, nil
}

// newDirectBudget builds the direct vantage's daily byte budget, or nil when it
// is disabled with NETWEATHER_DAILY_MB=0.
func newDirectBudget(lg *logging.Logger) (*bytecap.Budget, error) {
	mb, err := directDailyMB()
	if err != nil {
		return nil, err
	}
	if mb == 0 {
		return nil, nil
	}

	budget := bytecap.New(int64(mb) << 20)
	budget.OnExhausted = func(used int64) {
		lg.Warn("daily egress budget exhausted; probing stops until UTC midnight",
			"event", "egress_budget_exhausted", "component", "agent",
			"used_bytes", used, "limit_bytes", budget.Limit)
	}
	return budget, nil
}

// rateLimitFromEnv reads NETWEATHER_RATE_LIMIT, the global probe ceiling in
// requests per second. The default is the documented 20. This is a guardrail,
// so a malformed, zero, or absurd value refuses startup rather than silently
// removing the ceiling.
func rateLimitFromEnv() (float64, error) {
	raw := strings.TrimSpace(os.Getenv("NETWEATHER_RATE_LIMIT"))
	if raw == "" {
		return 20, nil
	}
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil || n <= 0 || n > 1000 {
		return 0, fmt.Errorf("NETWEATHER_RATE_LIMIT %q is not a rate between 0 and 1000 req/s", raw)
	}
	return n, nil
}

// trustedProxiesFromEnv parses NETWEATHER_TRUSTED_PROXIES, a comma-separated
// list of CIDRs or bare addresses whose X-Forwarded-For the UI and API will
// believe (NETWORKING.md §7.3). Empty means "trust nobody", which is the safe
// default: trusting every peer turns a header into a rate-limit bypass.
//
// A bare address becomes a single-host prefix, and a malformed entry refuses
// startup rather than being skipped: an operator who mistypes a CIDR would
// otherwise see per-client limits silently fall back to the proxy address.
func trustedProxiesFromEnv() ([]netip.Prefix, error) {
	raw := strings.TrimSpace(os.Getenv("NETWEATHER_TRUSTED_PROXIES"))
	if raw == "" {
		return nil, nil
	}

	var out []netip.Prefix
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			prefix, err := netip.ParsePrefix(entry)
			if err != nil {
				return nil, fmt.Errorf("NETWEATHER_TRUSTED_PROXIES %q is not a CIDR: %w", entry, err)
			}
			out = append(out, prefix.Masked())
			continue
		}
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			return nil, fmt.Errorf("NETWEATHER_TRUSTED_PROXIES %q is not an address or CIDR: %w", entry, err)
		}
		addr = addr.Unmap()
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}

// calendarPathFromEnv resolves the atypical-day calendar (DESIGN.md §7.2).
// NETWEATHER_CALENDAR wins; otherwise a calendar.yaml beside the target config
// is used, whether that config is a file or a directory. An absent file is not
// an error and disables the calendar.
func calendarPathFromEnv() string {
	if v := strings.TrimSpace(os.Getenv("NETWEATHER_CALENDAR")); v != "" {
		return v
	}
	cfgPath := strings.TrimSpace(os.Getenv("NETWEATHER_CONFIG"))
	if cfgPath == "" {
		return ""
	}
	base := filepath.Dir(cfgPath)
	if info, err := os.Stat(cfgPath); err == nil && info.IsDir() { // #nosec G703 -- operator-supplied NETWEATHER_CONFIG.
		base = cfgPath
	}
	candidate := filepath.Join(base, "calendar.yaml")
	if _, err := os.Stat(candidate); err == nil { // #nosec G703 -- operator-supplied NETWEATHER_CONFIG plus a fixed name.
		return candidate
	}
	return ""
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
// corridorTraceStep is the one-shot traceroute seam the corridor runner uses on
// a step change. A failure (no raw sockets, an unresolvable endpoint, a walk
// that cannot start) is returned as an error; the event still opens without a
// trace, because the step change is the fact and the walk is the extra.
func corridorTraceStep(lg *logging.Logger) func(context.Context, string) (string, error) {
	dialer := netx.New()
	return func(ctx context.Context, endpoint string) (string, error) {
		host, _, err := net.SplitHostPort(endpoint)
		if err != nil {
			return "", err
		}
		addrs, err := dialer.CheckHost(ctx, host)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", host, err)
		}
		if len(addrs) == 0 {
			return "", fmt.Errorf("resolve %s: no address", host)
		}

		res := probe.NewTrace().Run(ctx, addrs[0])
		if res.Failed() {
			return "", fmt.Errorf("%s: %s: %w", host, res.ErrorClass, res.Err)
		}
		lg.Debug("corridor trace captured",
			"event", "corridor_trace", "component", "corridor",
			"endpoint", endpoint, "hops", len(res.Hops), "reached", res.Reached)
		return formatTraceHops(res), nil
	}
}

// formatTraceHops renders one walk for the corridor event. A silent hop is a
// star because router rate limiting is a hint, not loss (DESIGN.md §5.4).
func formatTraceHops(res probe.TraceResult) string {
	var b strings.Builder
	for _, hop := range res.Hops {
		if hop.Addr.IsValid() {
			fmt.Fprintf(&b, "%2d  %-15s  %s\n", hop.TTL, hop.Addr.String(),
				hop.RTT.Round(time.Millisecond))
			continue
		}
		fmt.Fprintf(&b, "%2d  *\n", hop.TTL)
	}
	destination := "unresolved"
	if res.Destination.IsValid() {
		destination = res.Destination.String()
	}
	fmt.Fprintf(&b, "destination %s reached=%t\n", destination, res.Reached)
	if !res.Reached && len(res.Hops) <= 1 {
		// NETWORKING.md §8: a walk that stops at the bridge gateway is a
		// container artifact, not a path failure.
		_, _ = fmt.Fprintln(&b, "note: the walk stopped after the first hop; in a container this is a NAT/conntrack artifact, not a path failure")
	}
	return b.String()
}

func corridorLoop(ctx context.Context, st *store.Store, paused func() bool, lg *logging.Logger, traceStep func(context.Context, string) (string, error)) {
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
				// One trace per step change, and only here: a walk touches ~30
				// third-party routers, so it is evidence captured once rather
				// than a scheduled action (DESIGN.md §9, §9.1).
				trace := ""
				if traceStep != nil && len(c.Endpoints) > 1 {
					text, err := traceStep(ctx, c.Endpoints[1])
					if err != nil {
						lg.Debug("corridor trace unavailable",
							"event", "corridor_trace_failed", "component", "corridor",
							"corridor", c.ID, "error", err)
					} else {
						trace = text
					}
				}
				event := store.CorridorEvent{
					ID:         fmt.Sprintf("ce-%d-%s", now.UnixMilli(), c.ID),
					CorridorID: c.ID, Kind: verdict.Kind,
					StartedAt: now, Detail: verdict.Detail, Trace: trace,
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
	// torProbeTimeout bounds one Tor probe end to end. Exits are slow and
	// variable, so a direct-probe ceiling would produce false DOWN states; the
	// probe loop is sequential, so an unbounded circuit would also stall every
	// other target (DESIGN.md §5.7).
	torProbeTimeout = 30 * time.Second
)

// newTorAgent builds the optional corroboration vantage. It returns nil when
// NETWEATHER_TOR is not "true": Tor is off by default, because it is a
// different trust domain and a different volume budget.
// cymruLookup returns an ASN and country resolver for an egress address. The
// query goes to one well-known resolver over the guarded dialer, so the host's
// resolver is not part of the measurement.
// cymruResolver returns a resolver that sends its queries to one well-known
// server over the guarded dialer, so the host's resolver is not part of the
// measurement.
func cymruResolver() *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return netx.New().DialContext(ctx, "udp", "1.1.1.1:53")
		},
	}
}

func cymruLookup() func(context.Context, netip.Addr) (int, string) {
	resolver := cymruResolver()
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

// torControlPassword matches the hashed value baked into torrc. It protects an
// internal, never-published control port from unrelated containers on egress;
// it is not an operator secret and grants no relay capability.
const torControlPassword = "nwverify"

// torExitLookup combines the two facts needed for a country label: the ASN from
// Cymru's guarded DNS lookup, and the country from Tor's own GeoIP database via
// the same process that carried the probe. Cymru's country is registrar data
// and can disagree with the relay's actual location (a German exit registered
// under a US foundation, for example), so it is only a fallback for a
// user-supplied SOCKS endpoint that has no control port.
func torExitLookup(control string) func(context.Context, netip.Addr) (int, string) {
	cymru := cymruLookup()
	return func(ctx context.Context, addr netip.Addr) (int, string) {
		asn, cymruCountry := cymru(ctx, addr)
		if control == "" {
			return asn, cymruCountry
		}
		if country := torGeoIPCountry(ctx, control, addr.Unmap().String()); country != "" {
			return asn, country
		}
		return asn, cymruCountry
	}
}

// torGeoIPCountry asks the sidecar's control port where an address is, using
// GETINFO ip-to-country. The query never leaves the egress network and uses
// Tor's shipped GeoIP database rather than a third-party geolocation API.
func torGeoIPCountry(ctx context.Context, control, ip string) string {
	if control == "" || ip == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", control)
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := fmt.Fprintf(conn, "AUTHENTICATE %q\r\nGETINFO ip-to-country/%s\r\nQUIT\r\n",
		torControlPassword, ip); err != nil {
		return ""
	}

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "250") || !strings.Contains(line, "ip-to-country/") {
			continue
		}
		_, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.ToUpper(strings.TrimSpace(value))
		if alpha2(value) {
			return value
		}
	}
	return ""
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

const (
	torVerifyTimeout = 45 * time.Second
	torVerifyRetry   = 2 * time.Second
)

// verifyTorExitWithRetry tolerates a cold sidecar still building its first
// circuit. A real mismatch is not retried: StrictNodes cannot turn a wrong
// exit into the declared country, and repeating that check would only delay
// the dashboard's health gate.
func verifyTorExitWithRetry(ctx context.Context, httpProbe *probe.HTTP,
	lookup func(context.Context, netip.Addr) (int, string), want string,
) (string, error) {
	deadline := time.Now().Add(torVerifyTimeout)
	for {
		value, err := torExitCountry(ctx, httpProbe, lookup, want)
		if err == nil {
			return value, nil
		}
		if !strings.Contains(err.Error(), "could not be learned") &&
			!strings.Contains(err.Error(), "unavailable") {
			return "", err
		}
		if !time.Now().Before(deadline) {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(torVerifyRetry):
		}
	}
}

// torSpec is one pinned Tor exit. Country is verified through that exit's own
// SOCKS hop before the vantage is allowed to report, so the label is never
// guessed from a container name or an operator's intention (ACCEPTABLE_USE.md §8).
type torSpec struct {
	Country string // ISO-3166 alpha-2, upper case; empty means unpinned
	Socks   string // host:port of this Tor instance's SOCKS5 listener
	Control string // host:port of the same instance's read-only control port
	ID      string // vantage id: "tor" or "tor-<cc>"
}

// parseTorSpecs reads NETWEATHER_TOR_VANTAGES, a comma-separated list of
// CC=host:port pairs. Each pair names a real Tor client with that exit pinned
// in its own torrc. The single-vantage variables remain the fallback so an
// existing deployment keeps working unchanged.
func parseTorSpecs() ([]torSpec, error) {
	raw := strings.TrimSpace(os.Getenv("NETWEATHER_TOR_VANTAGES"))
	if raw == "" {
		cc := strings.ToUpper(strings.TrimSpace(os.Getenv("NETWEATHER_TOR_EXIT_COUNTRY")))
		socks := strings.TrimSpace(os.Getenv("NETWEATHER_TOR_SOCKS"))
		if socks == "" {
			socks = defaultTorProxy
		}
		id := "tor"
		if cc != "" {
			id = "tor-" + strings.ToLower(cc)
		}
		return []torSpec{{Country: cc, Socks: socks, Control: torControlAddr(socks), ID: id}}, nil
	}

	var out []torSpec
	seen := make(map[string]bool)
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		cc, addr, ok := strings.Cut(item, "=")
		rawCC := strings.TrimSpace(cc)
		cc = strings.ToUpper(rawCC)
		addr = strings.TrimSpace(addr)
		if !ok || !alpha2(rawCC) || addr == "" {
			return nil, fmt.Errorf("NETWEATHER_TOR_VANTAGES entry %q is not CC=host:port", item)
		}
		if seen[cc] {
			return nil, fmt.Errorf("NETWEATHER_TOR_VANTAGES lists %s twice", cc)
		}
		seen[cc] = true
		out = append(out, torSpec{
			Country: cc, Socks: addr, Control: torControlAddr(addr),
			ID: "tor-" + strings.ToLower(cc),
		})
	}
	if len(out) == 0 {
		return nil, errors.New("NETWEATHER_TOR_VANTAGES is set but empty")
	}
	return out, nil
}

func alpha2(s string) bool {
	if len(s) != 2 {
		return false
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// torControlAddr pairs a SOCKS listener with the control port of the same Tor
// process. The sidecars ship both on 9050/9051 inside one container namespace.
func torControlAddr(socks string) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(socks))
	if err != nil {
		return ""
	}
	return net.JoinHostPort(host, "9051")
}

// combinedBudget is the display total across pinned exits. It never gates a
// probe: each agent suspends on its own budget, so one exhausted country does
// not stop the others. The board sums the allowances to explain a full pause.
type combinedBudget []*bytecap.Budget

func (b combinedBudget) Used() int64 {
	var total int64
	for _, budget := range b {
		total += budget.Used()
	}
	return total
}

func (b combinedBudget) Allowance() int64 {
	var total int64
	for _, budget := range b {
		total += budget.Allowance()
	}
	return total
}

func (b combinedBudget) Exhausted() bool {
	if len(b) == 0 {
		return false
	}
	for _, budget := range b {
		if !budget.Exhausted() {
			return false
		}
	}
	return true
}

// torLimits reads the caps shared by every Tor vantage. One set of limits is
// applied per exit, not to the group, so a slow country cannot starve another.
func torLimits() (maxTargets, dailyMB int, err error) {
	maxTargets = defaultTorMax
	if raw := strings.TrimSpace(os.Getenv("NETWEATHER_TOR_MAX")); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n <= 0 {
			return 0, 0, fmt.Errorf("NETWEATHER_TOR_MAX %q is not a positive integer", raw)
		}
		maxTargets = n
	}
	dailyMB = torDailyMBDefault
	if raw := strings.TrimSpace(os.Getenv("NETWEATHER_TOR_DAILY_MB")); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 0 {
			return 0, 0, fmt.Errorf("NETWEATHER_TOR_DAILY_MB %q is not a non-negative integer", raw)
		}
		dailyMB = n
	}
	return maxTargets, dailyMB, nil
}

// buildTorVantage builds one pinned-exit corroboration agent. The same target
// subset is selected for every vantage, so a difference between rows is a
// difference about the path rather than about which hosts were measured.
func buildTorVantage(ctx context.Context, st *store.Store, direct []agent.Target, lg *logging.Logger,
	userAgent string, spec torSpec, maxTargets, dailyMB int,
) (*agent.Agent, *bytecap.Budget, error) {
	dial, err := netx.Socks5(spec.Socks)
	if err != nil {
		return nil, nil, err
	}

	// ACCEPTABLE_USE.md §8: the Tor vantage carries its own daily volume budget,
	// counted on the SOCKS connection so a retry cannot slip past it.
	budget := bytecap.New(int64(dailyMB) << 20)
	budget.OnExhausted = func(used int64) {
		lg.Warn("tor daily volume budget exhausted; the vantage stops until UTC midnight",
			"event", "tor_budget_exhausted", "component", "agent",
			"used_bytes", used, "limit_bytes", budget.Limit,
			"vantage", spec.ID, "proxy", spec.Socks)
	}
	torDial := probe.DialFunc(budget.Wrap(dial))

	selected := selectTorTargets(direct, maxTargets)
	if len(selected) == 0 {
		return nil, nil, fmt.Errorf("%s: no tcp-based targets are configured", spec.ID)
	}

	// A country label is only honest when the exit is actually there. The
	// identity is fetched through the same SOCKS hop the probes will use, so a
	// mismatched or unverifiable exit refuses to start rather than fabricating
	// a country measurement (ACCEPTABLE_USE.md §8).
	if spec.Country != "" {
		exitProbe := probe.NewHTTPWithDial(torDial)
		exitProbe.UserAgent = userAgent
		value, err := verifyTorExitWithRetry(ctx, exitProbe, torExitLookup(spec.Control), spec.Country)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", spec.ID, err)
		}
		lg.Info("tor exit country verified",
			"event", "tor_exit_verified", "component", "agent",
			"vantage", spec.ID, "exit", value, "country", spec.Country)
	}
	for i := range selected {
		selected[i].Vantage = spec.ID
	}

	if err := st.EnsureVantage(ctx, spec.ID, "socks5", "corroboration"); err != nil {
		return nil, nil, err
	}

	torHTTP := probe.NewHTTPWithDial(torDial)
	torHTTP.UserAgent = userAgent
	runner := agent.Instruments{
		HTTP: torHTTP,
		TLS:  probe.NewTLSWithDial(torDial),
		TCP:  probe.NewTCPWithDial(torDial),
	}
	ag := agent.New(selected, timeoutRunner{inner: runner, timeout: torProbeTimeout}, st)
	ag.Logger = lg.Logger
	ag.Pace = torPace
	ag.Suspend = budget.Exhausted

	lg.Info("tor vantage enabled",
		"event", "tor_enabled", "component", "agent",
		"vantage", spec.ID, "proxy", spec.Socks,
		"country", spec.Country, "targets", len(selected), "pace", torPace.String())
	return ag, budget, nil
}

// newTorAgents builds every configured Tor vantage. One bad exit is logged and
// skipped: a country with no healthy exits at that moment must not take the
// primary vantage or the other continents down with it.
func newTorAgents(ctx context.Context, st *store.Store, direct []agent.Target, lg *logging.Logger,
	userAgent string,
) ([]*agent.Agent, []*bytecap.Budget, []torSpec, error) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("NETWEATHER_TOR")), "true") {
		return nil, nil, nil, nil
	}
	specs, err := parseTorSpecs()
	if err != nil {
		return nil, nil, nil, err
	}
	maxTargets, dailyMB, err := torLimits()
	if err != nil {
		return nil, nil, nil, err
	}

	// Verification talks through each proxy, so build them concurrently: a
	// cold Tor circuit can take tens of seconds, and six sequential checks would
	// delay the dashboard's health gate. Results are written by index to keep
	// the vantage order deterministic.
	type buildResult struct {
		agent  *agent.Agent
		budget *bytecap.Budget
	}
	results := make([]buildResult, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		go func(i int, spec torSpec) {
			defer wg.Done()
			ag, budget, err := buildTorVantage(ctx, st, direct, lg, userAgent, spec, maxTargets, dailyMB)
			if err != nil {
				lg.Error("tor vantage unavailable; the other vantages keep running",
					"event", "tor_vantage_failed", "component", "agent",
					"vantage", spec.ID, "proxy", spec.Socks, "error", err)
				return
			}
			results[i] = buildResult{agent: ag, budget: budget}
		}(i, spec)
	}
	wg.Wait()

	var agents []*agent.Agent
	var budgets []*bytecap.Budget
	var live []torSpec
	for i, res := range results {
		if res.agent == nil {
			continue
		}
		agents = append(agents, res.agent)
		budgets = append(budgets, res.budget)
		live = append(live, specs[i])
	}
	if len(agents) == 0 {
		return nil, nil, nil, errors.New("no tor vantage could be started")
	}
	return agents, budgets, live, nil
}

// newTorAgent is the strict single-vantage form used by the unit tests. The
// server uses newTorAgents so one unavailable exit cannot stop the fleet.
func newTorAgent(ctx context.Context, st *store.Store, direct []agent.Target, lg *logging.Logger,
	userAgent string,
) (*agent.Agent, *bytecap.Budget, error) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("NETWEATHER_TOR")), "true") {
		return nil, nil, nil
	}
	specs, err := parseTorSpecs()
	if err != nil {
		return nil, nil, err
	}
	maxTargets, dailyMB, err := torLimits()
	if err != nil {
		return nil, nil, err
	}
	return buildTorVantage(ctx, st, direct, lg, userAgent, specs[0], maxTargets, dailyMB)
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

	client := feedClient()

	var out []*feeds.Poller
	for _, name := range strings.Split(enabled, ",") {
		switch strings.TrimSpace(name) {
		case "ioda":
			ioda := feeds.NewIODA(client)
			if endpoint := strings.TrimSpace(os.Getenv("NETWEATHER_IODA_URL")); endpoint != "" {
				if feedURLIsHTTPS(endpoint) {
					ioda.Endpoint = endpoint
				} else {
					lg.Warn("feed URL override is not HTTPS; keeping the compiled endpoint",
						"event", "feed_url_insecure", "component", "feeds",
						"feed", "ioda", "url", endpoint)
				}
			}
			out = append(out, &feeds.Poller{
				Feed:   "ioda",
				Source: ioda,
				Sink:   st,
				// DESIGN.md §8.1: IODA is the build-first feed and polls every
				// five minutes. The other feeds stay at ten, the slow end of
				// their documented range, because they are corroboration.
				Every: 5 * time.Minute,
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
				if feedURLIsHTTPS(base) {
					radar.BaseURL = base
				} else {
					lg.Warn("feed URL override is not HTTPS; keeping the compiled endpoint",
						"event", "feed_url_insecure", "component", "feeds",
						"feed", "radar", "url", base)
				}
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
			var urls []string
			for _, candidate := range splitList(raw) {
				if feedURLIsHTTPS(candidate) {
					urls = append(urls, candidate)
					continue
				}
				lg.Warn("statuspage URL is not HTTPS; dropping it",
					"event", "feed_url_insecure", "component", "feeds",
					"feed", "statuspage", "url", candidate)
			}
			if len(urls) == 0 {
				lg.Warn("statuspage feed has no HTTPS URLs; skipping",
					"event", "feed_not_configured", "component", "feeds", "feed", "statuspage")
				continue
			}
			out = append(out, &feeds.Poller{
				Feed:   "statuspage",
				Source: feeds.NewStatuspage(client, urls),
				Sink:   st,
				Every:  5 * time.Minute,
			})
		case "aws-status":
			aws := feeds.NewAWSStatus(client)
			if endpoint := strings.TrimSpace(os.Getenv("NETWEATHER_AWS_STATUS_URL")); endpoint != "" {
				if feedURLIsHTTPS(endpoint) {
					aws.URL = endpoint
				} else {
					lg.Warn("feed URL override is not HTTPS; keeping the compiled endpoint",
						"event", "feed_url_insecure", "component", "feeds",
						"feed", "aws-status", "url", endpoint)
				}
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
				if feedURLIsHTTPS(endpoint) {
					gcp.URL = endpoint
				} else {
					lg.Warn("feed URL override is not HTTPS; keeping the compiled endpoint",
						"event", "feed_url_insecure", "component", "feeds",
						"feed", "gcp-status", "url", endpoint)
				}
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
				if feedURLIsHTTPS(endpoint) {
					azure.URL = endpoint
				} else {
					lg.Warn("feed URL override is not HTTPS; keeping the compiled endpoint",
						"event", "feed_url_insecure", "component", "feeds",
						"feed", "azure-status", "url", endpoint)
				}
			}
			out = append(out, &feeds.Poller{
				Feed:   "azure-status",
				Source: azure,
				Sink:   st,
				Every:  5 * time.Minute,
			})
		case "atlas":
			raw := strings.TrimSpace(os.Getenv("NETWEATHER_ATLAS_MEASUREMENTS"))
			if raw == "" {
				lg.Warn("atlas feed requested without NETWEATHER_ATLAS_MEASUREMENTS",
					"event", "feed_not_configured", "component", "feeds", "feed", "atlas")
				continue
			}
			var measurements []int
			for _, part := range splitList(raw) {
				n, err := strconv.Atoi(strings.TrimSpace(part))
				if err != nil || n <= 0 {
					lg.Warn("atlas measurement id is not a positive integer; skipping",
						"event", "feed_not_configured", "component", "feeds",
						"feed", "atlas", "value", part)
					continue
				}
				measurements = append(measurements, n)
			}
			if len(measurements) == 0 {
				lg.Warn("atlas feed has no usable measurement ids; skipping",
					"event", "feed_not_configured", "component", "feeds", "feed", "atlas")
				continue
			}
			atlas := feeds.NewAtlas(client, strings.TrimSpace(os.Getenv("NETWEATHER_ATLAS_KEY")), measurements)
			if endpoint := strings.TrimSpace(os.Getenv("NETWEATHER_ATLAS_URL")); endpoint != "" {
				if feedURLIsHTTPS(endpoint) {
					atlas.Endpoint = endpoint
				} else {
					lg.Warn("feed URL override is not HTTPS; keeping the compiled endpoint",
						"event", "feed_url_insecure", "component", "feeds",
						"feed", "atlas", "url", endpoint)
				}
			}
			out = append(out, &feeds.Poller{
				Feed:   "atlas",
				Source: atlas,
				Sink:   st,
				Every:  10 * time.Minute,
			})
		case "ris-live", "ris":
			raw := strings.TrimSpace(os.Getenv("NETWEATHER_RIS_ASNS"))
			if raw == "" {
				lg.Warn("ris-live feed requested without NETWEATHER_RIS_ASNS",
					"event", "feed_not_configured", "component", "feeds", "feed", "ris-live")
				continue
			}
			var asns []int
			for _, part := range splitList(raw) {
				n, err := strconv.Atoi(strings.TrimSpace(part))
				if err != nil || n <= 0 {
					lg.Warn("ris-live ASN is not a positive integer; skipping",
						"event", "feed_not_configured", "component", "feeds",
						"feed", "ris-live", "value", part)
					continue
				}
				asns = append(asns, n)
			}
			if len(asns) == 0 {
				lg.Warn("ris-live feed has no usable ASNs; skipping",
					"event", "feed_not_configured", "component", "feeds", "feed", "ris-live")
				continue
			}
			ris := feeds.NewRISLive(client, asns)
			if endpoint := strings.TrimSpace(os.Getenv("NETWEATHER_RIS_URL")); endpoint != "" {
				if feedURLIsHTTPS(endpoint) {
					ris.Endpoint = endpoint
				} else {
					lg.Warn("feed URL override is not HTTPS; keeping the compiled endpoint",
						"event", "feed_url_insecure", "component", "feeds",
						"feed", "ris-live", "url", endpoint)
				}
			}
			out = append(out, &feeds.Poller{
				Feed:   "ris-live",
				Source: ris,
				Sink:   st,
				Every:  10 * time.Minute,
			})
		}
	}
	return out
}

// buildLevelPollers returns a poller for each enabled bounded level feed. These
// are separate from buildFeedPollers because their output is a small per-country
// snapshot read by the world view, not a stream of evidence events.
func buildLevelPollers(st *store.Store, lg *logging.Logger) []*feeds.LevelPoller {
	enabled := strings.ToLower(os.Getenv("NETWEATHER_FEEDS"))
	if !strings.Contains(enabled, "ioda-level") {
		return nil
	}

	client := feedClient()
	ioda := feeds.NewIODA(client)
	if endpoint := strings.TrimSpace(os.Getenv("NETWEATHER_IODA_SUMMARY_URL")); endpoint != "" {
		if feedURLIsHTTPS(endpoint) {
			ioda.SummaryEndpoint = endpoint
		} else {
			lg.Warn("feed URL override is not HTTPS; keeping the compiled endpoint",
				"event", "feed_url_insecure", "component", "feeds",
				"feed", "ioda-level", "url", endpoint)
		}
	}
	if raw := strings.TrimSpace(os.Getenv("NETWEATHER_IODA_LEVEL_LIMIT")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < feeds.MinIODALevelLimit || limit > feeds.MaxIODALevelLimit {
			lg.Warn("ioda level limit out of range; using the default",
				"event", "feed_config_invalid", "component", "feeds",
				"feed", "ioda-level", "value", raw, "default", feeds.DefaultIODALevelLimit)
		} else {
			ioda.LevelLimit = limit
		}
	}

	lg.Info("bounded ioda country level feed enabled",
		"event", "feed_configured", "component", "feeds",
		"feed", "ioda-level", "limit", ioda.LevelLimit)

	return []*feeds.LevelPoller{{
		Feed:   "ioda-level",
		Source: ioda,
		Sink:   st,
		Every:  5 * time.Minute,
	}}
}

// buildRISStreams returns the real-time RIS Live stream when it is opted in.
// NETWEATHER_FEEDS containing "ris-live-ws" or NETWEATHER_RIS_STREAM=true both
// enable it; the same NETWEATHER_RIS_ASNS list feeds the poller and the stream.
func buildRISStreams(st *store.Store, lg *logging.Logger) []*feeds.RISLiveWS {
	enabled := strings.ToLower(os.Getenv("NETWEATHER_FEEDS"))
	if !strings.Contains(enabled, "ris-live-ws") &&
		!strings.EqualFold(strings.TrimSpace(os.Getenv("NETWEATHER_RIS_STREAM")), "true") {
		return nil
	}

	var asns []int
	for _, part := range splitList(os.Getenv("NETWEATHER_RIS_ASNS")) {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n <= 0 {
			lg.Warn("ris live stream ASN is not a positive integer; skipping",
				"event", "feed_config_invalid", "component", "feeds",
				"feed", feeds.RISLiveWSFeed, "value", part)
			continue
		}
		asns = append(asns, n)
	}
	if len(asns) == 0 {
		lg.Warn("ris live stream requested without NETWEATHER_RIS_ASNS",
			"event", "feed_not_configured", "component", "feeds", "feed", feeds.RISLiveWSFeed)
		return nil
	}

	stream := feeds.NewRISLiveWS(st, asns)
	if endpoint := strings.TrimSpace(os.Getenv("NETWEATHER_RIS_WS_URL")); endpoint != "" {
		if feedURLIsWSS(endpoint) {
			stream.URL = endpoint
		} else {
			lg.Warn("stream URL override is not WSS; keeping the compiled endpoint",
				"event", "feed_url_insecure", "component", "feeds",
				"feed", feeds.RISLiveWSFeed, "url", endpoint)
		}
	}

	lg.Info("real-time ris live stream enabled",
		"event", "feed_configured", "component", "feeds",
		"feed", feeds.RISLiveWSFeed, "asns", len(asns))
	return []*feeds.RISLiveWS{stream}
}

// feedClient is the HTTP client every feed uses. TLS is required, not assumed:
// a status page is evidence, and plaintext evidence can be rewritten in
// transit (THREAT_MODEL.md T32). Redirects are re-checked because the scheme
// can change mid-chain.
func feedClient() *http.Client {
	client := &http.Client{
		Transport: probe.Transport(netx.New()),
		Timeout:   20 * time.Second,
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !strings.EqualFold(req.URL.Scheme, "https") {
			return errors.New("feeds: refusing a non-HTTPS redirect")
		}
		if len(via) >= 5 {
			return errors.New("feeds: too many redirects")
		}
		return nil
	}
	return client
}

// feedURLIsHTTPS reports whether an operator-supplied feed URL is usable. The
// compiled endpoints are already HTTPS; an override must be too.
func feedURLIsHTTPS(raw string) bool {
	u, err := neturl.Parse(raw)
	return err == nil && strings.EqualFold(u.Scheme, "https") && u.Host != ""
}

// feedURLIsWSS is the WebSocket equivalent of feedURLIsHTTPS. The compiled
// endpoint is wss; an operator override must be too, because a plaintext stream
// of routing evidence can be rewritten in transit.
func feedURLIsWSS(raw string) bool {
	u, err := neturl.Parse(raw)
	return err == nil && strings.EqualFold(u.Scheme, "wss") && u.Host != ""
}

// staleWAL reports a non-empty write-ahead log beside dbPath. A WAL left by an
// unclean shutdown is recovered by SQLite; after a restore, the -wal and -shm
// siblings belong to the replaced file and must be deleted first (SCHEMA.md
// §14). The size is returned so the log line can name what it saw.
func staleWAL(dbPath string) (int64, bool) {
	info, err := os.Stat(dbPath + "-wal") // #nosec G703 -- operator-supplied data directory plus a fixed suffix.
	if err != nil || info.Size() == 0 {
		return 0, false
	}
	return info.Size(), true
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

// nextDropReport decides whether a maintenance tick should report lost log
// lines. The counters are cumulative, so a tick reports the deltas and the
// running totals, and a quiet tick reports nothing. LOGGING.md §8 asks for one
// line per interval rather than one per lost record, because a failing sink
// would otherwise multiply the problem it is reporting.
func nextDropReport(prevDropped, prevWriteErrors, dropped, writeErrors uint64) (bool, uint64, uint64) {
	if dropped <= prevDropped && writeErrors <= prevWriteErrors {
		return false, 0, 0
	}
	var droppedDelta, writeDelta uint64
	if dropped > prevDropped {
		droppedDelta = dropped - prevDropped
	}
	if writeErrors > prevWriteErrors {
		writeDelta = writeErrors - prevWriteErrors
	}
	return true, droppedDelta, writeDelta
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

		// A different egress address or resolver set is a different path, so the
		// learned DNS answers are no longer this vantage's history
		// (CLASSIFIER.md §3.2). Verdicts are kept: they are what happened, not
		// what is expected next.
		resetDNSBaselines := func(reason string) {
			dropped, err := st.ResetDNSBaselines(ctx)
			if err != nil {
				lg.Warn("dns baseline reset failed",
					"event", "dns_baseline_reset_failed", "component", "maintenance",
					"reason", reason, "error", err)
				return
			}
			lg.Info("dns baselines reset after a vantage change",
				"event", "dns_baselines_reset", "component", "maintenance",
				"reason", reason, "rows", dropped)
		}

		for _, res := range results {
			switch res.Check {
			case "egress-ip":
				if changed, detail := selfcheck.VantageChange(previous["egress-ip"], res); changed {
					// Record the move so the board can say why its baselines
					// reset, not only the log (DESIGN.md §7.5).
					if err := st.RecordVantageChange(ctx, previous["egress-ip"].Value, res.Value, time.Now()); err != nil {
						lg.Warn("vantage change record failed",
							"event", "vantage_change_record_failed", "component", "maintenance", "error", err)
					}
					lg.Warn("vantage changed",
						"event", "vantage_changed", "component", "maintenance", "detail", detail)
					resetDNSBaselines("egress changed")
				}
			case "resolver-identity":
				if changed, detail := selfcheck.ValueChange(previous["resolver-identity"], res); changed {
					lg.Warn("resolver set changed",
						"event", "resolver_changed", "component", "maintenance", "detail", detail)
					resetDNSBaselines("resolver set changed")
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
	// LOGGING.md §8: dropped lines and write errors are counted, and the count
	// is reported once per maintenance interval rather than once per lost
	// line. The counters are cumulative, so the event carries the delta since
	// the previous tick plus the running totals.
	var lastDropped, lastWriteErrors uint64
	run := func() {
		runSelfChecks()
		runDebugPrune()

		dropped, writeErrors := lg.Dropped(), lg.WriteErrors()
		if report, droppedDelta, writeDelta := nextDropReport(lastDropped, lastWriteErrors, dropped, writeErrors); report {
			lg.Warn("log sink dropped lines",
				"event", "log_dropped", "component", "logging",
				"dropped", droppedDelta,
				"write_errors", writeDelta,
				"dropped_total", dropped,
				"write_errors_total", writeErrors)
		}
		lastDropped, lastWriteErrors = dropped, writeErrors

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

		// The rolling blocked share decides which targets are measurable at
		// all (DESIGN.md §3.2). It runs before scope sampling so the sample
		// and the detector see the same denominator.
		if change, err := st.RefreshMeasurability(ctx, "local", now); err != nil {
			lg.Warn("measurability refresh failed",
				"event", "measurability_failed", "component", "maintenance", "error", err)
		} else if change.Marked > 0 || change.Cleared > 0 {
			lg.Info("measurability changed",
				"event", "measurability_changed", "component", "maintenance",
				"marked", change.Marked, "cleared", change.Cleared)
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
