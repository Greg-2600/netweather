package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Greg-2600/netweather/internal/state"
)

// TargetState is the latest observation per (target, vantage).
type TargetState struct {
	TargetID   string
	VantageID  string
	State      state.State
	Since      time.Time
	PrevState  string
	FailStreak int
	OKStreak   int
	LastProbe  time.Time
	LastError  string
}

// States returns the latest state for every target at one vantage.
func (s *Store) States(ctx context.Context, vantageID string) (map[string]TargetState, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT target_id, vantage_id, state, since, COALESCE(prev_state, ''),
		       fail_streak, ok_streak, last_probe_at, COALESCE(last_error, '')
		FROM target_state
		WHERE vantage_id = ?`, vantageID)
	if err != nil {
		return nil, fmt.Errorf("store: states: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]TargetState)
	for rows.Next() {
		var (
			st     TargetState
			since  int64
			probed int64
		)
		if err := rows.Scan(&st.TargetID, &st.VantageID, &st.State, &since,
			&st.PrevState, &st.FailStreak, &st.OKStreak, &probed, &st.LastError); err != nil {
			return nil, fmt.Errorf("store: scan state: %w", err)
		}
		st.Since = time.UnixMilli(since).UTC()
		st.LastProbe = time.UnixMilli(probed).UTC()
		out[st.TargetID] = st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: states: %w", err)
	}
	return out, nil
}

// Stats is the small diagnostic surface used by /api/v1/meta and by tests: it
// proves the pragmas actually applied rather than trusting the DSN.
type Stats struct {
	Path          string
	SchemaVersion int
	JournalMode   string
	ForeignKeys   bool
}

// Stats reads the effective pragmas from the reader pool.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	out := Stats{Path: s.path}

	if err := s.reader.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&out.JournalMode); err != nil {
		return out, fmt.Errorf("store: journal_mode: %w", err)
	}

	var fk int
	if err := s.reader.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		return out, fmt.Errorf("store: foreign_keys: %w", err)
	}
	out.ForeignKeys = fk == 1

	version, err := s.SchemaVersion(ctx)
	if err != nil {
		return out, err
	}
	out.SchemaVersion = version
	return out, nil
}

// ProbeDetail returns the most recent stored detail line for a target. It is
// used by diagnostics and tests; the drawer uses a list query that lands later.
func (s *Store) ProbeDetail(ctx context.Context, targetID, vantageID string) (string, error) {
	var detail string
	err := s.reader.QueryRowContext(ctx, `
		SELECT COALESCE(detail, '') FROM probe
		WHERE target_id = ? AND vantage_id = ?
		ORDER BY ts DESC, id DESC LIMIT 1`, targetID, vantageID).Scan(&detail)
	if err != nil {
		return "", fmt.Errorf("store: probe detail: %w", err)
	}
	return detail, nil
}

// TargetRow is a target joined with its latest state for one vantage.
type TargetRow struct {
	Target
	State state.State
	Since time.Time
	// Last observation, for the board's corner badge. Empty stage means
	// "never probed".
	Stage      string
	ErrorClass string
	HTTPStatus int
	// Leaf certificate from the last TLS observation. Zero values mean the
	// last probe was not TLS, not that the certificate is unknown.
	CertIssuer   string
	CertNotAfter time.Time
	// CertPinned reports whether any issuer has been learned for this target.
	CertPinned bool
}

// TargetsWithState is the board query: every enabled target plus its current
// state, defaulting to UNKNOWN when it has never been probed.
func (s *Store) TargetsWithState(ctx context.Context, vantageID string) ([]TargetRow, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT t.id, t.host, t.port, t.probe, t.name, t.class,
		       COALESCE(t.sector, ''), COALESCE(t.country, ''), t.tier, t.interval_s,
		       COALESCE(t.asn, 0), COALESCE(t.prefix, ''), COALESCE(t.cdn, ''), t.measurability,
		       COALESCE(t.as_country, ''), COALESCE(t.as_name, ''), COALESCE(t.ix_names, ''),
		       COALESCE(ts.state, 'unknown'), COALESCE(ts.since, 0),
		       COALESCE(p.stage, ''), COALESCE(p.error_class, ''), COALESCE(p.http_status, 0),
		       COALESCE(p.cert_issuer, ''), COALESCE(p.cert_not_after, 0),
		       EXISTS(SELECT 1 FROM cert_pin cp WHERE cp.target_id = t.id)
		FROM target t
		LEFT JOIN target_state ts ON ts.target_id = t.id AND ts.vantage_id = ?
		LEFT JOIN probe p ON p.id = (
			SELECT p2.id FROM probe p2
			WHERE p2.target_id = t.id AND p2.vantage_id = ?
			ORDER BY p2.ts DESC, p2.id DESC LIMIT 1
		)
		WHERE t.enabled = 1
		ORDER BY t.class, COALESCE(t.sector, ''), t.name`, vantageID, vantageID)
	if err != nil {
		return nil, fmt.Errorf("store: targets with state: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TargetRow
	for rows.Next() {
		var (
			row      TargetRow
			since    int64
			notAfter int64
		)
		if err := rows.Scan(
			&row.ID, &row.Host, &row.Port, &row.Probe, &row.Name, &row.Class,
			&row.Sector, &row.Country, &row.Tier, &row.IntervalS,
			&row.ASN, &row.Prefix, &row.CDN, &row.Measurability,
			&row.ASCountry, &row.ASName, &row.IXNames,
			&row.State, &since,
			&row.Stage, &row.ErrorClass, &row.HTTPStatus,
			&row.CertIssuer, &notAfter, &row.CertPinned,
		); err != nil {
			return nil, fmt.Errorf("store: scan target: %w", err)
		}
		if since > 0 {
			row.Since = time.UnixMilli(since).UTC()
		}
		if notAfter > 0 {
			row.CertNotAfter = time.UnixMilli(notAfter).UTC()
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: targets with state: %w", err)
	}
	return out, nil
}

// ProbeRow is one stored observation, shaped for the drawer.
type ProbeRow struct {
	TS         time.Time
	State      state.State
	Stage      string
	ErrorClass string
	HTTPStatus int
	LatencyMS  int
	Confidence string
	Detail     string
}

// RecentProbes returns the newest observations for a target and vantage.
func (s *Store) RecentProbes(ctx context.Context, targetID, vantageID string, limit int) ([]ProbeRow, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.reader.QueryContext(ctx, `
		SELECT ts, state, stage, COALESCE(error_class, ''), COALESCE(http_status, 0),
		       COALESCE(latency_ms, 0), confidence, COALESCE(detail, '')
		FROM probe
		WHERE target_id = ? AND vantage_id = ?
		ORDER BY ts DESC, id DESC
		LIMIT ?`, targetID, vantageID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: recent probes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ProbeRow
	for rows.Next() {
		var (
			row ProbeRow
			ts  int64
		)
		if err := rows.Scan(&ts, &row.State, &row.Stage, &row.ErrorClass,
			&row.HTTPStatus, &row.LatencyMS, &row.Confidence, &row.Detail); err != nil {
			return nil, fmt.Errorf("store: scan probe: %w", err)
		}
		row.TS = time.UnixMilli(ts).UTC()
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: recent probes: %w", err)
	}
	return out, nil
}

// TargetExists reports whether an enabled target has this id.
func (s *Store) TargetExists(ctx context.Context, id string) (bool, error) {
	var one int
	err := s.reader.QueryRowContext(ctx,
		`SELECT 1 FROM target WHERE id = ? AND enabled = 1`, id).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: target exists: %w", err)
	default:
		return true, nil
	}
}

// TargetIDs returns the enabled target ids. Config reconciliation uses it to
// find targets that were removed from the file.
func (s *Store) TargetIDs(ctx context.Context) ([]string, error) {
	rows, err := s.reader.QueryContext(ctx, `SELECT id FROM target WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: target ids: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan target id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: target ids: %w", err)
	}
	return out, nil
}

// DisableTarget marks a target disabled rather than deleting it, so history and
// incidents survive a config edit (SCHEMA.md §4.1).
func (s *Store) DisableTarget(ctx context.Context, id string) error {
	_, err := s.writer.ExecContext(ctx,
		`UPDATE target SET enabled = 0, updated_at = ? WHERE id = ?`,
		time.Now().UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("store: disable target %q: %w", id, err)
	}
	return nil
}

// TargetByID returns one enabled target with its latest state.
func (s *Store) TargetByID(ctx context.Context, vantageID, id string) (TargetRow, error) {
	var (
		row      TargetRow
		since    int64
		notAfter int64
	)
	err := s.reader.QueryRowContext(ctx, `
		SELECT t.id, t.host, t.port, t.probe, t.name, t.class,
		       COALESCE(t.sector, ''), COALESCE(t.country, ''), t.tier, t.interval_s,
		       COALESCE(t.asn, 0), COALESCE(t.prefix, ''), COALESCE(t.cdn, ''), t.measurability,
		       COALESCE(t.as_country, ''), COALESCE(t.as_name, ''), COALESCE(t.ix_names, ''),
		       COALESCE(ts.state, 'unknown'), COALESCE(ts.since, 0),
		       COALESCE(p.stage, ''), COALESCE(p.error_class, ''), COALESCE(p.http_status, 0),
		       COALESCE(p.cert_issuer, ''), COALESCE(p.cert_not_after, 0),
		       EXISTS(SELECT 1 FROM cert_pin cp WHERE cp.target_id = t.id)
		FROM target t
		LEFT JOIN target_state ts ON ts.target_id = t.id AND ts.vantage_id = ?
		LEFT JOIN probe p ON p.id = (
			SELECT p2.id FROM probe p2
			WHERE p2.target_id = t.id AND p2.vantage_id = ?
			ORDER BY p2.ts DESC, p2.id DESC LIMIT 1
		)
		WHERE t.id = ? AND t.enabled = 1`, vantageID, vantageID, id).Scan(
		&row.ID, &row.Host, &row.Port, &row.Probe, &row.Name, &row.Class,
		&row.Sector, &row.Country, &row.Tier, &row.IntervalS,
		&row.ASN, &row.Prefix, &row.CDN, &row.Measurability,
		&row.ASCountry, &row.ASName, &row.IXNames,
		&row.State, &since,
		&row.Stage, &row.ErrorClass, &row.HTTPStatus,
		&row.CertIssuer, &notAfter, &row.CertPinned,
	)
	if err != nil {
		return TargetRow{}, err
	}
	if since > 0 {
		row.Since = time.UnixMilli(since).UTC()
	}
	if notAfter > 0 {
		row.CertNotAfter = time.UnixMilli(notAfter).UTC()
	}
	return row, nil
}

// Rollup is one bucket at the requested resolution.
type Rollup struct {
	Bucket   time.Time
	N        int
	Up       int
	Degraded int
	Down     int
	Blocked  int
	Unknown  int
	// EWMAs, not percentiles (SCHEMA.md §6.2). Zero means "no latency sample".
	Ewma50MS int
	Ewma95MS int
}

// RollupFor reads one bucket at 5m, 1h, or 1d resolution. A missing bucket is a
// zero, not an error.
func (s *Store) RollupFor(ctx context.Context, resolution, targetID, vantageID string, bucket time.Time) (Rollup, error) {
	r := Rollup{Bucket: bucket.UTC()}

	var (
		query    string
		bucketID int64
	)
	switch resolution {
	case "5m":
		query = `SELECT n, up, degraded, down, blocked, unknown,
		                COALESCE(ewma50_ms, 0), COALESCE(ewma95_ms, 0)
		         FROM rollup_5m WHERE target_id = ? AND vantage_id = ? AND bucket = ?`
		bucketID = bucket.UnixMilli() / 300000
		r.Bucket = bucket.UTC().Truncate(5 * time.Minute)
	case "1h":
		query = `SELECT n, up, degraded, down, blocked, unknown,
		                COALESCE(ewma50_ms, 0), COALESCE(ewma95_ms, 0)
		         FROM rollup_1h WHERE target_id = ? AND vantage_id = ? AND bucket = ?`
		bucketID = bucket.UnixMilli() / 3600000
		r.Bucket = bucket.UTC().Truncate(time.Hour)
	case "1d":
		query = `SELECT n, up, degraded, down, blocked, unknown,
		                COALESCE(ewma50_ms, 0), COALESCE(ewma95_ms, 0)
		         FROM rollup_1d WHERE target_id = ? AND vantage_id = ? AND bucket = ?`
		bucketID = bucket.UnixMilli() / 86400000
		r.Bucket = bucket.UTC().Truncate(24 * time.Hour)
	default:
		return r, fmt.Errorf("store: unknown rollup resolution %q", resolution)
	}

	err := s.reader.QueryRowContext(ctx, query, targetID, vantageID, bucketID).
		Scan(&r.N, &r.Up, &r.Degraded, &r.Down, &r.Blocked, &r.Unknown,
			&r.Ewma50MS, &r.Ewma95MS)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, fmt.Errorf("store: rollup %s: %w", resolution, err)
	}
	return r, nil
}

// Series returns rollup buckets in [from, to) at the requested resolution.
func (s *Store) Series(ctx context.Context, targetID, vantageID, resolution string, from, to time.Time) ([]Rollup, error) {
	var (
		query string
		unit  int64
	)
	switch resolution {
	case "5m":
		query = `SELECT bucket, n, up, degraded, down, blocked, unknown,
		                COALESCE(ewma50_ms, 0), COALESCE(ewma95_ms, 0)
		         FROM rollup_5m WHERE target_id = ? AND vantage_id = ?
		           AND bucket >= ? AND bucket < ? ORDER BY bucket`
		unit = 300000
	case "1h":
		query = `SELECT bucket, n, up, degraded, down, blocked, unknown,
		                COALESCE(ewma50_ms, 0), COALESCE(ewma95_ms, 0)
		         FROM rollup_1h WHERE target_id = ? AND vantage_id = ?
		           AND bucket >= ? AND bucket < ? ORDER BY bucket`
		unit = 3600000
	case "1d":
		query = `SELECT bucket, n, up, degraded, down, blocked, unknown,
		                COALESCE(ewma50_ms, 0), COALESCE(ewma95_ms, 0)
		         FROM rollup_1d WHERE target_id = ? AND vantage_id = ?
		           AND bucket >= ? AND bucket < ? ORDER BY bucket`
		unit = 86400000
	default:
		return nil, fmt.Errorf("store: unknown resolution %q", resolution)
	}

	rows, err := s.reader.QueryContext(ctx, query, targetID, vantageID,
		from.UnixMilli()/unit, to.UnixMilli()/unit)
	if err != nil {
		return nil, fmt.Errorf("store: series: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Rollup
	for rows.Next() {
		var (
			r      Rollup
			bucket int64
		)
		if err := rows.Scan(&bucket, &r.N, &r.Up, &r.Degraded, &r.Down, &r.Blocked, &r.Unknown,
			&r.Ewma50MS, &r.Ewma95MS); err != nil {
			return nil, fmt.Errorf("store: scan series: %w", err)
		}
		r.Bucket = time.UnixMilli(bucket * unit).UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: series: %w", err)
	}
	return out, nil
}

// RollupAt reads one 5-minute bucket.
func (s *Store) RollupAt(ctx context.Context, targetID, vantageID string, bucket time.Time) (Rollup, error) {
	r := Rollup{Bucket: bucket.UTC().Truncate(5 * time.Minute)}

	err := s.reader.QueryRowContext(ctx, `
		SELECT n, up, degraded, down, blocked, unknown,
		       COALESCE(ewma50_ms, 0), COALESCE(ewma95_ms, 0)
		FROM rollup_5m
		WHERE target_id = ? AND vantage_id = ? AND bucket = ?`,
		targetID, vantageID, bucket.UnixMilli()/300000).
		Scan(&r.N, &r.Up, &r.Degraded, &r.Down, &r.Blocked, &r.Unknown,
			&r.Ewma50MS, &r.Ewma95MS)
	if errors.Is(err, sql.ErrNoRows) {
		// A missing bucket is a legitimate zero, not a failure.
		return r, nil
	}
	if err != nil {
		return r, fmt.Errorf("store: rollup: %w", err)
	}
	return r, nil
}

// StateChangeCount is used by tests and diagnostics.
// StateChange is one accepted transition, as stored at write time.
type StateChange struct {
	TS     time.Time
	From   state.State
	To     state.State
	Reason string
}

// StateChanges returns the newest transitions for one target, newest first.
func (s *Store) StateChanges(ctx context.Context, targetID, vantageID string, limit int) ([]StateChange, error) {
	if limit <= 0 {
		limit = 20
	}

	rows, err := s.reader.QueryContext(ctx, `
		SELECT ts, from_state, to_state, reason
		FROM state_change
		WHERE target_id = ? AND vantage_id = ?
		ORDER BY ts DESC, id DESC LIMIT ?`, targetID, vantageID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: state changes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []StateChange
	for rows.Next() {
		var (
			c  StateChange
			ts int64
		)
		if err := rows.Scan(&ts, &c.From, &c.To, &c.Reason); err != nil {
			return nil, fmt.Errorf("store: scan state change: %w", err)
		}
		c.TS = time.UnixMilli(ts).UTC()
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: state changes: %w", err)
	}
	return out, nil
}

func (s *Store) StateChangeCount(ctx context.Context, targetID, vantageID string) (int, error) {
	var n int
	err := s.reader.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM state_change WHERE target_id = ? AND vantage_id = ?`,
		targetID, vantageID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: state change count: %w", err)
	}
	return n, nil
}
