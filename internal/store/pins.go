package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// CertObservation is one TLS certificate observation. The issuer drives the
// pin; the chain shape is recorded alongside it and reported as informational
// (CLASSIFIER.md §3.5).
type CertObservation struct {
	TargetID    string
	Issuer      string
	ChainLength int
	// IntermediateSPKI is the SHA-256 hex of the signing intermediate's DER
	// subjectPublicKeyInfo. Empty when the server sent only a leaf.
	IntermediateSPKI string
}

// ObserveCert records an issuer for a target and reports whether it is a new
// issuer after a baseline already existed, plus any informational chain change.
//
// The set is deliberately forgiving: CDN-fronted sites rotate certificate
// authorities legitimately, so a new issuer is accepted on first sight and
// flagged once rather than treated as tampering (SCHEMA.md §8). Only the
// issuer DN's hash and the chain shape are stored, never the leaf key.
func (s *Store) ObserveCert(ctx context.Context, obs CertObservation, at time.Time) (changed bool, advisory string, err error) {
	targetID := strings.TrimSpace(obs.TargetID)
	issuer := strings.TrimSpace(obs.Issuer)
	if targetID == "" || issuer == "" {
		return false, "", fmt.Errorf("store: cert pin needs a target and an issuer")
	}

	sum := sha256.Sum256([]byte(issuer))
	set := hex.EncodeToString(sum[:])
	now := at.UnixMilli()

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return false, "", fmt.Errorf("store: cert pin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		seen     int
		chainLen int
		spki     sql.NullString
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(MAX(chain_len), 0), MAX(intermediate_spki)
		FROM cert_pin WHERE target_id = ? AND issuer_set = ?`,
		targetID, set).Scan(&seen, &chainLen, &spki); err != nil {
		return false, "", fmt.Errorf("store: cert pin lookup: %w", err)
	}

	if seen > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE cert_pin SET last_seen = ? WHERE target_id = ? AND issuer_set = ?`,
			now, targetID, set); err != nil {
			return false, "", fmt.Errorf("store: cert pin touch: %w", err)
		}

		// A known issuer with a different chain shape is informational: an
		// intermediate rotation is normal, and the chain is not a verdict.
		if obs.ChainLength > 0 && chainLen > 0 && obs.ChainLength != chainLen {
			advisory = fmt.Sprintf("chain length changed from %d to %d for a known issuer", chainLen, obs.ChainLength)
		} else if obs.IntermediateSPKI != "" && spki.Valid && spki.String != obs.IntermediateSPKI {
			advisory = "intermediate certificate changed for a known issuer"
		}
		if advisory != "" {
			if _, err := tx.ExecContext(ctx, `
				UPDATE cert_pin SET chain_len = ?, intermediate_spki = ?
				WHERE target_id = ? AND issuer_set = ?`,
				obs.ChainLength, nullString(obs.IntermediateSPKI), targetID, set); err != nil {
				return false, "", fmt.Errorf("store: cert pin chain: %w", err)
			}
		}

		if err := tx.Commit(); err != nil {
			return false, "", fmt.Errorf("store: cert pin: %w", err)
		}
		return false, advisory, nil
	}

	var baseline int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM cert_pin WHERE target_id = ?`, targetID).Scan(&baseline); err != nil {
		return false, "", fmt.Errorf("store: cert pin baseline: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO cert_pin (target_id, issuer_set, first_seen, last_seen, chain_len, intermediate_spki)
		VALUES (?, ?, ?, ?, ?, ?)`,
		targetID, set, now, now, obs.ChainLength, nullString(obs.IntermediateSPKI)); err != nil {
		return false, "", fmt.Errorf("store: cert pin insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, "", fmt.Errorf("store: cert pin: %w", err)
	}

	// The first issuer a target ever shows is a baseline, not a change.
	return baseline > 0, "", nil
}

// ObserveCertIssuer is the issuer-only form, kept for callers (and tests) that
// do not have a chain. It is the same rule with no chain advisory.
func (s *Store) ObserveCertIssuer(ctx context.Context, targetID, issuer string, at time.Time) (bool, error) {
	changed, _, err := s.ObserveCert(ctx, CertObservation{TargetID: targetID, Issuer: issuer}, at)
	return changed, err
}

// CertPinKnown reports whether a target has any learned issuer. It exists for
// the drawer, which must say "not learned yet" rather than "matches".
func (s *Store) CertPinKnown(ctx context.Context, targetID string) (bool, error) {
	var n int
	err := s.reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM cert_pin WHERE target_id = ?`, targetID).Scan(&n)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("store: cert pin known: %w", err)
	}
	return n > 0, nil
}

// nullString stores an empty value as SQL NULL, which is what "not observed"
// means; an empty TEXT would be a claim that the chain had no intermediate.
func nullString(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}
