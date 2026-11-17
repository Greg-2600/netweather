package store

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
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

// ScopeCorroboration returns feed events from the window that match a target's
// country or ASN. It is the drawer's "is anything known to be wrong on this
// network?" panel.
//
// It is deliberately a scope match, not a provider match: netweather does not
// map a target to its operator's status page, so the panel says "events in this
// target's country or ASN", never "the provider says it is down".
func (s *Store) ScopeCorroboration(ctx context.Context, country string, asn int, since time.Time, limit int) ([]ExternalEvent, error) {
	if limit <= 0 {
		limit = 10
	}
	if asn <= 0 && country == "" {
		return nil, nil
	}

	query := `
		SELECT id, feed, external_id, kind, COALESCE(severity, 0),
		       COALESCE(country, ''), COALESCE(asn, 0),
		       started_at, COALESCE(ended_at, 0), observed_at, payload
		FROM external_event
		WHERE started_at >= ?`
	args := []any{since.UnixMilli()}

	switch {
	case asn > 0 && country != "":
		query += ` AND (asn = ? OR country = ?)`
		args = append(args, asn, country)
	case asn > 0:
		query += ` AND asn = ?`
		args = append(args, asn)
	default:
		query += ` AND country = ?`
		args = append(args, country)
	}

	query += ` ORDER BY COALESCE(severity, 0) DESC, started_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: scope corroboration: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return collectExternalEvents(rows, "scope corroboration")
}

// ProviderCorroboration returns statuspage events from the operator's declared
// status page for a target (DESIGN.md §10.5).
//
// This is a provider match, unlike ScopeCorroboration's country/ASN match: it
// answers "what does this target's operator say?" and is what separates their
// outage from your path. The statuspage adapter stores ExternalID as
// "<host>:<incident id>", so the host is the stable key. substr() is used
// instead of LIKE so a hostname containing "%" cannot become a wildcard.
func (s *Store) ProviderCorroboration(ctx context.Context, statusURL string, since time.Time, limit int) ([]ExternalEvent, error) {
	u, err := url.Parse(strings.TrimSpace(statusURL))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("store: provider corroboration needs a status URL")
	}
	if limit <= 0 {
		limit = 10
	}
	prefix := u.Host + ":"

	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, feed, external_id, kind, COALESCE(severity, 0),
		       COALESCE(country, ''), COALESCE(asn, 0),
		       started_at, COALESCE(ended_at, 0), observed_at, payload
		FROM external_event
		WHERE substr(external_id, 1, ?) = ? AND started_at >= ?
		ORDER BY COALESCE(severity, 0) DESC, started_at DESC LIMIT ?`,
		len(prefix), prefix, since.UnixMilli(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: provider corroboration: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return collectExternalEvents(rows, "provider corroboration")
}
