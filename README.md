# netweather

> **netweather.** A self-hosted internet weather station. "Downdetector" is an Ookla trademark and is not this project's name.

A self-hosted internet weather station. One small container that answers three questions, in this order:

1. **Is something large happening on the internet right now?** — cyberwar, mass outage, cable cut, BGP hijack, national shutdown
2. **Is a specific service or sector down?** — banking, airlines, cloud, socials
3. **Is it me?** — my ISP, my DNS, my egress, my container

Most monitors answer only #2, and answer it badly: `curl != 200` becomes "down." That produces enough false alarms that people stop looking. This one classifies bot protection instead of evading it, and refuses to call a single vantage point a global truth.

**Status: phases 0-3 implemented (OIDC deferred to Later).** The binary probes configured targets over HTTP, TLS, TCP, DNS, DoH, DoT, QUIC (UDP/443 reachability), NTP, and ICMP on the tiered scheduler, classifies each result, writes to SQLite, and serves a store-backed board with live SSE updates plus the versioned API and a hardened container image. Incidents, baseline anomalies, coverage, self-checks, exports, feeds (IODA, Radar, Statuspage, AWS, GCP, Azure, Atlas, RIPE RIS Live), ASN/CDN/PeeringDB enrichment, certificate issuer pinning and chain/leaf records, per-resolver DNS divergence, provider status mapping, idempotent actions, on-demand traceroute, the world view, configuration-driven corridors with one captured trace per step change, the Internet Health Index, SSE replay, a Feeds status page, and a FinViz-style treemap map view are implemented; Tor (with an optional verified exit country) and Tailscale are compose profiles, traceroute has its own capability overlay, and RIPE's software probe is an opt-in profile (`docs/RIPE_ATLAS.md`). Remote vantage agents (federation-lite) are implemented: `netweather agent` runs the same instruments on a remote host and reports to `/api/v1/vantages/{id}/reports`. `NETWEATHER_SEED=none` starts with an empty board. Not built: OIDC multi-user auth (deferred to Later). Known limits, stated rather than hidden: QUIC is a UDP/443 reachability probe (not a full handshake); the DF-bit path-MTU probe is Linux-only (other platforms use the interface-MTU self-check); IPv6 measurement runs under host networking only (`compose.ipv6.yaml`); RIPE RIS Live is a polling adapter (real-time WebSocket is post-MVP); IODA shows stored events, not a live per-country score. The RIPE Atlas feed plugin ships as a read-only adapter (`internal/feeds/atlas`); it reads measurements the operator created and never spends credits. RIS Live ships as a polling adapter over RIPEstat (`internal/feeds/rislive`); the real-time WebSocket is post-MVP.

Build and test with one command: `go run ./internal/devtool verify`.

---

## What it is not

- Not a Downdetector clone — you are one vantage point, and the UI never claims otherwise
- Not a homelab uptime monitor — use Uptime Kuma or Gatus for "is my NAS up"
- Not a scraper, a load tester, or a port scanner — see `docs/ACCEPTABLE_USE.md`
- Not a paging system — no on-call, no SLOs

---

## Prior art

This project is the synthesis of three earlier experiments. Each contributed one idea; each had one thing wrong with it.

