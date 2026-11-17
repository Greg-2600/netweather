# netweather — Storage schema

> SQLite, WAL, `modernc.org/sqlite` (pure Go). One writer, many readers, append-only raw probes, incremental rollups. Sized for 750 targets at steady state in ~340 MB.

---

## 1. Principles

1. **Config is authoritative; the database is a projection.** Targets live in `config/` files. The `target` table is a materialized copy so SQL can group and join without re-reading YAML. A reload reconciles it; it never deletes.
2. **Raw probes are the only append-only truth.** Every other table is derived and can be rebuilt from raw probes plus archives. This is what makes a rollup bug recoverable instead of permanent.
3. **One writer.** A single dedicated connection owns every write. Readers are separate connections with `query_only=1`, so a slow dashboard query cannot block an insert.
4. **UTC everywhere, milliseconds, integer.** No `TEXT` timestamps, no local time in storage. Local time exists only at render.
5. **Rollups are incremented, never recomputed.** Scanning raw probes to rebuild a 5-minute bucket is the one query that would make this design slow at 750 targets.
6. **Stable identity.** A target's ID is a ULID keyed on `(host, port, probe)`. If that triple changes, it is a new target; the old row is disabled, never deleted, so history survives a config edit.
7. **Bodies are never stored.** `probe.detail` holds one human-readable line, capped at 200 bytes. Recorded responses live in `testdata/`, not in the database.

---

## 2. Pragmas and connections

```sql
-- bootstrap, once, on an empty database — before any CREATE TABLE
PRAGMA auto_vacuum = INCREMENTAL;

-- every connection
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA busy_timeout = 5000;
PRAGMA foreign_keys = ON;
PRAGMA temp_store = MEMORY;
PRAGMA cache_size = -16000;      -- 16 MB
PRAGMA mmap_size = 268435456;    -- 256 MB
PRAGMA wal_autocheckpoint = 1000;
```

`auto_vacuum` can only be set on an empty database without a full `VACUUM`. Set it in the bootstrap path, before migrations run, and assert it afterwards.

Two `database/sql` pools, two DSNs:

| Pool | DSN suffix | Max open | Purpose |
|---|---|---|---|
| writer | `_pragma=busy_timeout(5000)` | **1** | All inserts, upserts, deletes |
| reader | `_pragma=query_only(1)` | 4 | UI, API, SSE snapshot, exports |

`query_only=1` is not decoration: it turns a bug that writes from a handler into an immediate error rather than a lock contention mystery.

### 2.1 Why not `database/sql` defaults

The default pool will happily open the same file read-write from several connections, which is how SQLite deployments get `SQLITE_BUSY` under load. One writer plus N `query_only` readers is the only arrangement that stays predictable.

### 2.2 Checkpointing

WAL grows until checkpointed. `wal_autocheckpoint=1000` pages (~4 MB) is the default and is fine here; the risk is a long read transaction pinning an old snapshot during a large prune. Prune in batches of ≤5000 rows and commit between batches.

---

## 3. Entity map

```
target ──┬── probe ────────────── probe_archive (incident-pinned)
         ├── target_state         (latest per target+vantage)
         ├── state_change
         ├── rollup_5m ── rollup_1h ── rollup_1d
         ├── dns_baseline / dns_observation
         └── cert_pin

incident ── incident_target ── target
         ├── attribution
         └── anomaly (nullable link)

vantage ── probe / target_state / rollup_*

self_check, external_event, feed_state, coverage_snapshot   (standalone)
```

---

## 4. Reference tables

