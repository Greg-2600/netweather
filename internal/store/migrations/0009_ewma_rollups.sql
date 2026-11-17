-- The rollup latency columns were named p50_ms/p95_ms but the write path stored
-- a single sample, not a percentile. SCHEMA.md §6 is explicit that these are
-- EWMAs; the rename makes the schema tell the truth.
ALTER TABLE rollup_5m RENAME COLUMN p50_ms TO ewma50_ms;
ALTER TABLE rollup_5m RENAME COLUMN p95_ms TO ewma95_ms;

ALTER TABLE rollup_1h RENAME COLUMN p50_ms TO ewma50_ms;
ALTER TABLE rollup_1h RENAME COLUMN p95_ms TO ewma95_ms;

ALTER TABLE rollup_1d RENAME COLUMN p50_ms TO ewma50_ms;
ALTER TABLE rollup_1d RENAME COLUMN p95_ms TO ewma95_ms;
