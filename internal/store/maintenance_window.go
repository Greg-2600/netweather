package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MaintenanceWindow is an operator's declaration that work on one or more
// components is planned. CompletedAt is zero while the window is planned; the
// state is derived from that timestamp rather than a second column that could
// disagree with it (SCHEMA.md §7).
type MaintenanceWindow struct {
	ID          int64
	Components  string
	Reason      string
	StartsAt    time.Time
	EndsAt      time.Time
	CompletedAt time.Time
}

// CreateMaintenanceWindow records a planned window and returns its id. The
// caller owns validation: this layer only persists what the operator declared.
func (s *Store) CreateMaintenanceWindow(ctx context.Context, components, reason string, startsAt, endsAt time.Time) (int64, error) {
	res, err := s.writer.ExecContext(ctx, `
		INSERT INTO maintenance_window (components, reason, starts_at, ends_at, completed_at)
		VALUES (?, ?, ?, ?, NULL)`, components, reason, startsAt.Unix(), endsAt.Unix())
	if err != nil {
		return 0, fmt.Errorf("store: create maintenance window: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: maintenance window id: %w", err)
	}
	return id, nil
}

// CompleteMaintenanceWindow marks a planned window complete. Repeating the
// call is a no-op that reports the window as already completed, while an
// unknown id returns sql.ErrNoRows so the API cannot silently discard it.
func (s *Store) CompleteMaintenanceWindow(ctx context.Context, id int64, at time.Time) error {
	res, err := s.writer.ExecContext(ctx, `
		UPDATE maintenance_window SET completed_at = ?
		WHERE id = ? AND completed_at IS NULL`, at.Unix(), id)
	if err != nil {
		return fmt.Errorf("store: complete maintenance window: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: complete maintenance window rows: %w", err)
	}
	if n == 0 {
		var exists int
		err := s.reader.QueryRowContext(ctx, `SELECT 1 FROM maintenance_window WHERE id = ?`, id).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return sql.ErrNoRows
		}
		if err != nil {
			return fmt.Errorf("store: maintenance window exists: %w", err)
		}
	}
	return nil
}

// MaintenanceWindows returns the newest declared windows first, planned and
// completed together. A small hard cap keeps a page or an API response bounded
// even if a caller asks for more.
func (s *Store) MaintenanceWindows(ctx context.Context, limit int) ([]MaintenanceWindow, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, components, reason, starts_at, ends_at, completed_at
		FROM maintenance_window
		ORDER BY starts_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: maintenance windows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]MaintenanceWindow, 0)
	for rows.Next() {
		var (
			w         MaintenanceWindow
			starts    int64
			ends      int64
			completed sql.NullInt64
		)
		if err := rows.Scan(&w.ID, &w.Components, &w.Reason, &starts, &ends, &completed); err != nil {
			return nil, fmt.Errorf("store: scan maintenance window: %w", err)
		}
		w.StartsAt = time.Unix(starts, 0).UTC()
		w.EndsAt = time.Unix(ends, 0).UTC()
		if completed.Valid {
			w.CompletedAt = time.Unix(completed.Int64, 0).UTC()
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: maintenance windows rows: %w", err)
	}
	return out, nil
}
