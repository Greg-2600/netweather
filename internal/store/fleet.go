package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// FleetStats is the aggregate view from DESIGN.md §6.4: what the fleet is
// doing, not what one target is doing.
//
// It has no per-target data by design. The point is to answer "is my interval
// choice sane?" without turning a status endpoint into a second board.
type FleetStats struct {
	// ByClass counts enabled targets by class (service, country, network,
	// reference, or the empty class).
	ByClass map[string]int
	Total   int
	// EffectivePerSecond is the sum of 1/interval over enabled targets: the
	// scheduled probe rate if every target ran on time.
	EffectivePerSecond float64
	// Blocked and Unmeasurable are current states for this vantage.
	Blocked      int
	Unmeasurable int
	// ASNCount and CountryCount are distinct learned values, not target counts.
	ASNCount     int
	CountryCount int
	// TopASN is the most common origin ASN and its share of enabled targets.
	// Zero when no target has a learned ASN.
	TopASN      int
	TopASNName  string
	TopASNCount int
	TopASNShare float64
}

// OpenReadOnly opens an existing database without migrations and without a
// write pool. The doctor uses it so a diagnostic never changes the file it
// inspects.
func OpenReadOnly(ctx context.Context, path string) (*Store, error) {
	reader, err := openPool(ctx, path, false)
	if err != nil {
		return nil, err
	}
	return &Store{path: path, reader: reader}, nil
}

// FleetStats computes the aggregate view for one vantage.
func (s *Store) FleetStats(ctx context.Context, vantageID string) (FleetStats, error) {
	out := FleetStats{ByClass: make(map[string]int)}

	rows, err := s.reader.QueryContext(ctx, `
		SELECT COALESCE(class, ''), COUNT(*), COALESCE(SUM(1.0 / interval_s), 0)
		FROM target
		WHERE enabled = 1 AND interval_s > 0
		GROUP BY COALESCE(class, '')`)
	if err != nil {
		return out, fmt.Errorf("store: fleet classes: %w", err)
	}
	for rows.Next() {
		var (
			class string
			n     int
			rate  float64
		)
		if err := rows.Scan(&class, &n, &rate); err != nil {
			_ = rows.Close()
			return out, fmt.Errorf("store: scan fleet class: %w", err)
		}
		out.ByClass[class] = n
		out.Total += n
		out.EffectivePerSecond += rate
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: fleet classes: %w", err)
	}

	stateRows, err := s.reader.QueryContext(ctx, `
		SELECT COALESCE(ts.state, 'unknown'), COUNT(*)
		FROM target t
		LEFT JOIN target_state ts ON ts.target_id = t.id AND ts.vantage_id = ?
		WHERE t.enabled = 1
		GROUP BY 1`, vantageID)
	if err != nil {
		return out, fmt.Errorf("store: fleet states: %w", err)
	}
	for stateRows.Next() {
		var (
			st string
			n  int
		)
		if err := stateRows.Scan(&st, &n); err != nil {
			_ = stateRows.Close()
			return out, fmt.Errorf("store: scan fleet state: %w", err)
		}
		switch st {
		case "blocked":
			out.Blocked += n
		case "unmeasurable":
			out.Unmeasurable += n
		}
	}
	_ = stateRows.Close()
	if err := stateRows.Err(); err != nil {
		return out, fmt.Errorf("store: fleet states: %w", err)
	}

	if err := s.reader.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT CASE WHEN asn > 0 THEN asn END),
		       COUNT(DISTINCT CASE WHEN country <> '' THEN country END)
		FROM target WHERE enabled = 1`).Scan(&out.ASNCount, &out.CountryCount); err != nil {
		return out, fmt.Errorf("store: fleet coverage: %w", err)
	}

	// The most common origin ASN. Ties go to the lower ASN so a run is stable.
	err = s.reader.QueryRowContext(ctx, `
		SELECT asn, COALESCE(as_name, ''), COUNT(*)
		FROM target
		WHERE enabled = 1 AND asn > 0
		GROUP BY asn
		ORDER BY COUNT(*) DESC, asn
		LIMIT 1`).Scan(&out.TopASN, &out.TopASNName, &out.TopASNCount)
	// A fleet with no learned ASNs is not an error.
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, fmt.Errorf("store: fleet top ASN: %w", err)
	}
	if out.Total > 0 && out.TopASNCount > 0 {
		out.TopASNShare = float64(out.TopASNCount) / float64(out.Total)
	}
	return out, nil
}
