CREATE TABLE rollup_1h (
    target_id  TEXT NOT NULL,
    vantage_id TEXT NOT NULL,
    bucket     INTEGER NOT NULL,
    n          INTEGER NOT NULL,
    up         INTEGER NOT NULL,
    degraded   INTEGER NOT NULL,
    down       INTEGER NOT NULL,
    blocked    INTEGER NOT NULL,
    unknown    INTEGER NOT NULL,
    p50_ms     INTEGER,
    p95_ms     INTEGER,
    PRIMARY KEY (target_id, vantage_id, bucket)
) WITHOUT ROWID;

CREATE TABLE rollup_1d (
    target_id  TEXT NOT NULL,
    vantage_id TEXT NOT NULL,
    bucket     INTEGER NOT NULL,
    n          INTEGER NOT NULL,
    up         INTEGER NOT NULL,
    degraded   INTEGER NOT NULL,
    down       INTEGER NOT NULL,
    blocked    INTEGER NOT NULL,
    unknown    INTEGER NOT NULL,
    p50_ms     INTEGER,
    p95_ms     INTEGER,
    PRIMARY KEY (target_id, vantage_id, bucket)
) WITHOUT ROWID;
