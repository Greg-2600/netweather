package torcap_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/torcap"
)

func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

func TestBudgetAccumulatesAndExhausts(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	budget := torcap.New(100)
	budget.Now = fixedClock(now)

	if budget.Exhausted() {
		t.Fatal("a fresh budget reported exhausted")
	}
	budget.Add(60)
	if budget.Exhausted() {
		t.Fatal("60 of 100 reported exhausted")
	}
	if got := budget.Used(); got != 60 {
		t.Fatalf("used = %d, want 60", got)
	}

	// Reaching the limit is enough: the next probe must not start.
	budget.Add(40)
	if !budget.Exhausted() {
		t.Fatal("a spent budget reported available")
	}
}

func TestBudgetResetsOnANewUTCDay(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 23, 59, 0, 0, time.UTC)
	budget := torcap.New(10)
	budget.Now = func() time.Time { return now }

	budget.Add(10)
	if !budget.Exhausted() {
		t.Fatal("budget not exhausted")
	}

	// A minute later is a new UTC day, so the allowance is fresh.
	now = now.Add(2 * time.Minute)
	if budget.Exhausted() {
		t.Fatal("the new day did not reset the budget")
	}
	if got := budget.Used(); got != 0 {
		t.Fatalf("used after rollover = %d, want 0", got)
	}
}

func TestBudgetAnnouncesOncePerDay(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	budget := torcap.New(5)
	budget.Now = func() time.Time { return now }

	announcements := 0
	budget.OnExhausted = func(used int64) {
		announcements++
		if used != 5 {
			t.Errorf("announced used = %d, want 5", used)
		}
	}

	budget.Add(5)
	for i := 0; i < 5; i++ {
		if !budget.Exhausted() {
			t.Fatal("budget not exhausted")
		}
	}
	if announcements != 1 {
		t.Fatalf("announcements = %d, want exactly one per day", announcements)
	}

	now = now.Add(24 * time.Hour)
	if budget.Exhausted() {
		t.Fatal("the next day started exhausted")
	}
	budget.Add(5)
	if !budget.Exhausted() {
		t.Fatal("budget not exhausted on the second day")
	}
	if announcements != 2 {
		t.Fatalf("announcements = %d, want one for each day", announcements)
	}
}

func TestBudgetCanBeDisabled(t *testing.T) {
	t.Parallel()

	for _, limit := range []int64{0, -1} {
		budget := torcap.New(limit)
		budget.Add(1 << 30)
		if budget.Exhausted() {
			t.Errorf("limit %d capped the budget", limit)
		}
	}
}

type fakeConn struct {
	net.Conn
	reads  int
	writes int
}

func (f *fakeConn) Read(p []byte) (int, error) {
	n := copy(p, "abcdef")
	f.reads++
	return n, nil
}

func (f *fakeConn) Write(p []byte) (int, error) {
	f.writes++
	return len(p), nil
}

func TestWrapCountsBothDirections(t *testing.T) {
	t.Parallel()

	budget := torcap.New(1 << 20)
	conn := &fakeConn{}
	dial := budget.Wrap(func(context.Context, string, string) (net.Conn, error) {
		return conn, nil
	})

	wrapped, err := dial(context.Background(), "tcp", "example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrapped.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	if _, err := wrapped.Read(buf); err != nil {
		t.Fatal(err)
	}

	if got, want := budget.Used(), int64(10+6); got != want {
		t.Fatalf("used = %d, want %d", got, want)
	}
	if conn.writes != 1 || conn.reads != 1 {
		t.Fatalf("underlying conn calls = %d writes, %d reads", conn.writes, conn.reads)
	}

	// A dial failure passes through without counting anything.
	failing := budget.Wrap(func(context.Context, string, string) (net.Conn, error) {
		return nil, context.DeadlineExceeded
	})
	if _, err := failing(context.Background(), "tcp", "x"); err == nil {
		t.Fatal("dial failure was swallowed")
	}

	if torcap.New(1).Wrap(nil) != nil {
		t.Fatal("a nil dial should stay nil")
	}
}
