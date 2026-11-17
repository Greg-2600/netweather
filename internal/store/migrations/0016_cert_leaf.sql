-- The leaf certificate's fingerprint, learned per target. A change is
-- informational only: leaves rotate on a schedule (CLASSIFIER.md §3.5).
CREATE TABLE cert_leaf (
    target_id   TEXT PRIMARY KEY REFERENCES target(id),
    fingerprint TEXT NOT NULL,              -- SHA-256 hex of the leaf DER
    first_seen  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL
);