```sql
CREATE TABLE schema_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE vantage (
    id         TEXT PRIMARY KEY,              -- 'local', 'tor-de'
    kind       TEXT NOT NULL CHECK (kind IN ('direct', 'socks5')),
    role       TEXT NOT NULL CHECK (role IN ('primary', 'corroboration')),
    enabled    INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL
);

CREATE TABLE target (
    id           TEXT PRIMARY KEY,            -- ULID, stable across reloads
    host         TEXT NOT NULL,               -- registrable domain, or literal IP for anchors
    port         INTEGER NOT NULL,
    probe        TEXT NOT NULL,               -- 'http','tls','dns','tcp','icmp','ntp','tcpping'
    name         TEXT NOT NULL,
    class        TEXT NOT NULL CHECK (class IN ('service','country','network','reference')),
    sector       TEXT,                        -- 'financial/banking'
    country      TEXT,                        -- ISO-3166-1 alpha-2
    tier         INTEGER NOT NULL CHECK (tier BETWEEN 0 AND 3),
    interval_s   INTEGER NOT NULL,
    family       TEXT NOT NULL DEFAULT 'auto' CHECK (family IN ('auto','4','6')),

    -- learned, refreshed by enrichment jobs
    asn          INTEGER,
    prefix       TEXT,                        -- '203.0.113.0/24'
    cdn          TEXT,
    as_country   TEXT,                        -- ISO-3166-1 alpha-2 from asn.cymru.com
    as_name      TEXT,                        -- registered organisation name
    ix_names     TEXT,                        -- PeeringDB exchanges, comma-separated, capped
    measurability TEXT NOT NULL DEFAULT 'ok'
                 CHECK (measurability IN ('ok','mostly_blocked','unmeasurable')),
    last_validated_at INTEGER,

    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);

CREATE UNIQUE INDEX target_natural_key
    ON target(host, port, probe) WHERE enabled = 1;

CREATE INDEX target_scope
    ON target(class, sector, country, asn);

CREATE TABLE target_source (
    target_id  TEXT NOT NULL REFERENCES target(id) ON DELETE CASCADE,
    source     TEXT NOT NULL,                 -- 'seed:builtin', 'seed:url:https://…', 'config:overrides/chase.yaml'
    fetched_at INTEGER,
    sha256     TEXT,                          -- of the seed payload, for provenance
    PRIMARY KEY (target_id, source)
) WITHOUT ROWID;
```

`target_source` exists because a seeded target is **untrusted input** (`DESIGN.md` §4). Every seeded row carries where it came from and a hash of the payload, so a poisoned list can be traced and rolled back.

### 4.1 Reconciliation on config load

```
for each target in config:
    key = (host, port, probe)
    if row exists with key:  UPDATE name/class/sector/tier/interval; keep id, asn, cdn, as_country, as_name, measurability
    else:                    INSERT with new ULID
for each enabled row not in config:
    UPDATE enabled = 0        -- never DELETE; history must survive
```

A changed `(host, port, probe)` produces a new row by construction. That is intentional: repointing a target is not the same target.

**Migration `0010`** added `as_country` and `as_name` to `target`. They are written only by `store.UpdateEnrichment`, never by `UpsertTarget`, so a config reconciliation pass and an enrichment pass cannot erase each other's work.

**Migration `0012`** added `ix_names`, the PeeringDB exchange membership for the target's ASN, joined with `, ` and capped by the client. It follows the same ownership rule as the other enrichment columns.

---

## 5. Probes and state

```sql
CREATE TABLE probe (
    id          INTEGER PRIMARY KEY,          -- rowid; monotonic, usable as a cursor
    target_id   TEXT NOT NULL REFERENCES target(id),
    vantage_id  TEXT NOT NULL REFERENCES vantage(id),
    ts          INTEGER NOT NULL,             -- unix ms, UTC
    bucket_30s  INTEGER NOT NULL,             -- ts / 30000, computed at write
    state       TEXT NOT NULL,                -- UP..CONFIG_ERROR
    stage       TEXT NOT NULL,                -- 'dns','tcp','tls','http','icmp','ntp'
    error_class TEXT,                         -- 'tcp_refused','tls_expired','dns_nxdomain',…
    http_status INTEGER,
    latency_ms  INTEGER,
    family      TEXT,                         -- '4' | '6' — which stack was actually used
    cdn         TEXT,
    asn         INTEGER,
    confidence  TEXT NOT NULL,                -- 'semantic','reachability','low'
    detail      TEXT CHECK (length(detail) <= 200)
);

CREATE INDEX probe_target_ts   ON probe(target_id, ts DESC);
CREATE INDEX probe_vantage_ts  ON probe(vantage_id, ts);
CREATE INDEX probe_ts          ON probe(ts);
```

**Migration `0011`** added `cert_issuer` and `cert_not_after` (unix ms, NULL when the observation was not TLS). They are per-observation facts from the TLS instrument, not target configuration, so they live on `probe` rather than `target`. The `cert_pin` table below remains a separate, specified feature.

`probe_archive` is the same shape plus `pinned_at` and `incident_id`, with no `ts` index (it is always queried by target or incident):

