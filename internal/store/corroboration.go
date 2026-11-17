package store

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// Corroboration returns external feed events that overlap an incident's window
// and, when the incident names a country or ASN, match that scope.
//
// It is evidence, not attribution: a matching outage event makes an incident
// more believable, and a matching "operational" status page makes a local
// outage more likely to be your path. Neither changes the verdict.
func (s *Store) Corroboration(ctx context.Context, inc Incident, now time.Time, limit int) ([]ExternalEvent, error) {
	if limit <= 0 {
		limit = 20
	}

	end := inc.ClosedAt
	if end.IsZero() {
		end = now
	}
	// Two hours before the open catches an upstream event that preceded the
	// first probe failure, which is usually what happened.
	start := inc.OpenedAt.Add(-2 * time.Hour)
	if !end.After(start) {
		end = start.Add(2 * time.Hour)
	}

	query := `
		SELECT id, feed, external_id, kind, COALESCE(severity, 0),
		       COALESCE(country, ''), COALESCE(asn, 0),
		       started_at, COALESCE(ended_at, 0), observed_at, payload
		FROM external_event
		WHERE started_at < ? AND (ended_at IS NULL OR ended_at >= ?)`
	args := []any{end.UnixMilli(), start.UnixMilli()}

	switch inc.Scope {
	case "country":
		query += ` AND country = ?`
		args = append(args, inc.ScopeValue)
	case "asn":
		if n, err := strconv.Atoi(inc.ScopeValue); err == nil && n > 0 {
			query += ` AND asn = ?`
			args = append(args, n)
		}
	}

	query += ` ORDER BY COALESCE(severity, 0) DESC, started_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: corroboration: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return collectExternalEvents(rows, "corroboration")
}
