package schedule_test

import (
	"context"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/schedule"
)

func fixed(v float64) func() float64 { return func() float64 { return v } }

func TestDefaultInterval(t *testing.T) {
	t.Parallel()

	tests := map[schedule.Tier]time.Duration{
		schedule.TierReference: 30 * time.Second,
		schedule.TierCritical:  120 * time.Second,
		schedule.TierService:   300 * time.Second,
		schedule.TierAnchor:    120 * time.Second,
		schedule.Tier(99):      300 * time.Second,
	}
	for tier, want := range tests {
		if got := schedule.DefaultInterval(tier); got != want {
			t.Errorf("DefaultInterval(%d) = %v, want %v", tier, got, want)
		}
	}
}

func TestDefaultJitter(t *testing.T) {
	t.Parallel()

	if got := schedule.DefaultJitter(schedule.TierReference); got != 0.10 {
		t.Errorf("reference jitter = %v", got)
	}
	for _, tier := range []schedule.Tier{schedule.TierCritical, schedule.TierService, schedule.TierAnchor} {
		if got := schedule.DefaultJitter(tier); got != 0.20 {
			t.Errorf("tier %d jitter = %v", tier, got)
		}
	}
}

func TestNextIsAdditiveAndBounded(t *testing.T) {
	t.Parallel()

	interval := 30 * time.Second
	if got := schedule.Next(interval, 0.20, fixed(0)); got != interval {
		t.Fatalf("rnd=0 -> %v, want %v", got, interval)
	}

	// rnd=0.5 adds half the jitter budget, never subtracts.
	got := schedule.Next(interval, 0.20, fixed(0.5))
	if want := interval + 3*time.Second; got != want {
		t.Fatalf("rnd=0.5 -> %v, want %v", got, want)
	}

	// The upper edge stays under interval*(1+pct).
	got = schedule.Next(interval, 0.20, fixed(0.999))
	if got <= interval || got > time.Duration(float64(interval)*1.21) {
		t.Fatalf("rnd~1 -> %v, outside the additive band", got)
	}
}

func TestNextNeverGoesBelowFloor(t *testing.T) {
	t.Parallel()

	for _, interval := range []time.Duration{0, time.Second, 29 * time.Second} {
		if got := schedule.Next(interval, 0.20, fixed(0)); got < schedule.Floor {
			t.Errorf("interval %v produced %v, below the floor", interval, got)
		}
	}
}

func TestNextClampsFraction(t *testing.T) {
	t.Parallel()

	if got := schedule.Next(30*time.Second, -1, fixed(1)); got != 30*time.Second {
		t.Fatalf("negative jitter -> %v", got)
	}
	if got := schedule.Next(30*time.Second, 5, fixed(1)); got <= 30*time.Second {
		t.Fatalf("jitter fraction was not clamped: %v", got)
	}
}

func TestFirstRunStaysInsideTheFirstInterval(t *testing.T) {
	t.Parallel()

	interval := 120 * time.Second
	if got := schedule.FirstRun(interval, fixed(0)); got != 0 {
		t.Fatalf("rnd=0 -> %v", got)
	}
	got := schedule.FirstRun(interval, fixed(0.999))
	limit := time.Duration(float64(interval) * 0.95)
	if got <= 0 || got >= interval {
		t.Fatalf("first run %v outside (0, %v)", got, interval)
	}
	if got > limit {
		t.Fatalf("first run %v exceeds the 95%% spread", got)
	}
	if got := schedule.FirstRun(0, fixed(0.5)); got <= 0 || got >= schedule.Floor {
		t.Fatalf("floor-interval spread = %v", got)
	}
}

func TestDue(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	if !schedule.Due(now, time.Time{}, time.Minute) {
		t.Fatal("a target that never ran is due")
	}
	if schedule.Due(now, now.Add(-30*time.Second), time.Minute) {
		t.Fatal("not due yet")
	}
	if !schedule.Due(now, now.Add(-2*time.Minute), time.Minute) {
		t.Fatal("overdue target should be due")
	}
	// The floor applies even if a caller passes a short interval.
	if !schedule.Due(now, now.Add(-31*time.Second), time.Second) {
		t.Fatal("floor should bound the comparison interval")
	}
}

func TestRampRate(t *testing.T) {
	t.Parallel()

	const start, ceiling = 2.0, 20.0
	ten := 10 * time.Minute

	if got := schedule.RampRate(0, ten, start, ceiling); got != start {
		t.Fatalf("t=0 -> %v", got)
	}
	if got := schedule.RampRate(ten/2, ten, start, ceiling); got != 11 {
		t.Fatalf("midpoint -> %v, want 11", got)
	}
	if got := schedule.RampRate(ten, ten, start, ceiling); got != ceiling {
		t.Fatalf("end -> %v", got)
	}
	if got := schedule.RampRate(2*ten, ten, start, ceiling); got != ceiling {
		t.Fatalf("after -> %v", got)
	}
	if got := schedule.RampRate(-time.Second, ten, start, ceiling); got != start {
		t.Fatalf("negative elapsed -> %v", got)
	}
	if got := schedule.RampRate(time.Second, 0, start, ceiling); got != ceiling {
		t.Fatalf("zero duration -> %v", got)
	}
	if got := schedule.RampRate(time.Second, ten, 20, 5); got != 20 {
		t.Fatalf("ceiling below start should not invert: %v", got)
	}
}

func TestHostKey(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"chase.com":                "chase.com",
		"www.chase.com":            "chase.com",
		"Chase.COM.":               "chase.com",
		"foo.co.uk":                "foo.co.uk",
		"api.foo.co.uk":            "foo.co.uk",
		"a.b.c.example.com":        "example.com",
		"1.1.1.1":                  "1.1.1.1",
		"example.com:443":          "example.com",
		"localhost":                "localhost",
		"":                         "",
		"airline.example":          "airline.example",
		"www.airline.example:8443": "airline.example",
	}

	for in, want := range tests {
		if got := schedule.HostKey(in); got != want {
			t.Errorf("HostKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLimiterOnePerHost(t *testing.T) {
	t.Parallel()

	l := schedule.NewLimiter()

	release, ok := l.TryAcquire("www.chase.com")
	if !ok {
		t.Fatal("first acquire should succeed")
	}
	if l.InFlight() != 1 {
		t.Fatalf("in flight = %d", l.InFlight())
	}

	// Same registrable domain must share the slot.
	if _, ok := l.TryAcquire("chase.com"); ok {
		t.Fatal("chase.com and www.chase.com must share one slot")
	}
	// A different host is independent.
	if _, ok := l.TryAcquire("united.com"); !ok {
		t.Fatal("different hosts should not contend")
	}

	release()
	release() // idempotent
	if _, ok := l.TryAcquire("chase.com"); !ok {
		t.Fatal("slot not released")
	}
}

func TestLimiterAcquireBlocksUntilRelease(t *testing.T) {
	t.Parallel()

	l := schedule.NewLimiter()
	release, err := l.Acquire(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Acquire(ctx, "example.com"); err == nil {
		t.Fatal("a cancelled context should fail to acquire")
	}

	release()

	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	release2, err := l.Acquire(ctx2, "example.com")
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release2()
}