```sql
CREATE TABLE probe_archive (
    probe_id    INTEGER NOT NULL,
    target_id   TEXT NOT NULL,
    vantage_id  TEXT NOT NULL,
    ts          INTEGER NOT NULL,
    state       TEXT NOT NULL,
    stage       TEXT NOT NULL,
    error_class TEXT,
    http_status INTEGER,
    latency_ms  INTEGER,
    family      TEXT,
    cdn         TEXT,
    asn         INTEGER,
    confidence  TEXT NOT NULL,
    detail      TEXT,
    pinned_at   INTEGER NOT NULL,
    incident_id TEXT,
    PRIMARY KEY (target_id, vantage_id, ts)
) WITHOUT ROWID;

CREATE INDEX probe_archive_incident ON probe_archive(incident_id, ts);
```

`target_state` is a cache of the latest observation per `(target, vantage)`. It is rebuilt from the last `state_change` on startup if it is ever suspect.

```sql
CREATE TABLE target_state (
    target_id     TEXT NOT NULL REFERENCES target(id),
    vantage_id    TEXT NOT NULL REFERENCES vantage(id),
    state         TEXT NOT NULL,
    since         INTEGER NOT NULL,
    prev_state    TEXT,
    fail_streak   INTEGER NOT NULL DEFAULT 0,
    ok_streak     INTEGER NOT NULL DEFAULT 0,
    last_probe_at INTEGER NOT NULL,
    last_error    TEXT,
    attributed_to TEXT,                       -- 'local', or an incident id
    PRIMARY KEY (target_id, vantage_id)
) WITHOUT ROWID;

CREATE TABLE state_change (
    id          INTEGER PRIMARY KEY,
    target_id   TEXT NOT NULL REFERENCES target(id),
    vantage_id  TEXT NOT NULL REFERENCES vantage(id),
    ts          INTEGER NOT NULL,
    from_state  TEXT NOT NULL,
    to_state    TEXT NOT NULL,
    reason      TEXT NOT NULL,                -- the plain-English line shown in the drawer
    incident_id TEXT
);

CREATE INDEX state_change_target_ts ON state_change(target_id, ts DESC);
```

State changes are written only on an actual transition, after debounce (`DESIGN.md` §7.4). A flapping target that alternates every probe produces one row per accepted transition, not one per probe — that is the point of the debounce.

---

## 6. Rollups

```sql
CREATE TABLE rollup_5m (
    target_id  TEXT NOT NULL,
    vantage_id TEXT NOT NULL,
    bucket     INTEGER NOT NULL,              -- ts / 300000, UTC
    n          INTEGER NOT NULL,              -- observations in bucket
    up         INTEGER NOT NULL,
    degraded   INTEGER NOT NULL,
    down       INTEGER NOT NULL,
    blocked    INTEGER NOT NULL,
    unknown    INTEGER NOT NULL,
    ewma50_ms  INTEGER,                 -- EWMA, not a percentile
    ewma95_ms  INTEGER,                 -- EWMA, not a percentile
    PRIMARY KEY (target_id, vantage_id, bucket)
) WITHOUT ROWID;

CREATE TABLE rollup_1h (
    target_id  TEXT NOT NULL,
    vantage_id TEXT NOT NULL,
    bucket     INTEGER NOT NULL,              -- ts / 3600000
    n          INTEGER NOT NULL,
    up         INTEGER NOT NULL,
    degraded   INTEGER NOT NULL,
    down       INTEGER NOT NULL,
    blocked    INTEGER NOT NULL,
    unknown    INTEGER NOT NULL,
    ewma50_ms  INTEGER,                 -- EWMA, not a percentile
    ewma95_ms  INTEGER,                 -- EWMA, not a percentile
    PRIMARY KEY (target_id, vantage_id, bucket)
) WITHOUT ROWID;

CREATE TABLE rollup_1d (
    target_id  TEXT NOT NULL,
    vantage_id TEXT NOT NULL,
    bucket     INTEGER NOT NULL,              -- ts / 86400000
    n          INTEGER NOT NULL,
    up         INTEGER NOT NULL,
    degraded   INTEGER NOT NULL,
    down       INTEGER NOT NULL,
    blocked    INTEGER NOT NULL,
    unknown    INTEGER NOT NULL,
    ewma50_ms  INTEGER,                 -- EWMA, not a percentile
    ewma95_ms  INTEGER,                 -- EWMA, not a percentile
    PRIMARY KEY (target_id, vantage_id, bucket)
) WITHOUT ROWID;
```

### 6.1 Increment, don't recompute

Per dispatcher tick:

