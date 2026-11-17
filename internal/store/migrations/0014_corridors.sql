-- Corridors (DESIGN.md §9): an operator-defined pair of in-region endpoints,
-- sampled with TCP every five minutes, plus the events a step change opens.
CREATE TABLE corridor (
    id          TEXT PRIMARY KEY,
    from_label  TEXT NOT NULL,
    to_label    TEXT NOT NULL,
    enabled     INTEGER NOT NULL DEFAULT 1,
    updated_at  INTEGER NOT NULL
);

CREATE TABLE corridor_sample (
    id           INTEGER PRIMARY KEY,
    corridor_id  TEXT NOT NULL REFERENCES corridor(id),
    ts           INTEGER NOT NULL,
    rtt_ms       REAL,
    loss         REAL NOT NULL,
    ok           INTEGER NOT NULL
);

CREATE INDEX corridor_sample_recent ON corridor_sample(corridor_id, ts DESC);

CREATE TABLE corridor_event (
    id           TEXT PRIMARY KEY,
    corridor_id  TEXT NOT NULL REFERENCES corridor(id),
    kind         TEXT NOT NULL,               -- 'rtt_step' or 'loss_step'
    started_at   INTEGER NOT NULL,
    ended_at     INTEGER,
    detail       TEXT NOT NULL DEFAULT ''
);

CREATE INDEX corridor_event_recent ON corridor_event(corridor_id, started_at DESC);
