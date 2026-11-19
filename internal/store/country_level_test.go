package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/store"
)

func TestCountryLevelsRoundTripAndStaleGuard(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, _ := openTest(t)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	levels, err := s.CountryLevels(ctx)
	if err != nil || len(levels) != 0 {
		t.Fatalf("empty snapshot = %v, %v; want empty", levels, err)
	}

	batch := []store.CountryLevel{
		{Country: "NE", Level: 1, Score: 56448000, EventCount: 2, Rank: 1, ObservedAt: now, Source: "ioda-summary"},
		{Country: "bh", Level: 0.73, Score: 464607.4, EventCount: 1, Rank: 2, ObservedAt: now, Source: "ioda-summary"},
		{Country: "", Level: 1, ObservedAt: now}, // skipped: no country to key on
	}
	if err := s.UpsertCountryLevels(ctx, batch); err != nil {
		t.Fatalf("UpsertCountryLevels: %v", err)
	}

	got, err := s.CountryLevels(ctx)
	if err != nil {
		t.Fatalf("CountryLevels: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("levels = %d, want 2", len(got))
	}
	ne, ok := got["NE"]
	if !ok || ne.Score != 56448000 || ne.EventCount != 2 || ne.Rank != 1 || ne.Source != "ioda-summary" {
		t.Errorf("NE = %+v", ne)
	}
	if !ne.ObservedAt.Equal(now) {
		t.Errorf("NE observed = %v, want %v", ne.ObservedAt, now)
	}

	// A newer observation replaces the row; an older one must not drag it back.
	newer := []store.CountryLevel{{
		Country: "NE", Level: 0.5, Score: 100, ObservedAt: now.Add(time.Minute), Source: "ioda-summary",
	}}
	if err := s.UpsertCountryLevels(ctx, newer); err != nil {
		t.Fatalf("upsert newer: %v", err)
	}
	if err := s.UpsertCountryLevels(ctx, batch); err != nil {
		t.Fatalf("upsert older: %v", err)
	}
	got, err = s.CountryLevels(ctx)
	if err != nil {
		t.Fatalf("CountryLevels after updates: %v", err)
	}
	if got["NE"].Score != 100 || !got["NE"].ObservedAt.Equal(now.Add(time.Minute)) {
		t.Errorf("stale observation overwrote a newer level: %+v", got["NE"])
	}
}
