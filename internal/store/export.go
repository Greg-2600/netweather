package store

import (
	"context"
	"fmt"
	"time"

	"github.com/Greg-2600/netweather/internal/state"
)

// ExportRow is one observation flattened for export.
type ExportRow struct {
	TargetID   string
	Host       string
	VantageID  string
	TS         time.Time
	State      state.State
	Stage      string
	ErrorClass string
	HTTPStatus int
	LatencyMS  int
	Confidence string
	Detail     string
}

// ExportProbes streams observations in [from, to) ordered by time. The callback
// receives one row at a time, so an export never materialises the result set.
func (s *Store) ExportProbes(ctx context.Context, from, to time.Time, fn func(ExportRow) error) error {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT p.target_id, t.host, p.vantage_id, p.ts, p.state, p.stage,
		       COALESCE(p.error_class, ''), COALESCE(p.http_status, 0),
		       COALESCE(p.latency_ms, 0), p.confidence, COALESCE(p.detail, '')
		FROM probe p
		JOIN target t ON t.id = p.target_id
		WHERE p.ts >= ? AND p.ts < ?
		ORDER BY p.ts, p.id`, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return fmt.Errorf("store: export probes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			row ExportRow
			ts  int64
		)
		if err := rows.Scan(&row.TargetID, &row.Host, &row.VantageID, &ts,
			&row.State, &row.Stage, &row.ErrorClass, &row.HTTPStatus,
			&row.LatencyMS, &row.Confidence, &row.Detail); err != nil {
			return fmt.Errorf("store: scan export row: %w", err)
		}
		row.TS = time.UnixMilli(ts).UTC()
		if err := fn(row); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: export probes: %w", err)
	}
	return nil
}