```sql
INSERT INTO rollup_5m (target_id, vantage_id, bucket, n, up, degraded, down, blocked, unknown, ewma50_ms, ewma95_ms)
VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (target_id, vantage_id, bucket) DO UPDATE SET
    n        = n + 1,
    up       = up + excluded.up,
    degraded = degraded + excluded.degraded,
    down     = down + excluded.down,
    blocked  = blocked + excluded.blocked,
    unknown  = unknown + excluded.unknown,
    ewma50_ms = CASE WHEN ewma50_ms IS NULL THEN excluded.ewma50_ms
                     ELSE CAST(0.3*excluded.ewma50_ms + 0.7*ewma50_ms AS INTEGER) END,
    ewma95_ms = CASE WHEN ewma95_ms IS NULL THEN excluded.ewma95_ms
                     ELSE CAST(0.3*excluded.ewma95_ms + 0.7*ewma95_ms AS INTEGER) END;
```

Hourly is folded from 5-minute rows at bucket close; daily from hourly. Neither ever reads `probe`.

### 6.2 Latency percentiles in a rollup

A rollup cannot hold a true p50/p95 without keeping every sample. Two honest options:

- **Store a smoothed latency** (what the SQL above does). Cheap, and correct as a trend, wrong as a percentile.
- **Store a t-digest or a fixed histogram** (16 log-spaced buckets) in a `BLOB`. Correct, ~128 bytes per row.

Decision: the columns are **EWMA of the bucket's samples**, not percentiles, named `ewma50_ms`/`ewma95_ms`. Migration `0009` renamed them and the write path now updates the value with `0.3*new + 0.7*old`, so one slow probe cannot rewrite a bucket and a sustained shift is visible within a few probes. Folding averages the EWMAs weighted by `n`. Calling an EWMA a p95 is the kind of quiet lie this project is supposed to avoid. If true percentiles are needed for `DEGRADED`, add the histogram in phase 2.

---

## 7. Correlation

```sql
CREATE TABLE anomaly (
    id          INTEGER PRIMARY KEY,
    ts          INTEGER NOT NULL,
    scope       TEXT NOT NULL,                -- 'global','sector','country','asn'
    scope_value TEXT,
    kind        TEXT NOT NULL,                -- 'health_drop','coverage_drop'
    observed    REAL NOT NULL,
    baseline    REAL NOT NULL,
    mad         REAL NOT NULL,
    sigma       REAL NOT NULL,
    buckets     INTEGER NOT NULL,
    incident_id TEXT                          -- NULL for advisory events
);

CREATE INDEX anomaly_ts ON anomaly(ts DESC);
CREATE INDEX anomaly_scope ON anomaly(scope, scope_value, ts DESC);

CREATE TABLE incident (
    id            TEXT PRIMARY KEY,           -- ULID
    opened_at     INTEGER NOT NULL,
    closed_at     INTEGER,
    verdict       TEXT NOT NULL,              -- 'INCIDENT','REGIONAL','TAMPER'
    scope         TEXT NOT NULL,
    scope_value   TEXT,
    headline      TEXT NOT NULL,
    confidence    TEXT NOT NULL,              -- 'high','medium','low'
    closed_reason TEXT,
    annotation    TEXT,                       -- operator ground truth, free text
    annotated_at  INTEGER
);

CREATE INDEX incident_open ON incident(closed_at, opened_at DESC);

CREATE TABLE incident_target (
    incident_id TEXT NOT NULL REFERENCES incident(id) ON DELETE CASCADE,
    target_id   TEXT NOT NULL REFERENCES target(id),
    first_seen  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL,
    PRIMARY KEY (incident_id, target_id)
) WITHOUT ROWID;

CREATE TABLE attribution (
    incident_id TEXT NOT NULL REFERENCES incident(id) ON DELETE CASCADE,
    rank        INTEGER NOT NULL,             -- 1 = winner
    kind        TEXT NOT NULL,                -- 'cdn','asn','prefix_24','country','sector','stage'
    value       TEXT NOT NULL,
    failing     INTEGER NOT NULL,
    total       INTEGER NOT NULL,
    share       REAL NOT NULL,
    score       REAL NOT NULL,
    PRIMARY KEY (incident_id, rank)
) WITHOUT ROWID;
```

`attribution` stores **all** candidates with their rank, not just the winner. When the winner is later shown to be wrong, the second-best explanation is already on disk — and that is exactly the data needed to improve the scorer (`DESIGN.md` §7.3).

