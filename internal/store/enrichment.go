package store

import (
	"context"
	"fmt"
	"time"
)

// EnrichmentTarget is a target whose ASN has not been learned yet.
type EnrichmentTarget struct {
	ID   string
	Host string
}

// Enrichment is the learned metadata for one target. Zero values mean "not
// learned", never "empty is a fact".
type Enrichment struct {
	ASN       int
	Prefix    string
	CDN       string
	ASCountry string
	ASName    string
}

// TargetsNeedingEnrichment returns enabled hostname targets with no ASN yet.
// IP-literal anchors are excluded: they are prefixes and countries, not services
// with a CDN in front of them.
func (s *Store) TargetsNeedingEnrichment(ctx context.Context, limit int) ([]EnrichmentTarget, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, host FROM target
		WHERE enabled = 1 AND COALESCE(asn, 0) = 0 AND class IN ('service', 'reference')
		ORDER BY updated_at DESC, id
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: targets needing enrichment: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []EnrichmentTarget
	for rows.Next() {
		var t EnrichmentTarget
		if err := rows.Scan(&t.ID, &t.Host); err != nil {
			return nil, fmt.Errorf("store: scan enrichment target: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: targets needing enrichment: %w", err)
	}
	return out, nil
}

// UpdateEnrichment records the learned facts. Config reconciliation never
// touches these columns, so an enrichment pass and a config edit cannot erase
// each other's work.
func (s *Store) UpdateEnrichment(ctx context.Context, id string, e Enrichment) error {
	_, err := s.writer.ExecContext(ctx, `
		UPDATE target SET
			asn = NULLIF(?, 0),
			prefix = NULLIF(?, ''),
			cdn = NULLIF(?, ''),
			as_country = NULLIF(?, ''),
			as_name = NULLIF(?, ''),
			updated_at = ?
		WHERE id = ?`,
		e.ASN, e.Prefix, e.CDN, e.ASCountry, e.ASName, time.Now().UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("store: enrichment %q: %w", id, err)
	}
	return nil
}
