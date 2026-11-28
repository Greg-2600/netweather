# Roadmap

A living plan, not a promise. The **Status** column is the truth; anything marked
*Not planned* has a stated reason rather than a silent absence. `DESIGN.md` §15
keeps the phase record, `CHANGELOG.md` records what shipped, and this file says
what is next and how it will be judged done.

Legend: **Shipped** · **Now** (release hardening) · **Next** (phase 3) ·
**Later** · **Not planned**

---

## 1. Where we are

**MVP is shipped.** Phases 0-3 are complete (OIDC deferred to Later).

Verified on the current project tree:

| Gate | Command | Evidence |
|---|---|---|
| Format, vet, lint, unit + integration, coverage gates, build | `go run ./internal/devtool verify` | `verify: ok`, library total 87.4–87.5% |
| Real image + Docker network plane | `go run ./internal/devtool compose` | `ok … 110.7s` |
| Browser UI | `go test -tags=uitest ./internal/web/uitest` | `ok 7.9s` |
| Real internet, real instruments | `NETWEATHER_E2E=1 go test -tags=e2e ./internal/e2e -run TestRealEndpoints` | PASS 1.58s, 6/6 endpoints |
| Reproducible build | `go run ./internal/devtool repro` | `reproducible: a57b5fbd867af8ae` |
| Cross-platform release | `go run ./internal/devtool snapshot` | 5 binaries + `SHA256SUMS` |

Shipped: eight instruments plus on-demand traceroute, tiered scheduler with
pacing/ramp, confirmation rules, SQLite storage with retention and rollups,
baseline/anomaly detection, incidents with attribution, sectors/ASN/country
views, coverage, corridors, IHI, SSE with replay, the documented v1 API, auth
and browser hardening, feeds and enrichment (IODA, Radar, Statuspage, AWS, GCP,
Azure, Atlas, RIS Live), incident timelines, DNS divergence, certificate pins, and the operational
CLI (seed, export, doctor, backup, debug bundle). Containers: hardened release
image, Tor/Tailscale/RIPE/trace profiles, and a two-hour deploy cadence. UI:
board, world, corridors, coverage, incidents, feeds status, and a FinViz-style
treemap map view.

Known limits, stated rather than hidden: single shared token (no user accounts);
detection is floor-only until ~28 weeks of hour-of-week baseline exist; no
multi-day soak has been run; migrations are forward-only, with a pre-migration
copy as the rollback point.

---

## 2. Now - release hardening (complete)

All five items below are implemented and verified on this tree. The section is
kept as a record of what "release hardening" meant, so a future reader can see
the bar rather than re-deriving it.

### 2.1 Soak run — **Implemented (verification)**

**Outcome:** a documented 72-hour run at the default fleet, with no restart, no
unbounded growth, and a stable probe rate. The run itself is an operator step;
what is implemented is the evidence that the process is soak-ready, plus the
recipe.

- `docs/SOAK.md` gives the run recipe (`NETWEATHER_LOG_LEVEL=info`,
  `NETWEATHER_DAILY_MB` set, disk/free checks) and the numbers to record.
- Evidence on this tree: the compose suite ran the release image for ~110 s
  with no restart and no unbounded growth; the real-endpoint suite ran six
  public targets through the real instrument and classifier in 1.58 s; and three
  consecutive full-suite runs with `-shuffle=on` and `-count=1` passed with zero
  failures across api, web, probe, store, and e2e. No state flapping beyond the
  classifier's own low-confidence rule was observed.
- Acceptance met for the code side: database growth within the `SCHEMA.md` §16
  estimate, `dropped`/`write_errors` counters at zero, no state flapping beyond
  the low-confidence rule. The 72-hour wall-clock run is an operator step
  recorded in `docs/SOAK.md`.

### 2.2 First-run experience — **Implemented**

**Outcome:** a fresh install does not read as calm or broken while its first
probes are still being scheduled.

- The board renders a `cold-start` banner when at least half of the fleet is
  still `UNKNOWN`, stating that probes are being spread across each target's
  interval and that baselines need about 28 weeks of history before anomaly
  detection runs. It disappears as soon as the fleet is mostly probed.
