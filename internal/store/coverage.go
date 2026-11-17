package store

import (
	"context"
	"fmt"
	"time"
)

// CoverageBucket is one slice of the blind-spot panel: a class, sector, country,
// or ASN with how many of its targets are actually measurable.
type CoverageBucket struct {
	Kind         string
	Value        string
	Total        int
	Measurable   int
	Unmeasurable int
}

// CoverageTotals is the fleet-wide summary.
type CoverageTotals struct {
	Total        int
	Measurable   int
	Unmeasurable int
}

// StaleTarget is a target the scheduler has not reached recently.
type StaleTarget struct {
	ID        string
	Host      string
	State     string
	LastProbe time.Time
	IntervalS int
}

// Coverage is the answer to "where are we blind?". It is deliberately about
// measurement, not health: a down target is still measured.
type Coverage struct {
	Totals        CoverageTotals
	Concentration CoverageConcentration
	Classes       []CoverageBucket
	Sectors       []CoverageBucket
	BlindSpots    []CoverageBucket
	Stale         []StaleTarget
}

// CoverageConcentration is the concentration block from DESIGN.md §10.6: how
// much of the fleet sits behind a few networks, and how thin the anchor
// coverage is. It is the difference between "750 targets" and "750 targets
// behind 341 ASNs, a fifth of them behind one".
type CoverageConcentration struct {
	ASNCount     int
	CountryCount int
	TopASN       int
	TopASNName   string
	TopASNShare  float64
	Top10Share   float64
	// SingleAnchor counts countries that have exactly one country anchor, so
	// losing that target loses the country.
	SingleAnchor int
	// ThinSectors counts sectors below 50% measurable, which the board renders
	// as UNKNOWN rather than healthy.
	ThinSectors int
}

// minBlindSpot is the member count below which a slice is too small to call a
// blind spot; 2 of 3 is noise, 20 of 100 is a problem.
const minBlindSpot = 5

// blindSpotCoverage is the measurable share at which a slice stops being
// representative.
const blindSpotCoverage = 0.5

// Coverage builds the blind-spot panel for one vantage.
func (s *Store) Coverage(ctx context.Context, vantageID string, now time.Time) (Coverage, error) {
	out := Coverage{}

	totals, err := s.coverageTotals(ctx, vantageID)
	if err != nil {
		return out, err
	}
	out.Totals = totals

	if out.Classes, err = s.coverageByClass(ctx, vantageID); err != nil {
		return out, err
	}
	if out.Sectors, err = s.coverageBySector(ctx, vantageID); err != nil {
		return out, err
	}

	countries, err := s.coverageByCountry(ctx, vantageID)
	if err != nil {
		return out, err
	}
	asns, err := s.coverageByASN(ctx, vantageID)
	if err != nil {
		return out, err
	}

	for _, bucket := range append(countries, asns...) {
		if bucket.Total < minBlindSpot {
			continue
		}
		if share(bucket.Measurable, bucket.Total) >= blindSpotCoverage {
			continue
		}
		out.BlindSpots = append(out.BlindSpots, bucket)
	}
	sortBuckets(out.BlindSpots)

	if out.Stale, err = s.staleTargets(ctx, vantageID, now); err != nil {
		return out, err
	}

	if out.Concentration, err = s.coverageConcentration(ctx, vantageID, out.Totals.Total); err != nil {
		return out, err
	}
	for _, sector := range out.Sectors {
		if sector.Total > 0 && share(sector.Measurable, sector.Total) < blindSpotCoverage {
			out.Concentration.ThinSectors++
		}
	}
	return out, nil
}

