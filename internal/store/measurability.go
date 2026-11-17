package store

import (
	"context"
	"fmt"
	"time"
)

const (
	// MeasurabilityWindow is DESIGN.md §3.2's rolling window. It matches the
	// 5-minute rollup retention, which is the only table that still holds a
	// week of observations after the raw 72-hour prune.
	MeasurabilityWindow = 7 * 24 * time.Hour
	// measurabilityMinSamples keeps one challenge page from classifying a
	// target as permanently blind.
	measurabilityMinSamples = 10
	// measurabilityBlockedShare is the documented 80% threshold.
	measurabilityBlockedShare = 0.80
)

// MeasurabilityChange reports how many targets were marked blind or restored.
type MeasurabilityChange struct {
	Marked  int
	Cleared int
}

// RefreshMeasurability applies the calibration DESIGN.md §3.2 calls the single
// most important decision in the product: a target whose probes were BLOCKED
// more than 80% of the time over the last seven days is UNMEASURABLE, because a
// permanent bot wall says nothing about the origin. The window rolls, so a
// target returns to 'ok' when the wall lifts.
//
// The share comes from rollup_5m, not raw probes: raw rows are pruned at 72
// hours, so a "seven-day" share computed from them would silently be a
// three-day share.
func (s *Store) RefreshMeasurability(ctx context.Context, vantageID string, now time.Time) (MeasurabilityChange, error) {
	var change MeasurabilityChange

	cutoff := now.Add(-MeasurabilityWindow).UnixMilli() / 300000
	rows, err := s.reader.QueryContext(ctx, `
		SELECT t.id, t.measurability,
		       COALESCE(SUM(r.n), 0), COALESCE(SUM(r.blocked), 0)
		FROM target t
		LEFT JOIN rollup_5m r
		  ON r.target_id = t.id AND r.vantage_id = ? AND r.bucket >= ?
		WHERE t.enabled = 1
		GROUP BY t.id`, vantageID, cutoff)
	if err != nil {
		return change, fmt.Errorf("store: measurability read: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type update struct {
		id   string
		want string
	}
	var updates []update

	for rows.Next() {
		var (
			id      string
			current string
			n       int
			blocked int
		)
		if err := rows.Scan(&id, &current, &n, &blocked); err != nil {
			return change, fmt.Errorf("store: scan measurability: %w", err)
		}
		if n < measurabilityMinSamples {
			// Too little evidence either way. A target that was already marked
			// blind keeps that mark until enough new probes say otherwise.
			continue
		}

		want := current
		if float64(blocked)/float64(n) > measurabilityBlockedShare {
			want = "unmeasurable"
		} else if current == "unmeasurable" || current == "mostly_blocked" {
			want = "ok"
		}
		if want != current {
			updates = append(updates, update{id: id, want: want})
		}
	}
	if err := rows.Err(); err != nil {
		return change, fmt.Errorf("store: measurability read: %w", err)
	}

	for _, u := range updates {
		if _, err := s.writer.ExecContext(ctx,
			`UPDATE target SET measurability = ? WHERE id = ?`, u.want, u.id); err != nil {
			return change, fmt.Errorf("store: measurability update %s: %w", u.id, err)
		}
		if u.want == "unmeasurable" {
			change.Marked++
		} else {
			change.Cleared++
		}
	}
	return change, nil
}
