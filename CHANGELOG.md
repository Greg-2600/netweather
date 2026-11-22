# Changelog

Behaviour changes are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
`<major>.<minor>.<patch>` tags from the release job. The detailed record of
every commit is the git history; this file exists so an operator can read what
changed between two images without it.

## [Unreleased]

### Added

- `/hubs` maps real US airport and city-portal targets with the same health
  treemap as `/map`: one city block per `City, ST hub — site` group on sector
  `us/hub`, one box per endpoint, worst cities and sites first. The 42 targets
  that stayed blocked from every vantage were replaced with egress-verified
  public endpoints under their original ids; the fleet stays at 749 and the
  schema stays at 25. `scripts/check_fieldtrial.sh` now checks the hub blocks.
- One bounded DNS timeout retry: a lost UDP datagram gets a second attempt
  inside the existing 5s budget, while negative answers, policy blocks, and
  malformed replies remain final. `NETWEATHER_DNS_RETRIES` (0–2) and
  `NETWEATHER_DNS_RETRY_DELAY_MS` (0–200) are the opt-out/tuning knobs.
- Multi-continent Tor corroboration: `NETWEATHER_TOR_VANTAGES` runs several
  client-only Tor sidecars with pinned exits (DE, JP, AU, SG, CL, MX in the
  field trial). Each exit's real country is verified through its own SOCKS hop,
  each has an independent probe subset, pace, and byte budget, and one failed
  exit no longer stops the others. Target drawers and the API show each
  vantage's verified country and continent.
- A bounded live IODA overlay: the opt-in `ioda-level` feed polls IODA's ranked
  `/v2/outages/summary` every five minutes and stores at most 25 countries
  (`country_level`, migration `0025`). `/world` shows a clearly labelled
  relative level beside IODA's raw score and event count; it never calls IODA
  from a page view and never invents an absolute probability.
- An opt-in real-time RIS Live WebSocket stream (`ris-live-ws`) alongside the
  RIPEstat poller, with per-ASN `UPDATE` subscriptions, batched events,
  ping/pong, bounded reconnect backoff, and egress-policy dialing.
- `scripts/deploy_fieldtrial.sh` (POSIX deploy) and `scripts/check_fieldtrial.sh`
  (health, fleet, auth, /map, /hubs, /corridors, feed, and Tor verification plus soak)
  for the field trial, mirroring the Windows deploy script.
- Ten operator-supplied corridors in the field-trial config (and its generator):
  real public TCP:443 endpoint pairs for the design's path pairs, each verified
  to connect before inclusion. `/corridors` now shows live RTT and loss samples.
  The project still ships no default endpoint list; these belong to this trial.
- `state_change` at `INFO`: one line per confirmed transition, written after the
  batch commits, with the same reason string the drawer reads.
- Per-stage `TRACE` records (`event=probe_stage`) from the guarded dialer
  (`dns`, `tcp`) and the HTTP instrument (`http`), selected by a time-boxed
  debug scope; nothing is emitted at the normal level.
- `netweather debug enable --until <RFC 3339>`, alongside `--for`.
- A recorded-response classifier fixture set and its parser, covering the
  challenge, block, rate-limit, legal, origin-error, configuration-error,
  reachability, semantic-pass, semantic-mismatch, and JS-shell branches.
- The Feeds status page now lists recent external events, not only feed
  health, matching /api/v1/feeds.
- A stock-market style cluster map at /map that answers "how healthy is the
  internet?" at a glance. It defaults to health clustering (long-running
  outages → recent outages → blocked → degraded → no data → healthy), packed
  most-urgent first; sector/country/ASN clustering remain available. Tile and
  cluster area grow with severity and time in state, so a long outage is large
  and a healthy target is a compact heat-map cell. A front panel leads with the
  reachable percentage, per-state counts, the longest outage, and the eight
  worst targets ranked by severity × duration; each row links to the detail.
  Tile colour is a diverging green→amber→red scale, labels are gated on box
  size (compact ticker / full name / colour-only with a full tooltip) so names
  are never cut off, and clicking a tile opens its target detail. ccTLD anchors
  now carry expanded names from CLDR and IANA ("NP ccTLD" is "Nepal ccTLD",
  "UZ ccTLD" is "Uzbekistan ccTLD"; IDN labels show their English country name
  and Unicode form), so codes are understandable at a glance on every page.
  Every tile now has a styled hover card (and an equivalent accessible label)
  naming what the target is — root server, ccTLD, resolver, network anchor or
  public service — with its endpoint, raw signal, time in state and a plain
  English explanation of why the classifier gave it that status. Root servers
  keep their letter on small tiles (`ROOT A`, not four identical `ROOT` labels).
