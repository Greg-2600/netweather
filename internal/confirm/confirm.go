// Package confirm applies the state-transition rules from DESIGN.md §7.4.
//
// The classifier answers "what did this observation look like?". It does not
// answer "has the target changed state?", and a monitor that treats one failed
// probe as an outage is the thing this project exists to avoid. These rules are
// deliberately pure: inputs in, decision out, no clock and no database.
package confirm

import (
	"time"

	"github.com/Greg-2600/netweather/internal/state"
)

// Class is the confirmation class of a target. It is derived from the config
// class rather than stored separately, so an operator cannot set a class whose
// transition rules disagree with its role.
type Class int

const (
	// Service targets are the bulk: three consecutive failures to call DOWN.
	Service Class = iota
	// Reference targets answer "is it me?": two failures out of three.
	Reference
	// Anchor targets are country and network anchors: two consecutive failures.
	Anchor
)

// ClassFor maps config classes onto confirmation classes. Anything that is not
// a reference or an anchor is a service target, which is the stricter rule.
func ClassFor(class string) Class {
	switch class {
	case "reference":
		return Reference
	case "country", "network":
		return Anchor
	default:
		return Service
	}
}

// Rule is one class's transition table.
type Rule struct {
	// Fails is the consecutive-failure threshold when FailWindow is zero.
	Fails int
	// FailWindow and FailWindowCount express "N of M": a DOWN needs
	// FailWindowCount failures among the last FailWindow observations.
	FailWindow      int
	FailWindowCount int
	// Passes is the consecutive-pass threshold for recovering from DOWN.
	Passes int
	// Debounce is the minimum time between two confirmed transitions.
	Debounce time.Duration
}

// blockedFails is the BLOCKED threshold from DESIGN.md §7.4: two consecutive
// blocks with the same challenge signature.
const blockedFails = 2

// RuleFor returns the table row for a class.
func RuleFor(c Class) Rule {
	switch c {
	case Reference:
		return Rule{FailWindow: 3, FailWindowCount: 2, Passes: 2, Debounce: 30 * time.Second}
	case Anchor:
		return Rule{Fails: 2, Passes: 1, Debounce: 120 * time.Second}
	default:
		return Rule{Fails: 3, Passes: 2, Debounce: 90 * time.Second}
	}
}

// Inputs is one observation plus the stored history the rule needs.
type Inputs struct {
	Rule Rule
	// Prev is the stored state. Empty means this is the first observation, and
	// the caller adopts the candidate: establishing a state is not a
	// transition.
	Prev      state.State
	PrevSince time.Time
	// Candidate is what the classifier returned for this probe.
	Candidate state.State
	// FailStreak and OKStreak are the stored counters before this probe.
	FailStreak int
	OKStreak   int
	// RecentFails counts DOWN observations in the class window, including this
	// one. It is only used when Rule.FailWindow is non-zero.
	RecentFails int
	// BlockedStreak counts consecutive BLOCKED observations with the same
	// challenge signature, including this one.
	BlockedStreak int
	Now           time.Time
}

// Decision is the state to store. Held is true when a candidate transition was
// not confirmed; the caller records the probe either way, because the
// observation is real even when the state did not change.
type Decision struct {
	State      state.State
	FailStreak int
	OKStreak   int
	Changed    bool
	Held       bool
}

// Decide applies the rule table. It never invents a state: the result is always
// either the previous state or the candidate.
func Decide(in Inputs) Decision {
	d := Decision{State: in.Prev, FailStreak: in.FailStreak, OKStreak: in.OKStreak}

	// The first observation establishes the baseline. Confirmation governs
	// transitions, and there is nothing to transition from.
	if in.Prev == "" {
		d.State = in.Candidate
		d.Changed = in.Candidate != ""
		return d
	}

	switch in.Candidate {
	case state.Down:
		d.FailStreak, d.OKStreak = in.FailStreak+1, 0
	case state.Up, state.UpReachability:
		d.FailStreak, d.OKStreak = 0, in.OKStreak+1
	case state.Blocked, state.Degraded, state.Unknown, state.ConfigError, state.Unmeasurable:
		// No streak change: only a confirmed DOWN or a pass moves the counters.
	}

	// BLOCKED has its own row: two consecutive blocks with the same signature,
	// and one non-challenge response to recover.
	if in.Candidate == state.Blocked {
		if in.BlockedStreak >= blockedFails {
			return adopt(d, in, state.Blocked)
		}
		d.Held = true
		return d
	}
	if in.Prev == state.Blocked {
		return adopt(d, in, in.Candidate)
	}

	switch in.Candidate {
	case state.Down:
		confirmed := d.FailStreak >= in.Rule.Fails
		if in.Rule.FailWindow > 0 {
			confirmed = in.RecentFails >= in.Rule.FailWindowCount
		}
		if !confirmed || !debounceOK(in) {
			d.Held = true
			return d
		}
		return adopt(d, in, state.Down)

	case state.Up, state.UpReachability:
		if in.Prev == state.Down {
			if d.OKStreak < in.Rule.Passes || !debounceOK(in) {
				d.Held = true
				return d
			}
		}
		return adopt(d, in, in.Candidate)

	case state.Degraded, state.Blocked, state.Unknown, state.ConfigError, state.Unmeasurable:
		// DEGRADED, UNKNOWN, UNMEASURABLE, and CONFIG_ERROR are visibility or
		// soft signals. Holding them would hide a blind vantage or a content
		// change, which is worse than a fast transition. BLOCKED already
		// returned above.
		return adopt(d, in, in.Candidate)
	}

	// Unreachable: the cases above cover the state vocabulary. The return
	// keeps the function total if a new state is added without a rule.
	return d
}

// debounceOK reports whether enough time has passed since the previous
// transition. A zero PrevSince is a bootstrap and never blocks.
func debounceOK(in Inputs) bool {
	if in.PrevSince.IsZero() || in.Rule.Debounce <= 0 {
		return true
	}
	return in.Now.Sub(in.PrevSince) >= in.Rule.Debounce
}

func adopt(d Decision, in Inputs, next state.State) Decision {
	d.State = next
	d.Changed = next != in.Prev
	d.Held = false
	return d
}
