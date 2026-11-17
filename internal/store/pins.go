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

// ObserveCertIssuer records an issuer for a target and reports whether it is a
// new issuer after a baseline already existed.
//
// The set is deliberately forgiving: CDN-fronted sites rotate certificate
// authorities legitimately, so a new issuer is accepted on first sight and
// flagged once rather than treated as tampering (SCHEMA.md §8). Only the
// issuer DN's hash is stored, never the leaf key.
func (s *Store) ObserveCertIssuer(ctx context.Context, targetID, issuer string, at time.Time) (bool, error) {
	targetID = strings.TrimSpace(targetID)
	issuer = strings.TrimSpace(issuer)
	if targetID == "" || issuer == "" {
		return false, fmt.Errorf("store: cert pin needs a target and an issuer")
	}

	sum := sha256.Sum256([]byte(issuer))
	set := hex.EncodeToString(sum[:])
	now := at.UnixMilli()

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: cert pin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var seen int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM cert_pin WHERE target_id = ? AND issuer_set = ?`,
		targetID, set).Scan(&seen); err != nil {
		return false, fmt.Errorf("store: cert pin lookup: %w", err)
	}

	if seen > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE cert_pin SET last_seen = ? WHERE target_id = ? AND issuer_set = ?`,
			now, targetID, set); err != nil {
			return false, fmt.Errorf("store: cert pin touch: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("store: cert pin: %w", err)
		}
		return false, nil
	}

	var baseline int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM cert_pin WHERE target_id = ?`, targetID).Scan(&baseline); err != nil {
		return false, fmt.Errorf("store: cert pin baseline: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO cert_pin (target_id, issuer_set, first_seen, last_seen)
		VALUES (?, ?, ?, ?)`, targetID, set, now, now); err != nil {
		return false, fmt.Errorf("store: cert pin insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: cert pin: %w", err)
	}

	// The first issuer a target ever shows is a baseline, not a change.
	return baseline > 0, nil
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
