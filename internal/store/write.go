package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Greg-2600/netweather/internal/state"
)

// Probe is one observation to persist. Reason becomes state_change.reason and is
// expected to match the log line (LOGGING.md §3).
type Probe struct {
	TargetID   string
	VantageID  string
	TS         time.Time
	State      state.State
	Stage      string
	ErrorClass string
	HTTPStatus int
	LatencyMS  int
	Family     string
	CDN        string
	ASN        int
	Confidence string
	Detail     string
	Reason     string
	// TLS observations only. Zero values mean "not a TLS probe", not "unknown".
	CertIssuer   string
	CertNotAfter time.Time
}

// Target is the persisted materialisation of a config entry.
type Target struct {
	ID        string
	Host      string
	Port      int
	Probe     string
	Name      string
	Class     string
	Sector    string
	Country   string
	Tier      int
	IntervalS int
	// Enrichment, learned after the target exists. It is not part of the
	// config contract, so UpsertTarget never overwrites it.
	ASN           int
	Prefix        string
	CDN           string
	ASCountry     string
	ASName        string
	Measurability string
}

// EnsureVantage inserts a vantage or updates its labels.
func (s *Store) EnsureVantage(ctx context.Context, id, kind, role string) error {
	_, err := s.writer.ExecContext(ctx, `
		INSERT INTO vantage (id, kind, role, enabled, created_at)
		VALUES (?, ?, ?, 1, ?)
		ON CONFLICT (id) DO UPDATE SET kind = excluded.kind, role = excluded.role`,
		id, kind, role, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("store: vantage %q: %w", id, err)
	}
	return nil
}

// UpsertTarget inserts or updates a target, preserving its learned enrichment
// columns (ASN, CDN, measurability) on update (SCHEMA.md §4.1).
func (s *Store) UpsertTarget(ctx context.Context, t Target) error {
	now := time.Now().UnixMilli()
	_, err := s.writer.ExecContext(ctx, `
		INSERT INTO target (
			id, host, port, probe, name, class, sector, country, tier, interval_s,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			name = excluded.name,
			class = excluded.class,
			sector = excluded.sector,
			country = excluded.country,
			tier = excluded.tier,
			interval_s = excluded.interval_s,
			updated_at = excluded.updated_at`,
		t.ID, t.Host, t.Port, t.Probe, t.Name, t.Class, t.Sector, t.Country,
		t.Tier, t.IntervalS, now, now)
	if err != nil {
		return fmt.Errorf("store: target %q: %w", t.ID, err)
	}
	return nil
}

// Record persists a batch in one transaction: probe rows, the latest state per
// (target, vantage), accepted transitions, and the 5-minute rollup increment.
func (s *Store) Record(ctx context.Context, probes []Probe) error {
	if len(probes) == 0 {
		return nil
	}

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, p := range probes {
		if err := recordOne(ctx, tx, p); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

func recordOne(ctx context.Context, tx *sql.Tx, p Probe) error {
	ts := p.TS.UnixMilli()
	detail := sanitizeDetail(p.Detail)

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO probe (
			target_id, vantage_id, ts, bucket_30s, state, stage, error_class,
			http_status, latency_ms, family, cdn, asn, confidence, detail,
			cert_issuer, cert_not_after
		) VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, 0), NULLIF(?, 0),
		          NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, 0), ?, NULLIF(?, ''),
		          NULLIF(?, ''), NULLIF(?, 0))`,
		p.TargetID, p.VantageID, ts, ts/30000, string(p.State), p.Stage,
		p.ErrorClass, p.HTTPStatus, p.LatencyMS, p.Family, p.CDN, p.ASN,
		confidenceOrDefault(p.Confidence), detail,
		p.CertIssuer, certExpiry(p.CertNotAfter)); err != nil {
		return fmt.Errorf("store: probe %s: %w", p.TargetID, err)
	}

	up, degraded, down, blocked, unknown := counters(p.State)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rollup_5m (
			target_id, vantage_id, bucket, n, up, degraded, down, blocked, unknown, ewma50_ms, ewma95_ms
		) VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (target_id, vantage_id, bucket) DO UPDATE SET
			n        = n + 1,
			up       = up + excluded.up,
			degraded = degraded + excluded.degraded,
			down     = down + excluded.down,
			blocked  = blocked + excluded.blocked,
			unknown  = unknown + excluded.unknown,
			-- SCHEMA.md §6: these are EWMAs, not percentiles. A new sample moves
			-- the value 30% of the way, so one slow probe cannot rewrite a bucket
			-- and a sustained shift is still visible within a few probes.
			ewma50_ms = CASE
				WHEN excluded.ewma50_ms IS NULL THEN ewma50_ms
				WHEN ewma50_ms IS NULL THEN excluded.ewma50_ms
				ELSE CAST(0.3*excluded.ewma50_ms + 0.7*ewma50_ms AS INTEGER)
			END,
			ewma95_ms = CASE
				WHEN excluded.ewma95_ms IS NULL THEN ewma95_ms
				WHEN ewma95_ms IS NULL THEN excluded.ewma95_ms
				ELSE CAST(0.3*excluded.ewma95_ms + 0.7*ewma95_ms AS INTEGER)
			END`,
		p.TargetID, p.VantageID, ts/300000,
		up, degraded, down, blocked, unknown,
		nullInt(p.LatencyMS), nullInt(p.LatencyMS)); err != nil {
		return fmt.Errorf("store: rollup %s: %w", p.TargetID, err)
	}

	return recordState(ctx, tx, p, ts, detail)
}

