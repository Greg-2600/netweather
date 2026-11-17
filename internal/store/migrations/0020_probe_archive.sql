-- Raw probes around a confirmed incident, kept for a year (SCHEMA.md §5).
--
-- The nightly prune deletes raw probes after 72 hours. A drawer opened a month
-- later can still show the samples around the event because they were copied
-- here when the incident opened. The primary key makes the copy idempotent, so
-- a retried open cannot duplicate rows.
CREATE TABLE probe_archive (
    probe_id    INTEGER NOT NULL,
    target_id   TEXT NOT NULL,
    vantage_id  TEXT NOT NULL,
    ts          INTEGER NOT NULL,
    state       TEXT NOT NULL,
    stage       TEXT NOT NULL,
    error_class TEXT,
    http_status INTEGER,
    latency_ms  INTEGER,
    family      TEXT,
    cdn         TEXT,
    asn         INTEGER,
    confidence  TEXT NOT NULL,
    detail      TEXT,
    pinned_at   INTEGER NOT NULL,
    incident_id TEXT,
    PRIMARY KEY (target_id, vantage_id, ts)
) WITHOUT ROWID;

CREATE INDEX probe_archive_incident ON probe_archive(incident_id, ts);