- `TestColdStartBannerReflectsTheFleetState` pins the show/hide behaviour.
- Acceptance met: rendered on a fresh volume, with a UI test; no change to the
  state model.

### 2.3 Backup/restore drill — **Implemented**

**Outcome:** the documented restore path is exercised, not just described.

- `internal/store.TestRestoreFromBackupDrillsTheDocumentedProcedure` opens a
  database, seeds a target, takes a `VACUUM INTO` backup, deletes the live file
  and any `-wal`/`-shm` siblings, replaces the file from the backup, re-opens
  it, and asserts the schema version and the row survived. The stale-sibling
  hazard the procedure warns about is cleared before the open.
- The compose suite adds a `backup produces a usable copy` scenario: the live
  container runs `/netweather backup`, and a helper container opens the copy
  with `sqlite3` and runs `PRAGMA integrity_check`. This is the only honest
  test of the documented restore path, because the release image has no shell
  and the binary is the only tool available inside it.
- Acceptance met: both scenarios run in `devtool verify`/`devtool compose` and
  fail if the restore steps regress.

### 2.4 Flake budget — **Implemented**

**Outcome:** `devtool verify` is repeatable.

- The SSE tracker baseline race, the DNS TCP/UDP port pairing, and the web
  snapshot-on-connect bug are fixed. Every flaky test from the audit was fixed
  rather than widened.
- Acceptance met: three consecutive full-suite runs with `-shuffle=on` and
  `-count=1` passed with zero failures (api, web, probe, store, e2e, ui).

### 2.5 Continuous deployment every two hours — **Implemented**

**Outcome:** a fresh build of the working software lands in the running
field trial on a fixed cadence, so the running instance never drifts from the
repository, and a broken build never replaces a working one.

- `scripts/deploy_fieldtrial.ps1` builds the release and Tor sidecar images,
  seeds the external secret volume at mode 0600/uid 65532, recreates the
  `compose.yaml` + `compose.fieldtrial.yaml` stack, and gates on `/healthz`
  for up to two minutes. A failing health check retags the previous image and
  recreates the stack instead of leaving the instance down. `-DryRun` prints
  every command without touching Docker; `-NoBuild` reuses the current images.
- `scripts/deploy.ps1` (Windows) and `scripts/deploy.sh` (POSIX) remain the
  legacy standalone-demo path; `install-schedule.ps1 -Legacy` still selects it.
- `scripts/install-schedule.ps1` registers a Windows Task Scheduler job that
  runs the **field-trial** deploy every two hours; cron is the same job on
  Linux/macOS. The job runs from the repository root and falls back to an
  interactive logon when elevation is unavailable, so a non-admin user still
  gets a working schedule (runs while logged in; re-run as Administrator for
  runs while logged out).
- Acceptance: a dry run prints the build and compose commands; a real run leaves
  `/healthz` answering and the stack on the new image; a broken build leaves the
  previous image running and exits non-zero. **Verified on this host:** the
  `netweather-deploy` task is `Ready`, trigger `PT2H`, and its action now calls
  `scripts\deploy_fieldtrial.ps1`. A real run rebuilt both images, recreated
  the stack, tagged `netweather:fieldtrial-rollback`, and exited 0 with
  `/healthz` returning 200, `targets=749` at the time (1000 after the fleet
  expansion), and `vantages=local,tor`.
- The two-hour cadence is deliberately short: the build is reproducible
  (`devtool repro`) and the container starts in seconds, so a fresh image is
  cheap; a broken one is caught by the health gate before it serves traffic.

---

## 3. Phase 3 (status)

### 3.1 Remote vantage agent (federation-lite) — **Implemented**

**Outcome:** a second machine measures the same targets and reports a real
`REGIONAL` rather than a proxy's approximation.

- `netweather agent` runs the same instruments and classifier as the primary on
  a remote host and POSTs each batch to `/api/v1/vantages/{id}/reports`. The
  primary registers the vantage ids in `NETWEATHER_REMOTE_VANTAGES` and records
  the batch under that vantage, so REGIONAL detection compares the two paths.
  Only the primary may declare DOWN; remote vantages are corroborating.
- Transport is HTTPS + bearer token today; mTLS and signed batches are a
  hardening item (tracked under Later).
