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
	ScopeStates(ctx context.Context, vantageID, scope, value string) (map[state.State]int, int, error)
	ScopeHistory(ctx context.Context, scope, value string, at time.Time) ([]float64, error)
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

		counts, judgeable, err := e.Source.ScopeStates(ctx, e.Vantage, ref.Scope, ref.Value)
		if err != nil {
			return triggered, err
		}
		if judgeable == 0 {
			continue
		}

		measurable := counts[state.Up] + counts[state.UpReachability] +
			counts[state.Degraded] + counts[state.Down]
		coverage := float64(measurable) / float64(judgeable)
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
			continue
		}

		widener := e.widener(key)
		band, _ := widener.Observe(now, health, stats)

		detector := e.detector(key)
		fired, _ := detector.Push(health, band)
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

// Loop evaluates every interval until ctx is done.
func Loop(ctx context.Context, src Source, sink Sink, vantage string, every time.Duration, logger *slog.Logger) {
	if every <= 0 {
		every = 5 * time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}

	ev := New(src, sink, vantage)
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
