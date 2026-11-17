package store

import (
	"context"
	"fmt"
	"time"
)

// SelfCheck is one self-health observation. It exists to answer "is the monitor
// itself healthy?" before anyone reads the board and believes it.
type SelfCheck struct {
	ID     int64
	TS     time.Time
	Check  string
	OK     bool
	Value  string
	Detail string
}

// RecordSelfCheck appends one observation.
func (s *Store) RecordSelfCheck(ctx context.Context, c SelfCheck) error {
	_, err := s.writer.ExecContext(ctx, `
		INSERT INTO self_check (ts, "check", ok, value, detail)
		VALUES (?, ?, ?, NULLIF(?, ''), NULLIF(?, ''))`,
		c.TS.UnixMilli(), c.Check, boolInt(c.OK), c.Value, c.Detail)
	if err != nil {
		return fmt.Errorf("store: self check %q: %w", c.Check, err)
	}
	return nil
}

// LatestSelfChecks returns the newest observation per check name.
func (s *Store) LatestSelfChecks(ctx context.Context) ([]SelfCheck, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT sc.id, sc.ts, sc."check", sc.ok, COALESCE(sc.value, ''), COALESCE(sc.detail, '')
		FROM self_check sc
		JOIN (SELECT "check", MAX(ts) AS ts FROM self_check GROUP BY "check") newest
		  ON newest."check" = sc."check" AND newest.ts = sc.ts
		ORDER BY sc."check"`)
	if err != nil {
		return nil, fmt.Errorf("store: latest self checks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SelfCheck
	for rows.Next() {
		var (
			c  SelfCheck
			ts int64
			ok int
		)
		if err := rows.Scan(&c.ID, &ts, &c.Check, &ok, &c.Value, &c.Detail); err != nil {
			return nil, fmt.Errorf("store: scan self check: %w", err)
		}
		c.TS = time.UnixMilli(ts).UTC()
		c.OK = ok == 1
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: latest self checks: %w", err)
	}
	return out, nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
