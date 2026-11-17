package store

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/Greg-2600/netweather/internal/baseline"
	"github.com/Greg-2600/netweather/internal/state"
)

// ScopeStates returns state counts for a scope plus the structural denominator.
//
// judgeable excludes targets already classified as permanently unmeasurable:
// that is a known blind spot, not a fresh visibility failure every cycle
// (DESIGN.md §7.2). Unprobed targets still count toward judgeable, which is what
// makes coverage fall when the scheduler is not keeping up.
func (s *Store) ScopeStates(ctx context.Context, vantageID, scope, value string) (map[state.State]int, int, error) {
	return s.scopeStates(ctx, vantageID, scope, value, time.Time{})
}

// ScopeStatesFresh is the detection variant: it ignores targets whose last probe
// is outside their own freshness window, so a stalled scheduler cannot turn
// stale state into an incident. The window is DESIGN.md §7.2's
// min(2*interval + 30s, 15m): a target probed every ten minutes is a
// low-resolution sensor, not a missing one.
//
// Coverage intentionally uses ScopeStates (unfiltered), because a target that
// stopped being probed is exactly the blind spot coverage reports.
func (s *Store) ScopeStatesFresh(ctx context.Context, vantageID, scope, value string, now time.Time) (map[state.State]int, int, error) {
	return s.scopeStates(ctx, vantageID, scope, value, now)
}

func (s *Store) scopeStates(ctx context.Context, vantageID, scope, value string, now time.Time) (map[state.State]int, int, error) {
	const base = `
		SELECT COALESCE(ts.state, ''), COUNT(*)
		FROM target t
		LEFT JOIN target_state ts ON ts.target_id = t.id AND ts.vantage_id = ?
		WHERE t.enabled = 1 AND t.measurability <> 'unmeasurable'`

	query := base
	args := []any{vantageID}

	switch scope {
	case "global":
	case "sector":
		query += ` AND t.sector = ?`
		args = append(args, value)
	case "country":
		query += ` AND t.country = ?`
		args = append(args, value)
	case "asn":
		asn, err := strconv.Atoi(value)
		if err != nil || asn <= 0 {
			return nil, 0, fmt.Errorf("store: scope asn %q is not a positive integer", value)
		}
		query += ` AND t.asn = ?`
		args = append(args, asn)
	default:
		return nil, 0, fmt.Errorf("store: unknown scope %q", scope)
	}

	if !now.IsZero() {
		query += ` AND ts.last_probe_at >= ? - MIN(2 * t.interval_s * 1000 + 30000, 900000)`
		args = append(args, now.UnixMilli())
	}

	query += ` GROUP BY ts.state`

	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: scope states: %w", err)
	}
	defer func() { _ = rows.Close() }()

	counts := make(map[state.State]int)
	judgeable := 0
	for rows.Next() {
		var (
			name  string
			count int
		)
		if err := rows.Scan(&name, &count); err != nil {
			return nil, 0, fmt.Errorf("store: scan scope state: %w", err)
		}
		judgeable += count
		if name == "" {
			continue
		}
		counts[state.State(name)] += count
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: scope states: %w", err)
	}
	return counts, judgeable, nil
}

// ScopeRef identifies one sampled scope.
type ScopeRef struct {
	Scope string
	Value string
}