- Acceptance met: unit tests for the ingest endpoint (auth, empty batch,
  mismatched vantage, malformed body, recorder error, disabled) and for the
  agent's remote sink (posting, server-error surfacing). The two-container
  compose scenario is the next increment.

### 3.2 RIS Live feed plugin (BGP) — **Implemented (poller + opt-in stream)**

**Outcome:** BGP update history becomes corroborating feed events, not a second
detector.

- `internal/feeds/rislive` polls RIPEstat's bgp-updates endpoint for the ASNs
  the operator names (`NETWEATHER_RIS_ASNS`), with an optional HTTPS endpoint
  override (`NETWEATHER_RIS_URL`). It maps each update to an event: a
  withdrawal is severity 1.0, an announcement 0.0, both carrying the ASN and the
  prefix in the payload. This remains the default.
- `internal/feeds/rislive_ws.go` is the opt-in real-time stream over
  `wss://ris-live.ripe.net/v1/ws/`, enabled by `NETWEATHER_FEEDS=ris-live-ws` or
  `NETWEATHER_RIS_STREAM=true`. It subscribes to `UPDATE` frames for the same
  ASN list, maps them into the same `bgp_update` / `bgp_withdrawal` events under
  feed `ris-live-ws`, batches writes, and reconnects with bounded backoff. The
  TCP dial goes through `netx` and a plaintext URL override is ignored.
  (`docs/DESIGN.md` §8.3.)
- Both are read-only and spend no credits: they only read RIPE/RIPEstat.
- An event is evidence, not a verdict: it appears in the feed strip and the
  drawer's corroboration list.
- Acceptance met: a recorded-payload contract test for the poller and a
  scripted-socket contract test (subscribe, ping/pong, mapping, reconnect) for
  the stream, plus env-doc and API-path guards.

### 3.3 RIPE Atlas feed plugin — **Implemented**

**Outcome:** operator-created measurements can be attached as evidence where a
target id is known; never guessed.

- `internal/feeds/atlas` is a read-only adapter over the RIPE Atlas results API.
  It reads the measurement ids the operator names (`NETWEATHER_ATLAS_MEASUREMENTS`),
  with an optional operator key (`NETWEATHER_ATLAS_KEY`) for private measurements
  and an optional endpoint override (`NETWEATHER_ATLAS_URL`). It reads only the
  results endpoint, which costs no credits, and never creates a measurement.
- An Atlas result carries no target id, so it appears as evidence in the feed
  strip and the drawer's corroboration list rather than as a verdict about one
  of this monitor's targets.
- Acceptance met: a contract test with a recorded payload, plus the env-doc and
  API-path guards; `ACCEPTABLE_USE.md` §9 and `docs/RIPE_ATLAS.md` describe it
  as shipped.

### 3.4 Live per-country IODA overlay — **Implemented (events + bounded level)**

**Outcome:** the world view shows a current per-country level without turning a
page view into an API loop.

- The world view carries the highest-severity feed event for each country from
  the last 24 hours (`row.Feed`, `row.FeedSeverity`), from one query for the
  whole page. A missing reader, an empty window, and a failed query each state
  their case rather than leaving a tile looking quiet.
- The bounded level feed (`NETWEATHER_FEEDS=ioda-level`) polls IODA's single
  ranked `/v2/outages/summary` endpoint every five minutes and stores at most
  `NETWEATHER_IODA_LEVEL_LIMIT` countries (default 25, hard max 100) in
  `country_level` (migration `0025`). `/world` reads that map with one query.
  IODA's `overall` is an unbounded magnitude, not a 0–1 probability, so the tile
  shows a clearly labelled **relative** level beside the raw score and event
  count; no absolute probability is invented. The event and level notes are
  separate so one feed's failure cannot silence the other.

### 3.5 Multi-user auth (OIDC) — **Later**

**Outcome:** named operators and an audit trail that says who did what. Moved
from Next on first principles: the single shared token already carries the
subject on every audit line (token identity is the operator), and OIDC adds an
external IdP dependency plus a mock-IdP test harness that does not exist in
this repo. The auth surface (CSRF, secure cookies, rate limiting, trusted
proxy) is complete for the shipped single-team model; multi-user is a
deployment-scale concern, not a correctness one.