`annotation` is the ground-truth hook. One line from the operator ("AWS us-east-1, confirmed on status page") turns a stored incident into a labelled example for the backtest corpus.

`anomaly` is written by `internal/anomaly` (migration `0005`), not by the probe path. One row is a detector event: the observed health, the baseline median, the MAD and derived sigma, and how many consecutive evaluations were below the band. It is deliberately separate from `incident`: most anomalies are advisory, and an incident is a later, stronger claim. `incident_id` stays null until something links the two.

---

## 8. DNS and certificates

```sql
CREATE TABLE dns_baseline (
    resolver     TEXT NOT NULL,               -- 'local', 'doh:1.1.1.1', 'doh:9.9.9.9'
    target_id    TEXT NOT NULL REFERENCES target(id),
    answer_set   TEXT NOT NULL,               -- JSON array, capped at 32
    asn_set      TEXT NOT NULL,               -- JSON array
    cname_suffix TEXT,
    delegation   TEXT,                        -- JSON NS set from the parent
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (resolver, target_id)
) WITHOUT ROWID;

CREATE TABLE dns_observation (
    id         INTEGER PRIMARY KEY,
    ts         INTEGER NOT NULL,
    target_id  TEXT NOT NULL REFERENCES target(id),
    resolver   TEXT NOT NULL,
    rcode      INTEGER NOT NULL,
    answers    TEXT,
    signal     TEXT,                          -- NULL, 'new_asn','nxdomain_regression','delegation_change'
    strength   TEXT,                          -- 'weak','medium','strong'
    evidence   TEXT
);

CREATE INDEX dns_observation_ts ON dns_observation(ts DESC);
CREATE INDEX dns_observation_target ON dns_observation(target_id, ts DESC);

CREATE TABLE cert_pin (
    target_id  TEXT NOT NULL REFERENCES target(id),
    issuer_set TEXT NOT NULL,                 -- JSON array of issuer DN hashes — never leaf SPKI
    first_seen INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL,
    PRIMARY KEY (target_id, issuer_set)
) WITHOUT ROWID;
```

Per-resolver baselines are the fix for `CLASSIFIER.md` §3.2. A resolver's own history is the only comparison that survives GeoDNS.

`cert_pin` keeps a **set** of accepted issuer sets, with a grace rule in code: a new issuer is accepted on first sight and flagged once, never treated as `TAMPER` on arrival. CDN-fronted sites rotate CAs legitimately.

**Implemented in migration `0013`:** `store.ObserveCertIssuer` records one row per target and issuer hash (SHA-256 of the issuer DN; the leaf SPKI is never stored). The first issuer for a target is a baseline and is not flagged. A new issuer after a baseline is inserted and reported as a one-time change, which the agent turns into `tls_issuer_unexpected`. A repeat of an accepted issuer only refreshes `last_seen`. The drawer's "Certificate pin" row shows whether a baseline exists, not that the certificate is trusted.

---

## 9. Self-health and external data

```sql
CREATE TABLE self_check (
    id     INTEGER PRIMARY KEY,
    ts     INTEGER NOT NULL,
    "check" TEXT NOT NULL,                    -- 'clock','egress','resolver','disk','mtu','port53','heartbeat'
    ok     INTEGER NOT NULL,
    value  TEXT,                              -- e.g. '203.0.113.9,AS7922,US'
    detail TEXT
);

CREATE INDEX self_check_name_ts ON self_check("check", ts DESC);
```

`check` is a reserved word in SQLite, so it is quoted in the migration and in every query. The column keeps the name because "check" is what it holds.

**Implemented in migration `0006`:** `disk`, `resolver`, `egress`, `clock`, `mtu`, and `port53` are recorded by `internal/selfcheck`. `mtu` reports the smallest local interface MTU (a path MTU needs a DF-bit probe and raw sockets); `port53` proves both UDP/53 and TCP/53 through the guarded dialer.

CREATE TABLE external_event (
    id          INTEGER PRIMARY KEY,
    feed        TEXT NOT NULL,                -- 'ioda','radar'
    external_id TEXT NOT NULL,
    kind        TEXT NOT NULL,                -- 'outage','hijack','leak','traffic_anomaly'
    severity    REAL,
    country     TEXT,
    asn         INTEGER,
    started_at  INTEGER NOT NULL,
    ended_at    INTEGER,
    observed_at INTEGER NOT NULL,
    payload     TEXT NOT NULL,                -- raw JSON, capped at 8 KB
    UNIQUE (feed, external_id)
);

