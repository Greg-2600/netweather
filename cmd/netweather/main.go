// Command netweather is the single binary: probe scheduler, classifier, store,
// and dashboard in one process. See docs/DESIGN.md.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/Greg-2600/netweather/internal/logging"
	"github.com/Greg-2600/netweather/internal/startup"
	"github.com/Greg-2600/netweather/internal/version"
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

		lg.Warn("serve is not implemented yet", "event", "not_implemented", "phase", 0)
		return errors.New("serve is not implemented yet: this is phase 0 scaffolding (docs/DESIGN.md §15)")

	default:
		return fmt.Errorf("unknown command %q (try: serve, version, healthcheck)", command)
	}
}
