package logging_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/Greg-2600/netweather/internal/logging"
)

func TestRingKeepsNewestRecords(t *testing.T) {
	t.Parallel()

	r := logging.NewRing(3, 0)
	ctx := context.Background()

	for _, msg := range []string{"one", "two", "three", "four", "five"} {
		if err := r.Handle(ctx, slog.NewRecord(testTime(), slog.LevelInfo, msg, 0)); err != nil {
			t.Fatal(err)
		}
	}

	got := r.Snapshot()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].Message != "three" || got[2].Message != "five" {
		t.Fatalf("wrong window: %q..%q", got[0].Message, got[2].Message)
	}
}

func TestRingQueryFilters(t *testing.T) {
	t.Parallel()

	r := logging.NewRing(10, 0)
	ctx := context.Background()

	info := slog.NewRecord(testTime(), slog.LevelInfo, "state change", 0)
	info.AddAttrs(slog.String("target", "chase.com"))
	debug := slog.NewRecord(testTime(), slog.LevelDebug, "probe", 0)
	debug.AddAttrs(slog.String("target", "united.com"))

	_ = r.Handle(ctx, info)
	_ = r.Handle(ctx, debug)

	byLevel := r.Query(slog.LevelInfo, "", 0)
	if len(byLevel) != 1 || byLevel[0].Message != "state change" {
		t.Fatalf("level filter returned %d records", len(byLevel))
	}

	byTarget := r.Query(logging.LevelTrace, "united", 0)
	if len(byTarget) != 1 || byTarget[0].Attrs["target"] != "united.com" {
		t.Fatalf("target filter returned %d records", len(byTarget))
	}

	limited := r.Query(logging.LevelTrace, "", 1)
	if len(limited) != 1 || limited[0].Message != "probe" {
		t.Fatalf("limit should keep the newest record, got %d", len(limited))
	}
}

func TestRingGroupsAndRedaction(t *testing.T) {
	t.Parallel()

	r := logging.NewRing(5, 0)
	handler := r.WithAttrs([]slog.Attr{slog.String("vantage", "local")}).WithGroup("probe")

	rec := slog.NewRecord(testTime(), slog.LevelInfo, "attempt", 0)
	rec.AddAttrs(slog.String("token", "leak-me"), slog.Int("status", 200))

	if err := handler.Handle(context.Background(), rec); err != nil {
		t.Fatal(err)
	}

	got := r.Snapshot()
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	// WithAttrs ran before WithGroup, so "vantage" is not nested (slog semantics).
	if got[0].Attrs["vantage"] != "local" {
		t.Fatalf("base attr lost: %v", got[0].Attrs)
	}
	if got[0].Attrs["probe.token"] != "[REDACTED]" {
		t.Fatalf("grouped secret not redacted: %v", got[0].Attrs)
	}
	if got[0].Attrs["probe.status"] != "200" {
		t.Fatalf("grouped int not flattened: %v", got[0].Attrs)
	}
}

func TestRingByteCap(t *testing.T) {
	t.Parallel()

	// 1 KB cap, records of ~200 bytes: the ring must evict rather than grow.
	r := logging.NewRing(1000, 1)
	for i := 0; i < 50; i++ {
		rec := slog.NewRecord(testTime(), slog.LevelInfo, string(make([]byte, 200)), 0)
		if err := r.Handle(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
	}

	got := r.Snapshot()
	if len(got) == 0 {
		t.Fatal("byte cap evicted everything")
	}
	if len(got) >= 50 {
		t.Fatalf("byte cap did not evict: %d records retained", len(got))
	}
}

func TestRingConcurrentHandlers(t *testing.T) {
	t.Parallel()

	r := logging.NewRing(500, 0)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = r.Handle(context.Background(), slog.NewRecord(testTime(), slog.LevelInfo, "line", 0))
			}
		}()
	}
	wg.Wait()

	if got := r.Len(); got != 500 {
		t.Fatalf("Len = %d, want 500", got)
	}
}
