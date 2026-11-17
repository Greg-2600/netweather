CREATE TABLE incident (
    id            TEXT PRIMARY KEY,
    opened_at     INTEGER NOT NULL,
    closed_at     INTEGER,
    verdict       TEXT NOT NULL,
    scope         TEXT NOT NULL,
    scope_value   TEXT,
    headline      TEXT NOT NULL,
    confidence    TEXT NOT NULL,
    closed_reason TEXT,
    annotation    TEXT,
    annotated_at  INTEGER
);

CREATE INDEX incident_open ON incident(closed_at, opened_at DESC);

CREATE TABLE incident_target (
    incident_id TEXT NOT NULL REFERENCES incident(id) ON DELETE CASCADE,
    target_id   TEXT NOT NULL REFERENCES target(id),
    first_seen  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL,
    PRIMARY KEY (incident_id, target_id)
) WITHOUT ROWID;

CREATE TABLE attribution (
    incident_id TEXT NOT NULL REFERENCES incident(id) ON DELETE CASCADE,
    rank        INTEGER NOT NULL,
    kind        TEXT NOT NULL,
    value       TEXT NOT NULL,
    failing     INTEGER NOT NULL,
    total       INTEGER NOT NULL,
    share       REAL NOT NULL,
    score       REAL NOT NULL,
    PRIMARY KEY (incident_id, rank)
) WITHOUT ROWID;
