package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// AtypicalScope is the reach of one atypical-day exclusion. An empty Scope
// means every scope; otherwise it names one scope and value (a country
// baseline, narrowed by a calendar entry's region tags).
type AtypicalScope struct {
	Scope      string
	ScopeValue string
}

// atypicalDaySpan returns the hour-bucket range covering one UTC day.
func atypicalDaySpan(day string) (from, to int64, err error) {
	start, err := time.Parse("2006-01-02", day)
	if err != nil {
		return 0, 0, fmt.Errorf("store: atypical day %q is not YYYY-MM-DD", day)
	}
	return start.UnixMilli() / 3600000, start.Add(24*time.Hour).UnixMilli() / 3600000, nil
}

// MarkAtypicalDay records that a UTC day must not contribute baseline samples
// (DESIGN.md §7.2). It deletes the samples already written for the day within
// the exclusion's reach, so a day that becomes atypical at 06:00 loses its
// morning rather than keeping it, and later samples are skipped by
// SampleScopes.
//
// The operation is idempotent: the insert ignores an existing row and the
// delete simply finds nothing the second time.
func (s *Store) MarkAtypicalDay(ctx context.Context, day, scope, scopeValue, source string, now time.Time) (int64, error) {
	from, to, err := atypicalDaySpan(day)
	if err != nil {
		return 0, err
	}

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: atypical day %s: %w", day, err)
	}
	defer func() { _ = tx.Rollback() }()

	var res sql.Result
	if scope == "" {
		res, err = tx.ExecContext(ctx,
			`DELETE FROM scope_sample WHERE bucket >= ? AND bucket < ?`, from, to)
	} else {
		res, err = tx.ExecContext(ctx,
			`DELETE FROM scope_sample WHERE scope = ? AND scope_value = ? AND bucket >= ? AND bucket < ?`,
			scope, scopeValue, from, to)
	}
	if err != nil {
		return 0, fmt.Errorf("store: atypical day %s: %w", day, err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO atypical_day (day, scope, scope_value, marked_at, source)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (day, scope, scope_value) DO NOTHING`,
		day, scope, scopeValue, now.UnixMilli(), source); err != nil {
		return 0, fmt.Errorf("store: atypical day %s: %w", day, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: atypical day %s: %w", day, err)
	}

	deleted, _ := res.RowsAffected()
	return deleted, nil
}

// AtypicalDayScopes returns the exclusions recorded for one UTC day.
func (s *Store) AtypicalDayScopes(ctx context.Context, day string) ([]AtypicalScope, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT scope, scope_value FROM atypical_day WHERE day = ?`, day)
	if err != nil {
		return nil, fmt.Errorf("store: atypical day scopes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []AtypicalScope
	for rows.Next() {
		var a AtypicalScope
		if err := rows.Scan(&a.Scope, &a.ScopeValue); err != nil {
			return nil, fmt.Errorf("store: scan atypical day: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: atypical day scopes: %w", err)
	}
	return out, nil
}

// atypicalMatches reports whether a day's exclusions cover a scope.
func atypicalMatches(exclusions []AtypicalScope, ref ScopeRef) bool {
	for _, a := range exclusions {
		if a.Scope == "" {
			return true
		}
		if a.Scope == ref.Scope && a.ScopeValue == ref.Value {
			return true
		}
	}
	return false
}
