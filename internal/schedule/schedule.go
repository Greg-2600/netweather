// Package schedule holds the pure timing rules from DESIGN.md §6: tier
// intervals, additive jitter, the hard floor, cold-start spread, the global
// ramp, and the per-host concurrency limit.
//
// It has no clock and no goroutines of its own. Every function takes the current
// time or a random source, which is what makes the scheduler testable without
// sleeping.
package schedule

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"
)

// Tier is the scheduling class from DESIGN.md §6.1.
type Tier int

const (
	TierReference Tier = iota // 0: the "is it me?" set
	TierCritical              // 1: top service targets, corridors
	TierService               // 2: the bulk
	TierAnchor                // 3: country and network anchors
)

// Floor is the minimum interval per host, enforced in code. It is applied after
// jitter, so no configuration or random draw can go below it.
const Floor = 30 * time.Second

// DefaultInterval returns the tier's default interval. Tier 2 is configurable
// from 300s to 600s; 300s is the default and the load model uses 450s as the
// midpoint.
func DefaultInterval(t Tier) time.Duration {
	switch t {
	case TierReference:
		return 30 * time.Second
	case TierCritical:
		return 120 * time.Second
	case TierAnchor:
		return 120 * time.Second
	default:
		return 300 * time.Second
	}
}

// DefaultJitter returns the one-sided jitter fraction for a tier.
func DefaultJitter(t Tier) float64 {
	if t == TierReference {
		return 0.10
	}
	return 0.20
}

// Next returns the delay before a target's next probe:
//
//	interval + rand(0, pct*interval), then clamped to Floor
//
// Jitter is additive only. Subtractive jitter would push tier 0 below the 30s
// floor, and the floor is the one guardrail that has to be literally true.
func Next(interval time.Duration, pct float64, rnd func() float64) time.Duration {
	interval = clampFloor(interval)
	pct = clampFraction(pct)

	spread := time.Duration(float64(interval) * pct * unit(rnd))
	d := interval + spread
	if d < Floor {
		return Floor
	}
	return d
}

// FirstRun returns the cold-start offset: uniformly spread across the first
// interval so the whole fleet does not fire in the same second.
func FirstRun(interval time.Duration, rnd func() float64) time.Duration {
	interval = clampFloor(interval)
	return time.Duration(float64(interval) * 0.95 * unit(rnd))
}

// Due reports whether a target is due, given its last run.
func Due(now, last time.Time, interval time.Duration) bool {
	if last.IsZero() {
		return true
	}
	return now.Sub(last) >= clampFloor(interval)
}

// RampRate is the global request ceiling during cold start: linear from start to
// ceiling over duration, then flat. It prevents a restart from producing a burst
// that looks like a scan.
func RampRate(elapsed, duration time.Duration, start, ceiling float64) float64 {
	if ceiling < start {
		ceiling = start
	}
	if duration <= 0 || elapsed >= duration {
		return ceiling
	}
	if elapsed <= 0 {
		return start
	}
	progress := float64(elapsed) / float64(duration)
	return start + (ceiling-start)*progress
}

func clampFloor(interval time.Duration) time.Duration {
	if interval < Floor {
		return Floor
	}
	return interval
}

func clampFraction(p float64) float64 {
	switch {
	case p < 0:
		return 0
	case p > 1:
		return 1
	default:
		return p
	}
}

func unit(rnd func() float64) float64 {
	if rnd == nil {
		return 0.5
	}
	v := rnd()
	switch {
	case v < 0:
		return 0
	case v >= 1:
		return 0.999999
	default:
		return v
	}
}

// HostKey is the per-host concurrency key: the registrable domain, so
// chase.com and www.chase.com share one slot (DESIGN.md §6.2).
//
// IP literals, single-label hosts and public suffixes that cannot be reduced
// are returned unchanged; they are rare in the target list and sharing a slot is
// the safe direction.
func HostKey(host string) string {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if h == "" {
		return ""
	}
	if ipHost, _, err := net.SplitHostPort(h); err == nil {
		h = ipHost
	}
	if net.ParseIP(h) != nil {
		return h
	}
	registrable, err := publicsuffix.EffectiveTLDPlusOne(h)
	if err != nil {
		return h
	}
	return registrable
}

// Limiter enforces one in-flight probe per host. It is per process and
// intentionally never evicts: the target list is capped at 2000, so the map is
// bounded by configuration.
type Limiter struct {
	mu    sync.Mutex
	slots map[string]chan struct{}
}

// NewLimiter returns an empty limiter.
func NewLimiter() *Limiter {
	return &Limiter{slots: make(map[string]chan struct{})}
}

func (l *Limiter) slot(host string) chan struct{} {
	key := HostKey(host)
	l.mu.Lock()
	defer l.mu.Unlock()
	ch, ok := l.slots[key]
	if !ok {
		ch = make(chan struct{}, 1)
		l.slots[key] = ch
	}
	return ch
}

// Acquire blocks until the host has a free slot or ctx is done. The returned
// release function is idempotent.
func (l *Limiter) Acquire(ctx context.Context, host string) (func(), error) {
	ch := l.slot(host)
	select {
	case ch <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-ch }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TryAcquire is the non-blocking form used by the dispatcher.
func (l *Limiter) TryAcquire(host string) (func(), bool) {
	ch := l.slot(host)
	select {
	case ch <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-ch }) }, true
	default:
		return nil, false
	}
}

// InFlight reports the number of hosts currently holding a slot. Diagnostics
// only.
func (l *Limiter) InFlight() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, ch := range l.slots {
		n += len(ch)
	}
	return n
}
