CREATE TABLE external_event (
    id          INTEGER PRIMARY KEY,
    feed        TEXT NOT NULL,
    external_id TEXT NOT NULL,
    kind        TEXT NOT NULL,
    severity    REAL,
    country     TEXT,
    asn         INTEGER,
    started_at  INTEGER NOT NULL,
    ended_at    INTEGER,
    observed_at INTEGER NOT NULL,
    payload     TEXT NOT NULL,
    UNIQUE (feed, external_id)
);

CREATE INDEX external_event_time ON external_event(started_at DESC);

CREATE TABLE feed_state (
    feed                 TEXT PRIMARY KEY,
    last_ok_at           INTEGER,
    last_err             TEXT,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    next_poll_at         INTEGER
);
