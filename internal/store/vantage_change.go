package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// VantageChange is one detected move of the egress identity. Previous and
// Current are the stored tuples ("203.0.113.9,AS7922,US"); the address is kept
// for debugging and the UI renders only the ASN and country.
type VantageChange struct {
	At       time.Time
	Previous string
	Current  string
}

// RecordVantageChange appends one detected egress change. It is called by
// maintenance when the self-check's tuple moves, so a restart does not lose the
// fact that the baselines belong to a different path (DESIGN.md §7.5).
func (s *Store) RecordVantageChange(ctx context.Context, previous, current string, at time.Time) error {
	if _, err := s.writer.ExecContext(ctx, `
		INSERT INTO vantage_change (changed_at, previous, current)
		VALUES (?, ?, ?)`, at.UnixMilli(), previous, current); err != nil {
		return fmt.Errorf("store: record vantage change: %w", err)
	}
	return nil
}

// LatestVantageChange returns the newest change, or false when the vantage has
// never moved. An empty history is not an error: a new install has no change to
// report.
func (s *Store) LatestVantageChange(ctx context.Context) (VantageChange, bool, error) {
	var (
		change VantageChange
		atMS   int64
	)
	err := s.reader.QueryRowContext(ctx, `
		SELECT changed_at, previous, current
		FROM vantage_change ORDER BY changed_at DESC, id DESC LIMIT 1`).
		Scan(&atMS, &change.Previous, &change.Current)
	if errors.Is(err, sql.ErrNoRows) {
		return VantageChange{}, false, nil
	}
	if err != nil {
		return VantageChange{}, false, fmt.Errorf("store: latest vantage change: %w", err)
	}
	change.At = time.UnixMilli(atMS)
	return change, true, nil
}
