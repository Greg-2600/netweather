package board_test

import (
	"testing"

	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/state"
)

func TestSeverityRankOrdersFailuresFirst(t *testing.T) {
	t.Parallel()

	order := []state.State{
		state.Down, state.Degraded, state.Blocked, state.Unknown,
		state.ConfigError, state.Unmeasurable, state.UpReachability,
		state.Up, state.State("future"),
	}
	for i := 1; i < len(order); i++ {
		if board.SeverityRank(order[i-1]) >= board.SeverityRank(order[i]) {
			t.Fatalf("%s should rank above %s", order[i-1], order[i])
		}
	}
}

func TestOrderClustersByAvailability(t *testing.T) {
	t.Parallel()

	in := []board.Target{
		{ID: "up", Name: "Up", Host: "up.example", State: state.Up},
		{ID: "down", Name: "Down", Host: "down.example", State: state.Down},
		{ID: "blocked", Name: "Blocked", Host: "blocked.example", State: state.Blocked},
		{ID: "degraded", Name: "Degraded", Host: "degraded.example", State: state.Degraded},
	}

	got := board.Order(in)
	want := []string{"down", "degraded", "blocked", "up"}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("position %d = %s, want %s", i, got[i].ID, id)
		}
	}
}

func TestOrderBreaksTiesByName(t *testing.T) {
	t.Parallel()

	in := []board.Target{
		{ID: "z", Name: "Zulu", Host: "z.example", State: state.Down},
		{ID: "a", Name: "Alpha", Host: "a.example", State: state.Down},
	}
	got := board.Order(in)
	if got[0].ID != "a" || got[1].ID != "z" {
		t.Fatalf("tie order = %s, %s", got[0].ID, got[1].ID)
	}
}

func TestOrderDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	in := []board.Target{
		{ID: "up", Name: "Up", Host: "up.example", State: state.Up},
		{ID: "down", Name: "Down", Host: "down.example", State: state.Down},
	}
	_ = board.Order(in)

	if in[0].ID != "up" || in[1].ID != "down" {
		t.Fatal("Order mutated its input")
	}
}

func TestOrderEmpty(t *testing.T) {
	t.Parallel()

	if got := board.Order(nil); len(got) != 0 {
		t.Fatalf("Order(nil) = %v", got)
	}
}