CREATE INDEX external_event_time ON external_event(started_at DESC);

CREATE TABLE feed_state (
    feed                 TEXT PRIMARY KEY,
    last_ok_at           INTEGER,
    last_err             TEXT,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    next_poll_at         INTEGER
);
```

`feed_state` is what makes `Radar: stale (2h)` possible instead of a silent blank (`DESIGN.md` §8.7).

**Implemented in migration `0007`:** `external_event` and `feed_state` are written by `internal/feeds`, with IODA as the first source (`NETWEATHER_FEEDS=ioda`). Events are deduplicated on `(feed, external_id)`, so a re-poll updates rather than duplicates. `feed_state.last_ok_at` is preserved across failures, because "stale since 09:58" is the fact worth keeping; the stale flag itself is computed on read so a clock change cannot freeze it.

---

## 10. Coverage

```sql
CREATE TABLE coverage_snapshot (
    bucket       INTEGER NOT NULL,            -- day bucket
    scope        TEXT NOT NULL,
    scope_value  TEXT NOT NULL,
    total        INTEGER NOT NULL,
    measurable   INTEGER NOT NULL,
    blocked      INTEGER NOT NULL,
    unmeasurable INTEGER NOT NULL,
    asn_count    INTEGER,
    country_count INTEGER,
    PRIMARY KEY (bucket, scope, scope_value)
) WITHOUT ROWID;
```

One row per scope per day. This is what makes "coverage is decaying over three weeks" visible; a point-in-time panel cannot show a trend.

**Implemented in migration `0008`:** `SnapshotCoverage` writes one row per scope for the UTC day, and re-running within the day updates that row instead of duplicating it, so a restart cannot invent a second snapshot. `CoverageHistory` reads oldest-first for `GET /api/v1/coverage/history`. `total` includes permanently unmeasurable targets, which `measurable` excludes, so the two numbers are not derived from each other by accident.

### 10.1 Hourly scope samples

```sql
CREATE TABLE scope_sample (
    scope        TEXT NOT NULL,               -- 'global','sector','country','asn'
    scope_value  TEXT NOT NULL,
    bucket       INTEGER NOT NULL,            -- unix hour
    health       REAL NOT NULL,
    coverage     REAL NOT NULL,
    measurable   INTEGER NOT NULL,
    judgeable    INTEGER NOT NULL,
    PRIMARY KEY (scope, scope_value, bucket)
) WITHOUT ROWID;
```

One snapshot per scope per hour, written by maintenance. Sampling the same hour twice replaces the snapshot rather than accumulating, so a restart cannot double-count.

The baseline reads the last 28 samples whose **hour-of-week** matches the evaluation instant (`internal/baseline`). That is why this table stores the raw hourly value rather than a pre-aggregated median: the window and the bucket key are detection policy, and policy changes should not require re-sampling history.

---

## 11. Retention

| Table | Keep | Job cadence | Notes |
|---|---|---|---|
| `probe` | 72h | 10 min, batches of 5000 | Pin ±1h around incidents first |
| `probe_archive` | 1y | nightly | Bounded by incident count |
| `rollup_5m` | 7d | hourly | |
| `rollup_1h` | 30d | daily | |
| `rollup_1d` | 1y | weekly | |
| `anomaly` | forever | — | Small; the evaluation corpus |
| `incident`, `attribution`, `incident_target` | forever | — | Small |
| `dns_observation` | 7d | hourly | |
| `dns_baseline` | forever | — | One row per resolver per target |
| `cert_pin` | forever | — | |
| `self_check` | 30d | daily | |
| `external_event` | 90d | daily | |
| `coverage_snapshot` | forever | — | 750 rows/day worst case |

Deletion is batched and never a single unbounded `DELETE`:

```sql
DELETE FROM probe WHERE id IN (
    SELECT id FROM probe WHERE ts < ? LIMIT 5000
);
```

Then, after the batch commits:

```sql
PRAGMA incremental_vacuum(1000);
```

Pinning runs **before** pruning, in the same job, so an incident opened at 03:58 cannot lose its 03:00–04:00 raw probes to a prune at 04:00.

---

## 12. Write path

One transaction per dispatcher tick, not one per probe:

```
BEGIN IMMEDIATE
  INSERT INTO probe ...            -- batch of rows completed this tick
  UPSERT target_state ...          -- only if state changed or streak advanced
  INSERT INTO state_change ...     -- only on an accepted transition
  UPSERT rollup_5m ...             -- increment the current bucket
