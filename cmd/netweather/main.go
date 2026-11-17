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
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Greg-2600/netweather/internal/api"
	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/logging"
	"github.com/Greg-2600/netweather/internal/startup"
	"github.com/Greg-2600/netweather/internal/version"
	"github.com/Greg-2600/netweather/internal/web"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "netweather:", err)
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
		fmt.Fprintln(stdout, version.String())
		return nil

	case "healthcheck":
		// In phase 0 this proves the binary starts and exits cleanly. It becomes an
		// HTTP probe of 127.0.0.1:2600 once the server exists (docs/NETWORKING.md §7.3).
		return nil

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

// serve binds the listener and blocks until a signal arrives. Configuration and
// the exposure guardrail are validated before the socket is opened, so a
// misconfigured bind fails loudly instead of serving anonymously.
func serve(lg *logging.Logger) error {
	cfg, err := api.FromEnv(os.Getenv)
	if err != nil {
		lg.Error("invalid ui configuration", "event", "config_invalid", "error", err)
		return err
	}

	// The store is phase 0 work. Until it lands the board is genuinely empty;
	// seeding fake targets here would make the dashboard lie.
	source := board.NewMemory()

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

	ln, err := net.Listen("tcp", cfg.Listen)
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	lg.Info("listening",
		"event", "startup",
		"addr", cfg.Listen,
		"auth", cfg.AuthMode(),
	)

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}

	lg.Info("stopped", "event", "shutdown")
	return nil
}
