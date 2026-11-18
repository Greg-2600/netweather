package main

import (
	"context"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/agent"
	"github.com/Greg-2600/netweather/internal/probe"
)

// slowRunner blocks until its context ends, so a test can prove the wrapper
// supplies a deadline rather than waiting for the inner probe.
type slowRunner struct {
	hadDeadline bool
	remaining   time.Duration
}

func (s *slowRunner) Run(ctx context.Context, _ agent.Target) probe.Result {
	deadline, ok := ctx.Deadline()
	s.hadDeadline = ok
	if ok {
		s.remaining = time.Until(deadline)
	}
	<-ctx.Done()
	return probe.Result{ErrorClass: "http_timeout", Err: ctx.Err()}
}

func TestTimeoutRunnerBoundsAProbe(t *testing.T) {
	t.Parallel()

	inner := &slowRunner{}
	r := timeoutRunner{inner: inner, timeout: 20 * time.Millisecond}

	start := time.Now()
	res := r.Run(context.Background(), agent.Target{ID: "t1"})
	elapsed := time.Since(start)

	if !inner.hadDeadline {
		t.Fatal("the wrapper did not set a deadline")
	}
	if inner.remaining <= 0 || inner.remaining > time.Second {
		t.Fatalf("deadline was %v away", inner.remaining)
	}
	if elapsed > time.Second {
		t.Fatalf("the probe ran %v; the deadline did not bound it", elapsed)
	}
	if !res.Failed() {
		t.Fatal("a cancelled probe reported success")
	}
}

func TestTimeoutRunnerWithoutATimeoutPassesThrough(t *testing.T) {
	t.Parallel()

	// A zero timeout is the direct vantage's shape: no extra bound is added.
	inner := &deadlineProbe{}
	r := timeoutRunner{inner: inner, timeout: 0}
	r.Run(context.Background(), agent.Target{ID: "t1"})
	if inner.hadDeadline {
		t.Fatal("a zero timeout added a deadline")
	}
}

type deadlineProbe struct{ hadDeadline bool }

func (d *deadlineProbe) Run(ctx context.Context, _ agent.Target) probe.Result {
	_, d.hadDeadline = ctx.Deadline()
	return probe.Result{Status: 200}
}
