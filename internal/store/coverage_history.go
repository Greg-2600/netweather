package store

import (
	"context"
	"fmt"
	"time"

	"github.com/Greg-2600/netweather/internal/state"
)

// CoveragePoint is one day's coverage for one scope.
type CoveragePoint struct {
	Bucket       time.Time
	Scope        string
	ScopeValue   string
	Total        int
	Measurable   int
	Blocked      int
	Unmeasurable int
}

// SnapshotCoverage writes one row per scope for the UTC day containing now.
// Re-running during the same day updates the row rather than duplicating it, so
// a restart cannot invent a second snapshot.
func (s *Store) SnapshotCoverage(ctx context.Context, vantageID string, now time.Time) error {
	refs, err := s.Scopes(ctx)
	if err != nil {
		return err
	}

	bucket := now.UTC().Truncate(24 * time.Hour)

	for _, ref := range refs {
		counts, judgeable, err := s.ScopeStates(ctx, vantageID, ref.Scope, ref.Value)
		if err != nil {
			return err
		}
		if judgeable == 0 {
			continue
		}

		unmeasurable, err := s.scopeUnmeasurable(ctx, ref.Scope, ref.Value)
		if err != nil {
			return err
		}

		if _, err := s.writer.ExecContext(ctx, `
			INSERT INTO coverage_snapshot (
				bucket, scope, scope_value, total, measurable, blocked, unmeasurable,
				asn_count, country_count
			) VALUES (?, ?, ?, ?, ?, ?, ?, NULL, NULL)
			ON CONFLICT (bucket, scope, scope_value) DO UPDATE SET
				total = excluded.total,
				measurable = excluded.measurable,
				blocked = excluded.blocked,
				unmeasurable = excluded.unmeasurable`,
			bucket.UnixMilli()/86400000, ref.Scope, ref.Value,
			judgeable+unmeasurable, counts[state.Up]+counts[state.UpReachability]+
				counts[state.Degraded]+counts[state.Down],
			counts[state.Blocked], unmeasurable); err != nil {
			return fmt.Errorf("store: coverage snapshot %s=%s: %w", ref.Scope, ref.Value, err)
		}
	}
	return nil
}

// scopeUnmeasurable counts the targets a scope has permanently given up on.
// ScopeStates excludes them from judgeable on purpose, so the total has to be
// reconstructed rather than inferred.
func (s *Store) scopeUnmeasurable(ctx context.Context, scope, value string) (int, error) {
	var (
		count int
		err   error
	)
	switch scope {
	case "global":
		err = s.reader.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM target
			WHERE enabled = 1 AND measurability = 'unmeasurable'`).Scan(&count)
	case "sector":
		err = s.reader.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM target
			WHERE enabled = 1 AND measurability = 'unmeasurable' AND sector = ?`, value).Scan(&count)
	case "country":
		err = s.reader.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM target
			WHERE enabled = 1 AND measurability = 'unmeasurable' AND country = ?`, value).Scan(&count)
	case "asn":
		err = s.reader.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM target
			WHERE enabled = 1 AND measurability = 'unmeasurable' AND CAST(asn AS TEXT) = ?`, value).Scan(&count)
	default:
		return 0, fmt.Errorf("store: unknown scope %q", scope)
	}
	if err != nil {
		return 0, fmt.Errorf("store: unmeasurable %s: %w", scope, err)
	}
	return count, nil
}

// CoverageHistory reads daily snapshots for one scope, oldest first.
func (s *Store) CoverageHistory(ctx context.Context, scope, value string, days int) ([]CoveragePoint, error) {
	if days <= 0 {
		days = 30
	}
	if days > 365 {
		days = 365
	}

	rows, err := s.reader.QueryContext(ctx, `
		SELECT bucket, total, measurable, blocked, unmeasurable
		FROM coverage_snapshot
		WHERE scope = ? AND scope_value = ?
		ORDER BY bucket DESC LIMIT ?`, scope, value, days)
	if err != nil {
		return nil, fmt.Errorf("store: coverage history: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []CoveragePoint
	for rows.Next() {
		var (
			p      CoveragePoint
			bucket int64
		)
		if err := rows.Scan(&bucket, &p.Total, &p.Measurable, &p.Blocked, &p.Unmeasurable); err != nil {
			return nil, fmt.Errorf("store: scan coverage history: %w", err)
		}
		p.Bucket = time.UnixMilli(bucket * 86400000).UTC()
		p.Scope = scope
		p.ScopeValue = value
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: coverage history: %w", err)
	}

	// The query is newest-first for the LIMIT; the API reads oldest-first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
