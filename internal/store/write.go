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

	"github.com/Greg-2600/netweather/internal/confirm"
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
	ID      string
	Host    string
	Port    int
	Probe   string
	Name    string
	Class   string
	Sector  string
	Country string
	// StatusURL is the operator's status page for this target. It is
	// config-owned, like the name, so UpsertTarget writes it.
	StatusURL string
	Tier      int
	IntervalS int
	// Enrichment, learned after the target exists. It is not part of the
	// config contract, so UpsertTarget never overwrites it.
	ASN           int
	Prefix        string
	CDN           string
	ASCountry     string
	ASName        string
	IXNames       string
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
			id, host, port, probe, name, class, sector, country, status_url, tier, interval_s,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			name = excluded.name,
			class = excluded.class,
			sector = excluded.sector,
			country = excluded.country,
			status_url = excluded.status_url,
			tier = excluded.tier,
			interval_s = excluded.interval_s,
			-- A load is the documented way back from a runtime disable
			-- (ACCEPTABLE_USE.md §7, API.md §6.9.1): the row was never deleted,
			-- so bringing it back is an upsert rather than a new target.
			enabled = 1,
			updated_at = excluded.updated_at`,
		t.ID, t.Host, t.Port, t.Probe, t.Name, t.Class, t.Sector, t.Country,
		t.StatusURL, t.Tier, t.IntervalS, now, now)
	if err != nil {
		return fmt.Errorf("store: target %q: %w", t.ID, err)
	}
	return nil
}

// Transition is one confirmed state change, returned to the caller after the
// batch commits. The store does not log it: the process boundary decides what a
// transition means and writes the one INFO line (LOGGING.md §3, §4).
type Transition struct {
	TargetID   string
	VantageID  string
	Host       string
	Tier       int
	TS         time.Time
	From       state.State
	To         state.State
	Stage      string
	ErrorClass string
	HTTPStatus int
	LatencyMS  int
	Confidence string
	Reason     string
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

	var transitions []Transition
	for _, p := range probes {
		transition, err := recordOne(ctx, tx, p)
		if err != nil {
			return err
		}
		if transition != nil {
			transitions = append(transitions, *transition)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}

	// After the commit, so a subscriber never sees a transition that failed to
	// persist. The callback is expected to be quick (a log write).
	if s.OnTransition != nil {
		for _, transition := range transitions {
			s.OnTransition(transition)
		}
	}
	return nil
}

func recordOne(ctx context.Context, tx *sql.Tx, p Probe) (*Transition, error) {
	ts := p.TS.UnixMilli()
	detail := sanitizeDetail(p.Detail)

	res, err := tx.ExecContext(ctx, `
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
		p.CertIssuer, certExpiry(p.CertNotAfter))
	if err != nil {
		return nil, fmt.Errorf("store: probe %s: %w", p.TargetID, err)
	}
	probeID, _ := res.LastInsertId()

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
		return nil, fmt.Errorf("store: rollup %s: %w", p.TargetID, err)
	}

	transition, err := recordState(ctx, tx, p, ts, detail)
	if err != nil {
		return nil, err
	}
	if err := applyFlapConfidence(ctx, tx, p, ts, probeID); err != nil {
		return nil, err
	}
	return transition, nil
}

// flapTransitions and flapWindow are CLASSIFIER.md §9: a target that has
// changed state more than six times in a day is flapping. Its observations are
// still real, but they are weak evidence, so the stored confidence becomes low
// and a reader does not have to replay history to learn that.
const (
	flapTransitions = 6
	flapWindow      = 24 * time.Hour
)

// applyFlapConfidence downgrades the probe that was just written when its
// target has been bouncing. The count is windowed on the probe's own
// timestamp, so a backfill is judged against the day it belongs to rather than
// against wall-clock now.
func applyFlapConfidence(ctx context.Context, tx *sql.Tx, p Probe, ts, probeID int64) error {
	var transitions int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM state_change
		WHERE target_id = ? AND vantage_id = ? AND ts >= ? AND ts <= ?`,
		p.TargetID, p.VantageID, ts-flapWindow.Milliseconds(), ts).
		Scan(&transitions); err != nil {
		return fmt.Errorf("store: flap count %s: %w", p.TargetID, err)
	}
	if transitions <= flapTransitions {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE probe SET confidence = 'low' WHERE id = ?`, probeID); err != nil {
		return fmt.Errorf("store: flap confidence %s: %w", p.TargetID, err)
	}
	return nil
}

