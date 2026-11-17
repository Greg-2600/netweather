package store_test

import (
	"context"
	"testing"
	"time"
)

func TestTargetSourceProvenance(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	seed(t, s)
	ctx := context.Background()

	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	if err := s.RecordTargetSource(ctx, "t1", "seed:builtin", "abc123", at); err != nil {
		t.Fatal(err)
	}
	sources, err := s.TargetSources(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 {
		t.Fatalf("sources = %+v", sources)
	}
	if sources[0].Source != "seed:builtin" || sources[0].SHA256 != "abc123" ||
		!sources[0].FetchedAt.Equal(at) {
		t.Fatalf("source = %+v", sources[0])
	}

	// A later load updates the payload hash in place rather than adding a
	// second row for the same source.
	if err := s.RecordTargetSource(ctx, "t1", "seed:builtin", "def456", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	sources, err = s.TargetSources(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].SHA256 != "def456" ||
		!sources[0].FetchedAt.Equal(at.Add(time.Hour)) {
		t.Fatalf("after upsert = %+v", sources)
	}

	// A target can have more than one source (a seed and a config override);
	// the list is stable and ordered.
	if err := s.RecordTargetSource(ctx, "t1", "config:/config/targets.yaml", "ccc", at); err != nil {
		t.Fatal(err)
	}
	sources, err = s.TargetSources(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 ||
		sources[0].Source != "config:/config/targets.yaml" ||
		sources[1].Source != "seed:builtin" {
		t.Fatalf("sources = %+v", sources)
	}

	// Provenance for an unknown target is empty, not an error: a drawer must
	// not fail because a row predates the table.
	if empty, err := s.TargetSources(ctx, "missing"); err != nil || len(empty) != 0 {
		t.Fatalf("unknown target = (%+v, %v)", empty, err)
	}
}

func TestTargetSourceRejectsAnUnknownTarget(t *testing.T) {
	t.Parallel()

	s, _ := openTest(t)
	if err := s.RecordTargetSource(context.Background(), "missing", "seed:builtin", "abc", time.Now()); err == nil {
		t.Fatal("a source row for an unknown target was accepted; the foreign key should reject it")
	}
}