COMMIT
```

At 750 targets the tick completes ~4 rows/s. That is trivial for SQLite; the failure mode to avoid is per-probe commits, which turn ~4 fsyncs/s into ~4 fsyncs *per probe* under load.

`BEGIN IMMEDIATE` takes the write lock up front, which avoids the deferred-transaction upgrade deadlock that produces `SQLITE_BUSY` under concurrent readers.

---

## 13. Query shapes

These are the five queries that decide whether the schema is right.

### 13.1 Board (latest state, primary vantage)

```sql
SELECT t.id, t.name, t.sector, t.country, t.cdn,
       s.state, s.since, s.last_error, s.attributed_to
FROM target t
JOIN target_state s
  ON s.target_id = t.id AND s.vantage_id = ?
WHERE t.enabled = 1
ORDER BY t.class, t.sector, t.name;
```

### 13.2 Scope health and coverage for an evaluation instant

Correlation reads `target_state` with a freshness filter, not `rollup_5m` counts — see `DESIGN.md` §7.2 ("Resolution and cadence"). A tier-2 target probed every 10 minutes is still an observation; it is just a low-resolution one.

```sql
WITH scope AS (
    SELECT id FROM target
    WHERE enabled = 1
      AND class = 'service'
      AND sector = ?
      AND measurability <> 'unmeasurable'
),
fresh AS (
    SELECT s.target_id, s.state
    FROM target_state s
    JOIN scope ON scope.id = s.target_id
    WHERE s.vantage_id = ?
      AND s.last_probe_at >= ?          -- now - freshness(tier)
),
counts AS (
    SELECT
        COALESCE(SUM(state IN ('UP','DEGRADED')), 0)        AS healthy,
        COALESCE(SUM(state IN ('UP','DEGRADED','DOWN')), 0) AS measurable
    FROM fresh
)
SELECT healthy, measurable,
       (SELECT COUNT(*) FROM scope) AS judgeable,
       CASE WHEN measurable = 0 THEN NULL
            ELSE 1.0 * healthy / measurable END AS health,
       CASE WHEN (SELECT COUNT(*) FROM scope) = 0 THEN NULL
            ELSE 1.0 * measurable / (SELECT COUNT(*) FROM scope) END AS coverage
FROM counts;
```

`health`, `coverage`, and `judgeable` come from one row so the `UNKNOWN` rule (`DESIGN.md` §7.2) is applied without a second round trip. The `measurable < 20` / `measurable < 8` floors are applied in code, not in SQL, because they are policy.

The same query with the `scope` CTE swapped is the sector, country, and ASN path; `global` drops the `sector = ?` predicate.

### 13.3 Drawer: last 20 probes

```sql
SELECT ts, state, stage, error_class, http_status, latency_ms, detail
FROM probe
WHERE target_id = ? AND vantage_id = ?
ORDER BY ts DESC
LIMIT 20;
```

### 13.4 Attribution candidates for an incident

```sql
SELECT kind, value, failing, total, share
FROM attribution
WHERE incident_id = ?
ORDER BY rank;
```

### 13.5 Coverage panel

```sql
SELECT
    COUNT(DISTINCT t.asn)      AS asn_count,
    COUNT(DISTINCT t.country)  AS country_count,
    SUM(t.measurability = 'unmeasurable') AS unmeasurable,
    SUM(CASE WHEN s.state IN ('UP','DEGRADED','DOWN') THEN 1 ELSE 0 END) AS measurable,
    COUNT(*) AS total
FROM target t
LEFT JOIN target_state s
  ON s.target_id = t.id AND s.vantage_id = ?
