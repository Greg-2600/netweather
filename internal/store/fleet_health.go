package store

import (
	"context"
	"fmt"
	"time"

	"github.com/Greg-2600/netweather/internal/baseline"
	"github.com/Greg-2600/netweather/internal/state"
)

// FleetHealth is the overview line from DESIGN.md §10.1: the current judged
// share of the fleet, its learned band, and the counters that explain what the
// number is over.
type FleetHealth struct {
	// Healthy is the judgeable share that is up: (up + up_reachability +
	// degraded) / (up + up_reachability + degraded + down). Degraded counts as
	// healthy here, which is the same definition the hourly scope sample and
	// the anomaly detector use, so the band and the number are comparable.
	Healthy float64
	Judged  int
	// Total is every enabled target; Unmeasurable is the confirmed
	// UNMEASURABLE count. Both are shown beside the percentage so a healthy
	// number over a shrunken denominator is visible.
	Total        int
	Unmeasurable int
	// LastProbe is the newest confirmed observation across the vantage.
	LastProbe time.Time
	// Band reports whether the learned band is usable. When it is not, Samples
	// and Window say how far the baseline has come.
	Band    bool
	Lower   float64
	Upper   float64
	Samples int
	Window  int
	// Widened is true when the band was widened for a small or partly
	// measurable scope, exactly as the detector widens it (DESIGN.md §7.2).
	Widened bool
	// Coverage is the measurable share of the judgeable fleet at this instant.
	Coverage float64
}

// FleetHealth reads the overview in one place. A failed history read leaves the
// band unavailable rather than inventing one; a failed count is an error,
// because a percentage over an unknown denominator is not a number to show.
func (s *Store) FleetHealth(ctx context.Context, vantageID string, now time.Time) (FleetHealth, error) {
	var out FleetHealth

	counts, err := s.StateCounts(ctx, vantageID)
	if err != nil {
		return out, err
	}

	var judgeable int
	if err := s.reader.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN t.measurability <> 'unmeasurable'
		                          AND COALESCE(ts.state, '') <> 'unmeasurable'
		                         THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN t.measurability = 'unmeasurable'
		                           OR ts.state = 'unmeasurable'
		                         THEN 1 ELSE 0 END), 0)
		FROM target t
		LEFT JOIN target_state ts ON ts.target_id = t.id AND ts.vantage_id = ?
		WHERE t.enabled = 1`, vantageID).Scan(&out.Total, &judgeable, &out.Unmeasurable); err != nil {
		return out, fmt.Errorf("store: fleet totals: %w", err)
	}

	measurable := counts[state.Up] + counts[state.UpReachability] +
		counts[state.Degraded] + counts[state.Down]
	healthy := counts[state.Up] + counts[state.UpReachability] + counts[state.Degraded]
	out.Judged = measurable
	if measurable > 0 {
		out.Healthy = float64(healthy) / float64(measurable)
	}
	if judgeable > 0 {
		out.Coverage = float64(measurable) / float64(judgeable)
	}

	if last, err := s.LastProbeAt(ctx, vantageID); err == nil {
		out.LastProbe = last
	}

	out.Window = baseline.Window
	history, err := s.ScopeHistory(ctx, "global", "", now)
	if err != nil {
		return out, nil
	}
	stats := baseline.Compute(history)
	out.Samples = stats.N
	if !stats.Usable() {
		return out, nil
	}

	k := float64(baseline.DefaultK)
	out.Widened = measurable < baseline.MinMeasurable || out.Coverage < baseline.LowCoverageShare
	if out.Widened {
		k *= baseline.LowCoverageWidening
	}
	out.Band = true
	out.Lower = stats.Lower(k)
	out.Upper = stats.Upper(k)
	return out, nil
}
