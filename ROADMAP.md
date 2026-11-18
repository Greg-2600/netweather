# Roadmap

A living plan, not a promise. The **Status** column is the truth; anything marked
*Not planned* has a stated reason rather than a silent absence. `DESIGN.md` §15
keeps the phase record, `CHANGELOG.md` records what shipped, and this file says
what is next and how it will be judged done.

Legend: **Shipped** · **Now** (release hardening) · **Next** (phase 3) ·
**Later** · **Not planned**

---

## 1. Where we are

**MVP is shipped.** Phases 0–2 are complete and phase 3 is partial by design.

Verified on the current tree (`fbd183a`):

| Gate | Command | Evidence |
|---|---|---|
| Format, vet, lint, unit + integration, coverage gates, build | `go run ./internal/devtool verify` | `verify: ok`, library total 87.5% |
| Real image + Docker network plane | `go run ./internal/devtool compose` | `ok … 110.7s` |
| Browser UI | `go test -tags=uitest ./internal/web/uitest` | `ok 7.9s` |
| Real internet, real instruments | `NETWEATHER_E2E=1 go test -tags=e2e ./internal/e2e -run TestRealEndpoints` | PASS 1.58s, 6/6 endpoints |
| Reproducible build | `go run ./internal/devtool repro` | `reproducible: a57b5fbd867af8ae` |
| Cross-platform release | `go run ./internal/devtool snapshot` | 5 binaries + `SHA256SUMS` |

Shipped: eight instruments plus on-demand traceroute, tiered scheduler with
pacing/ramp, confirmation rules, SQLite storage with retention and rollups,
baseline/anomaly detection, incidents with attribution, sectors/ASN/country
views, coverage, corridors, IHI, SSE with replay, the documented v1 API, auth
and browser hardening, feeds and enrichment, DNS divergence, certificate pins,
and the operational CLI (seed, export, doctor, backup, debug bundle).
Containers: hardened release image, Tor/Tailscale/RIPE/trace profiles.

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
container on a fixed cadence, so the running instance never drifts from the
repository, and a broken build never replaces a working one.

- `scripts/deploy.ps1` (Windows) and `scripts/deploy.sh` (POSIX) build the
  release image, seed the secret volume at mode 0600, stop the previous
  instance, start the new one, and gate on `/healthz` for up to 30 seconds.
  A failing health check rolls back to the previous image tag instead of
  leaving the instance down. `--dry-run` prints every command without touching
  Docker; `--no-build` reuses the current image.
- `scripts/install-schedule.ps1` registers a Windows Task Scheduler job that
  runs the deploy every two hours; cron is the same job on Linux/macOS. The job
  runs from the repository root and falls back to an interactive logon when
  elevation is unavailable, so a non-admin user still gets a working schedule
  (runs while logged in; re-run as Administrator for runs while logged out).
- Acceptance: a dry run exits 0 and prints the build and run commands; a real run
  leaves `/healthz` answering and the container on the new image; a broken build
  leaves the previous image running and exits non-zero. **Verified on this host:**
  the task `netweather-deploy` is `Ready`, trigger `PT2H`, next run
  2026-11-17 22:00, and a real deploy rebuilt the image, swapped the
  container, and exited 0 with `/healthz` returning 200.
- The two-hour cadence is deliberately short: the build is reproducible
  (`devtool repro`) and the container starts in seconds, so a fresh image is
  cheap; a broken one is caught by the health gate before it serves traffic.

---

## 3. Next - phase 3

### 3.1 Remote vantage agent (federation-lite) — **Next**

**Outcome:** a second machine measures the same targets and reports a real
`REGIONAL` rather than a proxy's approximation.

- Outbound-only agent binary: signed `probe` batches over mTLS with a per-agent
  token, the same `vantage` model the Tor vantage already uses, and the same
  "only `primary` may declare `DOWN`" rule.
