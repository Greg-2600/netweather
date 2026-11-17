// Package anomaly runs the baseline-relative detector. It samples each scope's
// current health against its own hour-of-week history; two consecutive buckets
// below the band (and at least two percentage points down) produce one event.
package anomaly

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Greg-2600/netweather/internal/baseline"
	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
)

// Source supplies scopes, current counts, and hour-of-week history.
type Source interface {
	Scopes(ctx context.Context) ([]store.ScopeRef, error)
	ScopeStatesFresh(ctx context.Context, vantageID, scope, value string, now time.Time) (map[state.State]int, int, error)
	ScopeHistory(ctx context.Context, scope, value string, at time.Time) ([]float64, error)
	// ScopeCoverageHistory is the same window for the measurable share. A
	// coverage drop is its own advisory (DESIGN.md §7.2).
	ScopeCoverageHistory(ctx context.Context, scope, value string, at time.Time) ([]float64, error)
}

// Sink records detections.
type Sink interface {
	RecordAnomaly(ctx context.Context, a store.Anomaly) error
}

// Evaluator holds the per-scope detector state between ticks.
type Evaluator struct {
	Source   Source
	Sink     Sink
	Vantage  string
	Cooldown time.Duration
	Now      func() time.Time

	detectors map[string]*baseline.Detector
	wideners  map[string]*baseline.DayWidener
	last      map[string]time.Time

	// OnAtypicalDay is called once per UTC day when the both-direction rule
	// marks the day atypical, so the caller can exclude its samples from the
	// baseline (DESIGN.md §7.2). The callback owns logging and error handling;
	// detection must not fail because a bookkeeping write did.
	OnAtypicalDay func(day string)
	atypicalDays  map[string]struct{}

	// Coverage has its own detectors, widening, and cooldown: a health drop and
	// a visibility drop are different advisories and must not suppress each
	// other (DESIGN.md §7.2).
	coverageDetectors map[string]*baseline.Detector
	coverageWideners  map[string]*baseline.DayWidener
	coverageLast      map[string]time.Time
}

// New builds an evaluator with the documented cooldown.
func New(src Source, sink Sink, vantage string) *Evaluator {
	return &Evaluator{
		Source:    src,
		Sink:      sink,
		Vantage:   vantage,
		Cooldown:  30 * time.Minute,
		Now:       time.Now,
		detectors: make(map[string]*baseline.Detector),
		wideners:  make(map[string]*baseline.DayWidener),
		last:      make(map[string]time.Time),

		atypicalDays: make(map[string]struct{}),

		coverageDetectors: make(map[string]*baseline.Detector),
		coverageWideners:  make(map[string]*baseline.DayWidener),
		coverageLast:      make(map[string]time.Time),
	}
}

// Evaluate runs one detection pass and returns how many anomalies were recorded.
func (e *Evaluator) Evaluate(ctx context.Context) (int, error) {
	refs, err := e.Source.Scopes(ctx)
	if err != nil {
		return 0, err
	}

	now := e.now()
	triggered := 0

	for _, ref := range refs {
		key := ref.Scope + "|" + ref.Value

		counts, judgeable, err := e.Source.ScopeStatesFresh(ctx, e.Vantage, ref.Scope, ref.Value, now)
		if err != nil {
			return triggered, err
		}
		if judgeable == 0 {
			continue
		}

		measurable := counts[state.Up] + counts[state.UpReachability] +
			counts[state.Degraded] + counts[state.Down]
		coverage := float64(measurable) / float64(judgeable)

		// A collapse in coverage is the advisory that separates a mass bot-wall
		// rollout from a mass outage, so it is checked before the health floors:
		// the case that matters is exactly the one where the health detector
		// must stay silent (DESIGN.md §7.2).
		fired, err := e.evaluateCoverage(ctx, ref, key, coverage, now)
		if err != nil {
			return triggered, err
		}
		if fired {
			triggered++
		}

		// Below these floors the scope is UNKNOWN, not unhealthy. Running the
		// detector on it would turn a visibility failure into an incident.
		if measurable < 8 || coverage < 0.5 {
			continue
		}

		healthy := counts[state.Up] + counts[state.UpReachability] + counts[state.Degraded]
		health := float64(healthy) / float64(measurable)

		history, err := e.Source.ScopeHistory(ctx, ref.Scope, ref.Value, now)
		if err != nil {
			return triggered, err
		}
		stats := baseline.Compute(history)
		if !stats.Usable() {
			// DESIGN.md §7.2's LEARNING row: with no usable baseline the
			// detector still applies an absolute floor, so a monitor does not
			// wait 21 weeks before saying that half the internet is down. It
			// stays an anomaly, not an incident: §3.3 says LEARNING never
			// emits INCIDENT.
			floor, ok := learningFloor(ref.Scope)
			if !ok {
				continue
			}
			detector := e.detector(key)
			fired, _ := detector.PushFloor(health, floor)
			if !fired {
				continue
			}
			detector.Reset()

			if last, ok := e.last[key]; ok && now.Sub(last) < e.cooldown() {
				continue
			}
			e.last[key] = now

			if err := e.Sink.RecordAnomaly(ctx, store.Anomaly{
				TS:         now,
				Scope:      ref.Scope,
				ScopeValue: ref.Value,
				Kind:       "health_floor",
				Observed:   health,
				Baseline:   floor,
				Buckets:    2,
			}); err != nil {
				return triggered, fmt.Errorf("anomaly: record floor: %w", err)
			}
			triggered++
			continue
		}

		widener := e.widener(key)
		wasWidened := widener.Widened()
		band, _ := widener.Observe(now, health, stats)
		if !wasWidened && widener.Widened() && e.OnAtypicalDay != nil {
			day := now.UTC().Format("2006-01-02")
			if _, done := e.atypicalDays[day]; !done {
				e.atypicalDays[day] = struct{}{}
				e.OnAtypicalDay(day)
			}
		}

		// A small or partially measurable denominator is noisy, so the band
		// widens 1.5x (DESIGN.md §7.2). The floors below stop detection
		// entirely; this keeps a scope of 15 at full coverage from producing a
		// confident alarm on a three-point wobble.
		if measurable < baseline.MinMeasurable || coverage < baseline.LowCoverageShare {
			band.MAD *= baseline.LowCoverageWidening
		}

		detector := e.detector(key)
		fired, _ = detector.Push(health, band)
		if !fired {
			continue
		}
		detector.Reset()

		if last, ok := e.last[key]; ok && now.Sub(last) < e.cooldown() {
			continue
		}
		e.last[key] = now

		if err := e.Sink.RecordAnomaly(ctx, store.Anomaly{
			TS:         now,
			Scope:      ref.Scope,
			ScopeValue: ref.Value,
			Kind:       "health_drop",
			Observed:   health,
			Baseline:   band.Median,
			MAD:        band.MAD,
			Sigma:      band.Sigma(),
			Buckets:    2,
		}); err != nil {
			return triggered, fmt.Errorf("anomaly: record: %w", err)
		}
		triggered++
	}

	return triggered, nil
}

