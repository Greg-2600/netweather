package store

import (
	"context"
	"fmt"
	"time"
)

// Anomaly is one baseline-relative detection event. It is an observation, not a
// verdict: an incident may later be opened from it.
type Anomaly struct {
	ID         int64
	TS         time.Time
	Scope      string
	ScopeValue string
	Kind       string
	Observed   float64
	Baseline   float64
	MAD        float64
	Sigma      float64
	Buckets    int
	IncidentID string
}

// RecordAnomaly appends one detection event.
func (s *Store) RecordAnomaly(ctx context.Context, a Anomaly) error {
	_, err := s.writer.ExecContext(ctx, `
		INSERT INTO anomaly (ts, scope, scope_value, kind, observed, baseline, mad, sigma, buckets, incident_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''))`,
		a.TS.UnixMilli(), a.Scope, a.ScopeValue, a.Kind,
		a.Observed, a.Baseline, a.MAD, a.Sigma, a.Buckets, a.IncidentID)
	if err != nil {
		return fmt.Errorf("store: anomaly: %w", err)
	}
	return nil
}

// Anomalies lists recent detection events, newest first.
func (s *Store) Anomalies(ctx context.Context, limit int) ([]Anomaly, error) {
	if limit <= 0 {
		limit = 50
	}

	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, ts, scope, scope_value, kind, observed, baseline, mad, sigma, buckets,
		       COALESCE(incident_id, '')
		FROM anomaly ORDER BY ts DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: anomalies: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Anomaly
	for rows.Next() {
		var (
			a  Anomaly
			ts int64
		)
		if err := rows.Scan(&a.ID, &ts, &a.Scope, &a.ScopeValue, &a.Kind,
			&a.Observed, &a.Baseline, &a.MAD, &a.Sigma, &a.Buckets, &a.IncidentID); err != nil {
			return nil, fmt.Errorf("store: scan anomaly: %w", err)
		}
		a.TS = time.UnixMilli(ts).UTC()
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: anomalies: %w", err)
	}
	return out, nil
}
