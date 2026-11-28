-- Operator-declared maintenance windows. A planned window explains why a
-- component is quiet before the detector calls it an incident; completing one
-- records when the operator closed the work. These are few, operator-written
-- rows, so they stay with the incident log rather than being pruned.
CREATE TABLE IF NOT EXISTS maintenance_window (
    id           INTEGER PRIMARY KEY,
    components   TEXT NOT NULL,
    reason       TEXT NOT NULL,
    starts_at    INTEGER NOT NULL,
    ends_at      INTEGER NOT NULL,
    completed_at INTEGER
);

CREATE INDEX IF NOT EXISTS maintenance_window_starts ON maintenance_window(starts_at DESC);
