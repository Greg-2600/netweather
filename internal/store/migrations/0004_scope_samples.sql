CREATE TABLE scope_sample (
    scope        TEXT NOT NULL,
    scope_value  TEXT NOT NULL,
    bucket       INTEGER NOT NULL,
    health       REAL NOT NULL,
    coverage     REAL NOT NULL,
    measurable   INTEGER NOT NULL,
    judgeable    INTEGER NOT NULL,
    PRIMARY KEY (scope, scope_value, bucket)
) WITHOUT ROWID;
