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

// CrossVantageDisagreement reports a target that is DOWN from one vantage and
// reachable from another. That is the only evidence that separates "the
// service" from "this path" from "this country" (DESIGN.md §3.3, REGIONAL).
//
// A target with only one vantage cannot disagree with itself, so a single
// vantage instance answers false rather than guessing.
func (s *Store) CrossVantageDisagreement(ctx context.Context) (bool, error) {
	var found int
	err := s.reader.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM target_state down
			JOIN target_state up
			  ON up.target_id = down.target_id AND up.vantage_id <> down.vantage_id
			WHERE down.state = 'down'
			  AND up.state IN ('up', 'up_reachability')
		)`).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("store: cross-vantage disagreement: %w", err)
	}
	return found == 1, nil
}