// coverageConcentration computes the network spread of the fleet. Ties go to
// the lower ASN so a run is stable. The shares are over enabled targets, the
// same denominator the overview line uses, so the two numbers agree.
func (s *Store) coverageConcentration(ctx context.Context, vantageID string, total int) (CoverageConcentration, error) {
	var out CoverageConcentration

	if err := s.reader.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT CASE WHEN asn > 0 THEN asn END),
		       COUNT(DISTINCT CASE WHEN country <> '' THEN country END)
		FROM target WHERE enabled = 1`).Scan(&out.ASNCount, &out.CountryCount); err != nil {
		return out, fmt.Errorf("store: coverage concentration: %w", err)
	}

	// The top ASN and the share of the fleet behind the ten largest.
	rows, err := s.reader.QueryContext(ctx, `
		SELECT asn, COALESCE(as_name, ''), COUNT(*)
		FROM target
		WHERE enabled = 1 AND asn > 0
		GROUP BY asn
		ORDER BY COUNT(*) DESC, asn
		LIMIT 10`)
	if err != nil {
		return out, fmt.Errorf("store: coverage top ASNs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	top10, topCount := 0, 0
	for rank := 0; rows.Next(); rank++ {
		var (
			asn   int
			name  string
			count int
		)
		if err := rows.Scan(&asn, &name, &count); err != nil {
			return out, fmt.Errorf("store: scan top ASN: %w", err)
		}
		if rank == 0 {
			out.TopASN, out.TopASNName, topCount = asn, name, count
		}
		top10 += count
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: coverage top ASNs: %w", err)
	}
	if total > 0 {
		out.TopASNShare = float64(topCount) / float64(total)
		out.Top10Share = float64(top10) / float64(total)
	}

	// A country with one country anchor has no redundancy: that target is the
	// country's only measurement.
	if err := s.reader.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM (
			SELECT country FROM target
			WHERE enabled = 1 AND class = 'country' AND country <> ''
			GROUP BY country HAVING COUNT(*) = 1
		)`).Scan(&out.SingleAnchor); err != nil {
		return out, fmt.Errorf("store: coverage single-anchor countries: %w", err)
	}

	return out, nil
}

func share(part, total int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(part) / float64(total)
}

func sortBuckets(buckets []CoverageBucket) {
	// Biggest slice first, then kind and value, so the output is deterministic.
	for i := 1; i < len(buckets); i++ {
		for j := i; j > 0; j-- {
			a, b := buckets[j-1], buckets[j]
			if a.Total > b.Total ||
				(a.Total == b.Total && (a.Kind < b.Kind ||
					(a.Kind == b.Kind && a.Value < b.Value))) {
				break
			}
			buckets[j-1], buckets[j] = b, a
		}
	}
}

const coverageSelect = `
	SELECT COUNT(*),
	       COALESCE(SUM(CASE WHEN ts.state IN ('up','up_reachability','degraded','down')
	                         THEN 1 ELSE 0 END), 0),
	       COALESCE(SUM(CASE WHEN t.measurability = 'unmeasurable' THEN 1 ELSE 0 END), 0)
	FROM target t
	LEFT JOIN target_state ts ON ts.target_id = t.id AND ts.vantage_id = ?
	WHERE t.enabled = 1`

func (s *Store) coverageTotals(ctx context.Context, vantageID string) (CoverageTotals, error) {
	var t CoverageTotals
	err := s.reader.QueryRowContext(ctx, coverageSelect, vantageID).
		Scan(&t.Total, &t.Measurable, &t.Unmeasurable)
	if err != nil {
		return t, fmt.Errorf("store: coverage totals: %w", err)
	}
	return t, nil
}

