package store

import (
	"context"
	"fmt"
	"time"

	"github.com/Greg-2600/netweather/internal/baseline"
)

// FoldStats reports how many buckets were folded.
type FoldStats struct {
	Hours int
	Days  int
}

// Fold rebuilds closed hourly and daily rollups from their finer source.
//
// It is deliberately recompute-not-increment: a fold is idempotent, so a restart
// or a missed tick self-heals. The window is bounded (48h of hours, 7d of days)
// because a long outage should not turn into an unbounded catch-up query.
func (s *Store) Fold(ctx context.Context, now time.Time) (FoldStats, error) {
	var stats FoldStats

	hourNow := now.UTC().Truncate(time.Hour)
	for i := 1; i <= 48; i++ {
		hour := hourNow.Add(-time.Duration(i) * time.Hour)
		rows, err := s.foldHour(ctx, hour)
		if err != nil {
			return stats, err
		}
		if rows > 0 {
			stats.Hours++
		}
	}

	dayNow := now.UTC().Truncate(24 * time.Hour)
	for i := 1; i <= 7; i++ {
		day := dayNow.AddDate(0, 0, -i)
		rows, err := s.foldDay(ctx, day)
		if err != nil {
			return stats, err
		}
		if rows > 0 {
			stats.Days++
		}
	}

	return stats, nil
}

