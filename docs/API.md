# HTTP API

> The UI is server-rendered HTML. The API exists for everything a browser page is bad at: scripts, exports, alerting glue, and debugging a remote instance that has no shell.

This document is **normative** for `/api/v1`. Where it and the code disagree, one of them is a bug.

---

## 1. What exists today

Honesty first, because a spec that implies more than the binary does is worse than no spec.

| Surface | Status |
|---|---|
| `GET /healthz` | **implemented** — liveness only, no auth, no data |
| `GET /api/v1/meta` | **implemented** — build, uptime, auth mode, capabilities |
| `GET /api/v1/logs` | **implemented** - reads the in-memory ring (`LOGGING.md` §9) |
| `GET /api/v1/board` | **implemented** - the grid's data from the store |
| `GET /api/v1/targets` | **implemented** - the target list with class/sector/country/state filters |
| `POST /api/v1/targets/{id}/disable` | **implemented** - stops probing one target at runtime; the row and history stay |
| `GET /api/v1/targets/{id}` | **implemented** - one target with its current state |
| `GET /api/v1/targets/{id}/series` | **implemented** - rollup buckets at 5m/1h/1d |
| `GET /api/v1/scopes/{scope}` | **implemented** - health, coverage, and the UNKNOWN floors |
| `GET /api/v1/coverage` | **implemented** - the blind-spot panel |
| `GET /api/v1/anomalies` | **implemented** - baseline-relative detector output |
| `GET /api/v1/control`, `POST /api/v1/pause`, `POST /api/v1/resume` | **implemented** - the probe kill switch |
| `GET /api/v1/targets/{id}/probes` | **implemented** - recent raw observations for one target |
| `GET /api/v1/incidents`, `GET /api/v1/incidents/{id}` | **implemented** - the incident log and ranked attribution |
| `GET /api/v1/corridors` | **implemented** - configured path pairs with their newest sample and open step change |
| `GET /api/v1/ihi` | **implemented** - the Internet Health Index with its components and what was dropped (`DESIGN.md` §11) |
| `GET /metrics` | **implemented** - aggregate Prometheus text; off unless `NETWEATHER_METRICS=true`; bearer auth when a token is set |
| `GET /events` | **implemented** - the UI's SSE stream; session cookie auth, not bearer. One `snapshot` on connect, then `state` deltas. The API-facing counterpart is `GET /api/v1/events` (§6.17) |
| Everything else in §7 | **implemented** — the section is kept as the action/index surface, not a backlog |

`GET /api/v1/meta` returns the list of implemented capabilities, so a client can discover the truth at runtime instead of trusting this table.

---

## 2. Base contract

| Property | Value |
|---|---|
| Base path | `/api/v1` |
| Media type | `application/json; charset=utf-8` |
| Encoding | UTF-8, no BOM |
| Field naming | `snake_case` |
| Timestamps | RFC 3339, UTC, milliseconds (`2026-11-16T03:14:15.123Z`) |
| Booleans | JSON `true` / `false`, never `"yes"` |
| Null handling | Absent means "not applicable"; `null` means "known to be unset" |
| Trailing slashes | Not accepted |
| Unknown query parameters | Ignored (forward compatibility) |
| Unknown body fields | Rejected on writes (`invalid_argument`) |

### Versioning

- `v1` is frozen once released. **Additive changes only**: new endpoints, new optional fields, new enum values that a client must tolerate.
- Removing a field, renaming one, changing a type, or changing a default is a breaking change → `/api/v2`.
- A deprecated endpoint carries `Deprecation: true` and `Sunset: <HTTP-date>` for at least 90 days.
- The server never negotiates versions by header. The path is the version.

### Stability tiers

Every endpoint is documented with one:

| Tier | Promise |
|---|---|
| `stable` | Covered by contract tests; breaking changes require `v2` |
| `experimental` | May change in a minor release; may disappear; no deprecation period |
| `internal` | Used by the UI; not for third parties; may change without notice |

---

## 3. Authentication

Two mechanisms, deliberately separate:

| Caller | Mechanism |
|---|---|
| API clients | `Authorization: Bearer <token>` |
| Browser (UI) | Session cookie from the login form, `HttpOnly` + `SameSite=Lax` |

Rules:

- The API never accepts a token in a query string. Tokens in URLs end up in logs, `Referer` headers, and shell history.
- Comparison is constant-time over SHA-256 digests, so token length does not leak.
- `401` responses include `WWW-Authenticate: Bearer realm="netweather"`.
- `403` is not used for missing credentials; that is `401`. `403` is for an authenticated caller without permission (reserved for future multi-user).
- `/healthz` is unauthenticated **by design** and must never disclose target data.
- The token comes from `NETWEATHER_UI_TOKEN_FILE` (preferred) or `NETWEATHER_UI_TOKEN`. Minimum 16 characters; the loader rejects shorter values.
- A non-loopback listen with no token **refuses to start** unless `NETWEATHER_UI_ALLOW_PUBLIC=true` (`NETWORKING.md` §7.1).

There is no API key rotation endpoint. Rotate by replacing the file and restarting; sessions are invalidated because they derive from the token.

---

## 4. Response envelope

Success:

```json
{
  "data": { "...": "..." },
  "meta": { "request_id": "9f2c1a7b4e8d0356" }
}
```

Lists add pagination fields:

```json
{
  "data": [ { "...": "..." } ],
  "meta": {
    "request_id": "9f2c1a7b4e8d0356",
    "count": 42,
    "limit": 100,
    "next_cursor": "eyJ0cyI6MTc2MzI2..."
  }
}
```

Failure:

```json
{
  "error": {
    "code": "invalid_argument",
    "message": "limit must be between 1 and 500",
    "request_id": "9f2c1a7b4e8d0356"
  }
}
```

`message` is for humans and may change. `code` is the stable contract. Clients branch on `code`, never on prose.

### Error codes

| Code | HTTP | Meaning |
|---|---|---|
| `invalid_argument` | 400 | Malformed parameter, body, or timestamp |
| `unauthorized` | 401 | Missing or wrong bearer token |
| `forbidden` | 403 | Authenticated but not allowed |
| `not_found` | 404 | No such endpoint or resource |
| `method_not_allowed` | 405 | Wrong method for a known path (`Allow` header is set) |
| `conflict` | 409 | State changed under the request; retry |
| `payload_too_large` | 413 | Body over the limit |
| `unprocessable` | 422 | Well-formed but semantically invalid |
| `rate_limited` | 429 | Too many requests (`Retry-After` is set) |
| `internal` | 500 | Bug; the request ID is the support handle |
| `not_implemented` | 501 | Endpoint reserved, not built |
| `unavailable` | 503 | Store or dependency not ready |

