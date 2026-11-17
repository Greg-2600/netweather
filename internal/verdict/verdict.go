// Package verdict computes the one global verdict from DESIGN.md §3.3.
//
// The verdict is the sentence the top bar says: is this the internet, is it me,
// is it a region, or is the monitor too blind to speak. It is a summary of
// signals that already exist elsewhere, never a new detector: it changes no
// target state and opens no incident.
package verdict

import (
	"fmt"
	"time"
)

// Verdict is one of the eight labels from DESIGN.md §3.3.
type Verdict string

const (
	// OK is health within the baseline band.
	OK Verdict = "OK"
	// Incident is a dominant failure cluster, or an open incident.
	Incident Verdict = "INCIDENT"
	// Local says the monitor's own path is the problem.
	Local Verdict = "LOCAL"
	// Regional says one vantage reaches a target another cannot.
	Regional Verdict = "REGIONAL"
	// Tamper says the path is being manipulated, not merely failing.
	Tamper Verdict = "TAMPER"
	// Stale says no probe cycle completed when one should have.
	Stale Verdict = "STALE"
	// Starting says fewer than one reference cycle has completed.
	Starting Verdict = "STARTING"
	// Learning says the baseline is not usable yet.
	Learning Verdict = "LEARNING"
)

// Defaults from DESIGN.md §3.3 and §7.2.
const (
	DefaultReferenceInterval = 30 * time.Second
	DefaultTamperWindow      = time.Hour
	DefaultBaselineWindow    = 28
)

// Inputs is the evidence the rule table reads. A zero value is "unknown" and
// never counts as a positive signal: a failed read must not invent an outage or
// a tamper.
type Inputs struct {
	Now       time.Time
	StartedAt time.Time
	// LastProbe is the newest confirmed probe across enabled targets.
	LastProbe time.Time
	// ReferenceInterval is the cadence all lateness is measured against.
	ReferenceInterval time.Duration
	// Probed is how many enabled targets have a confirmed state at all.
	Probed int
	// OpenIncidents is the number of unclosed incidents.
	OpenIncidents int
	// ReferenceTotal and ReferenceFailing describe the "is it me?" set.
	ReferenceTotal   int
	ReferenceFailing int
	// TamperAt is the newest DNS TAMPER verdict.
	TamperAt     time.Time
	TamperWindow time.Duration
	// CrossVantage is true when one target is DOWN from one vantage and up
	// from another.
	CrossVantage bool
	// BaselineSamples is the usable history for the current hour-of-week
	// bucket; BaselineWindow is the target sample count.
	BaselineSamples int
	BaselineWindow  int
}

// Result is the verdict and the one line that explains it.
type Result struct {
	Verdict Verdict
	Detail  string
}

// Evaluate applies the rule table. Priority is deliberate and documented:
// a blind monitor outranks every interpretation, a tamper outranks a plain
// outage, and the local path outranks a cluster that may be its shadow.
func Evaluate(in Inputs) Result {
	refInterval := in.ReferenceInterval
	if refInterval <= 0 {
		refInterval = DefaultReferenceInterval
	}
	tamperWindow := in.TamperWindow
	if tamperWindow <= 0 {
		tamperWindow = DefaultTamperWindow
	}
	baselineWindow := in.BaselineWindow
	if baselineWindow <= 0 {
		baselineWindow = DefaultBaselineWindow
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}

	// Less than one full cycle since the process started, with nothing
	// confirmed yet: a skeleton, not an outage. A process that has been up
	// longer than one cycle with no probe at all is STALE below, because a
	// scheduler that never ran is a failure rather than a startup.
	if in.Probed == 0 && in.LastProbe.IsZero() && now.Sub(in.StartedAt) < refInterval {
		return Result{Starting, "no probe cycle has completed yet"}
	}

	// A monitor that is not probing cannot speak about anything else.
	if in.LastProbe.IsZero() {
		return Result{Stale, "no probe cycle has completed"}
	}
	if late := now.Sub(in.LastProbe); late > 3*refInterval {
		return Result{Stale, fmt.Sprintf("no probe in %s (reference cadence %s)",
			late.Round(time.Second), refInterval.Round(time.Second))}
	}

	if !in.TamperAt.IsZero() && now.Sub(in.TamperAt) <= tamperWindow {
		return Result{Tamper, "DNS tamper verdict at " +
			in.TamperAt.UTC().Format("15:04")}
	}

	if in.ReferenceTotal >= 3 && in.ReferenceFailing >= 2 {
		return Result{Local, fmt.Sprintf(
			"%d of %d reference targets failing; this is likely your path",
			in.ReferenceFailing, in.ReferenceTotal)}
	}

	if in.CrossVantage {
		return Result{Regional, "the same target is down from one vantage and up from another"}
	}

	if in.OpenIncidents > 0 {
		return Result{Incident, fmt.Sprintf("%d open incident(s)", in.OpenIncidents)}
	}

	if in.BaselineSamples < baselineWindow {
		return Result{Learning, fmt.Sprintf("learning %d/%d baseline samples",
			in.BaselineSamples, baselineWindow)}
	}

	return Result{OK, "within the baseline band"}
}
