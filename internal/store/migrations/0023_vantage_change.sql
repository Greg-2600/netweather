-- Egress identity changes (DESIGN.md §7.5).
--
-- A move between networks invalidates every latency and anomaly baseline, and
-- the UI must be able to say so plainly rather than leaving a reader to wonder
-- why the board looks different. One row per detected change; the newest row
-- within 24 hours is the banner.
CREATE TABLE vantage_change (
    id         INTEGER PRIMARY KEY,
    changed_at INTEGER NOT NULL,
    previous   TEXT NOT NULL,   -- the stored egress tuple: ip,AS...,CC
    current    TEXT NOT NULL
);

CREATE INDEX vantage_change_at ON vantage_change(changed_at DESC);
