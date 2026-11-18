# Logging and diagnostics

> The board tells you *what* is happening. The logs must tell you *why*, for one target, at one instant, without a debugger and without a shell — the release image has neither.

Companions: [`DESIGN.md`](DESIGN.md) §7.5 (self-health), [`THREAT_MODEL.md`](THREAT_MODEL.md) §5.7 (what must never be logged), [`QUALITY.md`](QUALITY.md) §5.1 (unit tests).

---

## 1. What logging is for here

Three questions, in order of frequency:

1. **"Why did this target flip?"** — one line, with the stage that failed and the classifier's reason.
2. **"Is the tool itself broken?"** — scheduler stalled, clock skewed, log sink dropping, feed stale.
3. **"What exactly did we send to this host?"** — the abuse-complaint question, and the security-review question.

Logging is **not** the data path. Probe outcomes live in SQLite (`SCHEMA.md`); the dashboard reads the database, never the log. If the two disagree, the database is right.

Corollary: log lines may be sampled, dropped under pressure, or rotated away. **State must never depend on a log line.**

---

## 2. Levels

| Level | Value | Contents | Default |
|---|---|---|---|
| `TRACE` | −8 | Per-stage wire detail: DNS answer, TCP timings, TLS issuer, HTTP status + which signature matched | off |
| `DEBUG` | −4 | One line per probe: outcome, stage, latency, confidence | off |
| `INFO` | 0 | Lifecycle, **state transitions**, incidents opened/closed, feed polls, config reloads | **on** |
| `WARN` | 4 | Degraded but running: feed stale, target blocked, clock skew, log drops | on |
| `ERROR` | 8 | Invariant broken, data at risk, cannot continue | on |

Levels are inclusive: `debug` also emits `info` and above.

