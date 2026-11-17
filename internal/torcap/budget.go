// Package torcap keeps the Tor vantage inside its documented daily volume
// budget.
//
// Tor is a separate trust domain and a separate agreement: the operator's exit
// node carries traffic for everyone else on the circuit, so an unbounded
// monitor is rude in a way an ordinary client is not. The budget counts bytes
// on the SOCKS connection and stops the Tor agent until the next UTC day.
package torcap

import (
	"context"
	"net"
	"sync"
	"time"
)

// DefaultDailyLimit is 50 MB. It is deliberately small: a corroboration
// vantage probes 25 targets every ten minutes, and a small page plus a TLS
// handshake per probe is a few hundred KB per day, not megabytes.
const DefaultDailyLimit = 50 << 20

// Budget tracks bytes moved through the Tor vantage for one UTC day.
type Budget struct {
	// Limit is the daily allowance in bytes. A non-positive limit means the
	// budget is disabled, which is how a test asks for "no cap".
	Limit int64
	// Now is injectable so the day rollover is testable without sleeping.
	Now func() time.Time
	// OnExhausted runs at most once per UTC day, the first time a caller sees
	// the budget exhausted. It is for the operator-visible warning.
	OnExhausted func(used int64)

	mu       sync.Mutex
	day      string
	used     int64
	announce bool
}

// New returns a budget with the given limit.
func New(limit int64) *Budget {
	return &Budget{Limit: limit, Now: time.Now}
}

// Add records bytes moved. It is safe for concurrent use: every probe wraps its
// own connection but shares the day's total.
func (b *Budget) Add(n int64) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	b.used += n
}

// Used reports the bytes moved today.
func (b *Budget) Used() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	return b.used
}

// Exhausted reports whether today's allowance is spent. It announces the first
// exhausted observation of the day through OnExhausted.
func (b *Budget) Exhausted() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.rollLocked()
	if b.Limit <= 0 || b.used < b.Limit {
		return false
	}
	if !b.announce {
		b.announce = true
		if b.OnExhausted != nil {
			// Called under the lock on purpose: the callback is a log line, and
			// running it outside would let a second caller announce too.
			b.OnExhausted(b.used)
		}
	}
	return true
}

// Wrap returns a dial function that counts every byte read or written on the
// connections it opens.
func (b *Budget) Wrap(dial func(ctx context.Context, network, address string) (net.Conn, error)) func(ctx context.Context, network, address string) (net.Conn, error) {
	if dial == nil {
		return nil
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &countingConn{Conn: conn, budget: b}, nil
	}
}

func (b *Budget) rollLocked() {
	now := time.Now
	if b.Now != nil {
		now = b.Now
	}
	day := now().UTC().Format("2006-01-02")
	if b.day == "" {
		b.day = day
		return
	}
	if day != b.day {
		b.day = day
		b.used = 0
		b.announce = false
	}
}

// countingConn adds transferred bytes to the budget without changing behaviour.
type countingConn struct {
	net.Conn
	budget *Budget
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.budget.Add(int64(n))
	return n, err
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.budget.Add(int64(n))
	return n, err
}
