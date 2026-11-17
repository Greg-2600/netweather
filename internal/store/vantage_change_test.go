package store_test

import (
	"context"
	"testing"
	"time"
)

func TestVantageChange(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	ctx := context.Background()

	// A new install has no move to report, and that is not an error.
	if _, ok, err := s.LatestVantageChange(ctx); err != nil || ok {
		t.Fatalf("empty history = (%v, %v)", ok, err)
	}

	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	if err := s.RecordVantageChange(ctx,
		"203.0.113.9,AS7922,US", "198.51.100.4,AS9009,NL", at); err != nil {
		t.Fatal(err)
	}
	change, ok, err := s.LatestVantageChange(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("a recorded change was not found")
	}
	if !change.At.Equal(at) ||
		change.Previous != "203.0.113.9,AS7922,US" ||
		change.Current != "198.51.100.4,AS9009,NL" {
		t.Fatalf("change = %+v", change)
	}

	// The newest row wins, so a second move during the banner's day replaces
	// the sentence rather than showing yesterday's networks.
	if err := s.RecordVantageChange(ctx,
		"198.51.100.4,AS9009,NL", "192.0.2.10,AS64500,DE", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	change, ok, err = s.LatestVantageChange(ctx)
	if err != nil || !ok {
		t.Fatalf("latest = (%+v, %v, %v)", change, ok, err)
	}
	if change.Current != "192.0.2.10,AS64500,DE" || !change.At.Equal(at.Add(time.Hour)) {
		t.Fatalf("latest = %+v, want the second move", change)
	}
}
