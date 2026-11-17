package tracebudget_test

import (
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/tracebudget"
)

func TestGovernorAllowsTheLimitPerWindow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	g := tracebudget.New(2, time.Hour)
	g.SetClock(func() time.Time { return now })

	if ok, _ := g.Allow(); !ok {
		t.Fatal("the first run was refused")
	}
	if ok, _ := g.Allow(); !ok {
		t.Fatal("the second run was refused")
	}

	ok, retry := g.Allow()
	if ok {
		t.Fatal("a third run was allowed")
	}
	if retry <= 0 || retry > time.Hour {
		t.Fatalf("retry = %v, want a positive time inside the window", retry)
	}

	// The next window starts a fresh count.
	now = now.Add(time.Hour + time.Second)
	if ok, _ := g.Allow(); !ok {
		t.Fatal("a new window refused its first run")
	}
}

func TestGovernorFailsClosed(t *testing.T) {
	t.Parallel()

	// A zero limit is a deliberate "no traces" configuration, not unlimited.
	g := tracebudget.New(0, time.Hour)
	if ok, _ := g.Allow(); ok {
		t.Fatal("a zero budget allowed a run")
	}
}

func TestGovernorZeroWindowUsesAnHour(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	g := tracebudget.New(1, 0)
	g.SetClock(func() time.Time { return now })

	if ok, _ := g.Allow(); !ok {
		t.Fatal("the first run was refused")
	}
	if ok, _ := g.Allow(); ok {
		t.Fatal("the second run was allowed in the same hour")
	}
	now = now.Add(time.Hour)
	if ok, _ := g.Allow(); !ok {
		t.Fatal("an hour did not reset the window")
	}
}
