package confirm_test

import (
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/confirm"
	"github.com/Greg-2600/netweather/internal/state"
)

func base() time.Time {
	return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
}

func TestClassFor(t *testing.T) {
	t.Parallel()

	for class, want := range map[string]confirm.Class{
		"reference": confirm.Reference,
		"country":   confirm.Anchor,
		"network":   confirm.Anchor,
		"service":   confirm.Service,
		"":          confirm.Service,
	} {
		if got := confirm.ClassFor(class); got != want {
			t.Errorf("ClassFor(%q) = %v, want %v", class, got, want)
		}
	}
}

func TestServiceNeedsThreeConsecutiveFailures(t *testing.T) {
	t.Parallel()

	rule := confirm.RuleFor(confirm.Service)
	now := base()
	prev := state.Up

	// The first two failures hold the previous state.
	streak := 0
	for i := 0; i < 2; i++ {
		d := confirm.Decide(confirm.Inputs{
			Rule: rule, Prev: prev, PrevSince: now.Add(-time.Hour), Candidate: state.Down,
			FailStreak: streak, Now: now,
		})
		if d.State != state.Up || !d.Held || d.Changed {
			t.Fatalf("failure %d = %+v, want a held UP", i+1, d)
		}
		streak = d.FailStreak
		if streak != i+1 {
			t.Fatalf("failure %d streak = %d", i+1, streak)
		}
	}

	third := confirm.Decide(confirm.Inputs{
		Rule: rule, Prev: prev, PrevSince: now.Add(-time.Hour), Candidate: state.Down,
		FailStreak: 2, Now: now,
	})
	if third.State != state.Down || !third.Changed || third.Held {
		t.Fatalf("third failure = %+v, want DOWN", third)
	}
}

func TestReferenceNeedsTwoOfThree(t *testing.T) {
	t.Parallel()

	rule := confirm.RuleFor(confirm.Reference)
	now := base()

	// One failure out of the window is not enough, even as the third sample.
	one := confirm.Decide(confirm.Inputs{
		Rule: rule, Prev: state.Up, PrevSince: now.Add(-time.Hour), Candidate: state.Down,
		FailStreak: 1, RecentFails: 1, Now: now,
	})
	if one.State != state.Up || !one.Held {
		t.Fatalf("one of three = %+v, want a held UP", one)
	}

	two := confirm.Decide(confirm.Inputs{
		Rule: rule, Prev: state.Up, PrevSince: now.Add(-time.Hour), Candidate: state.Down,
		RecentFails: 2, Now: now,
	})
	if two.State != state.Down {
		t.Fatalf("two of three = %+v, want DOWN", two)
	}
}

func TestAnchorNeedsTwoConsecutiveFailures(t *testing.T) {
	t.Parallel()

	rule := confirm.RuleFor(confirm.Anchor)
	now := base()

	first := confirm.Decide(confirm.Inputs{
		Rule: rule, Prev: state.Up, PrevSince: now.Add(-time.Hour), Candidate: state.Down, Now: now,
	})
	if first.State != state.Up || !first.Held {
		t.Fatalf("first = %+v, want a held UP", first)
	}

	second := confirm.Decide(confirm.Inputs{
		Rule: rule, Prev: state.Up, PrevSince: now.Add(-time.Hour), Candidate: state.Down,
		FailStreak: 1, Now: now,
	})
	if second.State != state.Down {
		t.Fatalf("second = %+v, want DOWN", second)
	}
}

