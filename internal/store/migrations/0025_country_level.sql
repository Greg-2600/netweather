-- A bounded, server-side per-country outage level from IODA's summary API.
-- One row per country, refreshed by a poll rather than by a page view: the
-- world page never calls IODA itself. IODA's own overall score, event count,
-- and rank are kept beside the normalised level so the UI can name its source
-- instead of presenting an invented probability.
CREATE TABLE IF NOT EXISTS country_level (
    country     TEXT PRIMARY KEY,
    level       REAL NOT NULL,
    score       REAL NOT NULL,
    event_count INTEGER NOT NULL,
    rank        INTEGER NOT NULL,
    observed_at INTEGER NOT NULL,
    source      TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS country_level_observed ON country_level(observed_at);
