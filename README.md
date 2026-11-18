# netweather

> **netweather.** A self-hosted internet weather station. "Downdetector" is an Ookla trademark and is not this project's name.

A self-hosted internet weather station. One small container that answers three questions, in this order:

1. **Is something large happening on the internet right now?** — cyberwar, mass outage, cable cut, BGP hijack, national shutdown
2. **Is a specific service or sector down?** — banking, airlines, cloud, socials
3. **Is it me?** — my ISP, my DNS, my egress, my container

Most monitors answer only #2, and answer it badly: `curl != 200` becomes "down." That produces enough false alarms that people stop looking. This one classifies bot protection instead of evading it, and refuses to call a single vantage point a global truth.

**Status: phases 0-2 implemented; phase 3 partial.** The binary probes configured targets over HTTP, TLS, TCP, DNS, DoH, NTP, and ICMP on the tiered scheduler, classifies each result, writes to SQLite, and serves a store-backed board with live SSE updates plus the versioned API and a hardened container image. Incidents, baseline anomalies, coverage, self-checks, exports, feeds (IODA, Radar, Statuspage, AWS, GCP, Azure), ASN/CDN/PeeringDB enrichment, certificate issuer pinning and chain/leaf records, per-resolver DNS divergence, provider status mapping, idempotent actions, on-demand traceroute, the world view, configuration-driven corridors with one captured trace per step change, the Internet Health Index, and SSE replay are implemented; Tor (with an optional verified exit country) and Tailscale are compose profiles, traceroute has its own capability overlay, and RIPE's software probe is an opt-in profile (`docs/RIPE_ATLAS.md`). `NETWEATHER_SEED=none` starts with an empty board. Not built: RIS Live, the RIPE Atlas **feed plugin** (the probe profile ships; netweather does not ingest its results), and remote multi-instance agents; `DESIGN.md` §15 records what each remaining phase means.

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

**Instruments** — DNS, DoH, TCP, TLS, HTTP(S), ICMP, TCP-ping, NTP, traceroute (on-demand only).

**Vantages** — `direct` (your connection) and optional `socks5` (Tor, with exit-country pinning). Only the primary vantage may declare `DOWN`.

**External intelligence** — IODA, Cloudflare Radar, the Statuspage family (including the AWS/GCP/Azure status feeds). All optional; the core works with zero feeds. RIPE RIS Live is designed but not built (`docs/DESIGN.md` §8.3).

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

Ships with ~14 targets across banking, airlines, cloud, social, developer, and reference. Scale is opt-in: point `NETWEATHER_CONFIG` at a target file or directory, or add the ~250 ccTLD country anchors with `netweather seed --country-anchors` (one IANA root-zone fetch; `NETWEATHER_ROOT_ZONE_URL` points it at a mirror). `netweather seed --diff` prints what a run would add or change without writing any targets, which is the review step for a seed you did not author.

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

Targets come from `NETWEATHER_CONFIG=/config/targets.yaml` (see [`config/targets.example.yaml`](config/targets.example.yaml)), or the ten shipped defaults when it is unset. The same variable may point at a **directory** laid out as `netweather.yaml`, `sites/<sector>/*.txt`, `overrides/*.yaml`, and `corridors.yaml` (`docs/DESIGN.md` §13.2); a sector is the file's path, so a large list needs no per-line tagging. `NETWEATHER_SEED=none` starts with an empty board. State lives in `NETWEATHER_DATA` (default `./data`). Config validation is strict: an unknown field, a blocked port, or an interval under 30s refuses to start.

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
| `NETWEATHER_FEEDS` | none | comma list of `ioda`, `radar`, `statuspage`, `aws-status`, `gcp-status`, `azure-status`; empty means no external calls |
| `NETWEATHER_STATUSPAGE_URLS` | none | status pages for the `statuspage` feed; **HTTPS only**, and a plaintext entry is dropped with `event=feed_url_insecure` |
| `NETWEATHER_RADAR_TOKEN` | none | Cloudflare Radar API token; the `radar` feed is skipped with a warning without it |
| `NETWEATHER_IODA_URL`, `NETWEATHER_RADAR_URL`, `NETWEATHER_AWS_STATUS_URL`, `NETWEATHER_GCP_STATUS_URL`, `NETWEATHER_AZURE_STATUS_URL` | upstream defaults | endpoint overrides for a mirror or a test; **HTTPS only**, and a plaintext override is ignored with `event=feed_url_insecure` |
| `NETWEATHER_TOR` | off | enables the Tor corroboration vantage |
| `NETWEATHER_TOR_SOCKS` | `127.0.0.1:9050` | SOCKS5 entry point; `socks5h` only, so the proxy resolves names. The shipped compose profile sets `tor:9050`, the sidecar's service name |
| `NETWEATHER_TOR_MAX` | `25` | maximum Tor targets |
| `NETWEATHER_TOR_DAILY_MB` | `50` | Tor daily volume budget; `0` disables it |
| `NETWEATHER_TOR_EXIT_COUNTRY` | none | two-letter code; the exit is verified through the proxy and a mismatch refuses to start |

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

At the full 750-target fleet this is ~430k probes/day and ~0.7 GB/day, dominated by TLS handshakes. The default install is ~10 targets and ~15 MB/day. Details and the arithmetic: [`docs/ACCEPTABLE_USE.md`](docs/ACCEPTABLE_USE.md) §4.

---

## License

Apache-2.0.
