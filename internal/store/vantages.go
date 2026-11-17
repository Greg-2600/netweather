package store

import (
	"context"
	"fmt"
	"time"
)

// VantageState is one vantage's confirmed state for a target. The primary
// vantage is what the board shows; a corroborating vantage that disagrees is how
// REGIONAL is told apart from a service outage (DESIGN.md §7.1).
type VantageState struct {
	ID        string
	Kind      string
	Role      string
	State     string
	Since     time.Time
	LastProbe time.Time
}

// TargetVantages returns every vantage's confirmed state for one target,
// primary first and then by id so the output is stable. A target with only the
// direct vantage returns one row; a deployment with Tor returns two.
func (s *Store) TargetVantages(ctx context.Context, targetID string) ([]VantageState, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT v.id, COALESCE(v.kind, ''), COALESCE(v.role, ''),
		       ts.state, ts.since, ts.last_probe_at
		FROM target_state ts
		JOIN vantage v ON v.id = ts.vantage_id
		WHERE ts.target_id = ?
		ORDER BY CASE v.role WHEN 'primary' THEN 0 ELSE 1 END, v.id`, targetID)
	if err != nil {
		return nil, fmt.Errorf("store: target vantages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []VantageState
	for rows.Next() {
		var (
			row   VantageState
			since int64
			last  int64
		)
		if err := rows.Scan(&row.ID, &row.Kind, &row.Role, &row.State, &since, &last); err != nil {
			return nil, fmt.Errorf("store: scan target vantage: %w", err)
		}
		if since > 0 {
			row.Since = time.UnixMilli(since).UTC()
		}
		if last > 0 {
			row.LastProbe = time.UnixMilli(last).UTC()
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: target vantages: %w", err)
	}
	return out, nil
}