func recordState(ctx context.Context, tx *sql.Tx, p Probe, ts int64, detail string) error {
	var (
		prevState  string
		prevSince  int64
		failStreak int
		okStreak   int
	)

	err := tx.QueryRowContext(ctx, `
		SELECT state, since, fail_streak, ok_streak
		FROM target_state WHERE target_id = ? AND vantage_id = ?`,
		p.TargetID, p.VantageID).Scan(&prevState, &prevSince, &failStreak, &okStreak)

	next := string(p.State)
	newFail, newOK := nextStreaks(p.State, failStreak, okStreak)
	lastError := ""
	if p.State == state.Down && detail != "" {
		lastError = detail
	}

	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, `
			INSERT INTO target_state (
				target_id, vantage_id, state, since, fail_streak, ok_streak,
				last_probe_at, last_error
			) VALUES (?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''))`,
			p.TargetID, p.VantageID, next, ts, newFail, newOK, ts, lastError)
		if err != nil {
			return fmt.Errorf("store: initial state %s: %w", p.TargetID, err)
		}
		return nil

	case err != nil:
		return fmt.Errorf("store: read state %s: %w", p.TargetID, err)
	}

	changed := prevState != next
	since := prevSince
	prevForRow := sql.NullString{}

	if changed {
		since = ts
		prevForRow = sql.NullString{String: prevState, Valid: true}
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE target_state SET
			state = ?, since = ?, prev_state = COALESCE(?, prev_state),
			fail_streak = ?, ok_streak = ?, last_probe_at = ?, last_error = NULLIF(?, '')
		WHERE target_id = ? AND vantage_id = ?`,
		next, since, prevForRow, newFail, newOK, ts, lastError,
		p.TargetID, p.VantageID); err != nil {
		return fmt.Errorf("store: update state %s: %w", p.TargetID, err)
	}

	if !changed {
		return nil
	}

	reason := strings.TrimSpace(p.Reason)
	if reason == "" {
		reason = strings.TrimSpace(detail)
	}
	if reason == "" {
		reason = "state changed"
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO state_change (
			target_id, vantage_id, ts, from_state, to_state, reason
		) VALUES (?, ?, ?, ?, ?, ?)`,
		p.TargetID, p.VantageID, ts, prevState, next, reason); err != nil {
		return fmt.Errorf("store: state change %s: %w", p.TargetID, err)
	}
	return nil
}

func nextStreaks(next state.State, fail, ok int) (int, int) {
	switch next {
	case state.Down:
		return fail + 1, 0
	case state.Up, state.UpReachability:
		return 0, ok + 1
	default:
		return fail, ok
	}
}

// counters maps the eight states onto the five rollup columns. CONFIG_ERROR and
// UNKNOWN both land in "unknown": neither is a health signal, and adding a column
// for a state that never counts would invite someone to count it.
func counters(s state.State) (up, degraded, down, blocked, unknown int) {
	switch s {
	case state.Up, state.UpReachability:
		return 1, 0, 0, 0, 0
	case state.Degraded:
		return 0, 1, 0, 0, 0
	case state.Down:
		return 0, 0, 1, 0, 0
	case state.Blocked:
		return 0, 0, 0, 1, 0
	default:
		return 0, 0, 0, 0, 1
	}
}

func confidenceOrDefault(c string) string {
	if c == "" {
		return "low"
	}
	return c
}

func nullInt(v int) any {
	if v <= 0 {
		return nil
	}
	return v
}

// certExpiry stores a zero time as NULL: "not a TLS probe" and "expires at the
// epoch" are different claims.
func certExpiry(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

// sanitizeDetail enforces the 200-byte, control-character-free contract from
// SCHEMA.md §1.7. Response bodies never reach this function.
func sanitizeDetail(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)

	if len(s) <= 200 {
		return s
	}
	b := []byte(s)[:200]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}