### The one exception

`GET /healthz` returns a bare object, not the envelope:

```json
{"status":"ok","version":"dev"}
```

Health probes should not have to unwrap an envelope, and orchestrators parse raw JSON.

---

## 5. Pagination, filtering, and limits

- **Cursors, not offsets.** `?limit=100&cursor=<opaque>`; the cursor encodes the last key seen, so inserts do not shift pages.
- `limit` defaults to 100 for lists, 500 maximum. Out-of-range is `invalid_argument`, never a silent clamp.
- `next_cursor` is absent on the last page. An empty page is legitimate.
- Cursors are opaque. Clients must not parse or construct them.
- Time filters use `from` / `to`, inclusive of `from`, exclusive of `to`, RFC 3339.
- Filtering by target accepts a registrable hostname, not a URL and not a wildcard.
- Unknown `sort` values are rejected; `sort` is a closed enum per endpoint.
- `GET /api/v1/logs` is the exception: it returns the newest N after filtering and does not paginate. Incidents (`GET /api/v1/incidents`) is the first cursor-paginated list; the other list endpoints still return the newest N, and their cursors land with the same key-based rule.

---

## 6. Implemented endpoints

### 6.1 `GET /healthz` — stable

No auth. Intended for Docker `HEALTHCHECK` and orchestrators.

```json
{"status":"ok","version":"dev","commit":"none","uptime_s":812}
```

- `200` when the process is alive and the store is readable.
- `503` when the store is unavailable. The body still parses.
- Never includes targets, counts, vantages, or egress identity.
- `Cache-Control: no-store`.

### 6.2 `GET /api/v1/meta` — stable

Auth: bearer.

```json
{
  "data": {
    "version": "dev",
    "commit": "none",
    "api": "v1",
    "started_at": "2026-11-16T03:00:00.000Z",
    "uptime_s": 812,
    "auth": "bearer",
    "schema_version": 23,
    "egress": {"used_bytes": 1048576, "limit_bytes": 2147483648, "exhausted": false},
    "capabilities": ["health", "meta", "logs", "board", "targets", "control", "probes", "series", "incidents", "annotations", "scopes", "anomalies", "coverage", "selfcheck", "export", "events", "feeds", "debug", "coverage_history", "trace", "corridors", "ihi", "target_control"],
    "ring": {"records": 128, "capacity": 2000}
  },
  "meta": {"request_id": "9f2c1a7b4e8d0356"}
}
```

`capabilities` is the runtime truth: a client should feature-detect here rather than assume an endpoint exists. `auth` is `bearer` or `none` (loopback development only). `schema_version` is the applied migration number, absent when it cannot be read; it is what a support bundle needs and cannot get from a remote CLI otherwise. `egress` is the direct vantage's daily byte budget (`ACCEPTABLE_USE.md` §4): `used_bytes`, `limit_bytes`, and `exhausted`; it is absent when the budget is disabled, and `exhausted: true` means probing has stopped until 00:00 UTC, so an empty board is a budget stop rather than silence.

No target data appears here. That is deliberate: `/meta` is the one endpoint a monitoring system may poll, and it must not become a target-list leak.

### 6.3 `GET /api/v1/logs` — experimental

Auth: bearer. Reads the in-memory ring, not the file sink. Redaction already happened at write time, so responses are safe to paste.

| Parameter | Type | Default | Notes |
|---|---|---|---|
| `level` | enum `trace\|debug\|info\|warn\|error` | `trace` | Minimum level |
| `target` | string | — | Case-insensitive match on the record's target attribute |
| `since` | RFC 3339 | — | Records strictly after this instant |
| `limit` | 1–500 | 100 | Newest N after filtering |

```json
{
  "data": [
    {
      "ts": "2026-11-16T03:14:15.123Z",
      "level": "INFO",
      "message": "state change",
      "attrs": {
        "event": "state_change",
        "target": "chase.com",
        "vantage": "local",
        "state_from": "UP",
        "state_to": "DOWN",
        "stage": "tls",
        "error_class": "tls_expired",
        "reason": "certificate expired 2h ago",
        "trace_id": "9f2c1a7b4e8d0356"
      }
    }
  ],
  "meta": {"request_id": "…", "count": 1, "limit": 100}
}
```

Sorted oldest → newest. The ring holds minutes, not days; for a longer window use `GET /api/v1/targets/{id}/probes` (§6.6) or `/series` (§6.10).

### 6.4 `GET /api/v1/board` — stable

Auth: bearer. The grid's data, straight from the store, in source order.

```json
{
  "data": [
    {
      "id": "svc-chase",
      "name": "Chase",
      "host": "www.chase.com",
      "sector": "financial/banking",
      "country": "US",
      "state": "up",
      "since": "2026-11-16T03:14:15Z",
      "measurability": "ok"
    }
  ],
  "meta": {"request_id": "…", "count": 1,
           "verdict": {"value": "OK", "detail": "within the baseline band"}}
}
```

- `state` is one of the eight target states, lower-cased (`DESIGN.md` §3.2).
- `since` is the start of the current state. It is absent for a target that has never been probed, which is different from "up since the epoch".
- `measurability` is the rolling calibration from `DESIGN.md` §3.2: `ok`, or `unmeasurable` when more than 80% of the last seven days' probes were `BLOCKED`. `unmeasurable` targets keep their state but are excluded from health and coverage, so a client should not read a `blocked` state alone as a service outage.
- `meta.verdict` is the one global verdict from `DESIGN.md` §3.3, with its sentence. `value` is one of `OK`, `INCIDENT`, `LOCAL`, `REGIONAL`, `TAMPER`, `STALE`, `STARTING`, or `LEARNING`. It is omitted when no verdict source is configured, and `STALE` means probing has stopped rather than that the internet is quiet.
- No server-side filtering in v1: the list is capped at 2000, and a client that wants only failures can filter locally. Filters arrive with `/api/v1/targets` if they prove necessary.
- `503 unavailable` when the store is not readable. An empty `data` means "no targets", not "cannot answer".

### 6.5 `POST /api/v1/pause`, `POST /api/v1/resume`, `GET /api/v1/control` — stable

The kill switch. Pausing stops outbound probing immediately; the UI, API, and stored data stay available. Resuming re-arms on the next scheduler tick. `control` reports the current state.

```json
{"data": {"paused": true}, "meta": {"request_id": "…"}}
```