| Repo | What it did | What it contributed | What was wrong |
|---|---|---|---|
| **[censor_sensor](https://github.com/Greg-2600/censor_sensor)** | Bash loop over a `site_list.txt`, `curl --head` each domain, grep for `200`, print a pass/fail count | The core model: **a list of domains, periodically checked, clustered by availability.** Also the reason the UI is a grid of favicons rather than a list of rows | One HEAD request, one bit of output. No history, no diagnosis, and `HEAD` is rejected by a lot of servers — so it reported failures that weren't |
| **[global_packet_throwing](https://github.com/Greg-2600/global_packet_throwing)** | Enumerated every ccTLD, generated `xx.xx` / `www.xx` / `nic.xx` / `gov.xx`, resolved, pinged, printed latency with a country label | The idea that **country is a dimension**, and that you can reach other countries from one box. That idea is now the country-anchor layer | The instrument was wrong: `www.jp` usually resolves to a CDN edge near *you*, so the "international circuit" was often a local PoP. Fixed by querying the ccTLD's own authoritative nameservers over DNS instead |
| **[proxy_scan](https://github.com/Greg-2600/proxy_scan)** | Installed `nmap` / `proxychains` / `tor`, wrote `torrc` and `proxychains.conf`, then scanned through the proxy | **Transport substitution**: the tool above doesn't know it's proxied. That became the `Dialer` interface and the Tor vantage model. Its client-only `torrc` (`ExitPolicy reject *:*`) is now mandatory here | Scraping public proxy lists is unreliable and ethically dubious — this project accepts user-supplied SOCKS5 only |

The through-line: `censor_sensor` asked *"can I reach it?"*, `global_packet_throwing` asked *"can I reach it from over there?"*, `proxy_scan` asked *"can I reach it through something else?"* netweather asks **"what does that mean, and how sure am I?"**

---

## Design docs

| Doc | Covers |
|---|---|
| [`docs/DESIGN.md`](docs/DESIGN.md) | Architecture, target classes, scheduler, state model, correlation, attribution, external feeds, vantages, sectors/ASN/country, UI, storage, deployment, guardrails |
| [`docs/CLASSIFIER.md`](docs/CLASSIFIER.md) | The rule table that turns a response into a state — error classes, WAF signatures, precedence, confidence, hard-target recipes, golden fixtures |
| [`docs/IMAGE.md`](docs/IMAGE.md) | Container base image, two-image strategy, hardening, capabilities, supply chain |
| [`docs/NETWORKING.md`](docs/NETWORKING.md) | Docker topology, MTU/PMTU, embedded DNS, SSRF reachability, Tailscale and proxy access, Tor sidecar wiring |
| [`docs/QUALITY.md`](docs/QUALITY.md) | Linters, security scanning, six backend test layers plus the opt-in Playwright browser layer, fuzzing, the fake internet, coverage gates, CI |
| [`docs/SCHEMA.md`](docs/SCHEMA.md) | SQLite tables, pragmas and pools, incremental rollups, retention jobs, query shapes, backup and export |
| [`docs/LOGGING.md`](docs/LOGGING.md) | Levels and volume, decision-record fields, redaction, sinks, backpressure, the in-memory ring, debug bundle |
| [`docs/API.md`](docs/API.md) | `/api/v1` contract: envelope, error codes, auth, pagination, SSE, implemented vs specified endpoints, CLI contract |
| [`docs/THREAT_MODEL.md`](docs/THREAT_MODEL.md) | Assets, adversaries, trust boundaries, SSRF and exposure threats, supply chain, invariants, residual risk, operator runbook |
| [`docs/ACCEPTABLE_USE.md`](docs/ACCEPTABLE_USE.md) | Wire-level behaviour, hard limits, volume math, target selection, prohibited uses, abuse handling, Tor policy |
| [`docs/RIPE_ATLAS.md`](docs/RIPE_ATLAS.md) | The optional RIPE Atlas profile: what the daemon is, what RIPE sees, and why netweather never reads from it |
| [`ROADMAP.md`](ROADMAP.md) | Where the project is now, what is next, what is deliberately not planned, and how work is accepted |

---

## At a glance

**Targets** — four classes, ~750 probes:

| Class | Count | Job |
|---|---|---|
| Service | 400 | Sector health (banking, airlines, cloud, …) |
| Country anchor | 250 | ccTLD nameservers — "is country X still on the internet?" |
| Network anchor | 100 | Root servers, public resolvers, IXP route servers |
| Reference | 10–100 | The "is it me?" baseline |

**States** - `UP` · `UP_REACHABILITY` · `DEGRADED` · `DOWN` · `BLOCKED` · `UNKNOWN` · `CONFIG_ERROR` · `UNMEASURABLE`. `BLOCKED`, `CONFIG_ERROR`, and `UNMEASURABLE` never count against health, which is what keeps the board from being permanently on fire.

**Verdicts** - `OK` · `INCIDENT` · `REGIONAL` · `LOCAL` · `TAMPER` · `STALE` · `LEARNING`.

**Instruments** - DNS, DoH, DoT, QUIC, TCP, TLS, HTTP(S), ICMP, TCP-ping, NTP, traceroute (on-demand only).

**Vantages** — `direct` (your connection) and optional `socks5` (Tor, with exit-country pinning). Only the primary vantage may declare `DOWN`.

**External intelligence** — IODA, Cloudflare Radar, the Statuspage family (including the AWS/GCP/Azure status feeds), and RIPE RIS Live (polling adapter over RIPEstat). All optional; the core works with zero feeds. RIPE RIS Live's real-time WebSocket is post-MVP; the polling adapter ships now (`docs/DESIGN.md` §8.3).

**Stack** - Go, SQLite (pure Go), server-rendered HTML + SSE. One binary. No JS build step, no framework. Browser tests use Playwright.

---

## Container

Single container, read-only rootfs, non-root, no shell in the release image. Outbound for probes; **inbound only on 2600** for the dashboard.

The image is built from [`Dockerfile`](Dockerfile) (Chainguard Go builder → static runtime, ~25 MB, no shell) and the stack is in [`compose.yaml`](compose.yaml). On a fresh install:

```sh
mkdir -p data secrets
printf '%s' "$(openssl rand -hex 32)" > secrets/ui_token
chmod 600 secrets/ui_token
sudo chown -R 65532:65532 data   # Linux bind mounts only; the image owns /data
docker compose up -d
```

The compose file binds `./data:/data` and `./config:/config:ro`, publishes only `2600`, and runs read-only with `cap_drop: ALL`. The named-volume path is also supported: `/data` exists in the image owned by uid 65532.

```yaml
services:
  netweather:
    image: ghcr.io/<owner>/netweather:latest
    networks: [ui, egress]
    ports: ["2600:2600"]
    volumes:
      - ./data:/data
      - ./config:/config:ro
      - ./secrets/ui_token:/run/secrets/ui_token:ro
    environment:
      NETWEATHER_UI_LISTEN: 0.0.0.0:2600
      NETWEATHER_UI_TOKEN_FILE: /run/secrets/ui_token
    read_only: true
    tmpfs: [/tmp]
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    sysctls:
      net.ipv4.ping_group_range: "0 2147483647"   # unprivileged ICMP, no NET_RAW
    restart: unless-stopped

networks:
  ui:
  egress:
    driver_opts:
      com.docker.network.driver.mtu: 1400          # VPN uplinks are not 1500
```

Ships with ~14 targets across banking, airlines, cloud, social, developer, and reference. Scale is opt-in: point `NETWEATHER_CONFIG` at a target file or directory, or add the ~250 ccTLD country anchors with `netweather seed --country-anchors` (one IANA root-zone fetch; `NETWEATHER_ROOT_ZONE_URL` points it at a mirror). `netweather seed --diff` prints what a run would add or change without writing any targets, which is the review step for a seed you did not author. The early field-trial fleet is pre-generated at `config/fieldtrial-targets.yaml`: 749 real targets (399 ccTLD anchors, 100 network anchors, 240 services, 10 references), rebuilt from the IANA root zone by `scripts/gen_fieldtrial_targets.py`.

**Verify the image before running it.** Releases are multi-arch, signed with cosign keyless, and carry SBOM and provenance attestations (`docs/IMAGE.md` §6). Pin the digest, not a tag:

```sh
cosign verify ghcr.io/Greg-2600/netweather@sha256:<digest> \
  --certificate-identity-regexp 'https://github.com/Greg-2600/netweather/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The release notes and step summary print the exact digest and command for each tag.

Everything optional is a compose profile, so the default file publishes one port and grants zero capabilities: `tor` (corroboration vantage), `tailscale` (tailnet access without publishing a port), `ripe-atlas` (RIPE's own probe, separate trust domain). Traceroute is a separate `compose.trace.yaml` because it needs `CAP_NET_RAW`.

**Port 2600** — 2600 Hz, phreaking, *2600: The Hacker Quarterly*; unassigned by IANA and above 1024 so the non-root UID can bind it without `NET_BIND_SERVICE`. The cool options that lose: `179` (BGP — privileged, collides with real BGP daemons) and `6667` (IRC/botnet C2 — Chrome and Firefox refuse to load it as an unsafe port). Full comparison in [`docs/NETWORKING.md`](docs/NETWORKING.md) §7.1.

`0.0.0.0` is the default bind, and that is fine — but an unauthenticated dashboard is not. A non-loopback listen with `ui.auth: none` refuses to start unless `ui.allow_public: true` is *also* set. The page publishes your target list, egress ASN, and coverage gaps; that is the disclosure, not the HTML.

Container networking is not an afterthought here: Docker's bridge defaults, embedded DNS, and MTU will all silently produce fake outages if unhandled. [`docs/NETWORKING.md`](docs/NETWORKING.md) covers the topology, the PMTU check, the SSRF reachability map, and Tailscale/reverse-proxy access. Base image and hardening: [`docs/IMAGE.md`](docs/IMAGE.md).

---

## Configuration

Targets come from `NETWEATHER_CONFIG=/config/targets.yaml` (see [`config/targets.example.yaml`](config/targets.example.yaml)), or the fourteen shipped defaults when it is unset. The same variable may point at a **directory** laid out as `netweather.yaml`, `sites/<sector>/*.txt`, `overrides/*.yaml`, and `corridors.yaml` (`docs/DESIGN.md` §13.2); a sector is the file's path, so a large list needs no per-line tagging. `NETWEATHER_SEED=none` starts with an empty board. State lives in `NETWEATHER_DATA` (default `./data`). Config validation is strict: an unknown field, a blocked port, or an interval under 30s refuses to start.

Every environment variable the binary reads, including the ones that exist for a mirror or a test:

| Variable | Default | Meaning |
|---|---|---|
| `NETWEATHER_CONFIG` | shipped seed list | target file |
| `NETWEATHER_CALENDAR` | `calendar.yaml` beside the target config | atypical-day calendar (`dates:` with `date:` and optional `regions:`; see [`config/calendar.example.yaml`](config/calendar.example.yaml)); a listed day is excluded from baseline samples and a malformed file refuses startup (`docs/DESIGN.md` §7.2) |
| `NETWEATHER_SEED` | `default` | `none` starts with an empty board |
| `NETWEATHER_DATA` | `./data` | SQLite database and rotated log file |
| `NETWEATHER_UI_LISTEN` | `0.0.0.0:2600` | HTTP bind address |
| `NETWEATHER_TRUSTED_PROXIES` | none | comma-separated CIDRs or addresses whose `X-Forwarded-For` is believed for per-client limits and log lines; empty trusts nobody, and a malformed entry refuses startup (`docs/NETWORKING.md` §7.3) |
| `NETWEATHER_REDACT_EGRESS` | `false` | hides the literal egress address on `/api/v1/selfcheck` while keeping its ASN and country; the stored tuple stays exact for `VANTAGE_CHANGED` (`docs/THREAT_MODEL.md` T65) |
| `NETWEATHER_UI_TOKEN` / `NETWEATHER_UI_TOKEN_FILE` | none | session and bearer token, minimum 16 characters; `_FILE` is preferred so the token is not in the environment |
| `NETWEATHER_UI_SECURE_COOKIES` | `false` | `true` marks the session cookie `Secure`; set it behind TLS |
| `NETWEATHER_CONTACT` | none | contact address in the probe `User-Agent`; unset logs `contact_missing` at startup and sends a visible `<contact>` placeholder, because an unattributed monitor is indistinguishable from a scanner |
| `NETWEATHER_TRACE_ENABLED` | `false` | `true` enables the on-demand traceroute action; the capability alone is not consent, so the endpoint answers `503` naming this variable until it is set. Use `compose.trace.yaml`, which sets it and grants `CAP_NET_RAW` |
| `NETWEATHER_UI_ALLOW_PUBLIC` | `false` | allows a non-loopback bind with no token; without it the process refuses to start, because the board discloses the target list |
| `NETWEATHER_LOG_LEVEL` | `info` | `trace`, `debug`, `info`, `warn`, `error` |
| `NETWEATHER_LOG_FORMAT` | `auto` | `auto` (text on a TTY, JSON otherwise), `text`, `json` |
| `NETWEATHER_LOG_FILE` | none | rotated file sink, written 0600; empty keeps logs on stdout only |
| `NETWEATHER_LOG_MAX_MB` | `50` | size before rotation |
| `NETWEATHER_LOG_KEEP` | `3` | rotated files retained |
| `NETWEATHER_LOG_RING` | `2000` | in-memory ring records; `0` disables the ring and its pages |
| `NETWEATHER_LOG_SOURCE` | unset | `1` adds `source=file.go:line` to every record |
| `NETWEATHER_API_URL` | `http://127.0.0.1:2600` | base URL the CLI verbs (`debug`, `doctor`) use |
| `NETWEATHER_HEALTHCHECK_URL` | derived from `NETWEATHER_UI_LISTEN` | `/healthz` target for `netweather healthcheck`; a non-loopback URL is refused |
| `NETWEATHER_METRICS` | `false` | mounts `/metrics` (Prometheus text, aggregates only) |
| `NETWEATHER_INCIDENT_INTERVAL` | `5m` | incident and anomaly evaluation cadence |
| `NETWEATHER_DAILY_MB` | `2048` | the direct vantage's daily egress budget; when spent, probing stops until 00:00 UTC and the board says why. `0` disables it |
| `NETWEATHER_RATE_LIMIT` | `20` | global probe ceiling in requests per second; fractional values are allowed, the cold-start ramp still starts at 2 req/s and lasts ten minutes, and a malformed or non-positive value refuses startup |
| `NETWEATHER_ENRICH` | on | `false` disables ASN, prefix, and CDN learning |
| `NETWEATHER_PEERINGDB` / `NETWEATHER_PEERINGDB_URL` | on | exchange-membership labels; `false` disables them without disabling enrichment, and `_URL` points at a mirror |
| `NETWEATHER_FEEDS` | none | comma list of `ioda`, `ioda-level`, `radar`, `statuspage`, `aws-status`, `gcp-status`, `azure-status`, `ris-live`/`ris`; empty means no external calls |
| `NETWEATHER_WEBHOOK_URL` | none | optional URL receiving a POST on incident open/close; empty disables webhooks |
| `NETWEATHER_WEBHOOK_SECRET` | none | optional secret sent as `X-Netweather-Secret` on webhook deliveries |
| `NETWEATHER_VANTAGE_SIGNING_SECRET` | none | optional shared secret; when set, vantage ingest requires an HMAC-SHA256 `X-Netweather-Signature` over the body |
| `NETWEATHER_VANTAGE_CLIENT_CA` | none | optional CA file; when set and the server runs TLS, client certificates are required on the ingest path |
| `NETWEATHER_AGENT_SIGNING_SECRET` | none | agent-side shared secret for signing batches (must match the primary's) |
| `NETWEATHER_AGENT_CLIENT_CERT` | none | agent client certificate for mTLS |
| `NETWEATHER_AGENT_CLIENT_KEY` | none | agent client key for mTLS |
| `NETWEATHER_REMOTE_VANTAGES` | none | comma list of vantage ids a primary registers for remote agents; they are corroborating by default |
| `NETWEATHER_SERVER_URL` | none | primary server base URL for a remote vantage agent (`agent` subcommand) |
| `NETWEATHER_VANTAGE` | remote | vantage id an agent reports under |
| `NETWEATHER_AGENT_TOKEN` | none | bearer token an agent presents to the primary |
| `NETWEATHER_STATUSPAGE_URLS` | none | status pages for the `statuspage` feed; **HTTPS only**, and a plaintext entry is dropped with `event=feed_url_insecure` |
| `NETWEATHER_ATLAS_KEY` | none | RIPE Atlas API key for the `atlas` feed; optional, public measurements are readable without one. It is read-only: the plugin never creates a measurement |
| `NETWEATHER_ATLAS_MEASUREMENTS` | none | comma list of RIPE Atlas measurement ids the `atlas` feed reads; required for the feed to run |
| `NETWEATHER_ATLAS_URL` | upstream default | endpoint override for a mirror or a test; **HTTPS only**, and a plaintext override is ignored with `event=feed_url_insecure` |
| `NETWEATHER_RADAR_TOKEN` | none | Cloudflare Radar API token; the `radar` feed is skipped with a warning without it |
| `NETWEATHER_RIS_ASNS` | none | comma list of ASNs the `ris-live` poller and the `ris-live-ws` stream read; required for either to run |
| `NETWEATHER_RIS_URL` | upstream default | endpoint override for a mirror or a test; **HTTPS only**, and a plaintext override is ignored with `event=feed_url_insecure` |
| `NETWEATHER_RIS_STREAM` | `false` | `true` enables the real-time RIS Live WebSocket (`ris-live-ws`) in addition to the poller; needs `NETWEATHER_RIS_ASNS` |
| `NETWEATHER_RIS_WS_URL` | upstream default | `wss` endpoint override for the stream; **WSS only**, plaintext is ignored with `event=feed_url_insecure` |
| `NETWEATHER_IODA_LEVEL_LIMIT` | `25` | maximum ranked countries stored per `ioda-level` poll; clamped to 1–100 so a poll stays bounded |
| `NETWEATHER_IODA_SUMMARY_URL` | upstream default | endpoint override for the `ioda-level` summary; **HTTPS only** |
| `NETWEATHER_IODA_URL`, `NETWEATHER_RADAR_URL`, `NETWEATHER_AWS_STATUS_URL`, `NETWEATHER_GCP_STATUS_URL`, `NETWEATHER_AZURE_STATUS_URL` | upstream defaults | endpoint overrides for a mirror or a test; **HTTPS only**, and a plaintext override is ignored with `event=feed_url_insecure` |
| `NETWEATHER_TOR` | off | enables the Tor corroboration vantage |
| `NETWEATHER_TOR_SOCKS` | `127.0.0.1:9050` | SOCKS5 entry point; `socks5h` only, so the proxy resolves names. The shipped compose profile sets `tor:9050`, the sidecar's service name |
| `NETWEATHER_TOR_MAX` | `25` | maximum Tor targets |
| `NETWEATHER_TOR_DAILY_MB` | `50` | Tor daily volume budget; `0` disables it |
| `NETWEATHER_TOR_EXIT_COUNTRY` | none | two-letter code; the exit is verified through the proxy and a mismatch refuses to start |

## Operations

**Backup.** The release image has no shell, so the binary is the backup tool:

```sh
docker compose exec netweather /netweather backup -o /data/backup.db
docker compose cp netweather:/data/backup.db ./backup.db
```

`VACUUM INTO` writes a consistent, compact copy while the service runs; the file is `0600`, and an existing destination is refused rather than overwritten.

**Upgrade.** Pull the new digest and restart. Migrations run in a transaction, and before a pending one the binary writes a rollback point beside the database as `netweather.db.pre-migration-v<from>-<UTC timestamp>.db`. A database written by a newer binary refuses to open instead of silently dropping columns.

**Restore.** Stop the service, replace `/data/netweather.db`, delete any `-wal`/`-shm` siblings (they belong to the file you just replaced), and start. Startup logs `event=db_wal_present` if it finds a WAL, because it cannot tell a crashed one from a stale one.

**Logs and support.** `docker compose logs` carries everything at the configured level; `/diagnostics` (behind auth) shows the ring, drops, and active debug scopes; `netweather debug bundle -o bundle.tar.gz` collects the build identity, redacted config, ring, self-checks, and the failing targets' last probes. Enable verbose output for one target rather than globally:

```sh
docker compose exec netweather /netweather debug enable --target chase.com --level trace --for 10m
```

## Continuous deployment

The field trial deploys through `compose.yaml` plus `compose.fieldtrial.yaml`: the production-sized target file, direct + Tor vantages, hardened runtime, an external secret volume at mode `0600`/uid `65532`, a `/healthz` gate, and image-tag rollback.

```powershell
.\scripts\deploy_fieldtrial.ps1               # Windows: build + deploy
.\scripts\deploy_fieldtrial.ps1 -DryRun       # print the commands, touch nothing
.\scripts\deploy_fieldtrial.ps1 -NoBuild      # reuse existing images
```

```sh
./scripts/deploy_fieldtrial.sh                # Linux/macOS/CI: same deploy
./scripts/deploy_fieldtrial.sh --dry-run
./scripts/deploy_fieldtrial.sh --no-build
```

Both wrappers seed the secret volume, tag a rollback image, recreate the stack, wait for `/healthz`, and roll back if the new instance is unhealthy. The field-trial overlay enables the bounded `ioda-level` feed: one ranked IODA summary poll every five minutes stores at most 25 countries, and `/world` reads that stored snapshot rather than calling IODA per country. The displayed level is **relative** (log-scaled against the top-ranked country in the same window) and is shown beside IODA's raw score and event count; it is evidence, not a verdict or a probability. The real-time RIS Live WebSocket is opt-in — set `NETWEATHER_RIS_STREAM=true` with `NETWEATHER_RIS_ASNS` — and runs alongside the polling adapter rather than replacing it.

After a deploy, verify the running instance (or soak it by raising `CYCLES`):

```sh
./scripts/check_fieldtrial.sh                          # health, 749 targets, auth, /map, feed, Tor
CYCLES=12 SLEEP=300 ./scripts/check_fieldtrial.sh     # ~1 hour soak
```

The token is read from `NETWEATHER_UI_TOKEN`, `TOKEN_FILE`, or the `nw-fieldtrial-secret` volume.

`scripts/deploy.ps1` (Windows) and `scripts/deploy.sh` (Linux/macOS) remain the legacy standalone-demo path. `scripts/install-schedule.ps1` registers the **field-trial** deploy every two hours; `.\scripts\install-schedule.ps1 -Legacy` selects the old standalone path instead. On Linux/macOS the same cadence is one cron line running `scripts/deploy_fieldtrial.sh`. The build is reproducible (`go run ./internal/devtool repro`), so a scheduled rebuild is cheap, and the health gate means a broken build never replaces a working instance.

## Safety posture

This tool sends periodic requests to third-party sites. That is dual-use, so the limits are enforced in code rather than documented and hoped for:

- 30s minimum interval per host, not configurable lower
- Per-host concurrency 1, a global rate ceiling (20 req/s steady, ramped from 2 req/s over ten minutes so a restart is not a burst), and a 2 GB/day egress budget with a hard stop
- Outbound ports 80/443/53/123/853 only
- Inbound: `2600` only, and a non-loopback bind requires auth
- Private ranges blocked after DNS resolution **and** after every redirect
- Traceroute: on-demand or anomaly-triggered, ≤10 runs/hour
- Tor vantages: off by default, ≤25 targets, ≥2s spacing, daily bandwidth budget
- No telemetry, ever

At the full 750-target fleet this is ~430k probes/day and ~0.7 GB/day, dominated by TLS handshakes. The default install is 14 targets and roughly 50 MB/day at the shipped intervals. Details and the arithmetic: [`docs/ACCEPTABLE_USE.md`](docs/ACCEPTABLE_USE.md) §4.

---

## License

Apache-2.0.