// Scopes lists global plus every sector and country that has an enabled target.
func (s *Store) Scopes(ctx context.Context) ([]ScopeRef, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT 'sector', sector FROM target
		WHERE enabled = 1 AND sector IS NOT NULL AND sector <> ''
		GROUP BY sector
		UNION
		SELECT 'country', country FROM target
		WHERE enabled = 1 AND country IS NOT NULL AND country <> ''
		GROUP BY country`)
	if err != nil {
		return nil, fmt.Errorf("store: scopes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []ScopeRef{{Scope: "global", Value: ""}}
	for rows.Next() {
		var ref ScopeRef
		if err := rows.Scan(&ref.Scope, &ref.Value); err != nil {
			return nil, fmt.Errorf("store: scan scope: %w", err)
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: scopes: %w", err)
	}
	return out, nil
}

// SampleScopes records one hourly health snapshot per scope. Sampling is
// idempotent within an hour: the latest evaluation replaces the hour's value.
func (s *Store) SampleScopes(ctx context.Context, vantageID string, now time.Time) error {
	refs, err := s.Scopes(ctx)
	if err != nil {
		return err
	}

	bucket := now.UTC().Truncate(time.Hour).UnixMilli() / 3600000

	for _, ref := range refs {
		counts, judgeable, err := s.ScopeStates(ctx, vantageID, ref.Scope, ref.Value)
		if err != nil {
			return err
		}
		if judgeable == 0 {
			continue
		}

		measurable := counts[state.Up] + counts[state.UpReachability] +
			counts[state.Degraded] + counts[state.Down]
		healthy := counts[state.Up] + counts[state.UpReachability] + counts[state.Degraded]

		var health float64
		if measurable > 0 {
			health = float64(healthy) / float64(measurable)
		}
		coverage := float64(measurable) / float64(judgeable)

		if _, err := s.writer.ExecContext(ctx, `
			INSERT INTO scope_sample (scope, scope_value, bucket, health, coverage, measurable, judgeable)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (scope, scope_value, bucket) DO UPDATE SET
				health = excluded.health, coverage = excluded.coverage,
				measurable = excluded.measurable, judgeable = excluded.judgeable`,
			ref.Scope, ref.Value, bucket, health, coverage, measurable, judgeable); err != nil {
			return fmt.Errorf("store: sample scope %s=%s: %w", ref.Scope, ref.Value, err)
		}
	}
	return nil
}

// coverageBaselineFrom is the start of the history window.
//
// The bucket is an hour-of-week, so one week contributes exactly one sample per
// bucket. A 28-sample baseline is therefore 28 weeks, not 28 days: with a
// 28-day lookback the same bucket held about four points, which is below the
// 21-sample floor and made the detector permanently "learning".
func coverageBaselineFrom(at time.Time) int64 {
	weeks := int64(7 * baseline.Window)
	return at.UTC().AddDate(0, 0, int(-weeks)).Truncate(time.Hour).UnixMilli() / 3600000
}

// ScopeHistory returns the last 28 hourly health samples whose hour-of-week
// matches at. That is the baseline the detector compares against.
func (s *Store) ScopeHistory(ctx context.Context, scope, value string, at time.Time) ([]float64, error) {
	from := coverageBaselineFrom(at)

	rows, err := s.reader.QueryContext(ctx, `
		SELECT bucket, health FROM scope_sample
		WHERE scope = ? AND scope_value = ? AND bucket >= ?
		ORDER BY bucket`, scope, value, from)
	if err != nil {
		return nil, fmt.Errorf("store: scope history: %w", err)
	}
	defer func() { _ = rows.Close() }()

	want := baseline.Key(at)
	samples := make([]float64, 0, baseline.Window)
	for rows.Next() {
		var (
			bucket int64
			health float64
		)
		if err := rows.Scan(&bucket, &health); err != nil {
			return nil, fmt.Errorf("store: scan scope history: %w", err)
		}
		if baseline.Key(time.UnixMilli(bucket*3600000)) != want {
			continue
		}
		samples = append(samples, health)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: scope history: %w", err)
	}
	if len(samples) > baseline.Window {
		samples = samples[len(samples)-baseline.Window:]
	}
	return samples, nil
}

// ScopeCoverageHistory returns the last 28 hourly coverage samples whose
// hour-of-week matches at. It is the same window as ScopeHistory, kept separate
// so a health drop and a visibility drop are two advisories rather than one
// ambiguous number (DESIGN.md §7.2).
func (s *Store) ScopeCoverageHistory(ctx context.Context, scope, value string, at time.Time) ([]float64, error) {
	from := coverageBaselineFrom(at)

	rows, err := s.reader.QueryContext(ctx, `
		SELECT bucket, coverage FROM scope_sample
		WHERE scope = ? AND scope_value = ? AND bucket >= ?
		ORDER BY bucket`, scope, value, from)
	if err != nil {
		return nil, fmt.Errorf("store: scope coverage history: %w", err)
	}
	defer func() { _ = rows.Close() }()

	want := baseline.Key(at)
	samples := make([]float64, 0, baseline.Window)
	for rows.Next() {
		var (
			bucket   int64
			coverage float64
		)
		if err := rows.Scan(&bucket, &coverage); err != nil {
			return nil, fmt.Errorf("store: scan scope coverage history: %w", err)
		}
		if baseline.Key(time.UnixMilli(bucket*3600000)) != want {
			continue
		}
		samples = append(samples, coverage)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: scope coverage history: %w", err)
	}
	if len(samples) > baseline.Window {
		samples = samples[len(samples)-baseline.Window:]
	}
	return samples, nil
}