func (e *Evaluator) now() time.Time {
	if e.Now == nil {
		return time.Now()
	}
	return e.Now()
}

// learningFloor is the absolute health floor used while a scope's hour-of-week
// baseline is not yet usable (DESIGN.md §7.2). Only the two scopes with a
// documented floor get one: a country or ASN with no baseline stays silent
// rather than being judged against a number the design never gave it.
func learningFloor(scope string) (float64, bool) {
	switch scope {
	case "global":
		return 0.90, true
	case "sector":
		return 0.75, true
	}
	return 0, false
}

func (e *Evaluator) cooldown() time.Duration {
	if e.Cooldown <= 0 {
		return 30 * time.Minute
	}
	return e.Cooldown
}

func (e *Evaluator) detector(key string) *baseline.Detector {
	d, ok := e.detectors[key]
	if !ok {
		d = baseline.NewDetector()
		e.detectors[key] = d
	}
	return d
}

func (e *Evaluator) widener(key string) *baseline.DayWidener {
	w, ok := e.wideners[key]
	if !ok {
		w = &baseline.DayWidener{}
		e.wideners[key] = w
	}
	return w
}

func (e *Evaluator) coverageDetector(key string) *baseline.Detector {
	d, ok := e.coverageDetectors[key]
	if !ok {
		d = baseline.NewDetector()
		e.coverageDetectors[key] = d
	}
	return d
}

func (e *Evaluator) coverageWidener(key string) *baseline.DayWidener {
	w, ok := e.coverageWideners[key]
	if !ok {
		w = &baseline.DayWidener{}
		e.coverageWideners[key] = w
	}
	return w
}

// evaluateCoverage reports a coverage collapse against the scope's own
// hour-of-week history. It is an advisory, never an incident: a mass bot-wall
// rollout and a mass outage look identical in the health number, and only the
// coverage line tells them apart (DESIGN.md §7.2).
func (e *Evaluator) evaluateCoverage(ctx context.Context, ref store.ScopeRef, key string, coverage float64, now time.Time) (bool, error) {
	history, err := e.Source.ScopeCoverageHistory(ctx, ref.Scope, ref.Value, now)
	if err != nil {
		return false, err
	}
	stats := baseline.Compute(history)
	if !stats.Usable() {
		return false, nil
	}

	ckey := "coverage|" + key
	band, _ := e.coverageWidener(ckey).Observe(now, coverage, stats)

	detector := e.coverageDetector(ckey)
	fired, _ := detector.Push(coverage, band)
	if !fired {
		return false, nil
	}
	detector.Reset()

	if last, ok := e.coverageLast[ckey]; ok && now.Sub(last) < e.cooldown() {
		return false, nil
	}
	e.coverageLast[ckey] = now

	if err := e.Sink.RecordAnomaly(ctx, store.Anomaly{
		TS:         now,
		Scope:      ref.Scope,
		ScopeValue: ref.Value,
		Kind:       "coverage_drop",
		Observed:   coverage,
		Baseline:   band.Median,
		MAD:        band.MAD,
		Sigma:      band.Sigma(),
		Buckets:    2,
	}); err != nil {
		return false, fmt.Errorf("anomaly: record coverage: %w", err)
	}
	return true, nil
}

// Loop evaluates every interval until ctx is done. onAtypical, when non-nil,
// is called once per UTC day when the both-direction rule marks the day
// atypical; the caller excludes that day's samples from the baseline.
func Loop(ctx context.Context, src Source, sink Sink, vantage string, every time.Duration, logger *slog.Logger, onAtypical func(day string)) {
	if every <= 0 {
		every = 5 * time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}

	ev := New(src, sink, vantage)
	ev.OnAtypicalDay = onAtypical
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := ev.Evaluate(ctx)
			if err != nil {
				logger.Warn("anomaly evaluation failed",
					"event", "anomaly_failed", "component", "anomaly", "error", err)
				continue
			}
			if n > 0 {
				logger.Info("anomalies detected",
					"event", "anomaly_detected", "component", "anomaly", "count", n)
			}
		}
	}
}