func recordState(ctx context.Context, tx *sql.Tx, p Probe, ts int64, detail string) (*Transition, error) {
	var (
		class       string
		host        string
		tier        int
		prevState   string
		prevSince   int64
		failStreak  int
		okStreak    int
		prevLastErr string
	)

	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(t.class, ''), t.host, t.tier, ts.state, ts.since,
		       ts.fail_streak, ts.ok_streak, COALESCE(ts.last_error, '')
		FROM target_state ts
		JOIN target t ON t.id = ts.target_id
		WHERE ts.target_id = ? AND ts.vantage_id = ?`,
		p.TargetID, p.VantageID).
		Scan(&class, &host, &tier, &prevState, &prevSince, &failStreak, &okStreak, &prevLastErr)

	// The confirmation rules need the last three observations: the reference
	// class confirms DOWN on two failures out of three, and BLOCKED needs the
	// same challenge signature twice (DESIGN.md §7.4). The current probe was
	// inserted just above, so it is the newest row.
	recentFails := 0
	blockedStreak := 0
	recent, errRecent := recentObservations(ctx, tx, p.TargetID, p.VantageID)
	if errRecent == nil {
		for i, row := range recent {
			if row.state == state.Down {
				if i < 3 {
					recentFails++
				}
			}
		}
		if p.State == state.Blocked && len(recent) > 0 {
			sig := recent[0].signature
			blockedStreak = 1
			for _, row := range recent[1:] {
				if row.state == state.Blocked && row.signature == sig {
					blockedStreak++
					continue
				}
				break
			}
		}
	}

	decision := confirm.Decide(confirm.Inputs{
		Rule:          confirm.RuleFor(confirm.ClassFor(class)),
		Prev:          state.State(prevState),
		PrevSince:     millisOrZero(prevSince),
		Candidate:     p.State,
		FailStreak:    failStreak,
		OKStreak:      okStreak,
		RecentFails:   recentFails,
		BlockedStreak: blockedStreak,
		Now:           time.UnixMilli(ts).UTC(),
	})

	next := string(decision.State)
	lastError := prevLastErr
	if decision.State != state.Down {
		lastError = ""
	} else if p.State == state.Down && detail != "" {
		lastError = detail
	}

	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, `
			INSERT INTO target_state (
				target_id, vantage_id, state, since, fail_streak, ok_streak,
				last_probe_at, last_error
			) VALUES (?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''))`,
			p.TargetID, p.VantageID, next, ts, decision.FailStreak, decision.OKStreak, ts, lastError)
		if err != nil {
			return nil, fmt.Errorf("store: initial state %s: %w", p.TargetID, err)
		}
		// Adopting the first observation is not a transition (DESIGN.md §7.4),
		// so there is nothing for the caller to log.
		return nil, nil

	case err != nil:
		return nil, fmt.Errorf("store: read state %s: %w", p.TargetID, err)
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
		next, since, prevForRow, decision.FailStreak, decision.OKStreak, ts, lastError,
		p.TargetID, p.VantageID); err != nil {
		return nil, fmt.Errorf("store: update state %s: %w", p.TargetID, err)
	}

	if !changed {
		return nil, nil
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
		return nil, fmt.Errorf("store: state change %s: %w", p.TargetID, err)
	}

	return &Transition{
		TargetID:   p.TargetID,
		VantageID:  p.VantageID,
		Host:       host,
		Tier:       tier,
		TS:         time.UnixMilli(ts).UTC(),
		From:       state.State(prevState),
		To:         decision.State,
		Stage:      p.Stage,
		ErrorClass: p.ErrorClass,
		HTTPStatus: p.HTTPStatus,
		LatencyMS:  p.LatencyMS,
		Confidence: p.Confidence,
		Reason:     reason,
	}, nil
}

// observation is the part of a stored probe the confirmation rules read.
type observation struct {
	state     state.State
	signature string
}

// recentObservations returns the newest probes for one target and vantage,
// newest first. Five are enough for the widest rule ("two of three") plus the
// BLOCKED signature walk.
func recentObservations(ctx context.Context, tx *sql.Tx, targetID, vantageID string) ([]observation, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT state, COALESCE(error_class, ''), COALESCE(detail, '')
		FROM probe
		WHERE target_id = ? AND vantage_id = ?
		ORDER BY id DESC
		LIMIT 5`, targetID, vantageID)
	if err != nil {
		return nil, fmt.Errorf("store: recent observations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]observation, 0, 5)
	for rows.Next() {
		var (
			row        observation
			rawState   string
			errorClass string
			detail     string
		)
		if err := rows.Scan(&rawState, &errorClass, &detail); err != nil {
			return nil, fmt.Errorf("store: scan observation: %w", err)
		}
		row.state = state.State(rawState)
		row.signature = errorClass + "\x00" + detail
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: recent observations: %w", err)
	}
	return out, nil
}

// millisOrZero turns a stored unix-millisecond value into a time. Zero means
// "no previous transition", which the debounce treats as a bootstrap.
func millisOrZero(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
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
