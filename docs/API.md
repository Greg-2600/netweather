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
| `GET /api/v1/control`, `POST /api/v1/pause`, `POST /api/v1/resume` | **implemented** - the probe kill switch |
| `GET /api/v1/targets/{id}/probes` | **implemented** - recent raw observations for one target |
| `GET /api/v1/incidents`, `GET /api/v1/incidents/{id}` | **implemented** - the incident log and ranked attribution |
| `GET /metrics` | **implemented** - aggregate Prometheus text; off unless `NETWEATHER_METRICS=true`; bearer auth when a token is set |
| `GET /events` | **implemented** - the UI's SSE stream; session cookie auth, not bearer. The API-facing counterpart is specified in §7.2 |
| Everything else in §7 | **specified, not implemented** |
| `/metrics` (`DESIGN.md` §6.4) | **specified, not implemented** — see §7.4 for why it is separate from `/api/v1` |

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
- `GET /api/v1/logs` is the exception: it returns the newest N after filtering and does not paginate. Cursors apply to the list endpoints in §7.1 when they land.

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
    "capabilities": ["health", "meta", "logs", "board", "targets", "control", "probes", "incidents"],
    "ring": {"records": 128, "capacity": 2000}
  },
  "meta": {"request_id": "9f2c1a7b4e8d0356"}
}
```

`capabilities` is the runtime truth: a client should feature-detect here rather than assume an endpoint exists. `auth` is `bearer` or `none` (loopback development only).

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

Sorted oldest → newest. The ring holds minutes, not days; for history use `GET /api/v1/targets/{id}/probes` once it exists.

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
      "since": "2026-11-16T03:14:15Z"
    }
  ],
  "meta": {"request_id": "…", "count": 1}
}
```

- `state` is one of the eight target states, lower-cased (`DESIGN.md` §3.2).
- `since` is the start of the current state. It is absent for a target that has never been probed, which is different from "up since the epoch".
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
- Raw probes are retained for 72h (`SCHEMA.md` §11); for longer ranges use `/series` when it lands.
- `since` is not a parameter here: this endpoint is "the last N", not a range scan.

### 6.7 `GET /api/v1/incidents` — stable

Auth: bearer. Newest first.

| Parameter | Default | Notes |
|---|---|---|
| `open` | `false` | `true` returns only unclosed incidents |
| `limit` | 50 | 1–200 |

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
  "meta": {"request_id": "…", "count": 1, "limit": 50}
}
```

- An empty list means "no incidents", not "cannot answer"; the latter is `503`.
- `closed_at` is absent while the incident is open.

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
    "targets": ["svc-united", "svc-delta"]
  },
  "meta": {"request_id": "…"}
}
```

`404` for an unknown id. Attribution is stored at open time and is not recomputed on read, so the incident keeps the evidence that produced it.

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

---

## 7. Specified, not implemented

Each of these is a promise about shape, not a claim that it works. `GET /api/v1/meta` will list them in `capabilities` only when they do.

### 7.1 Read surface

| Endpoint | Tier | Purpose |
|---|---|---|
| `GET /api/v1/targets/{id}` | stable | One target with its current state and measurement metadata |
| `GET /api/v1/targets/{id}/series` | stable | Rollups; `step=5m\|1h\|1d` |
| `GET /api/v1/scopes/{scope}` | stable | Health, coverage, and baseline for `global`, `sector`, `country`, `asn`; `?value=` selects |
| `GET /api/v1/anomalies` | stable | Detector output, advisory included |
| `GET /api/v1/coverage` | stable | The blind-spot panel: ASN/country spread, measurable share, stale anchors |
| `GET /api/v1/feeds` | stable | Feed state: last success, staleness, consecutive failures |
| `GET /api/v1/selfcheck` | internal | Self-health results (clock, egress, MTU, port 53, disk) |
| `GET /api/v1/export` | stable | Streaming CSV/JSON over a time range; never loads the set into memory |

### 7.2 Live updates

`GET /api/v1/events` - SSE, `stable`. **Specified, not implemented.**

There are two paths for one stream:

| Path | Auth | Audience | Status |
|---|---|---|---|
| `/events` | session cookie | the browser UI | **implemented** |
| `/api/v1/events` | bearer, or session cookie | API clients | specified |

`EventSource` cannot set an `Authorization` header, which is why the UI path uses the cookie. An API client that wants bearer auth uses a `fetch`-based SSE reader rather than `EventSource`. Both paths emit exactly the schema below; there is one handler, not two.

```
event: state
data: {"target":"chase.com","vantage":"local","state":"DOWN","since":"…","trace_id":"…"}

event: incident
data: {"id":"01J…","verdict":"INCIDENT","headline":"…"}

: heartbeat
```

- Deltas only, never a full page.
- Heartbeat comment every 15 s so proxies do not idle-close.
- `Last-Event-ID` resumes within the ring; beyond that the client refetches the board.
- Connection cap and per-IP limit (`THREAT_MODEL.md` T10).
- `WriteTimeout` is extended per-connection for this route only.

### 7.3 Actions (POST)

| Endpoint | Tier | Purpose |
|---|---|---|
| `POST /api/v1/debug/scopes` | experimental | Enable scoped `debug`/`trace` for a target, with a mandatory `ttl` ≤ 1h |
| `DELETE /api/v1/debug/scopes/{id}` | experimental | End a scope early |
| `POST /api/v1/targets/{id}/trace` | experimental | On-demand traceroute; subject to the ≤10/hour global limit |

Actions require `Content-Type: application/json` and an `Idempotency-Key` header. A repeated key returns the original result rather than acting twice — important for `pause` behind a retrying proxy.

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
| Compression | gzip/br for JSON over 1 KB; SSE never compressed. Specified: not implemented yet |
| CORS | None. Same-origin only. No `Access-Control-Allow-*` headers are emitted |
| Rate limit | 60 requests/minute per token, burst 20; `429` with `Retry-After` |
| Body limit | 64 KB for JSON actions; `413` beyond. Applies when the first POST route lands |
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
| `netweather seed` | 0 applied, 1 rejected | summary |
| `netweather export` | 0 success, 1 error | data to stdout, logs to stderr |

Commands that exist in the binary today: `serve`, `version`, `healthcheck`, `seed`, `export`. `debug` and the `pause` verb are specified; pause/resume are available over HTTP.

Rules:

- **stdout is data, stderr is diagnostics.** A pipeline must never receive a log line.
- `--json` is available on read commands and emits the same envelope as the HTTP API, so a script can use either.
- Exit codes are stable and documented; unknown command is `2`.

---

## 10. OpenAPI and contract tests

- `docs/openapi.yaml` will be **generated from the handlers**, not hand-written, when the endpoint set stabilises around phase 1. A hand-written spec for unimplemented endpoints rots immediately.
- Until then this document is the contract, and the implemented slice is covered by `internal/api/api_test.go`:
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
