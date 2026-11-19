package store

import (
	"context"
	"fmt"
	"time"
)

// CountryLevel is one country's current outage level from a bounded,
// server-side poll. It is evidence for the world view, never a country-wide
// verdict: Score, EventCount and Rank preserve what the upstream actually
// reported beside the normalised Level.
type CountryLevel struct {
	Country    string
	Level      float64
	Score      float64
	EventCount int
	Rank       int
	ObservedAt time.Time
	Source     string
}

// UpsertCountryLevels stores a batch of per-country levels. A row is replaced
// only by a newer observation, so an out-of-order poll cannot drag a country
// back to a stale level. The feed's country code is the identity.
func (s *Store) UpsertCountryLevels(ctx context.Context, levels []CountryLevel) error {
	if len(levels) == 0 {
		return nil
	}

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin country levels: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, l := range levels {
		if l.Country == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO country_level (
				country, level, score, event_count, rank, observed_at, source
			) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (country) DO UPDATE SET
				level = excluded.level,
				score = excluded.score,
				event_count = excluded.event_count,
				rank = excluded.rank,
				observed_at = excluded.observed_at,
				source = excluded.source
			WHERE excluded.observed_at > country_level.observed_at`,
			l.Country, l.Level, l.Score, l.EventCount, l.Rank,
			l.ObservedAt.UnixMilli(), l.Source); err != nil {
			return fmt.Errorf("store: country level %s: %w", l.Country, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit country levels: %w", err)
	}
	return nil
}

// CountryLevels returns every stored per-country level, keyed by uppercase
// ISO code. The feed is bounded at the source, so the map is small by design;
// a page reads it once rather than calling the upstream per country.
func (s *Store) CountryLevels(ctx context.Context) (map[string]CountryLevel, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT country, level, score, event_count, rank, observed_at, source
		FROM country_level ORDER BY rank, country`)
	if err != nil {
		return nil, fmt.Errorf("store: country levels: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]CountryLevel)
	for rows.Next() {
		var (
			l    CountryLevel
			seen int64
		)
		if err := rows.Scan(&l.Country, &l.Level, &l.Score, &l.EventCount, &l.Rank,
			&seen, &l.Source); err != nil {
			return nil, fmt.Errorf("store: scan country level: %w", err)
		}
		l.ObservedAt = time.UnixMilli(seen).UTC()
		out[l.Country] = l
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: country levels: %w", err)
	}
	return out, nil
}
