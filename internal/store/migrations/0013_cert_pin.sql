-- Learned certificate issuer sets, one row per target and issuer hash.
--
-- The point is a stable baseline, not a hard pin: CDN-fronted sites rotate CAs
-- legitimately, so a new issuer is accepted on first sight and flagged once
-- (SCHEMA.md §8). Only the issuer DN hash is stored; the leaf SPKI never is.
CREATE TABLE cert_pin (
    target_id  TEXT NOT NULL REFERENCES target(id),
    issuer_set TEXT NOT NULL,              -- SHA-256 hex of the issuer DN
    first_seen INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL,
    PRIMARY KEY (target_id, issuer_set)
) WITHOUT ROWID;
