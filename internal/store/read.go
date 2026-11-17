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
}

// TargetsWithState is the board query: every enabled target plus its current
// state, defaulting to UNKNOWN when it has never been probed.
func (s *Store) TargetsWithState(ctx context.Context, vantageID string) ([]TargetRow, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT t.id, t.host, t.port, t.probe, t.name, t.class,
		       COALESCE(t.sector, ''), COALESCE(t.country, ''), t.tier, t.interval_s,
		       COALESCE(ts.state, 'unknown'), COALESCE(ts.since, 0)
		FROM target t
		LEFT JOIN target_state ts ON ts.target_id = t.id AND ts.vantage_id = ?
		WHERE t.enabled = 1
		ORDER BY t.class, COALESCE(t.sector, ''), t.name`, vantageID)
	if err != nil {
		return nil, fmt.Errorf("store: targets with state: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TargetRow
	for rows.Next() {
		var (
			row   TargetRow
			since int64
		)
		if err := rows.Scan(
			&row.ID, &row.Host, &row.Port, &row.Probe, &row.Name, &row.Class,
			&row.Sector, &row.Country, &row.Tier, &row.IntervalS,
			&row.State, &since,
		); err != nil {
			return nil, fmt.Errorf("store: scan target: %w", err)
		}
		if since > 0 {
			row.Since = time.UnixMilli(since).UTC()
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

// Rollup is one bucket at the requested resolution.
type Rollup struct {
	Bucket   time.Time
	N        int
	Up       int
	Degraded int
	Down     int
	Blocked  int
	Unknown  int
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
		query = `SELECT n, up, degraded, down, blocked, unknown
		         FROM rollup_5m WHERE target_id = ? AND vantage_id = ? AND bucket = ?`
		bucketID = bucket.UnixMilli() / 300000
		r.Bucket = bucket.UTC().Truncate(5 * time.Minute)
	case "1h":
		query = `SELECT n, up, degraded, down, blocked, unknown
		         FROM rollup_1h WHERE target_id = ? AND vantage_id = ? AND bucket = ?`
		bucketID = bucket.UnixMilli() / 3600000
		r.Bucket = bucket.UTC().Truncate(time.Hour)
	case "1d":
		query = `SELECT n, up, degraded, down, blocked, unknown
		         FROM rollup_1d WHERE target_id = ? AND vantage_id = ? AND bucket = ?`
		bucketID = bucket.UnixMilli() / 86400000
		r.Bucket = bucket.UTC().Truncate(24 * time.Hour)
	default:
		return r, fmt.Errorf("store: unknown rollup resolution %q", resolution)
	}

	err := s.reader.QueryRowContext(ctx, query, targetID, vantageID, bucketID).
		Scan(&r.N, &r.Up, &r.Degraded, &r.Down, &r.Blocked, &r.Unknown)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, fmt.Errorf("store: rollup %s: %w", resolution, err)
	}
	return r, nil
}

// RollupAt reads one 5-minute bucket.
func (s *Store) RollupAt(ctx context.Context, targetID, vantageID string, bucket time.Time) (Rollup, error) {
	r := Rollup{Bucket: bucket.UTC().Truncate(5 * time.Minute)}

	err := s.reader.QueryRowContext(ctx, `
		SELECT n, up, degraded, down, blocked, unknown
		FROM rollup_5m
		WHERE target_id = ? AND vantage_id = ? AND bucket = ?`,
		targetID, vantageID, bucket.UnixMilli()/300000).
		Scan(&r.N, &r.Up, &r.Degraded, &r.Down, &r.Blocked, &r.Unknown)
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
