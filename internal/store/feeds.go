package store

import (
	"context"
	"fmt"
	"time"
)

// ExternalEvent is one corroborating event from an outside feed. It is evidence
// for an incident, never an incident itself.
type ExternalEvent struct {
	ID         int64
	Feed       string
	ExternalID string
	Kind       string
	Severity   float64
	Country    string
	ASN        int
	StartedAt  time.Time
	EndedAt    time.Time
	ObservedAt time.Time
	Payload    string
}

// FeedState is how a feed is doing: last success, last error, and how many
// failures in a row. "Radar: stale (2h)" is answerable only because this exists.
type FeedState struct {
	Feed                string
	LastOKAt            time.Time
	LastErr             string
	ConsecutiveFailures int
	NextPollAt          time.Time
}

// UpsertExternalEvents stores a batch idempotently. The feed's own id is the
// identity, so a re-poll that returns the same event updates rather than
// duplicates it.
func (s *Store) UpsertExternalEvents(ctx context.Context, events []ExternalEvent) error {
	if len(events) == 0 {
		return nil
	}

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin external events: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, e := range events {
		var ended any
		if !e.EndedAt.IsZero() {
			ended = e.EndedAt.UnixMilli()
		}
		var asn any
		if e.ASN > 0 {
			asn = e.ASN
		}
		var country any
		if e.Country != "" {
			country = e.Country
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO external_event (
				feed, external_id, kind, severity, country, asn,
				started_at, ended_at, observed_at, payload
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (feed, external_id) DO UPDATE SET
				kind = excluded.kind,
				severity = excluded.severity,
				country = excluded.country,
				asn = excluded.asn,
				started_at = excluded.started_at,
				ended_at = excluded.ended_at,
				observed_at = excluded.observed_at,
				payload = excluded.payload`,
			e.Feed, e.ExternalID, e.Kind, e.Severity, country, asn,
			e.StartedAt.UnixMilli(), ended, e.ObservedAt.UnixMilli(), e.Payload); err != nil {
			return fmt.Errorf("store: external event %s/%s: %w", e.Feed, e.ExternalID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit external events: %w", err)
	}
	return nil
}

// ExternalEvents lists recent events, newest first.
func (s *Store) ExternalEvents(ctx context.Context, limit int) ([]ExternalEvent, error) {
	if limit <= 0 {
		limit = 50
	}

	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, feed, external_id, kind, COALESCE(severity, 0),
		       COALESCE(country, ''), COALESCE(asn, 0),
		       started_at, COALESCE(ended_at, 0), observed_at, payload
		FROM external_event
		ORDER BY started_at DESC, id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: external events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ExternalEvent
	for rows.Next() {
		var (
			e     ExternalEvent
			start int64
			end   int64
			seen  int64
		)
		if err := rows.Scan(&e.ID, &e.Feed, &e.ExternalID, &e.Kind, &e.Severity,
			&e.Country, &e.ASN, &start, &end, &seen, &e.Payload); err != nil {
			return nil, fmt.Errorf("store: scan external event: %w", err)
		}
		e.StartedAt = time.UnixMilli(start).UTC()
		if end > 0 {
			e.EndedAt = time.UnixMilli(end).UTC()
		}
		e.ObservedAt = time.UnixMilli(seen).UTC()
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: external events: %w", err)
	}
	return out, nil
}

// RecordFeedSuccess resets the failure count and records the success time.
func (s *Store) RecordFeedSuccess(ctx context.Context, feed string, at time.Time) error {
	_, err := s.writer.ExecContext(ctx, `
		INSERT INTO feed_state (feed, last_ok_at, consecutive_failures)
		VALUES (?, ?, 0)
		ON CONFLICT (feed) DO UPDATE SET
			last_ok_at = excluded.last_ok_at,
			last_err = NULL,
			consecutive_failures = 0`,
		feed, at.UnixMilli())
	if err != nil {
		return fmt.Errorf("store: feed success %q: %w", feed, err)
	}
	return nil
}

// RecordFeedFailure increments the failure count and stores the error. The last
// success time is preserved: "stale since 09:00" is the useful fact.
func (s *Store) RecordFeedFailure(ctx context.Context, feed, detail string, at time.Time) error {
	_, err := s.writer.ExecContext(ctx, `
		INSERT INTO feed_state (feed, last_err, consecutive_failures)
		VALUES (?, ?, 1)
		ON CONFLICT (feed) DO UPDATE SET
			last_err = excluded.last_err,
			consecutive_failures = consecutive_failures + 1`,
		feed, detail)
	if err != nil {
		return fmt.Errorf("store: feed failure %q: %w", feed, err)
	}
	return nil
}

// FeedStates returns every feed that has ever run, ordered by name.
func (s *Store) FeedStates(ctx context.Context) ([]FeedState, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT feed, COALESCE(last_ok_at, 0), COALESCE(last_err, ''),
		       consecutive_failures, COALESCE(next_poll_at, 0)
		FROM feed_state ORDER BY feed`)
	if err != nil {
		return nil, fmt.Errorf("store: feed states: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []FeedState
	for rows.Next() {
		var (
			f    FeedState
			ok   int64
			next int64
		)
		if err := rows.Scan(&f.Feed, &ok, &f.LastErr, &f.ConsecutiveFailures, &next); err != nil {
			return nil, fmt.Errorf("store: scan feed state: %w", err)
		}
		if ok > 0 {
			f.LastOKAt = time.UnixMilli(ok).UTC()
		}
		if next > 0 {
			f.NextPollAt = time.UnixMilli(next).UTC()
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: feed states: %w", err)
	}
	return out, nil
}

// SetFeedNextPoll records when a feed should next run. Diagnostics only.
func (s *Store) SetFeedNextPoll(ctx context.Context, feed string, at time.Time) error {
	_, err := s.writer.ExecContext(ctx, `
		INSERT INTO feed_state (feed, next_poll_at, consecutive_failures)
		VALUES (?, ?, 0)
		ON CONFLICT (feed) DO UPDATE SET next_poll_at = excluded.next_poll_at`,
		feed, at.UnixMilli())
	if err != nil {
		return fmt.Errorf("store: feed next poll %q: %w", feed, err)
	}
	return nil
}