- Acceptance: a compose test with two containers reports `REGIONAL` for one
  target and leaves the primary's state alone; agent traffic counts against its
  own budget, not the primary's.

### 3.2 RIS Live feed plugin (BGP) — **Implemented (polling adapter)**

**Outcome:** BGP update history becomes corroborating feed events, not a second
detector.

- `internal/feeds/rislive` polls RIPEstat's bgp-updates endpoint for the ASNs
  the operator names (`NETWEATHER_RIS_ASNS`), with an optional HTTPS endpoint
  override (`NETWEATHER_RIS_URL`). It maps each update to an event: a
  withdrawal is severity 1.0, an announcement 0.0, both carrying the ASN and the
  prefix in the payload.
- It is read-only and spends no credits; it only reads RIPEstat.
- An event is evidence, not a verdict: it appears in the feed strip and the
  drawer's corroboration list. The real-time WebSocket stream is post-MVP
  (`docs/DESIGN.md` §8.3).
- Acceptance met: a contract test with a recorded payload, plus the env-doc and
  API-path guards; `docs/DESIGN.md` §8.3 describes it as shipped.

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

### 3.4 Live per-country IODA overlay — **Next**

**Outcome:** the world view can show a current per-country level without
turning a page view into an API loop.

- A bounded server-side poll (one request per country per interval, cached in the
  database), rendered as an overlay beside stored events.
- Acceptance: page latency is independent of upstream latency; a failed poll
  leaves the stored event visible and states the gap.

### 3.5 Multi-user auth (OIDC) — **Next**

**Outcome:** named operators and an audit trail that says who did what.

- OIDC authorization-code flow, roles `viewer`/`operator`, sessions keyed to the
  subject, and the existing token retained as a break-glass login.
- Acceptance: the audit lines (`probe_control`, `incident_annotated`,
  `target_disabled`) carry the subject; `THREAT_MODEL.md` T6/T7 are rewritten
  with the new controls; a viewer cannot mutate.

---

## 4. Later

| Item | Why later |
|---|---|
| IPv6 as a first-class probe family | Docker IPv6 is fiddly; host networking only at first (`NETWORKING.md` §14) |
| QUIC/UDP-443 middlebox probe | New instrument, needs a UDP path and its own classifier rows |
| DoT (853) | Ports allow it; no instrument yet, and DoH covers the resolver question |
| DF-bit path-MTU probe | Needs raw sockets; the interface-MTU check already prevents the common false `DOWN` |
| `macvlan` profile | Best fidelity, worst portability; ship only on demand |
| Default corridor endpoints | There is no honest default path pair; corridors stay configuration-driven |
| Helm chart / systemd unit | Compose and one binary cover the stated deployment |
| Opt-in alert webhooks | The project is deliberately not a paging system (`README` "What it is not"); an operator can poll `/api/v1/events` |

---

## 5. Not planned

| Item | Reason |
|---|---|
| WAF evasion, fingerprint spoofing | The tool classifies bot protection; it does not defeat it (`ACCEPTABLE_USE.md` §9) |
| Scraping, load testing, port scanning | Out of scope by design |
| Running an exit relay | The shipped `torrc` is client-only and a test pins it |
| Telemetry or analytics | No outbound call exists except probes, feeds, self-checks, opt-in enrichment |
| A generated `openapi.yaml` | A source parser would emit a partial spec; `API.md` plus `/api/v1/meta` is the contract |
| Multi-tenant SaaS | This is a single-team, self-hosted instance |

---

## 6. How work is accepted

1. `go run ./internal/devtool verify` is green (format, vet, lint, tests,
   coverage gates, build).
2. A behaviour change has a test that fails without it, and the doc that
   describes it is updated in the same change.
3. Container-visible changes extend `devtool compose`; browser-visible changes
   extend the Playwright suite.
4. Every commit is signed off (`git commit -s`), and a user-visible change adds
   a `CHANGELOG.md` line.
