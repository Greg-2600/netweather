package store

import (
	"context"
	"fmt"
	"time"
)

// TargetSource is one provenance row for a seeded or configured target. A
// target list is untrusted input, so the row says where a target came from and
// which payload produced it (THREAT_MODEL.md T35).
type TargetSource struct {
	TargetID  string
	Source    string
	FetchedAt time.Time
	SHA256    string
}

// RecordTargetSource upserts provenance for one target. The hash is updated on
// a later load, so a changed seed file is visible without adding a second row
// for the same source.
func (s *Store) RecordTargetSource(ctx context.Context, targetID, source, sha string, fetchedAt time.Time) error {
	_, err := s.writer.ExecContext(ctx, `
		INSERT INTO target_source (target_id, source, fetched_at, sha256)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (target_id, source) DO UPDATE SET
			fetched_at = excluded.fetched_at,
			sha256 = excluded.sha256`,
		targetID, source, fetchedAt.UnixMilli(), sha)
	if err != nil {
		return fmt.Errorf("store: target source %s: %w", targetID, err)
	}
	return nil
}

// TargetSources returns every provenance row for a target, newest source name
// first. An unknown target is an empty list, not an error: provenance is
// advisory and must not break a drawer.
func (s *Store) TargetSources(ctx context.Context, targetID string) ([]TargetSource, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT target_id, source, COALESCE(fetched_at, 0), COALESCE(sha256, '')
		FROM target_source WHERE target_id = ? ORDER BY source`, targetID)
	if err != nil {
		return nil, fmt.Errorf("store: target sources: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TargetSource
	for rows.Next() {
		var (
			ts   TargetSource
			atMS int64
		)
		if err := rows.Scan(&ts.TargetID, &ts.Source, &atMS, &ts.SHA256); err != nil {
			return nil, fmt.Errorf("store: scan target source: %w", err)
		}
		if atMS > 0 {
			ts.FetchedAt = time.UnixMilli(atMS)
		}
		out = append(out, ts)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: target sources: %w", err)
	}
	return out, nil
}
