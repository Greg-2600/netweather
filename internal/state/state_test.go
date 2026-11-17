package state_test

import (
	"strings"
	"testing"

	"github.com/Greg-2600/netweather/internal/state"
)

func TestText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		state state.State
		want  string
	}{
		{state.Up, "Up"},
		{state.UpReachability, "Up (reachability only)"},
		{state.Degraded, "Degraded"},
		{state.Down, "Down"},
		{state.Blocked, "Blocked"},
		{state.ConfigError, "Config error"},
		{state.Unmeasurable, "Unmeasurable"},
		{state.Unknown, "Unknown"},
		{state.State("something-new"), "Unknown"},
	}

	for _, tc := range tests {
		if got := tc.state.Text(); got != tc.want {
			t.Errorf("%q.Text() = %q, want %q", tc.state, got, tc.want)
		}
	}
}

// Every state needs a distinct, non-empty glyph: colour alone is not sufficient
// (DESIGN.md §10.7), and two states sharing a glyph is the same failure.
func TestGlyphIsDistinctAndNonEmpty(t *testing.T) {
	t.Parallel()

	all := []state.State{
		state.Up, state.UpReachability, state.Degraded, state.Down,
		state.Blocked, state.ConfigError, state.Unmeasurable, state.Unknown,
	}

	seen := make(map[string]state.State, len(all))
	for _, s := range all {
		glyph := s.Glyph()
		if strings.TrimSpace(glyph) == "" {
			t.Errorf("%q has no glyph", s)
			continue
		}
		if other, dup := seen[glyph]; dup {
			t.Errorf("%q and %q share glyph %q", s, other, glyph)
		}
		seen[glyph] = s
	}
}

func TestCSSClass(t *testing.T) {
	t.Parallel()

	if got := state.Down.CSSClass(); got != "state-down" {
		t.Fatalf("Down.CSSClass() = %q", got)
	}
	if got := state.UpReachability.CSSClass(); got != "state-up_reachability" {
		t.Fatalf("UpReachability.CSSClass() = %q", got)
	}
}

func TestValuesAreStable(t *testing.T) {
	t.Parallel()

	// These strings are persisted in SQLite, sent over SSE, and used as CSS
	// classes. Changing one is a migration, not a refactor.
	want := map[state.State]string{
		state.Up:             "up",
		state.UpReachability: "up_reachability",
		state.Degraded:       "degraded",
		state.Down:           "down",
		state.Blocked:        "blocked",
		state.Unknown:        "unknown",
		state.ConfigError:    "config_error",
		state.Unmeasurable:   "unmeasurable",
	}
	for s, value := range want {
		if string(s) != value {
			t.Errorf("state %q = %q, want %q", s, string(s), value)
		}
	}
}