- Both POST routes are idempotent: pausing twice is not an error.
- Bearer auth only. There is no CSRF token because the route is not reachable with a browser session cookie.
- The action is logged at `INFO` with the request id, so "who paused it" is answerable.
- **Pause is not persisted across a restart.** A crash must not leave a kill switch on forever, and a restart must not silently silence monitoring.

### 6.6 `GET /api/v1/targets/{id}/probes` — stable

Auth: bearer. The newest observations for one target, newest first. This is the drawer's data.

| Parameter | Default | Notes |
|---|---|---|
| `limit` | 20 | 1–200 |

```json
{
  "data": [
    {
      "ts": "2026-11-17T12:01:00Z",
      "state": "down",
      "stage": "tls",
      "error_class": "tls_cert_expired",
      "latency_ms": 41,
      "confidence": "high",
      "detail": "certificate expired"
    }
  ],
  "meta": {"request_id": "…", "count": 1, "limit": 20}
}
```

- `404` for an unknown target. A target that exists but has never been probed is `200` with an empty list — those mean different things.
- Raw probes are retained for 72h (`SCHEMA.md` §11); for a longer window use `/series` (`API.md` §6.10) or the export route (§6.16).
- `since` is not a parameter here: this endpoint is "the last N", not a range scan.

### 6.7 `GET /api/v1/incidents` — stable

Auth: bearer. Newest first.

| Parameter | Default | Notes |
|---|---|---|
| `open` | `false` | `true` returns only unclosed incidents |
| `limit` | 50 | 1-200 |
| `cursor` | - | Opaque `next_cursor` from the previous page |

```json
{
  "data": [
    {
      "id": "inc-9f2c1a7b4e8d0356",
      "opened_at": "2026-11-17T12:00:00Z",
      "verdict": "INCIDENT",
      "scope": "sector",
      "scope_value": "airline",
      "headline": "5 of 6 targets failing on sector=airline",
      "confidence": "high"
    }
  ],
  "meta": {"request_id": "…", "count": 1, "limit": 50, "next_cursor": "eyJ0cyI6MTc2MzI2…"}
}
```

- An empty list means "no incidents", not "cannot answer"; the latter is `503`.
- `closed_at` is absent while the incident is open.
- This is the first cursor-paginated list (§5). The cursor encodes the last `(opened_at, id)` a page returned, so an incident opening between two requests cannot shift a later page. `next_cursor` is absent on the last page.
- A malformed cursor is `400 invalid_argument`. A reader that cannot page answers `501 not_implemented` rather than silently repeating the first page; the shipped store always pages.

### 6.8 `GET /api/v1/incidents/{id}` — stable

The incident plus every ranked attribution candidate, not only the winner — when the winner is later shown to be wrong, the second-best explanation is already on disk.

```json
{
  "data": {
    "incident": {"id": "inc-…", "verdict": "INCIDENT", "scope": "sector", "…": "…"},
    "attribution": [
      {"rank": 1, "kind": "sector", "value": "airline", "failing": 5, "total": 6,
       "share": 0.833, "score": 1.5}
    ],
    "targets": ["svc-united", "svc-delta"],
    "corroboration": [
      {"feed": "ioda", "external_id": "1001", "kind": "outage", "severity": 0.87,
       "country": "IR", "started_at": "2026-11-17T10:00:00Z"}
    ],
    "probes": [
      {"ts": "2026-11-17T11:30:00Z", "state": "down", "stage": "http",
       "error_class": "tcp_refused", "confidence": "high", "detail": "refused"},
      {"ts": "2026-11-17T11:35:00Z", "state": "up", "stage": "http",
       "http_status": 200, "latency_ms": 42, "confidence": "high"}
    ]
  },
  "meta": {"request_id": "…"}
}
```

`404` for an unknown id. Attribution is stored at open time and is not recomputed on read, so the incident keeps the evidence that produced it.

`probes` is the ±1 hour window of observations pinned into `probe_archive` when the incident opened (`SCHEMA.md` §5), oldest first, up to 20 rows. It is what makes an incident readable after the raw probe stream is pruned at 72 hours. The array is always present: a failed archive read is logged and leaves it empty rather than hiding the incident, and an incident opened before the archive existed simply has none.

`corroboration` lists external feed events that overlap the incident's window (two hours before it opened through its close or now) and match its country or ASN when the scope names one. It is evidence, never a verdict: a matching outage makes the incident more believable, and a matching statuspage incident points at the provider rather than at your path. A sector or global incident has no place to match, so the list is everything in the window, highest severity first. The array is always present; a failed corroboration query is logged and leaves it empty rather than hiding the incident.

The incident object carries `annotation` and `annotated_at` when the operator has recorded ground truth (see §6.8.1).

### 6.8.1 `POST /api/v1/incidents/{id}/annotation` — stable

Auth: bearer. `Content-Type: application/json`. Records what actually happened. It is the hook that turns a stored incident into a labelled example for the backtest corpus.

```json
{"annotation": "AWS us-east-1, confirmed on the status page"}
```

- `annotation` is required and trimmed; 1-500 characters.
- A second call replaces the previous annotation: the incident keeps the latest ground truth, not a history.
- `404` for an unknown id. The write is never silently discarded.
- `415` unless the body is JSON; `400` on unknown fields or malformed JSON.
- The UI's incident page posts the same value through a CSRF-checked form.

### 6.9 `GET /api/v1/targets` — stable

Auth: bearer. The same rows as the board, with filters. All filters combine with AND.

| Parameter | Notes |
|---|---|
| `class` | `service`, `country`, `network`, `reference` |
| `sector` | exact match, e.g. `financial/banking` |
| `country` | ISO-3166-1 alpha-2, e.g. `US` |
| `state` | one of the eight states |

```json
{
  "data": [
    {"id": "svc-delta", "name": "Delta", "host": "www.delta.com", "class": "service",
     "sector": "airline", "country": "US", "state": "down", "since": "2026-11-17T12:01:00Z"}
  ],
  "meta": {"request_id": "…", "count": 1}
}
```

- An unknown `state` is `400 invalid_argument`: it is an enum.
- Unknown `class`, `sector`, or `country` values are data and simply match nothing.
- No pagination in v1. The list is capped at 2000 targets and returned whole; the board has the same bound.

### 6.9.1 `POST /api/v1/targets/{id}/disable` - stable

Auth: bearer. Stops probing one target without a restart. It is the abuse-response path from `ACCEPTABLE_USE.md` §7: the target row and its history stay, the live scheduler drops the target on the next tick, and a later config reload can bring it back. It is not a delete.

