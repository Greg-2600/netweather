package logging

import (
	"sync"
	"time"
)

// Limiter suppresses repeated identical events.
//
// A mass outage produces the same line for hundreds of targets in the same
// second; 750 copies of "tls handshake failed" is not more information than one,
// it is a denial of service against the operator reading the log. The first
// `max` events per key per window are emitted; the rest are counted and can be
// reported once as a summary.
type Limiter struct {
	mu     sync.Mutex
	window time.Duration
	max    int
	now    func() time.Time
	seen   map[string]*limiterEntry
	calls  int
}

type limiterEntry struct {
	start      time.Time
	count      int
	suppressed int
}

// NewLimiter creates a limiter allowing max emissions per key per window.
func NewLimiter(window time.Duration, max int) *Limiter {
	return NewLimiterWithClock(window, max, time.Now)
}

// NewLimiterWithClock is NewLimiter with an injectable clock, so the window
// behaviour is testable and so a future scheduler can share one clock source.
func NewLimiterWithClock(window time.Duration, max int, now func() time.Time) *Limiter {
	if window <= 0 {
		window = time.Minute
	}
	if max < 1 {
		max = 1
	}
	if now == nil {
		now = time.Now
	}
	return &Limiter{window: window, max: max, now: now, seen: make(map[string]*limiterEntry)}
}

// Allow reports whether this event should be logged now.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.calls++
	if l.calls%1024 == 0 {
		l.pruneLocked(now)
	}

	e, ok := l.seen[key]
	if !ok || now.Sub(e.start) >= l.window {
		l.seen[key] = &limiterEntry{start: now, count: 1}
		return true
	}
	if e.count < l.max {
		e.count++
		return true
	}
	e.suppressed++
	return false
}

// Suppressed reports how many events for key were dropped in the current window.
func (l *Limiter) Suppressed(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.seen[key]; ok {
		return e.suppressed
	}
	return 0
}

func (l *Limiter) pruneLocked(now time.Time) {
	cutoff := 2 * l.window
	for k, e := range l.seen {
		if now.Sub(e.start) > cutoff {
			delete(l.seen, k)
		}
	}
}
