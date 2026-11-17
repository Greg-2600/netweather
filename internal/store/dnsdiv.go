package store

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/Greg-2600/netweather/internal/dnsdiv"
)

// RecordDNSAnswer folds one resolver's observation into its baseline. Only
// successful answers are stored: the baseline answers "what did this resolver
// say last time", and a failure says nothing about that (CLASSIFIER.md §3.2).
// Addresses are the primary key; the origin ASNs and CNAME suffixes recorded
// alongside them are the hosting labels the divergence rule needs.
func (s *Store) RecordDNSAnswer(ctx context.Context, targetID string, ans dnsdiv.Answer) error {
	if ans.Resolver == "" {
		return fmt.Errorf("store: dns answer needs a resolver")
	}
	if len(ans.Addresses) == 0 && len(ans.ASNs) == 0 && len(ans.CNAMESuffixes) == 0 {
		return nil
	}

	now := ans.At.UnixMilli()
	if ans.At.IsZero() {
		now = time.Now().UnixMilli()
	}

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: dns answer: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	record := func(kind, value string) error {
		if value == "" {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO dns_observation (target_id, resolver, kind, value, first_seen, last_seen)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (target_id, resolver, kind, value) DO UPDATE SET
				last_seen = excluded.last_seen`,
			targetID, ans.Resolver, kind, value, now, now); err != nil {
			return fmt.Errorf("store: dns answer %q: %w", targetID, err)
		}
		return nil
	}

	for _, addr := range ans.Addresses {
		if err := record("address", addr); err != nil {
			return err
		}
	}
	for _, asn := range ans.ASNs {
		if asn <= 0 {
			continue
		}
		if err := record("asn", strconv.Itoa(asn)); err != nil {
			return err
		}
	}
	for _, suffix := range ans.CNAMESuffixes {
		if err := record("cname_suffix", suffix); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: dns answer: %w", err)
	}
	return nil
}

// DNSBaselines returns each resolver's learned address, ASN, and hosting-suffix
// sets within the window. A resolver with no rows is simply absent, which the
// engine treats as "learning", never as a deviation.
func (s *Store) DNSBaselines(ctx context.Context, targetID string, since time.Time) (map[string]dnsdiv.Baseline, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT resolver, kind, value, last_seen
		FROM dns_observation
		WHERE target_id = ? AND last_seen >= ?
		ORDER BY resolver, kind, value`, targetID, since.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("store: dns baselines: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]dnsdiv.Baseline)
	for rows.Next() {
		var (
			resolver string
			kind     string
			value    string
			lastSeen int64
		)
		if err := rows.Scan(&resolver, &kind, &value, &lastSeen); err != nil {
			return nil, fmt.Errorf("store: scan dns baseline: %w", err)
		}
		base := out[resolver]
		switch kind {
		case "asn":
			asn, err := strconv.Atoi(value)
			if err != nil || asn <= 0 {
				continue
			}
			if base.ASNs == nil {
				base.ASNs = make(map[int]struct{})
			}
			base.ASNs[asn] = struct{}{}
		case "cname_suffix":
			if base.CNAMESuffixes == nil {
				base.CNAMESuffixes = make(map[string]struct{})
			}
			base.CNAMESuffixes[value] = struct{}{}
		default:
			if base.Addresses == nil {
				base.Addresses = make(map[string]struct{})
			}
			base.Addresses[value] = struct{}{}
			base.Samples++
		}
		at := time.UnixMilli(lastSeen).UTC()
		if at.After(base.LastSeen) {
			base.LastSeen = at
		}
		out[resolver] = base
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: dns baselines: %w", err)
	}
	return out, nil
}

// RecordDNSDivergence stores one verdict. It is evidence for the drawer and the
// API, never a target state: DNS_ANOMALY is advisory and TAMPER is a verdict
// that needs the second signal the ladder already applied.
func (s *Store) RecordDNSDivergence(ctx context.Context, targetID string, v dnsdiv.Verdict, at time.Time) error {
	if v.Kind == "" {
		return nil
	}
	if _, err := s.writer.ExecContext(ctx, `
		INSERT INTO dns_divergence (target_id, ts, verdict, confidence, detail, resolvers)
		VALUES (?, ?, ?, ?, ?, ?)`,
		targetID, at.UnixMilli(), v.Kind, v.Confidence, v.Detail, v.Resolvers()); err != nil {
		return fmt.Errorf("store: dns divergence %q: %w", targetID, err)
	}
	return nil
}

// PruneDNSObservations drops baseline rows older than the cutoff. The window is
// the seven days CLASSIFIER.md §3.2 names; an empty window would call every old
// answer current.
func (s *Store) PruneDNSObservations(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.writer.ExecContext(ctx,
		`DELETE FROM dns_observation WHERE last_seen < ?`, before.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("store: prune dns observations: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune dns observations: %w", err)
	}
	return n, nil
}

// DNSDivergenceRecord is one stored verdict, with the target's host for a
// readable board strip.
type DNSDivergenceRecord struct {
	TargetID   string
	Host       string
	TS         time.Time
	Verdict    string
	Confidence string
	Detail     string
	Resolvers  string
}

// RecentDNSDivergences returns the newest verdicts across every target. It is
// what the board's collapsed DNS strip reads; the per-target copy is on the
// divergent target's own row.
func (s *Store) RecentDNSDivergences(ctx context.Context, limit int) ([]DNSDivergenceRecord, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.reader.QueryContext(ctx, `
		SELECT d.target_id, t.host, d.ts, d.verdict, d.confidence, d.detail, d.resolvers
		FROM dns_divergence d
		JOIN target t ON t.id = d.target_id
		ORDER BY d.ts DESC, d.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: recent dns divergences: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []DNSDivergenceRecord
	for rows.Next() {
		var (
			record DNSDivergenceRecord
			ts     int64
		)
		if err := rows.Scan(&record.TargetID, &record.Host, &ts, &record.Verdict,
			&record.Confidence, &record.Detail, &record.Resolvers); err != nil {
			return nil, fmt.Errorf("store: scan dns divergence: %w", err)
		}
		record.TS = time.UnixMilli(ts).UTC()
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: recent dns divergences: %w", err)
	}
	return out, nil
}
