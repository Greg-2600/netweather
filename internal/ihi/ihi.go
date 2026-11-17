// Package ihi computes the Internet Health Index (DESIGN.md §11): one 0-100
// number whose components are always available beside it.
//
// The index is a summary, never a detector. It cannot open an incident, change
// a target state, or silence a probe. Its only job is to answer "how is the
// internet doing from here" without hiding which parts of the answer are
// missing.
package ihi

import (
	"sort"
	"time"
)

// DefaultTTL is how old a component may be before it is dropped rather than
// averaged in. An hour matches the slowest truthful cadence in the system: the
// hourly fold. A component older than that is history, not a reading.
const DefaultTTL = time.Hour

// minCoverage is the share of a scope that must be measurable before the
// own-health component counts at all. Below it the component is dropped: a
// board that can see 40% of its targets must not report health for the other
// 60% (DESIGN.md §11).
const minCoverage = 0.5

// Component is one input to the index. Score runs 0-100, higher is healthier.
//
// Coverage is the share of the component's scope that was measurable. It is 1
// for components whose source is a single observation (a feed event, a corridor
// sample) and the measurable share for own health. A component with coverage
// below 50% is dropped instead of scored.
type Component struct {
	Name     string
	Weight   float64
	Score    float64
	At       time.Time
	OK       bool
	Coverage float64
	// Reason names why an unavailable component was unavailable. Empty means
	// the generic "not available", which is all a caller can honestly say
	// without knowing more.
	Reason string
}

// Part is a component that survived into the score.
type Part struct {
	Name   string
	Score  float64
	Weight float64 // effective weight, after coverage and renormalisation
	At     time.Time
}

// Dropped records a component that was left out and why. The index is never
// allowed to look complete when it is not.
type Dropped struct {
	Name   string
	Reason string
}

// Index is the answer. Score is 0 when nothing could be scored, which the
// caller states as "not enough data" rather than as a perfect zero.
type Index struct {
	Score   float64
	Parts   []Part
	Dropped []Dropped
}

// Options controls one computation. Zero values are sensible defaults.
type Options struct {
	Now time.Time
	TTL time.Duration
}

// Compute applies the DESIGN.md §11 rules:
//
//   - a component that is unavailable, stale, unweighted, or below the coverage
//     floor is dropped and named
//   - the own-health component's weight is multiplied by its coverage, so a
//     scope that is 60% measurable contributes 60% weight; below 50% it is
//     dropped entirely
//   - the remaining weights are renormalised, so a missing component does not
//     quietly drag the score toward zero
//
// The result is deterministic: parts are ordered by descending weight, then by
// name, so the same inputs always render the same way.
func Compute(components []Component, opt Options) Index {
	now := opt.Now
	if now.IsZero() {
		now = time.Now()
	}
	ttl := opt.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}

	out := Index{Parts: make([]Part, 0, len(components)), Dropped: make([]Dropped, 0)}

	type weighted struct {
		name   string
		score  float64
		weight float64
		at     time.Time
	}
	kept := make([]weighted, 0, len(components))
	total := 0.0

	for _, c := range components {
		switch {
		case c.Weight <= 0:
			out.Dropped = append(out.Dropped, Dropped{c.Name, "no weight"})
			continue
		case !c.OK:
			reason := c.Reason
			if reason == "" {
				reason = "not available"
			}
			out.Dropped = append(out.Dropped, Dropped{c.Name, reason})
			continue
		case c.At.IsZero():
			out.Dropped = append(out.Dropped, Dropped{c.Name, "no timestamp"})
			continue
		case now.Sub(c.At) > ttl:
			out.Dropped = append(out.Dropped, Dropped{c.Name, "stale"})
			continue
		case c.Score < 0:
			out.Dropped = append(out.Dropped, Dropped{c.Name, "no score"})
			continue
		case c.Coverage < minCoverage:
			out.Dropped = append(out.Dropped, Dropped{c.Name, "coverage below 50%"})
			continue
		}

		effective := c.Weight * c.Coverage
		kept = append(kept, weighted{c.Name, clamp(c.Score), effective, c.At})
		total += effective
	}

	if total <= 0 {
		// Nothing is scoreable. A zero with an empty part list is the honest
		// answer; the caller must not render it as "everything is down".
		out.Score = 0
		return out
	}

	sum := 0.0
	for _, k := range kept {
		share := k.weight / total
		sum += share * k.score
		out.Parts = append(out.Parts, Part{
			Name:   k.name,
			Score:  k.score,
			Weight: share,
			At:     k.at,
		})
	}
	sort.SliceStable(out.Parts, func(i, j int) bool {
		if out.Parts[i].Weight != out.Parts[j].Weight {
			return out.Parts[i].Weight > out.Parts[j].Weight
		}
		return out.Parts[i].Name < out.Parts[j].Name
	})
	out.Score = clamp(sum)
	return out
}

// clamp caps a score at 100. Negative components are dropped before they reach
// the mean, so a weighted mean of kept scores cannot be negative and there is no
// lower branch to test.
func clamp(score float64) float64 {
	if score > 100 {
		return 100
	}
	return score
}
