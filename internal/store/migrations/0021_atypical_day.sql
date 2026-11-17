-- Days excluded from baseline samples (DESIGN.md §7.2).
--
-- An atypical day (a calendar holiday, or a day whose first evaluations left
-- the band in both directions) must not become part of the hour-of-week
-- baseline: otherwise the band learns the anomaly it exists to catch. A row
-- with empty scope applies to every scope; a row with scope='country' and a
-- value applies only to that country's baseline, which is how a calendar
-- entry's optional region tags narrow it.
CREATE TABLE atypical_day (
    day         TEXT NOT NULL,              -- YYYY-MM-DD, UTC
    scope       TEXT NOT NULL DEFAULT '',   -- '' = all scopes
    scope_value TEXT NOT NULL DEFAULT '',
    marked_at   INTEGER NOT NULL,
    source      TEXT NOT NULL,              -- 'calendar' or 'detector'
    PRIMARY KEY (day, scope, scope_value)
) WITHOUT ROWID;
