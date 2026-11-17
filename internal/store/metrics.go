package store

import (
	"context"
	"fmt"

	"github.com/Greg-2600/netweather/internal/state"
)

// StateCounts returns the number of enabled targets per state for one vantage.
// A target disabled by a config reload keeps its history and its last state,
// but it is not part of the live fleet: the IHI, the verdict, the fleet line,
// and the metrics all read this, so an included disabled row would outlive its
// target in every summary.
func (s *Store) StateCounts(ctx context.Context, vantageID string) (map[state.State]int, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT ts.state, COUNT(*)
		FROM target_state ts
		JOIN target t ON t.id = ts.target_id AND t.enabled = 1
		WHERE ts.vantage_id = ?
		GROUP BY ts.state`,
		vantageID)
	if err != nil {
		return nil, fmt.Errorf("store: state counts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[state.State]int)
	for rows.Next() {
		var (
			name  string
			count int
		)
		if err := rows.Scan(&name, &count); err != nil {
			return nil, fmt.Errorf("store: scan state count: %w", err)
		}
		out[state.State(name)] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: state counts: %w", err)
	}
	return out, nil
}

// OpenIncidentCount is the number of unclosed incidents.
func (s *Store) OpenIncidentCount(ctx context.Context) (int, error) {
	var n int
	if err := s.reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM incident WHERE closed_at IS NULL`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: open incident count: %w", err)
	}
	return n, nil
}
