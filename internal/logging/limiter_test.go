package logging_test

import (
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/logging"
)

func TestLimiterEmitsUpToMaxPerWindow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 16, 3, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	l := logging.NewLimiterWithClock(time.Minute, 2, clock)

	if !l.Allow("chase.com/tls") {
		t.Fatal("first event should be allowed")
	}
	if !l.Allow("chase.com/tls") {
		t.Fatal("second event should be allowed")
	}
	if l.Allow("chase.com/tls") {
		t.Fatal("third event should be suppressed")
	}
	if got := l.Suppressed("chase.com/tls"); got != 1 {
		t.Fatalf("Suppressed = %d, want 1", got)
	}
}

func TestLimiterResetsAfterWindow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 16, 3, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	l := logging.NewLimiterWithClock(time.Minute, 1, clock)
	_ = l.Allow("k")
	if l.Allow("k") {
		t.Fatal("second event inside the window should be suppressed")
	}

	now = now.Add(61 * time.Second)
	if !l.Allow("k") {
		t.Fatal("event after the window should be allowed")
	}
}

func TestLimiterKeysAreIndependent(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 16, 3, 0, 0, 0, time.UTC)
	l := logging.NewLimiterWithClock(time.Minute, 1, func() time.Time { return now })

	_ = l.Allow("a")
	_ = l.Allow("a")
	if !l.Allow("b") {
		t.Fatal("a different key must have its own budget")
	}
}

func TestLimiterSuppressedUnknownKeyIsZero(t *testing.T) {
	t.Parallel()

	l := logging.NewLimiter(time.Minute, 1)
	if got := l.Suppressed("never-seen"); got != 0 {
		t.Fatalf("Suppressed = %d, want 0", got)
	}
}
