package store_test

import (
	"context"
	"testing"

	"github.com/Greg-2600/netweather/internal/store"
)

// TestUpsertReenablesADisabledTarget pins the recovery path the API and the
// abuse-response guide promise: disable stops probing without deleting the row,
// and a later config load brings the target back.
func TestUpsertReenablesADisabledTarget(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	ctx := context.Background()

	target := store.Target{
		ID: "t1", Host: "chase.com", Port: 443, Probe: "http",
		Name: "Chase", Class: "service", Sector: "financial/banking",
		Tier: 2, IntervalS: 300,
	}
	if err := s.UpsertTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	if err := s.DisableTarget(ctx, "t1"); err != nil {
		t.Fatal(err)
	}

	exists, err := s.TargetExists(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("the target was still enabled after DisableTarget")
	}

	// A load upserts the same id. The row was never deleted, so this is the
	// documented way back, not a new target with a new history.
	if err := s.UpsertTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	exists, err = s.TargetExists(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("a config load did not bring the target back")
	}

	ids, err := s.TargetIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("enabled ids = %v, want t1", ids)
	}
}
