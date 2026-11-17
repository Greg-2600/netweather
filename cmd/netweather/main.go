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
	"strconv"
	"syscall"
	"time"

	"github.com/Greg-2600/netweather/internal/agent"
	"github.com/Greg-2600/netweather/internal/api"
	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/config"
	"github.com/Greg-2600/netweather/internal/logging"
	"github.com/Greg-2600/netweather/internal/netx"
	"github.com/Greg-2600/netweather/internal/probe"
	"github.com/Greg-2600/netweather/internal/schedule"
	"github.com/Greg-2600/netweather/internal/seed"
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

	changes := board.NewBroadcast()
	source := &board.StoreSource{Store: st, Vantage: "local", Changes: changes}

	ui, err := web.New(web.Deps{
		Logger:        lg.Logger,
		Source:        source,
		Watcher:       source,
		Token:         cfg.Token,
		SecureCookies: os.Getenv("NETWEATHER_UI_SECURE_COOKIES") == "true",
		Version:       version.Version,
		StartedAt:     time.Now(),
	})
	if err != nil {
		return err
	}

	handler, err := api.New(cfg, api.Deps{
		Logger:    lg.Logger,
		Ring:      lg.Ring,
		Version:   version.Version,
		Commit:    version.Commit,
		StartedAt: time.Now(),
		Fallback:  ui,
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

	go probeLoop(ctx, lg, newAgent(targets, st, lg), changes)

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
		if t.Probe != "http" {
			return nil, fmt.Errorf("target %s: probe %q is not implemented yet (http only)", t.Host, t.Probe)
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
			URL:        targetURL(t.Host, t.Port),
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

func newAgent(targets []agent.Target, st *store.Store, lg *logging.Logger) *agent.Agent {
	ag := agent.New(targets, probe.NewHTTP(netx.New()), st)
	ag.Logger = lg.Logger
	return ag
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