- OIDC authorization-code flow, roles `viewer`/`operator`, sessions keyed to
  the subject, and the existing token retained as a break-glass login.
- When it lands: the audit lines (`probe_control`, `incident_annotated`,
  `target_disabled`) carry the subject; `THREAT_MODEL.md` T6/T7 are rewritten
  with the new controls; a viewer cannot mutate.

### 3.6 Public incident timeline — **Implemented**

**Outcome:** incident detail pages expose a chronological open/closed/annotation
record without a separate status-page service or schema migration.

- The detail view renders open and close milestones from the existing incident
  row, plus an operator-annotation milestone when ground truth is recorded.
- Existing attribution, affected targets, feed corroboration, archived probes,
  and annotations remain in place below the timeline.
- Acceptance: open, closed, and annotation branches are covered by incident
  page tests.

---

## 4. Public status-page polish

| Item | Status |
|---|---|
| Incident subscriptions | **Later:** email or webhook opt-in, unsubscribe tokens, and delivery suppression for duplicate updates |
| Maintenance windows | **Implemented:** operator-created windows with affected components and planned/completed states, declared through `POST /api/v1/maintenance` and shown on the incidents page as context rather than a detector verdict (migration `0026`) |
| Historical uptime | **Implemented:** bounded 24h/7d/30d percentages on the target drawer from retained rollups; missing, unknown, and blocked observations are excluded |

OIDC and subscriptions are not present merely because public incident detail
pages exist. Public incident timeline records, bounded historical uptime, and
operator-declared maintenance windows are implemented; the remaining
status-page capabilities need separate data and delivery boundaries.

---

## 5. Later

| Item | Why later |
|---|---|
| IPv6 as a first-class probe family | **Implemented:** `family: 4\|6` on targets; DNS/DoH/DoT AAAA queries; IPv6 literals allowed for country/network classes; netx already blocks IPv6 special-use ranges |
| DoT (853) | **Implemented:** `internal/probe/dot.go` (DNS over TLS), port 853 allowlisted |
| `macvlan` profile | **Implemented:** `compose.macvlan.yaml` override profile |
| Helm chart / systemd unit | **Implemented:** `deploy/helm/netweather` and `deploy/systemd/netweather.service` |
| Opt-in alert webhooks | **Implemented:** `internal/webhook`, `NETWEATHER_WEBHOOK_URL`/`SECRET`; best-effort POST on incident open/close |
| mTLS and signed batches for remote agents | **Implemented:** signed batches (HMAC-SHA256, `NETWEATHER_VANTAGE_SIGNING_SECRET` / `NETWEATHER_AGENT_SIGNING_SECRET`); client-certificate loading on the agent and verification on the ingest path when TLS is enabled |

---

## 6. Not planned

| Item | Reason |
|---|---|
| WAF evasion, fingerprint spoofing | The tool classifies bot protection; it does not defeat it (`ACCEPTABLE_USE.md` §9) |
| Scraping, load testing, port scanning | Out of scope by design |
| Running an exit relay | The shipped `torrc` is client-only and a test pins it |
| Telemetry or analytics | No outbound call exists except probes, feeds, self-checks, opt-in enrichment |
| A generated `openapi.yaml` | A source parser would emit a partial spec; `API.md` plus `/api/v1/meta` is the contract |
| Multi-tenant SaaS | This is a single-team, self-hosted instance |
| Default corridor endpoints | No honest default path pair exists; corridors stay configuration-driven (an operator supplies pairs they can defend) |
| QUIC/UDP-443 full handshake | Only a UDP reachability probe ships; a full QUIC/TLS handshake requires a QUIC stack (quic-go) that is not vendored, and the UDP probe already detects middlebox blocking |
| DF-bit path-MTU probe on non-Linux | Linux-only (raw socket options); other platforms use the interface-MTU self-check, which covers the documented failure mode |

---

## 7. How work is accepted

1. `go run ./internal/devtool verify` is green (format, vet, lint, tests,
   coverage gates, build).
2. A behaviour change has a test that fails without it, and the doc that
   describes it is updated in the same change.
3. Container-visible changes extend `devtool compose`; browser-visible changes
   extend the Playwright suite.
4. Every commit is signed off (`git commit -s`), and a user-visible change adds
   a `CHANGELOG.md` line.
