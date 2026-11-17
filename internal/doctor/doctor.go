// Package doctor answers "can this host monitor at all?".
//
// It is a startup diagnostic, not a health check: the point is to tell an
// operator which instruments are available before they read a board and believe
// it. A missing capability is reported as a warning, never as a startup
// failure (DESIGN.md §5.5).
package doctor

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"
)

// Status is one check's outcome.
type Status string

const (
	// OK means the instrument works.
	OK Status = "ok"
	// WARN means a non-critical capability is missing. Probing continues with
	// reduced fidelity, which is the documented degradation.
	WARN Status = "warn"
	// FAIL means the monitor cannot do its job on this host.
	FAIL Status = "fail"
)

// Check is one diagnostic.
type Check struct {
	Name string
	// Fatal marks a capability the monitor cannot work without. A missing raw
	// ICMP socket is not fatal; an unwritable data directory is.
	Fatal bool
	Run   func(ctx context.Context) (detail string, err error)
}

// Result is a finished check.
type Result struct {
	Name   string
	Status Status
	Detail string
	Fatal  bool
	Dur    time.Duration
}

// Runner executes a fixed set of checks.
type Runner struct {
	Checks []Check
	Now    func() time.Time
}

// Run executes every check once. A check that returns an error is FAIL when
// fatal and WARN otherwise; one failing check never stops the others.
func (r *Runner) Run(ctx context.Context) []Result {
	out := make([]Result, 0, len(r.Checks))
	for _, c := range r.Checks {
		start := r.now()
		detail, err := c.Run(ctx)
		status := OK
		if err != nil {
			status = WARN
			if c.Fatal {
				status = FAIL
			}
			if detail == "" {
				detail = err.Error()
			}
		}
		out = append(out, Result{
			Name: c.Name, Status: status, Detail: detail,
			Fatal: c.Fatal, Dur: time.Since(start),
		})
	}
	return out
}

func (r *Runner) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

// Failed reports whether any fatal check failed.
func Failed(results []Result) bool {
	for _, res := range results {
		if res.Status == FAIL {
			return true
		}
	}
	return false
}

// Write renders a fixed-width table. Status is text as well as a column, so the
// output is readable with `NO_COLOR` and in a log grep.
func Write(out io.Writer, results []Result) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "CHECK\tSTATUS\tDETAIL"); err != nil {
		return err
	}
	for _, res := range results {
		detail := res.Detail
		if detail == "" {
			detail = "-"
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\n", res.Name, res.Status, detail); err != nil {
			return err
		}
	}
	return w.Flush()
}