WHERE t.enabled = 1;
```

---

## 14. Backup and export

**Backup:** `VACUUM INTO '/data/backup-2026-11-16.db'`. It is consistent while the app is running, produces a compact file with no WAL, and needs no lock choreography. Copying `netweather.db` directly while WAL is active is the standard way to produce a corrupt backup.

**Export:** `netweather export --from … --to … --format csv|json`, streaming from `rollup_5m` or `rollup_1h`, never loading a result set into memory.

**Restore:** stop the container, replace `/data/netweather.db`, delete any `-wal`/`-shm` siblings, start. Document the sibling deletion — leaving a stale `-wal` next to a restored database is a silent corruption.

---

## 15. Migrations

- Forward-only, numbered (`0001_init.sql`, `0002_…`), embedded in the binary.
- `schema_meta.schema_version` records the applied version.
- Run at startup, inside a transaction, with a pre-migration `VACUUM INTO` backup.
- **Refuse to start if the database version is newer than the binary.** A downgrade silently dropping columns is worse than a refusal to boot.
- Every migration must be tested against a database populated by the previous release, not only against an empty one. The `internal/store` tests (`QUALITY.md` §5.1) own this.

---

## 16. Sizing

At 750 targets, ~5 probes/s, ~430k probes/day:

| Table | Rows/day | Bytes/row | Steady size |
|---|---|---|---|
| `probe` | 430,000 | ~55 + ~75 across indexes | ~170 MB at 72h |
| `rollup_5m` | ~190,000 | ~60 | ~80 MB at 7d |
| `rollup_1h` | ~17,500 | ~60 | ~32 MB at 30d |
| `rollup_1d` | 750 | ~60 | ~16 MB at 1y |
| `state_change` | ~200 | ~80 | ~5 MB at 1y |
| `self_check` | ~1,300 | ~60 | ~2 MB at 30d |
| `external_event` | < 200 | ~1 KB | ~18 MB at 90d |
| `coverage_snapshot` | 750 | ~60 | ~16 MB at 1y |

Steady state ≈ **340 MB**, with a hard ceiling well under 1 GB. If a deployment exceeds that, the first suspect is a rollup job recomputing instead of incrementing (§6.1).

The row counts follow from `DESIGN.md` §6.1: tier 0 at 30s, tiers 1 and 3 at 120s, tier 2 at a 450s midpoint. If an operator shortens intervals, storage scales linearly and the ceiling arrives sooner — which is why the interval floor is enforced in code rather than trusted to config.

---

## 17. What is deliberately not stored

- **Response bodies.** Never. One line in `detail`, capped at 200 bytes, stripped of control characters.
- **Full header sets.** Only the fields the classifier used (`cdn`, `http_status`, `error_class`).
- **Every probe from a Tor vantage.** The volume cap makes it small, but the point is that a Tor vantage is corroboration; its failures are expected and not worth a time series.
- **Per-hop traceroute history.** Traces attach to an incident as a blob, never a series (`DESIGN.md` §9.1).
- **Anything derived that can be recomputed** — except rollups, which are derived but too expensive to recompute.

---

## 18. Decisions and open questions

### 18.1 Resolved: resolution vs. cadence

**Decided, before phase 0:** detection cadence is decoupled from probe cadence.

| Concern | Cadence | Input |
|---|---|---|
| Probe | per tier, 30s–600s | scheduler |
| Scope health evaluation | fixed 5 min, UTC-aligned | `target_state` + freshness filter |
| Baseline sample | 1 per hour-of-week slot | median of that hour's 12 evaluations |
| Detection | 2 consecutive 5-min evaluations | `DESIGN.md` §7.2 trigger |

Scope health is `SELECT … FROM target_state WHERE last_probe_at >= now - freshness`, where `freshness = min(2 × interval + 30s, 15 min)`. It is **not** `SUM` over `rollup_5m` rows for the bucket. A 600s target therefore contributes a constant one observation per evaluation instead of flickering between one and zero, which is the entire source of the sawtooth.

Stale targets leave numerator and denominator and reduce `coverage`, which is correct: a stalled scheduler is a visibility failure, not an outage.

`probe.bucket_30s` stays. It is not a correlation input; it is attribution evidence ("these two fast-tier failures shared a 30-second window"). Slow-tier targets are attributed by state, never by bucket.

"Simultaneous" is now defined precisely as *current within the freshness window at the evaluation instant*.

### 18.2 Open

1. **Latency percentiles.** §6.2 chose EWMA over true percentiles and renamed the columns. If `DEGRADED` needs real p95s, add a 16-bucket histogram BLOB.
2. **Partition raw probes by day** into attached databases. Unnecessary at 340 MB; would make 72h retention a file drop. Revisit only above ~5 GB.
3. **`WITHOUT ROWID` on `probe`.** Rejected: rowid is the cursor for pagination and gives free insert ordering.
4. **Should `target_state` be rebuilt on startup** from `state_change`, or trusted as authoritative with a repair job? Currently trusted, rebuilt only on `VANTAGE_CHANGED` or a schema migration.
5. **Dual-stack rows.** `probe.family` exists so a v6-only failure can be represented without a migration, but the state semantics are still open (`DESIGN.md` §16 item 7).
