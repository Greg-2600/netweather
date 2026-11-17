// Package tracebudget caps on-demand traceroutes across a whole instance.
//
// A trace touches dozens of third-party routers and is the most visible thing
// this binary can do, so the ceiling is global rather than per client: a
// target's abuse desk sees the operator's egress address, not a user's
// (THREAT_MODEL.md T29, DESIGN.md §9.1). The API and the dashboard share one
// Governor so the documented ten-per-hour is a property of the instance, not
// of each surface.
package tracebudget

import (
	"sync"
	"time"
)

// Governor is a fixed-window counter. A caller gets a retry hint when the
// window is exhausted, because "try later" without a time is not an answer.
type Governor struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	started time.Time
	count   int
	now     func() time.Time
}

// New returns a governor that allows limit runs per window. A non-positive
// limit allows nothing, which fails closed; a non-positive window is treated
// as one hour.
func New(limit int, window time.Duration) *Governor {
	if window <= 0 {
		window = time.Hour
	}
	return &Governor{limit: limit, window: window, now: time.Now}
}

// SetClock replaces the clock. It exists for tests and never runs in
// production.
func (g *Governor) SetClock(now func() time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.now = now
}

// Allow spends one run from the window. It returns false and the time until the
// window resets when the budget is exhausted.
func (g *Governor) Allow() (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	if g.started.IsZero() || now.Sub(g.started) >= g.window {
		g.started = now
		g.count = 0
	}
	if g.count >= g.limit {
		return false, g.window - now.Sub(g.started)
	}
	g.count++
	return true, 0
}
