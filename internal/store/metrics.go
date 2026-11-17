package store

import (
	"context"
	"fmt"

	"github.com/Greg-2600/netweather/internal/state"
)

// StateCounts returns the number of targets per state for one vantage.
func (s *Store) StateCounts(ctx context.Context, vantageID string) (map[state.State]int, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT state, COUNT(*) FROM target_state WHERE vantage_id = ? GROUP BY state`,
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
