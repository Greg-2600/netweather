package store

import (
	"context"
	"fmt"
	"strconv"

	"github.com/Greg-2600/netweather/internal/state"
)

// ScopeStates returns state counts for a scope plus the structural denominator.
//
// judgeable excludes targets already classified as permanently unmeasurable:
// that is a known blind spot, not a fresh visibility failure every cycle
// (DESIGN.md §7.2). Unprobed targets still count toward judgeable, which is what
// makes coverage fall when the scheduler is not keeping up.
func (s *Store) ScopeStates(ctx context.Context, vantageID, scope, value string) (map[state.State]int, int, error) {
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