```json
{"data": {"target": "svc-chase", "state": "disabled"}, "meta": {"request_id": "…"}}
```

- The path id is the target id, not the host. `netweather disable <host-or-id>` resolves a host through `GET /api/v1/targets` first, so an operator does not have to know the derived id.
- `404` for an unknown id. `405` with `Allow: POST` for any other method. `503` when runtime target control is not configured, rather than a silent success.
- The action is logged at `WARN` with the request id and whether the live agent actually removed the target.
- Disabling is idempotent in effect: a second call on an already-disabled target returns `404`, because the target is no longer in the enabled set.

### 6.10 `GET /api/v1/targets/{id}/series` - stable

Auth: bearer. Rollup buckets over a window.

| Parameter | Default | Notes |
|---|---|---|
| `resolution` | `5m` | `5m`, `1h`, or `1d` |
| `from` | 24h ago | RFC 3339, inclusive |
| `to` | now | RFC 3339, exclusive |

```json
{
  "data": [
    {"bucket": "2026-11-17T10:00:00Z", "n": 12, "up": 10, "degraded": 0,
     "down": 2, "blocked": 0, "unknown": 0}
  ],
  "meta": {"request_id": "…", "count": 1}
}
```

- Buckets are UTC-aligned to the resolution.
- A bucket with no observations is absent, not zero-filled; a client that needs a continuous series must fill gaps itself.
- `resolution=1h` requires the hourly fold to have run (it runs every ten minutes); `1d` likewise.
- Unknown resolution, an unparseable timestamp, or `from >= to` is `400`.

### 6.11 `GET /api/v1/scopes/{scope}` — stable

Auth: bearer. `scope` is `global`, `sector`, `country`, or `asn`; `value` selects the member for the non-global scopes.

```json
{
  "data": {
    "scope": "sector",
    "value": "airline",
    "status": "ok",
    "mode": "learning",
    "health": 0.93,
    "coverage": 0.88,
    "measurable": 44,
    "judgeable": 50,
    "states": {"up": 40, "degraded": 1, "down": 3, "blocked": 6, "unknown": 0,
               "up_reachability": 0, "config_error": 0, "unmeasurable": 0},
    "baseline": {"n": 24, "median": 0.94, "mad": 0.01, "sigma": 0.0148, "usable": true}
  },
  "meta": {"request_id": "…"}
}
```

- `status` is `ok`, `low_coverage`, or `unknown`. The floors: `coverage >= 0.80` and `measurable >= 20` for `ok`; `coverage >= 0.50` and `measurable >= 8` to compute health at all.
- `health` is `null` when the scope is `unknown`. A client must not render null as zero.
- `judgeable` excludes targets marked permanently unmeasurable. Unprobed targets still count toward it, so a stalled scheduler lowers coverage instead of looking healthy.
- `mode` is `learning` until 21 of 28 hour-of-week samples exist; `baseline.usable` is false until then. The baseline always reports `n`, `median`, `mad`, and `sigma`, so a client can show how far along learning is.

### 6.12 `GET /api/v1/anomalies` — stable

Auth: bearer. Detector output, newest first. An anomaly is an observation relative to a baseline, not a verdict; an incident may or may not follow.

| Parameter | Default | Notes |
|---|---|---|
| `limit` | 50 | 1–200 |

```json
{
  "data": [
    {
      "id": 41,
      "ts": "2026-11-17T12:05:00Z",
      "scope": "sector",
      "scope_value": "airline",
      "kind": "health_drop",
      "observed": 0.62,
      "baseline": 0.94,
      "mad": 0.01,
      "sigma": 0.0148,
      "buckets": 2
    }
  ],
  "meta": {"request_id": "…", "count": 1, "limit": 50}
}
```

- `kind` is `health_drop` in v1: two consecutive evaluations below `baseline - 3σ` with at least a two-point drop.
- `buckets` is how many consecutive low evaluations produced the event.
- `incident_id` is absent until an incident links to the anomaly.
- The detector only evaluates scopes that clear the coverage floors, because running it on an `UNKNOWN` scope would turn a visibility failure into a false incident.
- Repeated detections for the same scope are suppressed for 30 minutes after one fires.

### 6.13 `GET /api/v1/targets/{id}` — stable

Auth: bearer. One target with its current state and metadata.

```json
{
  "data": {
    "id": "svc-chase",
    "name": "Chase",
    "host": "www.chase.com",
    "class": "service",
    "sector": "financial/banking",
    "country": "US",
    "state": "down",
    "since": "2026-11-17T12:00:00Z",
    "measurability": "ok",
    "cert_issuer": "CN=R11,O=Let's Encrypt,C=US",
    "cert_not_after": "2027-01-16T12:00:00Z",
    "cert_chain_len": 2,
    "cert_intermediate_spki": "aabbccddeeff00112233445566778899",
    "cert_leaf_fingerprint": "aabbccddeeff00112233445566778899",
    "dns_verdict": "DNS_ANOMALY",
    "dns_confidence": "medium",
    "dns_detail": "one resolver disagrees with its own baseline (1.1.1.1:53: NXDOMAIN where this resolver answered before)",
    "dns_at": "2026-11-17T12:00:00Z",
    "ix_names": "AMS-IX, DE-CIX Frankfurt",
    "status_url": "https://status.chase.com/",
    "vantages": [
      {"id": "local", "kind": "direct", "role": "primary", "state": "down",
       "since": "2026-11-17T12:00:00Z", "last_probe": "2026-11-17T12:04:00Z"},
      {"id": "tor-1", "kind": "socks5", "role": "corroboration", "state": "up",
       "since": "2026-11-17T11:58:00Z", "last_probe": "2026-11-17T12:03:00Z"}
    ],
    "sources": [
      {"source": "seed:builtin", "fetched_at": "2026-11-17T12:00:00Z",
       "sha256": "9f2c1a7b4e8d03569f2c1a7b4e8d03569f2c1a7b4e8d03569f2c1a7b4e8d0356"}
    ]
  },
  "meta": {"request_id": "…"}
}
```

