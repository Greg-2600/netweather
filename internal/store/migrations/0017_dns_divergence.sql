-- Per-resolver DNS baselines and the divergence verdicts they produce
-- (CLASSIFIER.md §3.2). Each resolver is compared only against its own
-- history, never against another resolver.
CREATE TABLE dns_observation (
    id         INTEGER PRIMARY KEY,
    target_id  TEXT NOT NULL REFERENCES target(id),
    resolver   TEXT NOT NULL,
    kind       TEXT NOT NULL,               -- 'address' for now
    value      TEXT NOT NULL,
    first_seen INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL
);

CREATE UNIQUE INDEX dns_observation_key ON dns_observation(target_id, resolver, kind, value);
CREATE INDEX dns_observation_recent ON dns_observation(target_id, resolver, last_seen DESC);

CREATE TABLE dns_divergence (
    id         INTEGER PRIMARY KEY,
    target_id  TEXT NOT NULL REFERENCES target(id),
    ts         INTEGER NOT NULL,
    verdict    TEXT NOT NULL,               -- 'DNS_ANOMALY' or 'TAMPER'
    confidence TEXT NOT NULL,
    detail     TEXT NOT NULL DEFAULT '',
    resolvers  TEXT NOT NULL DEFAULT ''     -- comma-separated, for the drawer
);

CREATE INDEX dns_divergence_recent ON dns_divergence(target_id, ts DESC);
