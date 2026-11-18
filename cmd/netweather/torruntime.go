package main

import (
	"context"
	"time"

	"github.com/Greg-2600/netweather/internal/agent"
	"github.com/Greg-2600/netweather/internal/probe"
)

// timeoutRunner bounds one probe with a context deadline. The direct vantage
// relies on each instrument's own ceiling; the Tor vantage needs a single,
// longer bound because an exit can be slow, and because the probe loop is
// sequential: a circuit that never answers would otherwise stall every later
// target in the same step (DESIGN.md §5.7).
type timeoutRunner struct {
	inner   agent.Runner
	timeout time.Duration
}

func (r timeoutRunner) Run(ctx context.Context, t agent.Target) probe.Result {
	if r.timeout <= 0 {
		return r.inner.Run(ctx, t)
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return r.inner.Run(ctx, t)
}