- `404` for an unknown or disabled target.
- `since` is absent for a target that has never been probed; the state is then `unknown`.
- `measurability` is `ok`, or `unmeasurable` when the target has been `BLOCKED` more than 80% of the time over the last seven days (`DESIGN.md` §3.2). An `unmeasurable` target keeps its last state but is excluded from health and coverage.
- `vantages` is every vantage's confirmed state for this target, primary first. It is how a client sees a regional split: one vantage `down` and another `up` means the path, not the service (`DESIGN.md` §7.1). The block is absent when no vantage reader is configured.
- Certificate fields come from the last TLS observation and are absent when the last probe was not TLS. They describe the leaf certificate.
- `cert_chain_len` and `cert_intermediate_spki` are the *learned* chain shape from the pin baseline (migration `0015`), not the last observation. A chain-shape change is informational and never changes `state`.
- `cert_leaf_fingerprint` is the learned leaf fingerprint (migration `0016`). It is absent until a TLS probe has been seen; a rotation is informational.
- `dns_verdict`, `dns_confidence`, `dns_detail`, and `dns_at` are the newest per-resolver divergence advisory (migration `0017`, §3.2 of `CLASSIFIER.md`). They are absent until one has been observed, and they never change `state`: `DNS_ANOMALY` is advisory and `TAMPER` is a ladder verdict that still leaves the recorded probe as its own measurement.
- `ix_names` is PeeringDB exchange membership for the target's ASN: enrichment only, never a signal. It is absent until enrichment has learned it.
- `status_url` is the operator's declared status page for this target: config data, not an observation. When present, the drawer's **Provider status** panel lists that page's incidents from the last 24 hours, matched by the page's host (`DESIGN.md` §10.5). It is absent when the target declares no page.
- `sources` is the target's provenance: the seed or config that produced the row and the SHA-256 of that payload (migration `0022`, `THREAT_MODEL.md` T35). `netweather seed` records `seed:builtin` or `seed:root-zone`, and a config load records `config:<path>`. The block is absent when no provenance reader is configured; a failed read is logged and leaves it absent rather than failing the target, and an empty list means the row predates the table.

### 6.14 `GET /api/v1/coverage` — stable

Auth: bearer. The blind-spot panel. It reports **measurement**, not health: a down target is still measured, and this endpoint exists to catch the targets that are not.

```json
{
  "data": {
    "total": 120,
    "measurable": 96,
    "coverage": 0.8,
    "concentration": {
      "asn_count": 42, "country_count": 18,
      "top_asn": 13335, "top_asn_name": "Cloudflare",
      "top_asn_share": 0.183, "top10_share": 0.55,
      "single_anchor_countries": 3, "thin_sectors": 2
    },
    "classes": [
      {"kind": "class", "value": "service", "total": 90, "measurable": 84, "unmeasurable": 2, "coverage": 0.933}
    ],
    "sectors": [
      {"kind": "sector", "value": "airline", "total": 12, "measurable": 11, "unmeasurable": 0, "coverage": 0.917}
    ],
    "blind_spots": [
      {"kind": "country", "value": "NL", "total": 10, "measurable": 2, "unmeasurable": 8, "coverage": 0.2}
    ],
    "stale": [
      {"id": "svc-delta", "host": "www.delta.com", "state": "unknown",
       "interval_s": 300, "age_seconds": 5400, "last_probe": "2026-11-17T07:30:00Z"}
    ]
  },
  "meta": {"request_id": "…"}
}
```

- `measurable` counts states that carry a usable signal: `up`, `up_reachability`, `degraded`, `down`. Everything else is a blind spot.
- `blind_spots` is every ASN or country with at least five targets and less than 50% measurable, biggest first. `kind` is `country` or `asn`, because both are in the same list and "NL" and "1103" would otherwise be indistinguishable.
- `stale` lists enabled targets the scheduler has not reached within three of their own intervals (minimum 15 minutes), including targets that have never been probed. A never-probed target has no `last_probe` and no `age_seconds`.
- `concentration` is the network spread from `DESIGN.md` §10.6: distinct learned ASNs and countries, the largest ASN and its share of enabled targets, the share behind the ten largest ASNs, countries with exactly one country anchor (no redundancy), and sectors below 50% measurable. Shares are over enabled targets, the same denominator the overview line uses.
- The panel is per-vantage: it says "this vantage cannot see these", not "these are broken".

### 6.15 `GET /api/v1/selfcheck` — internal

Auth: bearer. The newest result for each self-health check. This answers "can the monitor see at all?", which is a different question from "is the internet up?".

When `NETWEATHER_REDACT_EGRESS=true`, the `egress-ip` value's literal address is replaced with `[redacted]` and the ASN and country are kept. The stored value is unchanged, so `VANTAGE_CHANGED` still compares the real tuple.

```json
{
  "data": [
    {"check": "clock", "ok": true, "value": "2026-11-17T12:00:00Z", "detail": "1s skew",
     "ts": "2026-11-17T12:00:05Z"},
    {"check": "disk", "ok": false, "detail": "read-only file system",
     "ts": "2026-11-17T12:00:05Z"}
  ],
  "meta": {"request_id": "…", "count": 2}
}
```

- Checks are `disk`, `resolver`, `egress`, `egress-ip`, `port53`, `mtu`, and `clock`. `port53` proves both UDP/53 and TCP/53 to a public resolver, because a blocked egress port would turn every DNS anchor red while the internet is fine. `mtu` reports the smallest MTU among up, non-loopback interfaces and warns below a 1400-byte floor; it measures the local interface, not the path, because a path measurement needs a DF-bit probe and raw sockets. `egress-ip` records the public address as `ip,AS<n>,<country>` and maintenance logs `vantage_changed` when the tuple moves between successful observations.
- `ok: false` means the monitor's own path failed, which is why this endpoint is separate from the board: a broken monitor must not look like a broken internet.
- One row per check; the endpoint reports the latest run, not a history.
- Self-checks run at startup and with maintenance every ten minutes, so a failed check is already in the logs before anyone opens this page.

### 6.16 `GET /api/v1/export` — stable

Auth: bearer. Streams observations for a time range. Nothing is buffered: the store calls back one row at a time, so a 31-day export does not become a 500 MB slice.

| Parameter | Default | Notes |
|---|---|---|
| `format` | `csv` | `csv` or `jsonl` |
| `from` | 24h ago | RFC 3339, inclusive |
| `to` | now | RFC 3339, exclusive |

- `Content-Disposition` is set so a browser downloads rather than renders.
- A window longer than 31 days is `400`. The CLI has no such cap because an operator can wait; a client cannot.
- CSV columns are stable: `target_id, host, vantage, ts, state, stage, error_class, http_status, latency_ms, confidence, detail`.
- If the stream fails mid-way the status is already `200`; the failure is logged with `event=export_failed`. A client must treat a truncated file as truncated, not as complete.
- Raw probes are retained for 72h, so a longer window returns what exists, not an error.

### 6.17 `GET /api/v1/events` — stable

Auth: bearer. Server-sent events. One `snapshot` on connect, then only changes.

