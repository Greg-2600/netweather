CREATE TABLE self_check (
    id     INTEGER PRIMARY KEY,
    ts     INTEGER NOT NULL,
    -- "check" is a reserved word in SQLite, so it is quoted everywhere it is used.
    "check" TEXT NOT NULL,
    ok     INTEGER NOT NULL,
    value  TEXT,
    detail TEXT
);

CREATE INDEX self_check_name_ts ON self_check("check", ts DESC);
