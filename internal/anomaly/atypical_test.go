package anomaly_test

import (
	"context"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/anomaly"
	"github.com/Greg-2600/netweather/internal/state"
)

func TestBothDirectionDayCallsOnAtypicalDay(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	src := newSource(map[state.State]int{state.Up: 40, state.Down: 10}) // 0.80
	sink := &fakeSink{}

	var days []string
	ev := anomaly.New(src, sink, "local")
	ev.Now = func() time.Time { return now }
	ev.OnAtypicalDay = func(day string) { days = append(days, day) }

	// First evaluation below the band, second above it, third below again: the
	// first three of the day have now straddled the band in both directions.
	src.counts["global|"] = map[state.State]int{state.Up: 40, state.Down: 10}
	if _, err := ev.Evaluate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(days) != 0 {
		t.Fatalf("a one-directional day was marked early: %v", days)
	}

	now = now.Add(5 * time.Minute)
	src.counts["global|"] = map[state.State]int{state.Up: 48, state.Down: 2} // 0.96
	if _, err := ev.Evaluate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(days) != 0 {
		t.Fatalf("the day was marked before the third evaluation: %v", days)
	}

	now = now.Add(5 * time.Minute)
	src.counts["global|"] = map[state.State]int{state.Up: 40, state.Down: 10} // 0.80
	if _, err := ev.Evaluate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(days) != 1 || days[0] != "2026-11-17" {
		t.Fatalf("days = %v, want the UTC day once", days)
	}

	// A fourth evaluation on the same day does not repeat the callback.
	now = now.Add(5 * time.Minute)
	if _, err := ev.Evaluate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(days) != 1 {
		t.Fatalf("days = %v, want one call per day", days)
	}
}

func TestOneDirectionDayDoesNotCallOnAtypicalDay(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	src := newSource(map[state.State]int{state.Up: 40, state.Down: 10}) // always below
	sink := &fakeSink{}

	calls := 0
	ev := anomaly.New(src, sink, "local")
	ev.Now = func() time.Time { return now }
	ev.OnAtypicalDay = func(string) { calls++ }

	for i := 0; i < 4; i++ {
		if _, err := ev.Evaluate(context.Background()); err != nil {
			t.Fatal(err)
		}
		now = now.Add(5 * time.Minute)
	}
	if calls != 0 {
		t.Fatalf("a one-directional day was marked atypical %d times", calls)
	}
}
