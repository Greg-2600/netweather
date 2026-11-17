CREATE TABLE anomaly (
    id          INTEGER PRIMARY KEY,
    ts          INTEGER NOT NULL,
    scope       TEXT NOT NULL,
    scope_value TEXT NOT NULL,
    kind        TEXT NOT NULL,
    observed    REAL NOT NULL,
    baseline    REAL NOT NULL,
    mad         REAL NOT NULL,
    sigma       REAL NOT NULL,
    buckets     INTEGER NOT NULL,
    incident_id TEXT
);

CREATE INDEX anomaly_ts ON anomaly(ts DESC);
CREATE INDEX anomaly_scope ON anomaly(scope, scope_value, ts DESC);
