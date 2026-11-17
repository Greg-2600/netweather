package board_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
)

func TestStoreSourceReadsTheBoard(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	if err := st.EnsureVantage(ctx, "local", "direct", "primary"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertTarget(ctx, store.Target{
		ID: "t1", Host: "chase.com", Port: 443, Probe: "http",
		Name: "Chase", Class: "service", Sector: "financial/banking", Tier: 2, IntervalS: 300,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertTarget(ctx, store.Target{
		ID: "t2", Host: "united.com", Port: 443, Probe: "http",
		Name: "United", Class: "service", Sector: "airline", Tier: 2, IntervalS: 300,
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	if err := st.Record(ctx, []store.Probe{{
		TargetID: "t1", VantageID: "local", TS: now,
		State: state.Up, Stage: "http", Confidence: "high",
	}}); err != nil {
		t.Fatal(err)
	}

	src := &board.StoreSource{Store: st, Vantage: "local", Changes: board.NewBroadcast()}
	targets, err := src.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(targets))
	}

	byID := map[string]board.Target{}
	for _, tg := range targets {
		byID[tg.ID] = tg
	}
	if byID["t1"].State != state.Up {
		t.Errorf("t1 state = %s", byID["t1"].State)
	}
	if byID["t1"].Sector != "financial/banking" {
		t.Errorf("sector = %q", byID["t1"].Sector)
	}
	// Never-probed targets are visible and UNKNOWN, not missing: a missing tile
	// is indistinguishable from a deleted target.
	if byID["t2"].State != state.Unknown {
		t.Errorf("t2 state = %s, want unknown", byID["t2"].State)
	}
}

func TestStoreSourceClosedReturnsError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	src := &board.StoreSource{Store: st, Vantage: "local"}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := src.Snapshot(ctx); err == nil {
		t.Fatal("a closed store should surface an error, not an empty board")
	}
}

func TestStoreSourceWithoutBroadcastStillSubscribes(t *testing.T) {
	t.Parallel()

	src := &board.StoreSource{}
	ch, cancel := src.Subscribe()
	if ch == nil {
		t.Fatal("Subscribe returned a nil channel")
	}
	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed by cancel")
	}
}

func TestBroadcastCoalescesAndCancels(t *testing.T) {
	t.Parallel()

	b := board.NewBroadcast()
	ch, cancel := b.Subscribe()

	b.Notify()
	b.Notify() // coalesced into the same pending signal

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("no notification")
	}

	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after cancel")
	}
	b.Notify() // must not panic on a closed subscriber
	cancel()   // idempotent
}