// coverageGrouped is shared by the four dimensions. Each caller passes a literal
// query; no identifier ever comes from input.
func (s *Store) coverageGrouped(ctx context.Context, kind, query, vantageID string) ([]CoverageBucket, error) {
	rows, err := s.reader.QueryContext(ctx, query, vantageID)
	if err != nil {
		return nil, fmt.Errorf("store: coverage by %s: %w", kind, err)
	}
	defer func() { _ = rows.Close() }()

	var out []CoverageBucket
	for rows.Next() {
		var b CoverageBucket
		b.Kind = kind
		if err := rows.Scan(&b.Value, &b.Total, &b.Measurable, &b.Unmeasurable); err != nil {
			return nil, fmt.Errorf("store: scan coverage %s: %w", kind, err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: coverage by %s: %w", kind, err)
	}
	return out, nil
}

func (s *Store) coverageByClass(ctx context.Context, vantageID string) ([]CoverageBucket, error) {
	return s.coverageGrouped(ctx, "class", `
		SELECT t.class, COUNT(*),
		       COALESCE(SUM(CASE WHEN ts.state IN ('up','up_reachability','degraded','down')
		                         THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN t.measurability = 'unmeasurable' THEN 1 ELSE 0 END), 0)
		FROM target t
		LEFT JOIN target_state ts ON ts.target_id = t.id AND ts.vantage_id = ?
		WHERE t.enabled = 1
		GROUP BY t.class
		ORDER BY t.class`, vantageID)
}

func (s *Store) coverageBySector(ctx context.Context, vantageID string) ([]CoverageBucket, error) {
	return s.coverageGrouped(ctx, "sector", `
		SELECT COALESCE(t.sector, ''), COUNT(*),
		       COALESCE(SUM(CASE WHEN ts.state IN ('up','up_reachability','degraded','down')
		                         THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN t.measurability = 'unmeasurable' THEN 1 ELSE 0 END), 0)
		FROM target t
		LEFT JOIN target_state ts ON ts.target_id = t.id AND ts.vantage_id = ?
		WHERE t.enabled = 1
		GROUP BY COALESCE(t.sector, '')
		ORDER BY COALESCE(t.sector, '')`, vantageID)
}

func (s *Store) coverageByCountry(ctx context.Context, vantageID string) ([]CoverageBucket, error) {
	return s.coverageGrouped(ctx, "country", `
		SELECT COALESCE(t.country, ''), COUNT(*),
		       COALESCE(SUM(CASE WHEN ts.state IN ('up','up_reachability','degraded','down')
		                         THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN t.measurability = 'unmeasurable' THEN 1 ELSE 0 END), 0)
		FROM target t
		LEFT JOIN target_state ts ON ts.target_id = t.id AND ts.vantage_id = ?
		WHERE t.enabled = 1 AND t.country IS NOT NULL AND t.country <> ''
		GROUP BY t.country
		ORDER BY t.country`, vantageID)
}

func (s *Store) coverageByASN(ctx context.Context, vantageID string) ([]CoverageBucket, error) {
	return s.coverageGrouped(ctx, "asn", `
		SELECT CAST(t.asn AS TEXT), COUNT(*),
		       COALESCE(SUM(CASE WHEN ts.state IN ('up','up_reachability','degraded','down')
		                         THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN t.measurability = 'unmeasurable' THEN 1 ELSE 0 END), 0)
		FROM target t
		LEFT JOIN target_state ts ON ts.target_id = t.id AND ts.vantage_id = ?
		WHERE t.enabled = 1 AND t.asn IS NOT NULL AND t.asn > 0
		GROUP BY t.asn
		ORDER BY t.asn`, vantageID)
}

// staleTargets lists targets the scheduler has not reached within three of their
// own intervals (or 15 minutes, whichever is longer). A never-probed target is
// stale by definition: the board shows it as UNKNOWN for a reason.
func (s *Store) staleTargets(ctx context.Context, vantageID string, now time.Time) ([]StaleTarget, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT t.id, t.host, t.interval_s, COALESCE(ts.last_probe_at, 0),
		       COALESCE(ts.state, 'unknown')
		FROM target t
		LEFT JOIN target_state ts ON ts.target_id = t.id AND ts.vantage_id = ?
		WHERE t.enabled = 1
		ORDER BY COALESCE(ts.last_probe_at, 0), t.id`, vantageID)
	if err != nil {
		return nil, fmt.Errorf("store: stale targets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []StaleTarget
	for rows.Next() {
		var (
			t         StaleTarget
			lastProbe int64
		)
		if err := rows.Scan(&t.ID, &t.Host, &t.IntervalS, &lastProbe, &t.State); err != nil {
			return nil, fmt.Errorf("store: scan stale target: %w", err)
		}
		if lastProbe > 0 {
			t.LastProbe = time.UnixMilli(lastProbe).UTC()
		}

		grace := 3 * time.Duration(t.IntervalS) * time.Second
		if grace < 15*time.Minute {
			grace = 15 * time.Minute
		}
		if lastProbe != 0 && now.Sub(t.LastProbe) <= grace {
			continue
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: stale targets: %w", err)
	}
	return out, nil
}
