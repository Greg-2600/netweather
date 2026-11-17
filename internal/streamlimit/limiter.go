// Package streamlimit caps concurrent long-lived responses.
//
// SSE is the one route that holds a connection open and a goroutine with it.
// Without a cap, a client that never reads can pin server memory and an FD;
// without a per-IP cap, one host can take the whole budget (THREAT_MODEL.md
// T10).
package streamlimit

import "sync"

// Defaults are deliberately small: a self-hosted dashboard has one operator and
// a handful of browser tabs, and an API client that needs more than this is
// better served by polling than by holding 64 sockets.
const (
	DefaultTotal = 64
	DefaultPerIP = 8
)

// Limiter is a concurrent-connection limiter. The zero value is not usable;
// call New.
type Limiter struct {
	mu       sync.Mutex
	total    int
	perIP    map[string]int
	maxTotal int
	maxPerIP int
}

// New returns a limiter. Non-positive arguments fall back to the defaults, so a
// misconfiguration cannot silently disable the cap.
func New(maxTotal, maxPerIP int) *Limiter {
	if maxTotal <= 0 {
		maxTotal = DefaultTotal
	}
	if maxPerIP <= 0 {
		maxPerIP = DefaultPerIP
	}
	return &Limiter{
		perIP:    make(map[string]int),
		maxTotal: maxTotal,
		maxPerIP: maxPerIP,
	}
}

// Acquire reserves a slot for ip. It returns a release function and whether a
// slot was granted. Release is idempotent: calling it twice must not double
// decrement, because handlers defer it and may also close early.
func (l *Limiter) Acquire(ip string) (release func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.total >= l.maxTotal || l.perIP[ip] >= l.maxPerIP {
		return func() {}, false
	}
	l.total++
	l.perIP[ip]++

	released := false
	release = func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if released {
			return
		}
		released = true
		l.total--
		if l.perIP[ip] <= 1 {
			delete(l.perIP, ip)
			return
		}
		l.perIP[ip]--
	}
	return release, true
}

// Counts reports the current totals. It exists for tests and diagnostics.
func (l *Limiter) Counts() (total int, perIP map[string]int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := make(map[string]int, len(l.perIP))
	for ip, n := range l.perIP {
		out[ip] = n
	}
	return l.total, out
}