```
event: snapshot
data: {"targets":[{"target":"chase.com","name":"Chase","state":"up","state_text":"Up",
                   "glyph":"●","class":"state-up","vantage":"local"}]}

event: state
data: {"target":"united.com","name":"United","state":"down","state_text":"Down",
       "glyph":"✕","class":"state-down","since":"2026-11-17T12:00:00Z","vantage":"local"}

event: incident
data: {"id":"inc-9f2c","verdict":"INCIDENT","scope":"sector","scope_value":"airline",
       "headline":"5 of 6 targets failing on sector=airline","confidence":"high",
       "opened_at":"2026-11-17T12:05:00Z"}

event: log
data: {"time":"12:05:03","level":"WARN","message":"self-check failed"}

: heartbeat
```

- Heartbeat comment every 15 seconds so proxies do not idle-close.
- The write deadline is cleared for this route only; normal requests keep the 30-second timeout.
- `Last-Event-ID` **is** implemented. Every event carries an `id` of the form `<epoch>:<sequence>`; a reconnecting client sends the last id it saw and receives only the events it missed, then continues live. A bounded ring (256 events) is the window: a client away longer than that, or reconnecting after a restart (the epoch changes), gets a fresh `snapshot` instead of a partial history.
- The `snapshot` sent on connect also carries an id, so a client that disconnects immediately after reading it can still resume.
- A client that cannot keep up with the live stream is disconnected rather than silently skipped; it reconnects and resumes from the ring, which still holds the events.
- The initial `snapshot` is an addition to the original delta-only sketch; it removes the connect-then-fetch race without sending a full page twice.
- `incident` events carry **open** incidents, re-sent whole when the incident log changes; a client upserts by `id`. A close is not pushed as a diff - refetch `/api/v1/incidents` to see closures. This is a stated limit, not an omission.
- `log` events carry `WARN` and above from the in-memory ring, with `ts`, `level`, `message`, and `attrs` (LOGGING.md §9). They are best-effort: a slow client drops log lines before it drops state, and the ring still holds them for `/api/v1/logs`. An `INFO` line is never pushed, because a stream of them would bury the deltas.
- The UI's `/events` stream emits the same `incident` event; the incident pages listen for it and reload, because a reload cannot show a stale attribution or close reason. The UI stream refetches on reconnect instead of resuming, which is why its `Last-Event-ID` is not offered.
- `503` when no board source is configured, rather than a connection that can only heartbeat.

For browser `EventSource`, which cannot set an `Authorization` header, use the UI's cookie-authenticated `/events`; an API client should use a `fetch`-based SSE reader instead.

### 6.18 `GET /api/v1/feeds` — stable

Auth: bearer. Feed health and recent external events. Feeds are opt-in (`NETWEATHER_FEEDS=ioda`, `ioda,radar`, `ioda,statuspage`, `aws-status`, `gcp-status`, or `azure-status`), because they are outbound calls to someone else's API. The Radar feed also needs `NETWEATHER_RADAR_TOKEN`; the statuspage feed needs `NETWEATHER_STATUSPAGE_URLS` (comma-separated). A feed without its configuration is skipped with a startup warning rather than polling unauthenticated.

| Parameter | Default | Notes |
|---|---|---|
| `limit` | 50 | 1–200, applies to events |

```json
{
  "data": {
    "feeds": [
      {"feed": "ioda", "last_ok_at": "2026-11-17T11:58:00Z", "consecutive_failures": 0,
       "next_poll_at": "2026-11-17T12:08:00Z", "stale": false},
      {"feed": "radar", "last_ok_at": "2026-11-17T09:58:00Z", "last_err": "503 from upstream",
       "consecutive_failures": 3, "stale": true}
    ],
    "events": [
      {"feed": "ioda", "external_id": "1001", "kind": "outage", "severity": 0.87,
       "country": "IR", "started_at": "2026-11-17T10:00:00Z", "observed_at": "2026-11-17T12:00:00Z"}
    ]
  },
  "meta": {"request_id": "…", "count": 1, "limit": 50}
}
```

- `stale` is true when the last success is older than 30 minutes, or when the feed has never succeeded. Staleness is computed on read, not stored, so a clock change cannot freeze it.
- `last_ok_at` is preserved across failures: "stale since 09:58" is the useful fact.
- `severity` is the feed's own score. It is evidence, not a verdict, and it never changes a target's state on its own.
- An unknown feed name in `NETWEATHER_FEEDS` is ignored rather than fatal; the list is operator input, not a security boundary.

### 6.19 `POST/GET /api/v1/debug/scopes`, `DELETE /api/v1/debug/scopes/{id}` — experimental

Auth: bearer. Time-boxed verbose logging for one target, component, vantage, or error class. Global `trace` is deliberately not expressible.

`POST` body:

```json
{"target": "chase.com", "component": "agent", "error_class": "tls_timeout",
 "level": "trace", "ttl": "10m"}
```

- At least one of `target`, `vantage`, `component`, or `error_class` is required.
- `level` is `debug` (default) or `trace`. A `debug` scope admits `DEBUG` and more severe records; a `trace` scope additionally admits `TRACE`.
- `ttl` is required and capped at 1 hour. There is no "forever".
- The response is `201` with the created scope, including `id`, `created_at`, and `expires_at`.

`GET` lists active scopes (newest first). `DELETE /{id}` ends one early and returns `204`; an unknown id is `404`.

Scopes persist in `/data/debug.json`, so a restart does not silently keep one enabled; scopes that expired while the process was down are dropped at startup with one `INFO` line each. Expiry is logged as `debug scope expired`. Enable/disable actions are logged at `INFO` with the request id.

The scope handler is the only level decision: sinks are built at `TRACE`, so enabling a scope works even when `NETWEATHER_LOG_LEVEL=info`. Records matching an active scope enter the ring and the file sink; the ring is bounded (2,000 records / 4 MB) and evicts oldest first, so the durable evidence is the file sink rather than a pin in memory.

### 6.20 `GET /api/v1/coverage/history` — stable

Auth: bearer. Daily coverage snapshots for one scope, oldest first.

| Parameter | Default | Notes |
|---|---|---|
| `scope` | `global` | `global`, `sector`, `country`, `asn` |
| `value` | — | Required for non-global scopes |
| `days` | 30 | 1–365 |

```json
{
  "data": [
    {"bucket": "2026-11-16", "scope": "global", "total": 100, "measurable": 90,
     "blocked": 5, "unmeasurable": 5, "coverage": 0.9}
  ],
  "meta": {"request_id": "…", "count": 1, "limit": 30}
}
```