`TRACE` is a custom level rather than a `debug` sub-flag because it is *scoped*, not merely louder (§5). Rendering it as `DEBUG-4` (slog's default) would hide that distinction, so the level name is rewritten to `TRACE`.

### Volume at 750 targets

| Level | Lines/day | Approx. size | Verdict |
|---|---|---|---|
| `info` | < 5,000 | ~2 MB | the normal operating point |
| `debug` | ~430,000 | ~95 MB | temporary; enable, reproduce, disable |
| `trace` (global) | ~430,000 × 5–10 stages | 0.5–1 GB | **not a sane steady state** |
| `trace` (one target) | ~1,500 | ~1 MB | the intended use |

Flipping to `trace` globally logs a startup warning with that estimate. It is allowed for a bounded session and discouraged by the numbers, not by a prohibition the operator will route around.

---

## 3. The decision record

Every log line is structured (JSON in a container, text on a TTY). The fields that matter are canonical, so `jq` queries and future dashboards do not guess:

| Field | When | Meaning |
|---|---|---|
| `ts` | always | UTC, RFC 3339 with milliseconds, sortable |
| `level` | always | `TRACE`/`DEBUG`/`INFO`/`WARN`/`ERROR` |
| `msg` | always | Short, stable, lower-case. Not a sentence with interpolated values |
| `event` | always | Machine key: `startup`, `state_change`, `incident_open`, `probe`, `feed_poll`, `self_check`, `config_reload`, `log_dropped` |
| `component` | always | `scheduler`, `classify`, `netx`, `store`, `http`, `feed`, `selfhealth`, `logging` |
| `trace_id` | probe / request | 16 hex chars; joins a log line to a stored probe row and an HTTP request |
| `target` | target-scoped | Registrable host |
| `vantage` | target-scoped | `local`, `tor-de`, … |
| `tier` | probe | 0–3 |
| `attempt` | probe | 1-based attempt within the debounce window |
| `stage` | probe | `dns`, `doh`, `tcp`, `tls`, `http`, `icmp`, `ntp` |
| `state_from` / `state_to` | transition | The six states plus `CONFIG_ERROR` / `UNMEASURABLE` |
| `error_class` | failure | Classifier error class (`tcp_refused`, `tls_expired`, `dns_nxdomain`, …) |
| `http_status` | HTTP | Numeric status actually received |
| `latency_ms` | probe | Integer milliseconds, stage or total as labelled |
| `confidence` | probe | `semantic`, `reachability`, `low` |
| `reason` | transition | **The same string stored in `state_change.reason`** |
| `incident_id` | incident | ULID |
| `suppressed` | rollup | How many identical lines the limiter suppressed |

The `reason` invariant is load-bearing: the drawer and the log must not tell two different stories about the same transition.

Example, one line, no shell required:

```json
{"ts":"2026-11-16T03:14:15.123Z","level":"INFO","msg":"state change","event":"state_change","component":"classify",
 "trace_id":"9f2c1a7b4e8d0356","target":"chase.com","vantage":"local","tier":2,
 "state_from":"UP","state_to":"DOWN","stage":"tls","error_class":"tls_expired",
 "latency_ms":41,"confidence":"semantic","reason":"certificate expired 2h ago"}
```

---

## 4. What each layer logs

| Layer | Level | One line per | Must include |
|---|---|---|---|
| Scheduler | `DEBUG` | probe submission | target, tier, vantage, due_at, trace_id |
| Instrument | `TRACE` | stage | stage, duration, outcome, and no body |
| Classifier | `INFO` on transition, `DEBUG` otherwise | probe | state, stage, error_class, confidence, reason |
| Attribution | `INFO` | incident | winner, score, share, runner-up |
| Store | `WARN` | slow query (> 100 ms) or write retry | query shape, duration |
| Feeds | `INFO` on poll, `WARN` when stale | poll | feed, items, age, duration |
| Self-health | `WARN` on failure only | check | check, value, threshold |
| HTTP | `DEBUG` per request, `WARN` on 4xx/5xx | request | method, route (not full URL), status, duration, request_id |
| Startup | `INFO` | twice | Before wiring: version, commit, and the log sinks and level. At listen: config source, listen address, auth mode, vantages, target count, schema version, database path |

No layer logs the same failure twice. Wrap with `%w`, return, log once at the boundary that decides something.

---

## 5. Scoping verbose output

Global `debug` is acceptable for a minute. Global `trace` is not. The intended workflow:

```
netweather debug enable --target chase.com --level trace --for 10m
netweather debug enable --target chase.com --level trace --until 2026-11-16T04:00:00Z
netweather debug status
netweather debug disable --all
```

Semantics (**implemented** for the HTTP surface):

- Scope by `target`, `vantage`, `component`, or `error_class`. A scope must name at least one; "trace everything" is deliberately not expressible.
- **Time-boxed by construction.** There is no "forever"; the maximum is 1 hour, and the state is persisted in `/data/debug.json` so a restart does not silently keep it on.
- A scoped session admits its records to the ring and the file sink. The ring is bounded (2,000 records / 4 MB) and evicts oldest first, so it is not a pin: the durable evidence for a session is the file sink, and the ring is what the UI can show right now.
- Expiry logs one `INFO` line: `debug scope expired`.

The HTTP contract is in `API.md` §6.19 (`POST/GET /api/v1/debug/scopes`, `DELETE /api/v1/debug/scopes/{id}`), and the CLI verbs above are implemented: `netweather debug enable|status|disable` talk to that API with `NETWEATHER_UI_TOKEN` (or `_FILE`) and `NETWEATHER_API_URL` (default `http://127.0.0.1:2600`).

Level semantics: a `debug` scope admits `DEBUG` and more severe records matching its selectors; a `trace` scope additionally admits `TRACE`. Sinks are always built at `TRACE` and the scope handler is the only level decision, so enabling a scope works even when `NETWEATHER_LOG_LEVEL=info`.

---

## 6. Redaction

Applied centrally in the handler's `ReplaceAttr`, so a call site cannot forget it. The log file is as sensitive as the database: it contains the target list and the egress identity.

| Rule | Example |
|---|---|
| Key names a credential → `[REDACTED]` | `token`, `authorization`, `cookie`, `set-cookie`, `api_key`, `client_secret`, `password`, `ts_authkey` |
| Key contains a credential fragment | `...token`, `...secret`, `...password`, `...apikey`, `...credential` |
| URL values: strip userinfo, redact secret query params | `https://u:p@x/y?token=abc&since=5` → `https://[REDACTED]@x/y?token=[REDACTED]&since=5` |
| Free text (`error`, `detail`, `reason`): scrub embedded URLs | yes, same regexes |
| Bodies | **never read into a log, at any level** |

Deliberate non-rules: `key` alone is not a secret fragment (it would redact `monkey`), and a value is not scanned for entropy — heuristic secret detectors produce false positives that train people to ignore warnings.

Tests: `internal/logging/redact_test.go` (key table, innocuous keys, URL userinfo, query params, nil).

---

## 7. Sinks

| Sink | Default | Notes |
|---|---|---|
| **stdout** | on | 12-factor. Format `auto`: text on a TTY, JSON otherwise — a container's stdout is read by a machine |
| **file** | off | `/data/netweather.log`, JSON, 0600, size-rotated (50 MB × 3), directory 0700 |
| **ring** | on | Last 2,000 records / 4 MB in memory, redacted, for the UI and `/api/v1/logs` |
| **database** | never | Writing logs to SQLite turns a debugging aid into write amplification and a retention problem |

The file sink exists for operators who want evidence after the container is gone. It is off by default because the container's log driver usually already keeps it.

Rotated names are `netweather.log.1` … `.N`; the active file is always `netweather.log`.

Configuration:

| Variable | Default | Meaning |
|---|---|---|
| `NETWEATHER_LOG_LEVEL` | `info` | `trace`, `debug`, `info`, `warn`, `error` |
| `NETWEATHER_LOG_FORMAT` | `auto` | `auto`, `text`, `json` |
| `NETWEATHER_LOG_FILE` | — | Empty disables the file sink |
| `NETWEATHER_LOG_MAX_MB` | `50` | Size before rotation |
| `NETWEATHER_LOG_KEEP` | `3` | Rotated files retained |
| `NETWEATHER_LOG_RING` | `2000` | Ring records; `0` disables |
| `NETWEATHER_LOG_SOURCE` | unset | `1` adds `source=file.go:123` (costs ~5% throughput) |

---

## 8. Backpressure

Log writes go through a 4,096-line asynchronous buffer.

- **Writes never block.** If the buffer is full, the line is dropped and counted. A slow stdout — a paused `docker logs`, a full disk, a remote logging driver — must not stall the scheduler, because a stalled scheduler produces missed probes, which the rest of the system correctly reports as an outage. Logging must not be able to cause a fake incident.
- **Drops are visible.** `Dropped()` and `WriteErrors()` are cumulative counters on the logger. Every maintenance tick compares them with the previous tick and emits one `event=log_dropped` with the deltas and the running totals, never one line per dropped record: a failing sink must not multiply the problem it is reporting. The same counters appear on `/diagnostics`.
- **Shutdown flushes.** `Close()` drains the buffer before closing the file, in that order.

This is the one place where losing data is the right answer.

---

## 9. In-app diagnostics

Because the release image has no shell, the ring is the only practical way to see logs on a remote instance.

| Surface | Status | Behaviour |
|---|---|---|
| `/api/v1/logs?level=&target=&since=&limit=` | **implemented** | Reads the ring; bearer auth required; redacted by construction. Contract in `API.md` §6.3 |
| Drawer → "Why?" tab | **implemented** | Last 50 ring lines that name the target, beside the stored `state_change.reason` rows |
| Footer → "diagnostics" | **implemented** | The board footer links to `/diagnostics`, which shows ring size, drops, write errors, sink path, level, format, and the scopes still in force |
| SSE `log` events | **implemented** | Only for records at `WARN`+ by default; best-effort, so a slow reader drops lines before state. The board shows the newest three as a quiet `role="status"` notice and lets each expire; the full ring is on `/diagnostics` |

The ring is not a log shipper. It holds minutes, not days.

---

## 10. Debug bundle

A bug report about a monitor needs the monitor's own state, not a screenshot.

```
netweather debug bundle -o netweather-bundle.tar.gz
```

Contents (implemented):

- `version.txt` — build identity, Go version, OS/arch
- `config.redacted.yaml` — the effective config with anything key-shaped removed
- `meta.json` — the `/api/v1/meta` answer, including the applied `schema_version`
- `ring.jsonl` — up to the last 2,000 redacted records, every level
- `selfcheck.json` — the latest clock, egress IP/ASN, resolver, MTU, port 53, and disk rows
- `probes.jsonl` — the last 20 probes for each target currently `DOWN`, `DEGRADED`, `BLOCKED`, or `UNKNOWN`, each row tagged with its target
- `MANIFEST.txt` — every file with its SHA-256 and size

The command reads the public API with the same bearer token the other CLI verbs use, so it works from a laptop against a remote instance and needs no privileged local path. A failed optional read is recorded in the file as a stated error rather than aborting the bundle.

Not included: a separate `schema.txt` with the migration list, because a remote CLI has no database access and inventing one would mean a second data path. The schema **version** is in `meta.json`; the migration history is in the repository at that version. Never included either: the token file, feed API keys, response bodies, the raw database.

---

## 11. Never logged

- Response bodies, at any level, ever
- `Authorization`, `Cookie`, `Set-Cookie`, API keys, Tor control credentials
- The UI token, its hash, or the token file path's contents
- Full URLs with query strings that may carry a credential (use `SafeURL`)
- Other people's personal data encountered incidentally in a TLS certificate or status page
- The contents of `secrets/`

This list is enforced by tests where it can be, and by review where it cannot.

---

## 12. Testing and budget

| Test | Asserts |
|---|---|
| `TestLoggerWritesValidJSON` | one object per line, RFC 3339 time, canonical level name |
| `TestLoggerRedactsSecretsEndToEnd` | token, userinfo, and query credentials never reach the sink |
| `TestLoggerLevelFiltering` | `debug` is silent at `info` |
| `TestLoggerTraceLevel` | the custom level renders as `TRACE` |
| `TestLoggerFeedsRing` | the ring sees what stdout sees |
| `TestLoggerFileSink` | rotation sink receives the record |
| `TestAsyncWriterDropsInsteadOfBlocking` | a stuck sink drops and counts, never blocks |
| `TestAsyncWriterDeliversInOrder` | ordering survives the queue |
| `TestRotatingFileRotatesAndKeeps` | size rotation and retention count |
| `TestRotatingFilePermissions` | 0600 on the release platform |
| `TestRingKeepsNewestRecords` / `…ByteCap` / `…ConcurrentHandlers` | bounded memory, eviction, race-safety |
| `TestRingGroupsAndRedaction` | slog group semantics and per-sink redaction |
| `TestLimiter*` | dedupe window with an injected clock |
| `TestFromEnv*` | configuration parsing and rejection |
| `BenchmarkLogger` / `BenchmarkLoggerDisabledLevel` | enabled-line cost and the disable fast path |

Budget: a disabled level costs one `Enabled` check; an enabled `info` line costs < 5 µs including the async copy. `TRACE` is allowed to be 10× that because it is scoped.

Flagged level cost is measured by `BenchmarkLogger` (`internal/logging/bench_test.go`), which now has a real hot-path caller: the agent logs one line per probe. On the reference machine an enabled `INFO` line with five attributes costs about 4.1 µs and 5 allocations through the JSON sink, ring, and async console hand-off; a disabled level costs about 78 ns and zero allocations. The numbers are a machine-specific sanity check, not a contract, and `BenchmarkLoggerDisabledLevel` is what keeps the "one `Enabled` check" claim honest.

---

## 13. Known limits

- **Windows ignores POSIX modes.** The 0600 assertion skips there; the release platform is Linux.
- **`go test -race` needs a C compiler.** On Windows without cgo, run plain `go test`; CI runs `-race` on Linux.
- **The ring is per process.** Reading logs from a second replica shows that replica's ring, not a merged view.
- **Clock skew is the operator's problem until self-health catches it.** Timestamps are UTC and monotonic ordering is per-process; a skewed clock makes correlation wrong before it makes logs wrong.
- **No remote shipping.** Syslog/Loki/OTLP are deliberately out of scope; stdout inside the container is the contract.
