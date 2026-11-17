CREATE TABLE coverage_snapshot (
    bucket        INTEGER NOT NULL,
    scope         TEXT NOT NULL,
    scope_value   TEXT NOT NULL,
    total         INTEGER NOT NULL,
    measurable    INTEGER NOT NULL,
    blocked       INTEGER NOT NULL,
    unmeasurable  INTEGER NOT NULL,
    asn_count     INTEGER,
    country_count INTEGER,
    PRIMARY KEY (bucket, scope, scope_value)
) WITHOUT ROWID;