func (s *Store) foldHour(ctx context.Context, hour time.Time) (int64, error) {
	const upsert = `
		INSERT INTO rollup_1h (
			target_id, vantage_id, bucket, n, up, degraded, down, blocked, unknown, ewma50_ms, ewma95_ms
		)
		SELECT target_id, vantage_id, ?, SUM(n), SUM(up), SUM(degraded), SUM(down),
		       SUM(blocked), SUM(unknown),
		       CAST(SUM(ewma50_ms * n) / NULLIF(SUM(CASE WHEN ewma50_ms IS NOT NULL THEN n ELSE 0 END), 0) AS INTEGER),
		       CAST(SUM(ewma95_ms * n) / NULLIF(SUM(CASE WHEN ewma95_ms IS NOT NULL THEN n ELSE 0 END), 0) AS INTEGER)
		FROM rollup_5m
		WHERE bucket >= ? AND bucket < ?
		GROUP BY target_id, vantage_id
		ON CONFLICT (target_id, vantage_id, bucket) DO UPDATE SET
			n = excluded.n, up = excluded.up, degraded = excluded.degraded,
			down = excluded.down, blocked = excluded.blocked, unknown = excluded.unknown,
			ewma50_ms = excluded.ewma50_ms, ewma95_ms = excluded.ewma95_ms`

	from := hour.UnixMilli() / 300000
	to := hour.Add(time.Hour).UnixMilli() / 300000

	res, err := s.writer.ExecContext(ctx, upsert, hour.UnixMilli()/3600000, from, to)
	if err != nil {
		return 0, fmt.Errorf("store: fold hour %s: %w", hour.Format(time.RFC3339), err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *Store) foldDay(ctx context.Context, day time.Time) (int64, error) {
	const upsert = `
		INSERT INTO rollup_1d (
			target_id, vantage_id, bucket, n, up, degraded, down, blocked, unknown, ewma50_ms, ewma95_ms
		)
		SELECT target_id, vantage_id, ?, SUM(n), SUM(up), SUM(degraded), SUM(down),
		       SUM(blocked), SUM(unknown),
		       CAST(SUM(ewma50_ms * n) / NULLIF(SUM(CASE WHEN ewma50_ms IS NOT NULL THEN n ELSE 0 END), 0) AS INTEGER),
		       CAST(SUM(ewma95_ms * n) / NULLIF(SUM(CASE WHEN ewma95_ms IS NOT NULL THEN n ELSE 0 END), 0) AS INTEGER)
		FROM rollup_1h
		WHERE bucket >= ? AND bucket < ?
		GROUP BY target_id, vantage_id
		ON CONFLICT (target_id, vantage_id, bucket) DO UPDATE SET
			n = excluded.n, up = excluded.up, degraded = excluded.degraded,
			down = excluded.down, blocked = excluded.blocked, unknown = excluded.unknown,
			ewma50_ms = excluded.ewma50_ms, ewma95_ms = excluded.ewma95_ms`

	from := day.Unix() / 3600
	to := day.Add(24*time.Hour).Unix() / 3600

	res, err := s.writer.ExecContext(ctx, upsert, day.UnixMilli()/86400000, from, to)
	if err != nil {
		return 0, fmt.Errorf("store: fold day %s: %w", day.Format(time.RFC3339), err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PruneStats reports deleted rows per table.
type PruneStats struct {
	Probes         int64
	Archive        int64
	Rollup5m       int64
	Rollup1h       int64
	Rollup1d       int64
	Anomalies      int64
	ExternalEvents int64
	SelfChecks     int64
	ScopeSamples   int64
}

// Prune applies the retention policy from SCHEMA.md §11. Raw probes are deleted
// in batches so a long-running prune never holds the write lock for a large
// transaction.
func (s *Store) Prune(ctx context.Context, now time.Time) (PruneStats, error) {
	var stats PruneStats
	var err error

	probeCutoff := now.Add(-72 * time.Hour).UnixMilli()
	for {
		res, err := s.writer.ExecContext(ctx, `
			DELETE FROM probe WHERE id IN (
				SELECT id FROM probe WHERE ts < ? LIMIT 5000
			)`, probeCutoff)
		if err != nil {
			return stats, fmt.Errorf("store: prune probes: %w", err)
		}
		n, _ := res.RowsAffected()
		stats.Probes += n
		if n == 0 {
			break
		}
	}

	// Incident-pinned probes are the long memory (DESIGN.md §12, SCHEMA.md §5):
	// a year, then they go too. This runs after the raw prune so a pinned row
	// can never be deleted as a raw probe first.
	stats.Archive, err = s.deleteOlderThan(ctx, `DELETE FROM probe_archive WHERE ts < ?`,
		now.Add(-365*24*time.Hour).UnixMilli())
	if err != nil {
		return stats, err
	}

	stats.Rollup5m, err = s.deleteOlderThan(ctx, `DELETE FROM rollup_5m WHERE bucket < ?`,
		now.Add(-7*24*time.Hour).UnixMilli()/300000)
	if err != nil {
		return stats, err
	}
	stats.Rollup1h, err = s.deleteOlderThan(ctx, `DELETE FROM rollup_1h WHERE bucket < ?`,
		now.Add(-30*24*time.Hour).UnixMilli()/3600000)
	if err != nil {
		return stats, err
	}
	stats.Rollup1d, err = s.deleteOlderThan(ctx, `DELETE FROM rollup_1d WHERE bucket < ?`,
		now.Add(-365*24*time.Hour).UnixMilli()/86400000)
	if err != nil {
		return stats, err
	}

	// Advisories and feed events are a 90-day window (DESIGN.md §12): they are
	// evidence around an incident, not the permanent record the incident row
	// is. An event still being reported keeps refreshing observed_at, so a
	// long outage is not deleted while it is current.
	retention90 := now.Add(-90 * 24 * time.Hour).UnixMilli()
	stats.Anomalies, err = s.deleteOlderThan(ctx, `DELETE FROM anomaly WHERE ts < ?`, retention90)
	if err != nil {
		return stats, err
	}
	stats.ExternalEvents, err = s.deleteOlderThan(ctx,
		`DELETE FROM external_event WHERE observed_at < ?`, retention90)
	if err != nil {
		return stats, err
	}

	// Self-checks are 30 days (SCHEMA.md §11): a check this old is not the
	// explanation for anything the board shows now.
	stats.SelfChecks, err = s.deleteOlderThan(ctx, `DELETE FROM self_check WHERE ts < ?`,
		now.Add(-30*24*time.Hour).UnixMilli())
	if err != nil {
		return stats, err
	}

	// Scope samples are the hour-of-week baseline. Keep exactly the window the
	// detector reads (28 weekly samples = 28 weeks); older rows can never be
	// consulted again and would otherwise grow without bound.
	stats.ScopeSamples, err = s.deleteOlderThan(ctx, `DELETE FROM scope_sample WHERE bucket < ?`,
		now.AddDate(0, 0, -7*baseline.Window).UnixMilli()/3600000)
	if err != nil {
		return stats, err
	}

	return stats, nil
}

func (s *Store) deleteOlderThan(ctx context.Context, query string, cutoff int64) (int64, error) {
	res, err := s.writer.ExecContext(ctx, query, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: prune rollups: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CountProbes is a diagnostic used by tests and the debug bundle.
func (s *Store) CountProbes(ctx context.Context) (int, error) {
	var n int
	if err := s.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM probe`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count probes: %w", err)
	}
	return n, nil
}
