package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Observation is one target's state plus the enrichment correlation groups on.
type Observation struct {
	TargetID string
	Name     string
	Host     string
	CDN      string
	ASN      int
	Prefix24 string
	Country  string
	Sector   string
	Stage    string
	Failing  bool
	// IntervalS is the target's configured interval, used to decide whether its
	// last observation is still fresh enough to judge.
	IntervalS int
	// LastProbe is when this state was last refreshed. Detection skips stale
	// observations; coverage is where staleness is reported.
	LastProbe time.Time
}

// Attribution is one ranked explanation stored with an incident.
type Attribution struct {
	Rank    int
	Kind    string
	Value   string
	Failing int
	Total   int
	Share   float64
	Score   float64
}

// Incident is an opened cluster.
type Incident struct {
	ID           string
	OpenedAt     time.Time
	ClosedAt     time.Time
	Verdict      string
	Scope        string
	ScopeValue   string
	Headline     string
	Confidence   string
	ClosedReason string
	Annotation   string
	AnnotatedAt  time.Time
}

// Observations returns every probed target with its latest state and stage. Only
// targets that have been observed at least once are included, because an
// unprobed target cannot be evidence for or against an incident.
func (s *Store) Observations(ctx context.Context, vantageID string) ([]Observation, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT t.id, t.name, t.host, COALESCE(t.cdn, ''), COALESCE(t.asn, 0),
		       COALESCE(t.prefix, ''), COALESCE(t.country, ''), COALESCE(t.sector, ''),
		       COALESCE(p.stage, ''),
		       t.interval_s,
		       COALESCE(ts.last_probe_at, 0),
		       CASE WHEN ts.state = 'down' THEN 1 ELSE 0 END
		FROM target t
		JOIN target_state ts ON ts.target_id = t.id AND ts.vantage_id = ?
		LEFT JOIN probe p ON p.id = (
			SELECT id FROM probe
			WHERE target_id = t.id AND vantage_id = ts.vantage_id
			ORDER BY ts DESC, id DESC LIMIT 1
		)
		WHERE t.enabled = 1`, vantageID)
	if err != nil {
		return nil, fmt.Errorf("store: observations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Observation
	for rows.Next() {
		var (
			o         Observation
			failing   int
			lastProbe int64
		)
		if err := rows.Scan(&o.TargetID, &o.Name, &o.Host, &o.CDN, &o.ASN,
			&o.Prefix24, &o.Country, &o.Sector, &o.Stage, &o.IntervalS, &lastProbe, &failing); err != nil {
			return nil, fmt.Errorf("store: scan observation: %w", err)
		}
		if lastProbe > 0 {
			o.LastProbe = time.UnixMilli(lastProbe).UTC()
		}
		o.Failing = failing == 1
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: observations: %w", err)
	}
	return out, nil
}

// HasOpenIncident reports whether an incident is already open for this cluster,
// so a detector that runs every minute does not open the same one repeatedly.
func (s *Store) HasOpenIncident(ctx context.Context, scope, value string) (bool, error) {
	var one int
	err := s.reader.QueryRowContext(ctx, `
		SELECT 1 FROM incident
		WHERE closed_at IS NULL AND scope = ? AND COALESCE(scope_value, '') = ?
		LIMIT 1`, scope, value).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: open incident: %w", err)
	default:
		return true, nil
	}
}

// OpenIncident writes the incident, its affected targets, and every ranked
// attribution in one transaction.
func (s *Store) OpenIncident(ctx context.Context, inc Incident, targetIDs []string, attrs []Attribution) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin incident: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	opened := inc.OpenedAt.UnixMilli()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO incident (id, opened_at, verdict, scope, scope_value, headline, confidence)
		VALUES (?, ?, ?, ?, NULLIF(?, ''), ?, ?)`,
		inc.ID, opened, inc.Verdict, inc.Scope, inc.ScopeValue, inc.Headline, inc.Confidence); err != nil {
		return fmt.Errorf("store: insert incident: %w", err)
	}

	for _, id := range targetIDs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO incident_target (incident_id, target_id, first_seen, last_seen)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (incident_id, target_id) DO UPDATE SET last_seen = excluded.last_seen`,
			inc.ID, id, opened, opened); err != nil {
			return fmt.Errorf("store: incident target %s: %w", id, err)
		}
	}

	for _, a := range attrs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO attribution (incident_id, rank, kind, value, failing, total, share, score)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			inc.ID, a.Rank, a.Kind, a.Value, a.Failing, a.Total, a.Share, a.Score); err != nil {
			return fmt.Errorf("store: attribution %s: %w", a.Value, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit incident: %w", err)
	}
	return nil
}

// OpenIncidents returns unclosed incidents, newest first. The detector uses it
// to avoid reopening a cluster that is already being tracked.
func (s *Store) OpenIncidents(ctx context.Context) ([]Incident, error) {
	return s.Incidents(ctx, true, 200)
}

// CloseIncident closes an incident. Closing an already-closed incident is a
// no-op, not an error: reconciliation may race with a manual close.
func (s *Store) CloseIncident(ctx context.Context, id, reason string, at time.Time) error {
	_, err := s.writer.ExecContext(ctx, `
		UPDATE incident SET closed_at = ?, closed_reason = ?
		WHERE id = ? AND closed_at IS NULL`, at.UnixMilli(), reason, id)
	if err != nil {
		return fmt.Errorf("store: close incident: %w", err)
	}
	return nil
}

// AnnotateIncident records the operator's ground truth for an incident. It is
// what turns a stored incident into a labelled example for the backtest corpus
// (SCHEMA.md §7); an unknown id is an error so a typo does not silently vanish.
func (s *Store) AnnotateIncident(ctx context.Context, id, annotation string, at time.Time) error {
	res, err := s.writer.ExecContext(ctx, `
		UPDATE incident SET annotation = NULLIF(?, ''), annotated_at = ?
		WHERE id = ?`, annotation, at.UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("store: annotate incident: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: annotate incident: %w", err)
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Incidents lists incidents newest first; openOnly filters to unclosed ones.
func (s *Store) Incidents(ctx context.Context, openOnly bool, limit int) ([]Incident, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `
		SELECT id, opened_at, COALESCE(closed_at, 0), verdict, scope,
		       COALESCE(scope_value, ''), headline, confidence,
		       COALESCE(closed_reason, ''), COALESCE(annotation, ''), COALESCE(annotated_at, 0)
		FROM incident`
	if openOnly {
		query += ` WHERE closed_at IS NULL`
	}
	query += ` ORDER BY opened_at DESC LIMIT ?`

	rows, err := s.reader.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("store: incidents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Incident
	for rows.Next() {
		inc, err := scanIncident(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, inc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: incidents: %w", err)
	}
	return out, nil
}

// IncidentByID returns one incident with its attributions and target ids.
func (s *Store) IncidentByID(ctx context.Context, id string) (Incident, []Attribution, []string, error) {
	row := s.reader.QueryRowContext(ctx, `
		SELECT id, opened_at, COALESCE(closed_at, 0), verdict, scope,
		       COALESCE(scope_value, ''), headline, confidence,
		       COALESCE(closed_reason, ''), COALESCE(annotation, ''), COALESCE(annotated_at, 0)
		FROM incident WHERE id = ?`, id)

	inc, err := scanIncident(row.Scan)
	if err != nil {
		return Incident{}, nil, nil, err
	}

	attrRows, err := s.reader.QueryContext(ctx, `
		SELECT rank, kind, value, failing, total, share, score
		FROM attribution WHERE incident_id = ? ORDER BY rank`, id)
	if err != nil {
		return Incident{}, nil, nil, fmt.Errorf("store: attributions: %w", err)
	}
	defer func() { _ = attrRows.Close() }()

	var attrs []Attribution
	for attrRows.Next() {
		var a Attribution
		if err := attrRows.Scan(&a.Rank, &a.Kind, &a.Value, &a.Failing, &a.Total, &a.Share, &a.Score); err != nil {
			return Incident{}, nil, nil, fmt.Errorf("store: scan attribution: %w", err)
		}
		attrs = append(attrs, a)
	}
	if err := attrRows.Err(); err != nil {
		return Incident{}, nil, nil, fmt.Errorf("store: attributions: %w", err)
	}

	targetRows, err := s.reader.QueryContext(ctx, `
		SELECT target_id FROM incident_target WHERE incident_id = ? ORDER BY target_id`, id)
	if err != nil {
		return Incident{}, nil, nil, fmt.Errorf("store: incident targets: %w", err)
	}
	defer func() { _ = targetRows.Close() }()

	var targets []string
	for targetRows.Next() {
		var targetID string
		if err := targetRows.Scan(&targetID); err != nil {
			return Incident{}, nil, nil, fmt.Errorf("store: scan incident target: %w", err)
		}
		targets = append(targets, targetID)
	}
	if err := targetRows.Err(); err != nil {
		return Incident{}, nil, nil, fmt.Errorf("store: incident targets: %w", err)
	}

	return inc, attrs, targets, nil
}

type scanFunc func(dest ...any) error

func scanIncident(scan scanFunc) (Incident, error) {
	var (
		inc     Incident
		opened  int64
		closed  int64
		annotAt int64
	)
	if err := scan(&inc.ID, &opened, &closed, &inc.Verdict, &inc.Scope,
		&inc.ScopeValue, &inc.Headline, &inc.Confidence,
		&inc.ClosedReason, &inc.Annotation, &annotAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Incident{}, err
		}
		return Incident{}, fmt.Errorf("store: scan incident: %w", err)
	}
	inc.OpenedAt = time.UnixMilli(opened).UTC()
	if closed > 0 {
		inc.ClosedAt = time.UnixMilli(closed).UTC()
	}
	if annotAt > 0 {
		inc.AnnotatedAt = time.UnixMilli(annotAt).UTC()
	}
	return inc, nil
}
