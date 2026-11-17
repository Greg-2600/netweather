// Command netweather is the single binary: probe scheduler, classifier, store,
// and dashboard in one process. See docs/DESIGN.md.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

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
func run(args []string, out io.Writer) error {
	command := "serve"
	if len(args) > 0 {
		command = args[0]
	}

	switch command {
	case "version", "-v", "--version":
		fmt.Fprintln(out, version.String())
		return nil

	case "healthcheck":
		// In phase 0 this proves the binary starts and exits cleanly. It becomes an
		// HTTP probe of 127.0.0.1:2600 once the server exists (docs/NETWORKING.md §7.3).
		return nil

	case "serve":
		if err := startup.RefuseIfRoot(startup.EUID()); err != nil {
			return err
		}
		return errors.New("serve is not implemented yet: this is phase 0 scaffolding (docs/DESIGN.md §15)")

	default:
		return fmt.Errorf("unknown command %q (try: serve, version, healthcheck)", command)
	}
}
