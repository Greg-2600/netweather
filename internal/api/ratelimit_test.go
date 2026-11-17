package api

import (
	"testing"
	"time"
)

func TestRateLimitBurstThenRefill(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 16, 3, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	b := newBucket(1, 3, clock)
	for i := 0; i < 3; i++ {
		if !b.allow() {
			t.Fatalf("request %d denied inside the burst", i+1)
		}
	}
	if b.allow() {
		t.Fatal("request 4 should be denied")
	}

	now = now.Add(500 * time.Millisecond)
	if b.allow() {
		t.Fatal("half a token is not a token")
	}

	now = now.Add(1500 * time.Millisecond)
	if !b.allow() {
		t.Fatal("token did not refill after two seconds")
	}
}

func TestRateLimitNeverExceedsBurst(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 16, 3, 0, 0, 0, time.UTC)
	b := newBucket(10, 2, func() time.Time { return now })

	if !b.allow() {
		t.Fatal("burst not available (first token)")
	}
	if !b.allow() {
		t.Fatal("burst not available (second token)")
	}
	now = now.Add(time.Hour) // long idle must not accumulate beyond burst
	if !b.allow() {
		t.Fatal("refill after idle should give the first burst token")
	}
	if !b.allow() {
		t.Fatal("refill after idle should give the second burst token")
	}
	if b.allow() {
		t.Fatal("bucket exceeded its burst cap")
	}
}