func TestRecoveryThresholds(t *testing.T) {
	t.Parallel()

	now := base()

	// Service: the first pass is held, the second recovers.
	rule := confirm.RuleFor(confirm.Service)
	first := confirm.Decide(confirm.Inputs{
		Rule: rule, Prev: state.Down, PrevSince: now.Add(-time.Hour), Candidate: state.Up, Now: now,
	})
	if first.State != state.Down || !first.Held {
		t.Fatalf("first pass = %+v, want a held DOWN", first)
	}
	second := confirm.Decide(confirm.Inputs{
		Rule: rule, Prev: state.Down, PrevSince: now.Add(-time.Hour), Candidate: state.Up,
		OKStreak: 1, Now: now,
	})
	if second.State != state.Up {
		t.Fatalf("second pass = %+v, want UP", second)
	}

	// Anchor: one pass is enough.
	anchor := confirm.RuleFor(confirm.Anchor)
	recovered := confirm.Decide(confirm.Inputs{
		Rule: anchor, Prev: state.Down, PrevSince: now.Add(-time.Hour), Candidate: state.Up, Now: now,
	})
	if recovered.State != state.Up {
		t.Fatalf("anchor pass = %+v, want UP", recovered)
	}
}

func TestDebounceBlocksASecondTransition(t *testing.T) {
	t.Parallel()

	now := base()
	rule := confirm.RuleFor(confirm.Service)

	// DOWN was confirmed 30 seconds ago; a recovery with two passes is still
	// inside the 90-second window, so it is held.
	d := confirm.Decide(confirm.Inputs{
		Rule: rule, Prev: state.Down, PrevSince: now.Add(-30 * time.Second),
		Candidate: state.Up, OKStreak: 1, Now: now,
	})
	if d.State != state.Down || !d.Held {
		t.Fatalf("inside the debounce = %+v, want a held DOWN", d)
	}

	// Past the window it recovers.
	d = confirm.Decide(confirm.Inputs{
		Rule: rule, Prev: state.Down, PrevSince: now.Add(-91 * time.Second),
		Candidate: state.Up, OKStreak: 1, Now: now,
	})
	if d.State != state.Up || d.Changed != true {
		t.Fatalf("past the debounce = %+v, want UP", d)
	}
}

func TestBlockedNeedsTheSameSignatureTwice(t *testing.T) {
	t.Parallel()

	now := base()
	rule := confirm.RuleFor(confirm.Service)

	first := confirm.Decide(confirm.Inputs{
		Rule: rule, Prev: state.Up, PrevSince: now.Add(-time.Hour), Candidate: state.Blocked,
		BlockedStreak: 1, Now: now,
	})
	if first.State != state.Up || !first.Held {
		t.Fatalf("first block = %+v, want a held UP", first)
	}

	second := confirm.Decide(confirm.Inputs{
		Rule: rule, Prev: state.Up, PrevSince: now.Add(-time.Hour), Candidate: state.Blocked,
		BlockedStreak: 2, Now: now,
	})
	if second.State != state.Blocked || !second.Changed {
		t.Fatalf("second block = %+v, want BLOCKED", second)
	}

	// One non-challenge response recovers immediately, bypassing the pass
	// threshold and the debounce.
	recovered := confirm.Decide(confirm.Inputs{
		Rule: rule, Prev: state.Blocked, PrevSince: now,
		Candidate: state.Up, Now: now.Add(time.Second),
	})
	if recovered.State != state.Up || !recovered.Changed {
		t.Fatalf("recovery = %+v, want an immediate UP", recovered)
	}
}

func TestNonHealthStatesAreAdoptedImmediately(t *testing.T) {
	t.Parallel()

	now := base()
	rule := confirm.RuleFor(confirm.Service)
	for _, candidate := range []state.State{state.Unmeasurable, state.Unknown, state.ConfigError, state.Degraded} {
		d := confirm.Decide(confirm.Inputs{
			Rule: rule, Prev: state.Up, PrevSince: now, Candidate: candidate, Now: now,
		})
		if d.State != candidate || !d.Changed || d.Held {
			t.Errorf("%s = %+v, want an immediate adoption", candidate, d)
		}
	}
}

func TestFirstObservationEstablishesTheState(t *testing.T) {
	t.Parallel()

	d := confirm.Decide(confirm.Inputs{
		Rule: confirm.RuleFor(confirm.Service), Candidate: state.Down, Now: base(),
	})
	if d.State != state.Down || !d.Changed || d.Held {
		t.Fatalf("bootstrap = %+v, want the candidate adopted", d)
	}
}
