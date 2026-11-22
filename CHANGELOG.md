# Changelog

Behaviour changes are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
`<major>.<minor>.<patch>` tags from the release job. The detailed record of
every commit is the git history; this file exists so an operator can read what
changed between two images without it.

## [Unreleased]

### Added

- The board gains a **Region** grouping lens and `?region=` filter. Continents
  are derived from each target's existing country code by a checked-in map
  generated from public-domain Natural Earth 10m data; there is no schema change,
  no config field, and no runtime fetch. Unmapped and non-geographic codes get
  an explicit "Region unknown" bucket. The label is a grouping fact, not a
  measured hosting location.
- `/hubs` adds a contiguous-US Albers equal-area projection above the treemap.
  The 34 city aggregates reuse their published approximate city-centre anchors,
  now projected through the same frame as the same-origin Natural Earth 110m
  state-outline SVG. Markers carry worst-state colour, endpoint count, and an
  explicit "approximate city centre, not measured hosting location" hover title;
  clicking one jumps to its city block. An unanchored label stays in the treemap
  and is counted in a note. No scripts, third-party assets, or runtime fetches.
- `/corridors` adds a self-contained equirectangular projection above the table.
  It places only the configured operator `from`/`to` labels at approximate
  city-centre anchors and draws a straight line between known pairs, split at
  the antimeridian for Pacific corridors. Hover or focus text repeats that the
  labels are not measured locations and the line is not a physical or undersea
  cable route. No scripts, third-party assets, or runtime map fetches are added.
- `/world` keeps its country-tile grid and adds a self-contained equirectangular
  projection above it. Country circles are placed at public geographic centres
  from `mledoze/countries`; diamonds are approximate city-centre anchors for the
  existing `City, ST hub — site` labels. Markers reuse the board's state classes
  and glyphs, and each has an accessible hover/focus label with its identity,
  target count, worst state, and feed reason where one exists; every marker links
  back to the matching board or hub view. Hub titles say plainly that an anchor
  is not a verified hosting location. The base map is a simplified public-domain
  Natural Earth silhouette served from `/static`, so no third-party script, map
  service, or data fetch is needed.
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

- `/corridors` now shows the operator's readable path label as the corridor
  name, keeps the stable id underneath, and shows the real configured endpoint
  pair in the Path column. Both cells have hover text that separates the
  measured hosts from the approximate CDN labels; when no endpoints are
  configured the page says so instead of inventing a host.
  `scripts/check_fieldtrial.sh` now checks those names and endpoint hovers too.
- The coverage page no longer calls a 100%-measured five-target sector `low`.
  `low` now means under 80% measurable; `small` means an adequately measured
  sector with fewer than 20 measurable targets, so a thin but fully measured
  sector is not described as weak.
- The target drawer is no longer capped at the login card's 26rem width. Its
  metadata and evidence tables now use the page width, with slightly larger
  text so probe details are readable without zooming.
- Service targets were rebalanced into the thin airline, education, health, and
  social sectors using real public endpoints under their existing ids. The fleet
  stays at 749 targets and schema 25; no per-target city or state fields were
  added.
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

- Six field-trial service endpoints that were unjudgeable from the local egress
  (Turkish Airlines, HPE, Facebook, Best Buy, Uniqlo, and The Washington Post)
  now point at real public pages that answer with assessable HTML from the local
  egress and all six pinned Tor exits with the production User-Agent and URL
  shape: Finnair, Oracle Cloud, Flickr, Dollarama, Barnes & Noble, and CBS News
  respectively. Stable ids, sectors, and the 749-target fleet are unchanged.
  Sam's Club and Walmart Canada were rejected after Akamai returned 418 when
  the Host header carried the explicit `:443` that netweather probes use.
- `REGIONAL` now requires the **primary** vantage to be down while a
  corroborating vantage is up, matching the target drawer and `DESIGN.md`.
  Previously any down/up split across two vantages lit the top-bar badge, so a
  healthy primary with a down corroborator could show `REGIONAL` with no drawer
  explanation. Stale schema/quality doc references to REGIONAL as an incident
  verdict or a correlate test were corrected.
- A bare Imperva `x-iinfo` response header no longer marks a healthy page as
  `BLOCKED`. Imperva stamps that header on every reply, including a 200/206 with
  assessable content; the classifier now treats it as a challenge signature only
  on a non-2xx status. Real challenge bodies, redirects and statuses still win.
  The US hub portals behind `svc-nytimes` and `svc-tesco` can now reach
  `up_reachability` instead of being permanently unmeasurable.
- `svc-britishairways` now points at Ethiopian Airlines' public page under the
  same stable id. British Airways' apex times out awaiting HTTP/2 response
  headers from three of the six Tor exits on both `/` and `/robots.txt`, so no
  path override could make it measurable. Ethiopian Airlines returned assessable
  HTML from all six exits; the fleet stays at 749.
- `/incidents/{id}` uses the wide target-detail card instead of the narrow login
  card, so attribution, corroboration and archived-probe tables are readable.
- Service slots that consistently produced stream errors, `202`, bot challenges,
  or configuration errors were replaced with assessable public pages under the
  same ids: airline/travel, ITU, Telstra, diabetes, hospital and university
  sites, and two social platforms. No retry used a different identity and no
  failed endpoint was relabelled as healthy.
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
