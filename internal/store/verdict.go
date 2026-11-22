package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Greg-2600/netweather/internal/state"
)

// LastProbeAt returns the newest confirmed probe across enabled targets for one
// vantage. Zero means no target has a confirmed state yet, which the verdict
// reads as STARTING rather than as a silent board.
func (s *Store) LastProbeAt(ctx context.Context, vantageID string) (time.Time, error) {
	var ts sql.NullInt64
	if err := s.reader.QueryRowContext(ctx, `
		SELECT MAX(ts.last_probe_at)
		FROM target_state ts
		JOIN target t ON t.id = ts.target_id
		WHERE t.enabled = 1 AND ts.vantage_id = ?`, vantageID).Scan(&ts); err != nil {
		return time.Time{}, fmt.Errorf("store: last probe at: %w", err)
	}
	if !ts.Valid || ts.Int64 <= 0 {
		return time.Time{}, nil
	}
	return time.UnixMilli(ts.Int64).UTC(), nil
}

// ReferenceStates counts the confirmed states of the reference class. It is the
// "is it me?" set (DESIGN.md §7.1): an unmeasurable target is not a reference
// failure, so it is excluded here exactly as it is from scope health.
func (s *Store) ReferenceStates(ctx context.Context, vantageID string) (map[state.State]int, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT COALESCE(ts.state, ''), COUNT(*)
		FROM target t
		LEFT JOIN target_state ts ON ts.target_id = t.id AND ts.vantage_id = ?
		WHERE t.enabled = 1 AND t.class = 'reference' AND t.measurability <> 'unmeasurable'
		GROUP BY 1`, vantageID)
	if err != nil {
		return nil, fmt.Errorf("store: reference states: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[state.State]int, 8)
	for rows.Next() {
		var (
			raw   string
			count int
		)
		if err := rows.Scan(&raw, &count); err != nil {
			return nil, fmt.Errorf("store: scan reference state: %w", err)
		}
		out[state.State(raw)] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reference states: %w", err)
	}
	return out, nil
}

// CrossVantageDisagreement reports a target that is DOWN from the primary
// vantage and reachable from a corroborating one. That is the only evidence
// that separates "the service" from "this path" from "this country"
// (DESIGN.md §3.3, REGIONAL).
//
// The roles matter. An up primary with a down corroborator is not REGIONAL:
// what the board shows is healthy, so there is no local path problem to
// explain. This is the same rule the target drawer uses, so the top-bar badge
// and the drawer note cannot disagree. A target with only one vantage cannot
// disagree with itself, so a single-vantage instance answers false.
func (s *Store) CrossVantageDisagreement(ctx context.Context) (bool, error) {
	var found int
	err := s.reader.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM target_state down
			JOIN vantage down_v ON down_v.id = down.vantage_id
			JOIN target_state up
			  ON up.target_id = down.target_id AND up.vantage_id <> down.vantage_id
			JOIN vantage up_v ON up_v.id = up.vantage_id
			WHERE down_v.role = 'primary'
			  AND down.state = 'down'
			  AND up_v.role = 'corroboration'
			  AND up.state IN ('up', 'up_reachability')
		)`).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("store: cross-vantage disagreement: %w", err)
	}
	return found == 1, nil
}

// FailureSpread counts the distinct sectors and origin ASNs among confirmed
// DOWN targets for one vantage. It is the secondary "is it me?" heuristic from
// DESIGN.md §7.1: a handful of failures in one corner of the estate looks like
// a service or a region; failures spread across many sectors and networks at
// once are more likely to be this path.
func (s *Store) FailureSpread(ctx context.Context, vantageID string) (sectors, asns int, err error) {
	if err := s.reader.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT CASE WHEN t.sector <> '' THEN t.sector END),
		       COUNT(DISTINCT CASE WHEN t.asn > 0 THEN t.asn END)
		FROM target t
		JOIN target_state ts ON ts.target_id = t.id AND ts.vantage_id = ?
		WHERE t.enabled = 1 AND ts.state = 'down'`, vantageID).Scan(&sectors, &asns); err != nil {
		return 0, 0, fmt.Errorf("store: failure spread: %w", err)
	}
	return sectors, asns, nil
}