- One point per UTC day. Snapshots are written by maintenance, once per day, and re-running within a day updates the row rather than duplicating it.
- `coverage` is `measurable / total`; `total` includes permanently unmeasurable targets, which `measurable` does not.
- Oldest first so a client can draw a line without reversing it.
- A scope with no snapshots returns `200` and an empty list; history starts on the first day the process ran.

### 6.21 `POST /api/v1/targets/{id}/trace` — experimental

Auth: bearer. One on-demand IPv4 ICMP traceroute. The result is a hint about where a path bends; it never changes a target's state.

```json
{
  "data": {
    "target": "Chase",
    "host": "www.chase.com",
    "destination": "203.0.113.10",
    "reached": true,
    "hops": [
      {"ttl": 1, "addr": "10.0.0.1", "rtt_ms": 4},
      {"ttl": 2, "timeout": true},
      {"ttl": 3, "addr": "203.0.113.10", "rtt_ms": 11}
    ],
    "note": "hop 1 is a private address, likely the local gateway rather than a wide-area hop"
  },
  "meta": {"request_id": "…"}
}
```

- `POST` only. A trace is an action, not a read.
- Global budget: **10 runs per hour**, shared by every client, because a target's abuse desk sees the operator's egress address rather than a user. Exhaustion is `429` with `Retry-After`.
- `timeout: true` marks a hop that never answered. Routers rate-limit TTL-exceeded, so a star is not loss. Three consecutive silent hops end the walk early.
- `note` states the container caveats from `NETWORKING.md` §8 when they apply: a private first hop is likely the local gateway rather than a wide-area hop, and a walk that stops after that hop is a NAT/conntrack artifact rather than a path failure. It is absent when neither applies.
- The destination not answering is `200` with `reached: false`, not an error.
- Traceroute needs `CAP_NET_RAW`. Without it the endpoint is `503` with the socket error in the message, and the rest of the monitor is unaffected (`compose.trace.yaml` grants the capability). The dashboard's target drawer offers the same action through a CSRF-checked form and its own result page; both surfaces spend from one ten-per-hour governor, so the ceiling above is per instance, not per surface.
- The capability is not consent: the endpoint is also `503` unless `NETWEATHER_TRACE_ENABLED=true`, with that variable named in the refusal. The documented override sets both, so enabling raw sockets is a visible second file rather than a line someone uncomments.
- Every run logs `event=traceroute` with the request id, destination, hop count, and whether the destination answered.

### 6.22 `GET /api/v1/corridors` - stable

Auth: bearer. The configured corridors (DESIGN.md §9) with each one's newest sample and any open step change.

```json
{
  "data": [
    {
      "id": "nyc-lon",
      "from": "NYC",
      "to": "LON",
      "latest": {"ts": "2026-11-17T11:00:00Z", "rtt_ms": 83, "loss": 0, "ok": true},
      "open_event": {"kind": "rtt_step", "started_at": "2026-11-17T10:00:00Z",
                     "detail": "rtt 190ms for two samples against a 82ms baseline (±3ms)",
                     "trace": " 1  192.0.2.1      4ms\ndestination 198.51.100.9 reached=true\n"}
    },
    {"id": "lon-sin", "from": "LON", "to": "SIN"}
  ],
  "meta": {"request_id": "…", "count": 2}
}
```

- `latest` is absent until the first five-minute sample lands. `open_event` is absent while the corridor is normal.
- `trace` is the one traceroute captured when the event opened: the `corridor step change` trigger from DESIGN.md §9.1, captured once rather than scheduled. It is absent when the walk could not start (no `CAP_NET_RAW`, an unresolvable endpoint) or when the instance has no tracer; the step change is still the fact and the event still opens.
- `ok: false` means the last sample got no reply from either endpoint; it is not a verdict about a target.
- This is evidence about a path. It never changes a target's state and never opens a target incident.
- `503` when no corridor reader is configured, rather than an empty list that reads as "all normal". A configured instance with no corridors returns `200` and an empty list.

### 6.23 `GET /api/v1/ihi` - stable

Auth: bearer. The Internet Health Index (`DESIGN.md` §11): one 0-100 number with every component that produced it, and every component that was left out and why.

```json
{
  "data": {
    "score": 82.4,
    "parts": [
      {"name": "Own health", "score": 90, "weight": 0.7, "at": "2026-11-17T12:00:00Z"},
      {"name": "External outage severity", "score": 60, "weight": 0.3, "at": "2026-11-17T12:00:00Z"}
    ],
    "dropped": [
      {"name": "BGP stability", "reason": "stale"},
      {"name": "Corridor stability", "reason": "no corridors configured"}
    ]
  },
  "meta": {"request_id": "…"}
}
```

- `weight` is the effective share after coverage weighting and renormalisation, so `parts[].weight` sums to 1 whenever `parts` is non-empty.
- `score: 0` with an empty `parts` means "nothing could be scored", not "the internet is down". A client must check `parts` before rendering the number as health.
- A dropped component is always named with its reason: `stale`, `not available`, `no weight`, `no timestamp`, `no score`, `coverage below 50%`, `no corridors configured`, `no judgeable targets`, or `unavailable: <error>`.
- `503` when the index is not configured. The index changes nothing: it is a summary, never a detector.

---

## 7. Implementation status

This section used to list promises. Every read endpoint, both event streams, and every action endpoint in this document is now implemented, so it is kept as a status record rather than a gap list; the subsection numbers are stable because other documents cross-reference them.

### 7.1 Read surface

Nothing remains on the read surface. Every read endpoint in this document is implemented; the section is kept so cross-references do not silently break.

### 7.2 Live updates

`GET /api/v1/events` is **implemented**; the contract is in 6.17. The UI path `/events` uses the same event shape with a session cookie instead of a bearer token.

| Path | Auth | Audience | Status |
|---|---|---|---|
| `/events` | session cookie | the browser UI | **implemented** |
| `/api/v1/events` | bearer | API clients | **implemented** |

One limitation is stated rather than hidden: a closed incident is not pushed as a diff. The API stream resumes from a bounded `Last-Event-ID` ring (§6.17); the UI stream refetches on reconnect instead. Both streams emit `incident` events; the UI reloads the incident pages, and an API client upserts open incidents by id or refetches for closures.

Concurrent streams are capped at 64 total and 8 per peer address (`THREAT_MODEL.md` T10). Exceeding either returns `429` with `Retry-After: 60`. A per-peer cap uses `X-Forwarded-For` only when the immediate peer is inside `NETWEATHER_TRUSTED_PROXIES` and the walk finds an untrusted address (`NETWORKING.md` §7.3); otherwise it uses the peer, because an untrusted header would let one client multiply its stream budget.

