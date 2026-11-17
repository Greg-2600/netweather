package api

import (
	"math"
	"net/http"
	"sync"
	"time"
)

// bucket is a token bucket: a long-run rate with a short burst allowance.
// A fixed window would allow 60 requests in the last millisecond of one window
// and 60 in the first of the next, which is the opposite of smoothing.
type bucket struct {
	mu     sync.Mutex
	rate   float64 // tokens added per second
	burst  float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

func newBucket(ratePerSecond float64, burst int, now func() time.Time) *bucket {
	if burst < 1 {
		burst = 1
	}
	if now == nil {
		now = time.Now
	}
	return &bucket{
		rate:   ratePerSecond,
		burst:  float64(burst),
		tokens: float64(burst),
		last:   now(),
		now:    now,
	}
}

func (b *bucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(b.burst, b.tokens+elapsed*b.rate)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// withRateLimit enforces the documented 60 requests/minute with a burst of 20
// (docs/API.md §8). It sits outside auth so a flood of bad tokens is cheap to
// reject.
func withRateLimit(b *bucket, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !b.allow() {
			w.Header().Set("Retry-After", "1")
			writeError(w, r, http.StatusTooManyRequests, CodeRateLimited,
				"rate limit exceeded; retry after the interval in Retry-After")
			return
		}
		next.ServeHTTP(w, r)
	})
}
