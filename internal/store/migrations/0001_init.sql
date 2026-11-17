CREATE TABLE vantage (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL CHECK (kind IN ('direct', 'socks5')),
    role       TEXT NOT NULL CHECK (role IN ('primary', 'corroboration')),
    enabled    INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL
);

CREATE TABLE target (
    id           TEXT PRIMARY KEY,
    host         TEXT NOT NULL,
    port         INTEGER NOT NULL,
    probe        TEXT NOT NULL,
    name         TEXT NOT NULL,
    class        TEXT NOT NULL CHECK (class IN ('service', 'country', 'network', 'reference')),
    sector       TEXT,
    country      TEXT,
    tier         INTEGER NOT NULL CHECK (tier BETWEEN 0 AND 3),
    interval_s   INTEGER NOT NULL,
    family       TEXT NOT NULL DEFAULT 'auto' CHECK (family IN ('auto', '4', '6')),
    asn          INTEGER,
    prefix       TEXT,
    cdn          TEXT,
    measurability TEXT NOT NULL DEFAULT 'ok'
                 CHECK (measurability IN ('ok', 'mostly_blocked', 'unmeasurable')),
    last_validated_at INTEGER,
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);

CREATE UNIQUE INDEX target_natural_key ON target(host, port, probe) WHERE enabled = 1;
CREATE INDEX target_scope ON target(class, sector, country, asn);

CREATE TABLE probe (
    id          INTEGER PRIMARY KEY,
    target_id   TEXT NOT NULL REFERENCES target(id),
    vantage_id  TEXT NOT NULL REFERENCES vantage(id),
    ts          INTEGER NOT NULL,
    bucket_30s  INTEGER NOT NULL,
    state       TEXT NOT NULL,
    stage       TEXT NOT NULL,
    error_class TEXT,
    http_status INTEGER,
    latency_ms  INTEGER,
    family      TEXT,
    cdn         TEXT,
    asn         INTEGER,
    confidence  TEXT NOT NULL,
    detail      TEXT CHECK (length(detail) <= 200)
);

CREATE INDEX probe_target_ts ON probe(target_id, ts DESC);
CREATE INDEX probe_vantage_ts ON probe(vantage_id, ts);
CREATE INDEX probe_ts ON probe(ts);

CREATE TABLE target_state (
    target_id     TEXT NOT NULL REFERENCES target(id),
    vantage_id    TEXT NOT NULL REFERENCES vantage(id),
    state         TEXT NOT NULL,
    since         INTEGER NOT NULL,
    prev_state    TEXT,
    fail_streak   INTEGER NOT NULL DEFAULT 0,
    ok_streak     INTEGER NOT NULL DEFAULT 0,
    last_probe_at INTEGER NOT NULL,
    last_error    TEXT,
    attributed_to TEXT,
    PRIMARY KEY (target_id, vantage_id)
) WITHOUT ROWID;

CREATE TABLE state_change (
    id          INTEGER PRIMARY KEY,
    target_id   TEXT NOT NULL REFERENCES target(id),
    vantage_id  TEXT NOT NULL REFERENCES vantage(id),
    ts          INTEGER NOT NULL,
    from_state  TEXT NOT NULL,
    to_state    TEXT NOT NULL,
    reason      TEXT NOT NULL,
    incident_id TEXT
);

CREATE INDEX state_change_target_ts ON state_change(target_id, ts DESC);

CREATE TABLE rollup_5m (
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
