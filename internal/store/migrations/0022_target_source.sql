-- Provenance for seeded and configured targets (THREAT_MODEL.md T35).
--
-- A target list is untrusted input: a poisoned seed or config could add hosts
-- the operator did not intend. Every row records where the target came from and
-- a SHA-256 of the payload that produced it, so a list can be traced and rolled
-- back. The composite key keeps one row per (target, source) and the upsert
-- updates the hash on a later load.
CREATE TABLE target_source (
    target_id  TEXT NOT NULL REFERENCES target(id) ON DELETE CASCADE,
    source     TEXT NOT NULL,                 -- 'seed:builtin', 'seed:root-zone', 'config:/path'
    fetched_at INTEGER,
    sha256     TEXT,                          -- of the payload, for provenance
    PRIMARY KEY (target_id, source)
) WITHOUT ROWID;