### 7.3 Actions (POST)

All action endpoints are **implemented**: the debug scopes in §6.19, the traceroute in §6.21, and the incident annotation in §6.8.1.

`Idempotency-Key` is honored on every mutating request: a repeated key with the same method and path replays the original response (status, headers, body) with `Idempotent-Replayed: true`, instead of acting twice. This matters for `pause` behind a retrying proxy and for a client that retries a scope creation after a timeout.

- Scoped by `METHOD path key`, so one key reused across different actions is not a cross-action cache.
- Optional: a request without a key acts normally. The header is a retry contract, not a second authentication factor.
- Cached outcomes are `< 500` only. A 5xx is transient and a retry may succeed, so it is never replayed.
- The window is 24 hours, in memory, capped at 4096 entries. A restart forgets keys; the docs say so rather than implying durability.
- Keys over 128 characters are `400`. `Content-Type: application/json` is required by the action handlers that take a body.

### 7.4 `/metrics`

**Implemented.** Prometheus text exposition lives at `/metrics`, **not** under `/api/v1`:

- It is a different contract with its own versioning conventions.
- It is scraped by machines that do not speak JSON.
- It carries aggregates only — `netweather_targets{state="down"}` and `netweather_incidents_open` — never per-target labels, or cardinality explodes at 750 targets.

Off by default: set `NETWEATHER_METRICS=true` to mount it. When a UI token is configured the route requires the same bearer token, because the aggregates still describe the operator's fleet.

---

## 8. Cross-cutting behaviour

| Concern | Behaviour |
|---|---|
| Request ID | `X-Request-Id` honoured if 1–64 `[A-Za-z0-9_-]`; otherwise generated. Always echoed and always present in logs |
| Security headers | `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, `X-Robots-Tag: noindex, nofollow`, `Cache-Control: no-store` |
| Compression | gzip for JSON over 1 KB when the client offers it; SSE and CSV are never compressed, and small bodies stay identity |
| CORS | None. Same-origin only. No `Access-Control-Allow-*` headers are emitted |
| Rate limit | 60 requests/minute per token, burst 20; `429` with `Retry-After` |
| Body limit | 8 KiB for JSON actions (`/api/v1/debug/scopes`); unknown fields are rejected |
| Timeouts | Header 5 s, read 15 s, write 30 s (SSE excepted), idle 60 s |
| Panics | Recovered, logged with stack, returned as `500 internal` with the request ID |
| Method handling | Wrong method on a known path → JSON `405` + `Allow` |

---

## 9. CLI as an API

The binary is also scriptable, and its contract matters as much as HTTP:

| Command | Exit code | Output |
|---|---|---|
| `netweather version` | 0 | one line |
| `netweather healthcheck` | 0 healthy, 1 unhealthy | silent |
| `netweather serve` | 0 clean shutdown, 1 startup or runtime failure | logs to stdout |
| `netweather seed [--country-anchors] [--diff]` | 0 applied, 1 rejected | summary; `--country-anchors` fetches the IANA root zone and adds one DNS target per ccTLD; `--diff` prints additions and changes and writes no targets |
| `netweather export` | 0 success, 1 error | data to stdout, logs to stderr |
| `netweather disable <host-or-id>` | 0 success, 1 error | one line, or an envelope with `-json` |
| `netweather debug enable\|status\|disable` | 0 success, 1 error | one line per scope |
| `netweather debug bundle -o FILE` | 0 success, 1 error | one summary line, or an envelope with `-json` |
| `netweather doctor` | 0 all fatal checks pass, 1 a fatal check failed | capability table |
| `netweather backup -o FILE` | 0 success, 1 error | one line with the destination and size; refuses an existing file and writes the copy 0600 |

Commands that exist in the binary today: `serve`, `version`, `healthcheck`, `seed`, `export`, `debug`, `disable`, `doctor`, `backup`. Pause/resume remain HTTP-only (`POST /api/v1/pause`, `POST /api/v1/resume`).

`netweather doctor` reports which instruments work on this host: data-directory writability, resolver, TCP egress, TLS, HTTP, NTP offset, and whether raw ICMP/traceroute sockets can be opened. Missing optional capabilities are a warning, not a startup failure (`DESIGN.md` §5.5).

Rules:

- **stdout is data, stderr is diagnostics.** A pipeline must never receive a log line.
- `--json` is available on `version`, `healthcheck`, `doctor`, and `debug status`. It emits the same envelope as the HTTP API (`data` on success, `error` on failure), so a script can parse either surface with one decoder. A failed command still returns a non-zero exit code and still writes its human line to stderr.
- Exit codes are stable and documented; unknown command is `2`.

---

## 10. OpenAPI and contract tests

- **Deliberately not shipped: `docs/openapi.yaml`.** The earlier plan was to generate it from the handlers, but Go's `net/http` mux has no handler introspection, and two of these routes are subtree handlers (`/api/v1/targets/`, `/api/v1/incidents/`) whose real paths live inside the handler. A source-parsing generator would therefore emit a partial spec, and a partial spec is worse than none: a client would trust the absence of a path. Shipping a hand-written file instead is exactly the rot this document warns about. The contract is this document plus `GET /api/v1/meta` for runtime feature detection, and the shapes below are pinned by tests rather than by a generated file.
- This document is the contract, and the implemented slice is covered by `internal/api/api_test.go`:
  - envelope shape on success and failure
  - `code` values and status mapping
  - auth required on `/api/*`, absent on `/healthz`
  - `healthz` contains no target data
  - `logs` filtering by level, target, `since`, and `limit`
  - `405` carries `Allow` and a JSON body
  - request ID echoed and logged
  - non-loopback bind without a token refuses to start
- A JSON Schema per response type lives beside the handlers once there is more than one consumer.

---

## 11. Non-goals

- **No GraphQL.** The queries are fixed and few; a schema layer would be cost without a payoff.
- **No write API for targets.** Targets are config-as-code (`DESIGN.md` §13); the API reads them. Editing via HTTP would fork the source of truth.
- **No webhooks.** Notifications are explicitly out of scope; polling `/api/v1/incidents?open=true` is the supported integration.
- **No CORS and no browser tokens.** The UI is same-origin; third-party browser apps are not a use case.
- **No public/anonymous mode.** Every data endpoint requires the token. A read-only public board was considered and rejected: the board *is* the disclosure.