- An early field-trial deployment: `scripts/gen_fieldtrial_targets.py` builds a
  749-target config from the IANA root zone (399 ccTLD anchors, 100 network
  anchors, 240 real public services, 10 references); `compose.fieldtrial.yaml`
  and `scripts/deploy_fieldtrial.ps1` deploy it with the local + Tor vantages.
  The deploy script supports `-DryRun`, `-NoBuild`, and image-tag rollback; the
  two-hour scheduler now calls it instead of the legacy standalone deploy.
- Opt-in alert webhooks: `NETWEATHER_WEBHOOK_URL` (and optional
  `NETWEATHER_WEBHOOK_SECRET`) POSTs on incident open and close. Best-effort;
  delivery failures are logged and never block detection.
- DNS-over-TLS (`dot`) instrument on port 853, reusing the DNS wire format
  over a TLS connection, with the same RCODE handling as `dns`.
- Signed batches for remote vantage agents: HMAC-SHA256 over the body,
  verified by the primary when `NETWEATHER_VANTAGE_SIGNING_SECRET` is set;
  optional client-certificate (mTLS) loading on the agent.
- QUIC/UDP-443 middlebox probe: `internal/probe/quic.go` sends a UDP
  datagram to port 443 and reports whether the path is blocked.
- DF-bit path-MTU probe: `internal/probe/pmtu_linux.go` (Linux; stub elsewhere).
- Target provenance in the API detail and the drawer: the seed or config that
  produced a row and the SHA-256 of that payload.
- A config-reload banner that names a rejected edit and the reason; the previous
  target set stays live.
- The optional RIPE Atlas software-probe profile and its README; netweather
  never reads from it.

### Changed

- `/api/v1/meta` capabilities are computed from the wired dependencies, so a
  disabled action (trace, target control, a reader) is not advertised.
- The dashboard's pause and annotation audit lines carry the request id, like
  the API's.
- The compose Tor client points at the sidecar service (`tor:9050`); the
  binary's own default remains `127.0.0.1:9050`.
- An HTTP target may name a request `path`, which is how the robots.txt recipe is
  configured rather than silently rewritten.
- A DNS target's identity now includes its `qname` (migration 0024), so one
  authoritative nameserver can serve many country anchors without collapsing
  them into a single target.

### Fixed

- `svc-cathay` probes the canonical `/cx/en_US.html` page directly. The apex
  redirect alone takes ~10 s, which pushed the full chain past the 15 s timeout
  even though the service is healthy.
- `svc-spirit` now points at Sunwing's live public `/en/` page under the same
  stable id. `www.spirit.com` is NXDOMAIN because Spirit Airlines ceased
  operations in 2026; Sun Country and Air Transat return Incapsula challenges,
  so none of those could be measured as a healthy public service.
- Authoritative DNS anchors (ccTLD, network/glue and root references) no longer
  render as `DOWN` when their apex has NS/SOA records but no A/AAAA. The wire
  client now requires the AA bit and a delegation record, records such an answer
  as `UP_REACHABILITY` ("authoritative answer (zone served)"), and keeps genuine
  `dns_nodata`, NXDOMAIN, SERVFAIL, REFUSED and timeouts as failures.
- The session and CSRF cookies are `Secure` whenever the browser's effective
  scheme is HTTPS, including a trusted proxy's `X-Forwarded-For`/`Proto`
  (`NETWEATHER_UI_SECURE_COOKIES` remains an override).
- The Feeds nav link renders on its own line instead of a literal backtick-n
  on the board, world, corridors, coverage, incidents, IHI, and diagnostics
  pages.
- The content-security policy is sent by the shared middleware, so a static
  asset, icon, or method refusal cannot miss it.
- A CSRF failure returns `403`, matching the documented invariant.
- The Tor vantage bounds each probe at 30s, so a silent circuit cannot stall the
  sequential probe step.
- The shipped `torrc` enables `IsolateSOCKSAuth` and keeps a circuit for an hour.
- A disabled target returns when its config loads again.
- `seed --country-anchors` no longer fails on ccTLDs that share an
  authoritative nameserver; the DNS question is part of the natural key.
- The browser surface renders one generic error page with a request id; internal
  detail stays in the log line that carries the same id.

## Classifier

- No classification rule changed. The recorded fixtures pin the existing table:
  a challenge signature outranks status, a plain 403/429/451 is `BLOCKED`, an
  origin 5xx is `DOWN`, a 404/405/407/410 is `CONFIG_ERROR`, and a status the
  contract admits with a missing body marker is `DEGRADED` rather than `DOWN`.
