package store

import (
	"context"
	"fmt"
	"time"
)

// Corridor is one configured path pair. Endpoints live in the config, not the
// database: the store only needs the identity and the labels to draw.
type Corridor struct {
	ID   string
	From string
	To   string
}

// CorridorSample is one five-minute observation. Loss is a percentage.
type CorridorSample struct {
	CorridorID string
	TS         time.Time
	RTTMS      float64
	Loss       float64
	OK         bool
}

// CorridorEvent is an open or closed step change.
type CorridorEvent struct {
	ID         string
	CorridorID string
	Kind       string
	StartedAt  time.Time
	EndedAt    time.Time
	Detail     string
}

// CorridorStatus is what a page or the API needs: the corridor, its newest
// sample, and the open event if there is one.
type CorridorStatus struct {
	Corridor Corridor
	Latest   *CorridorSample
	Open     *CorridorEvent
}

// UpsertCorridors reconciles the configured set. Missing corridors are disabled
// rather than deleted, because their samples and events are history.
func (s *Store) UpsertCorridors(ctx context.Context, corridors []Corridor) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: corridors: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UnixMilli()
	keep := make(map[string]struct{}, len(corridors))
	for _, c := range corridors {
		keep[c.ID] = struct{}{}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO corridor (id, from_label, to_label, enabled, updated_at)
			VALUES (?, ?, ?, 1, ?)
			ON CONFLICT (id) DO UPDATE SET
				from_label = excluded.from_label,
				to_label = excluded.to_label,
				enabled = 1,
				updated_at = excluded.updated_at`,
			c.ID, c.From, c.To, now); err != nil {
			return fmt.Errorf("store: upsert corridor %q: %w", c.ID, err)
		}
	}

	rows, err := tx.QueryContext(ctx, `SELECT id FROM corridor WHERE enabled = 1`)
	if err != nil {
		return fmt.Errorf("store: corridors: %w", err)
	}
	var stale []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: corridors: %w", err)
		}
		if _, ok := keep[id]; !ok {
			stale = append(stale, id)
		}
	}
	_ = rows.Close()
	for _, id := range stale {
		if _, err := tx.ExecContext(ctx,
			`UPDATE corridor SET enabled = 0, updated_at = ? WHERE id = ?`, now, id); err != nil {
			return fmt.Errorf("store: disable corridor %q: %w", id, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: corridors: %w", err)
	}
	return nil
}

// Corridors returns the enabled set, id-ordered so the page is stable.
func (s *Store) Corridors(ctx context.Context) ([]Corridor, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT id, from_label, to_label FROM corridor WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: corridors: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Corridor
	for rows.Next() {
		var c Corridor
		if err := rows.Scan(&c.ID, &c.From, &c.To); err != nil {
			return nil, fmt.Errorf("store: scan corridor: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: corridors: %w", err)
	}
	return out, nil
}

// RecordCorridorSample stores one observation.
func (s *Store) RecordCorridorSample(ctx context.Context, sample CorridorSample) error {
	_, err := s.writer.ExecContext(ctx, `
		INSERT INTO corridor_sample (corridor_id, ts, rtt_ms, loss, ok)
		VALUES (?, ?, ?, ?, ?)`,
		sample.CorridorID, sample.TS.UnixMilli(), nullFloat(sample.RTTMS),
		sample.Loss, boolInt(sample.OK))
	if err != nil {
		return fmt.Errorf("store: corridor sample %q: %w", sample.CorridorID, err)
	}
	return nil
}

// CorridorSamples returns samples since the cutoff, oldest first, which is the
// order the detection rule reads them in.
func (s *Store) CorridorSamples(ctx context.Context, corridorID string, since time.Time) ([]CorridorSample, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT corridor_id, ts, COALESCE(rtt_ms, 0), loss, ok
		FROM corridor_sample
		WHERE corridor_id = ? AND ts >= ?
		ORDER BY ts, id`, corridorID, since.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("store: corridor samples: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []CorridorSample
	for rows.Next() {
		var (
			sample CorridorSample
			ts     int64
			ok     int
		)
		if err := rows.Scan(&sample.CorridorID, &ts, &sample.RTTMS, &sample.Loss, &ok); err != nil {
			return nil, fmt.Errorf("store: scan corridor sample: %w", err)
		}
		sample.TS = time.UnixMilli(ts).UTC()
		sample.OK = ok != 0
		out = append(out, sample)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: corridor samples: %w", err)
	}
	return out, nil
}

// CorridorStatus returns every enabled corridor with its newest sample and its
// open event. Three small queries are simpler than one wide join and the set is
// bounded by MaxCorridors.
func (s *Store) CorridorStatus(ctx context.Context) ([]CorridorStatus, error) {
	corridors, err := s.Corridors(ctx)
	if err != nil {
		return nil, err
	}
	if len(corridors) == 0 {
		return nil, nil
	}

	latest := make(map[string]CorridorSample, len(corridors))
	rows, err := s.reader.QueryContext(ctx, `
		SELECT cs.corridor_id, cs.ts, COALESCE(cs.rtt_ms, 0), cs.loss, cs.ok
		FROM corridor_sample cs
		JOIN (
			SELECT corridor_id, MAX(ts) AS ts FROM corridor_sample GROUP BY corridor_id
		) newest ON newest.corridor_id = cs.corridor_id AND newest.ts = cs.ts
		GROUP BY cs.corridor_id`)
	if err != nil {
		return nil, fmt.Errorf("store: corridor latest: %w", err)
	}
	for rows.Next() {
		var (
			sample CorridorSample
			ts     int64
			ok     int
		)
		if err := rows.Scan(&sample.CorridorID, &ts, &sample.RTTMS, &sample.Loss, &ok); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("store: scan corridor latest: %w", err)
		}
		sample.TS = time.UnixMilli(ts).UTC()
		sample.OK = ok != 0
		latest[sample.CorridorID] = sample
	}
	_ = rows.Close()

	open := make(map[string]CorridorEvent, len(corridors))
	rows, err = s.reader.QueryContext(ctx, `
		SELECT id, corridor_id, kind, started_at, COALESCE(ended_at, 0), detail
		FROM corridor_event WHERE ended_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("store: corridor events: %w", err)
	}
	for rows.Next() {
		var (
			event   CorridorEvent
			started int64
			ended   int64
		)
		if err := rows.Scan(&event.ID, &event.CorridorID, &event.Kind, &started, &ended, &event.Detail); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("store: scan corridor event: %w", err)
		}
		event.StartedAt = time.UnixMilli(started).UTC()
		if ended > 0 {
			event.EndedAt = time.UnixMilli(ended).UTC()
		}
		open[event.CorridorID] = event
	}
	_ = rows.Close()

	out := make([]CorridorStatus, 0, len(corridors))
	for _, c := range corridors {
		status := CorridorStatus{Corridor: c}
		if sample, ok := latest[c.ID]; ok {
			copied := sample
			status.Latest = &copied
		}
		if event, ok := open[c.ID]; ok {
			copied := event
			status.Open = &copied
		}
		out = append(out, status)
	}
	return out, nil
}

// OpenCorridorEvent records a new step change.
func (s *Store) OpenCorridorEvent(ctx context.Context, event CorridorEvent) error {
	_, err := s.writer.ExecContext(ctx, `
		INSERT INTO corridor_event (id, corridor_id, kind, started_at, detail)
		VALUES (?, ?, ?, ?, ?)`,
		event.ID, event.CorridorID, event.Kind, event.StartedAt.UnixMilli(), event.Detail)
	if err != nil {
		return fmt.Errorf("store: open corridor event %q: %w", event.ID, err)
	}
	return nil
}

// CloseCorridorEvent closes every open event for a corridor. Closing when none
// is open is a no-op.
func (s *Store) CloseCorridorEvent(ctx context.Context, corridorID string, at time.Time) error {
	_, err := s.writer.ExecContext(ctx,
		`UPDATE corridor_event SET ended_at = ? WHERE corridor_id = ? AND ended_at IS NULL`,
		at.UnixMilli(), corridorID)
	if err != nil {
		return fmt.Errorf("store: close corridor event %q: %w", corridorID, err)
	}
	return nil
}

// CorridorEvents lists the newest events, open or closed.
func (s *Store) CorridorEvents(ctx context.Context, limit int) ([]CorridorEvent, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, corridor_id, kind, started_at, COALESCE(ended_at, 0), detail
		FROM corridor_event ORDER BY started_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: corridor events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []CorridorEvent
	for rows.Next() {
		var (
			event   CorridorEvent
			started int64
			ended   int64
		)
		if err := rows.Scan(&event.ID, &event.CorridorID, &event.Kind, &started, &ended, &event.Detail); err != nil {
			return nil, fmt.Errorf("store: scan corridor event: %w", err)
		}
		event.StartedAt = time.UnixMilli(started).UTC()
		if ended > 0 {
			event.EndedAt = time.UnixMilli(ended).UTC()
		}
		out = append(out, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: corridor events: %w", err)
	}
	return out, nil
}

func nullFloat(v float64) any {
	if v == 0 {
		return nil
	}
	return v
}
