package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
)

// sampleFixture creates one US banking target and records a healthy probe, so
// SampleScopes writes global, sector, country, and ASN rows.
func sampleFixture(t *testing.T, s *store.Store, now time.Time) {
	t.Helper()

	ctx := context.Background()
	if err := s.EnsureVantage(ctx, "local", "direct", "primary"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTarget(ctx, store.Target{
		ID: "t1", Host: "chase.com", Port: 443, Probe: "http",
		Name: "Chase", Class: "service", Sector: "banking", Country: "US",
		Tier: 2, IntervalS: 300,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(ctx, []store.Probe{probeAt(now, state.Up)}); err != nil {
		t.Fatal(err)
	}
}

func historyLen(t *testing.T, s *store.Store, scope, value string, now time.Time) int {
	t.Helper()
	history, err := s.ScopeHistory(context.Background(), scope, value, now)
	if err != nil {
		t.Fatal(err)
	}
	return len(history)
}

func TestAtypicalDayIsExcludedFromBaselineSamples(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	sampleFixture(t, s, now)

	ctx := context.Background()
	if err := s.SampleScopes(ctx, "local", now); err != nil {
		t.Fatal(err)
	}
	if got := historyLen(t, s, "global", "", now); got != 1 {
		t.Fatalf("global history = %d before the mark, want 1", got)
	}

	deleted, err := s.MarkAtypicalDay(ctx, now.Format("2006-01-02"), "", "", "calendar", now)
	if err != nil {
		t.Fatal(err)
	}
	if deleted == 0 {
		t.Fatal("marking a day that already had samples deleted nothing")
	}
	if got := historyLen(t, s, "global", "", now); got != 0 {
		t.Fatalf("global history = %d after the mark, want 0", got)
	}

	// Later samples on the same day are skipped, not merely deleted once.
	if err := s.SampleScopes(ctx, "local", now.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := historyLen(t, s, "global", "", now); got != 0 {
		t.Fatalf("global history = %d after a later sample, want 0", got)
	}

	exclusions, err := s.AtypicalDayScopes(ctx, now.Format("2006-01-02"))
	if err != nil {
		t.Fatal(err)
	}
	if len(exclusions) != 1 || exclusions[0].Scope != "" {
		t.Fatalf("exclusions = %+v, want one all-scope row", exclusions)
	}
}

func TestAtypicalDayRegionNarrowsToThatCountry(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	sampleFixture(t, s, now)

	ctx := context.Background()
	if err := s.SampleScopes(ctx, "local", now); err != nil {
		t.Fatal(err)
	}

	// A US-only calendar entry must not silence the global or sector baseline.
	if _, err := s.MarkAtypicalDay(ctx, now.Format("2006-01-02"), "country", "US", "calendar", now); err != nil {
		t.Fatal(err)
	}
	if got := historyLen(t, s, "country", "US", now); got != 0 {
		t.Fatalf("US history = %d after the regional mark, want 0", got)
	}
	if got := historyLen(t, s, "global", "", now); got != 1 {
		t.Fatalf("global history = %d after a US-only mark, want 1", got)
	}
	if got := historyLen(t, s, "sector", "banking", now); got != 1 {
		t.Fatalf("sector history = %d after a US-only mark, want 1", got)
	}

	// A later sample still writes the non-US scopes and still skips US.
	if err := s.SampleScopes(ctx, "local", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := historyLen(t, s, "country", "US", now); got != 0 {
		t.Fatalf("US history = %d after a later sample, want 0", got)
	}
	if got := historyLen(t, s, "global", "", now); got < 1 {
		t.Fatalf("global history lost its sample: %d", got)
	}
}

func TestMarkAtypicalDayRejectsABadDate(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	if _, err := s.MarkAtypicalDay(context.Background(), "17/11/2026", "", "", "calendar", time.Now()); err == nil {
		t.Fatal("a malformed day was accepted")
	}
}
