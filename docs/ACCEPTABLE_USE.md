# netweather — Acceptable use

> This tool sends unsolicited requests to third-party infrastructure. That is legal in most places, unwelcome in some, and abusive if done badly. This document states what the tool does on the wire, what it must never do, and where the limits are enforced.

Not legal advice. If you operate this in a jurisdiction with unclear computer-misuse law, or against infrastructure whose terms of service you have not read, that is your decision to make with your own advice.

Companion documents: [`THREAT_MODEL.md`](THREAT_MODEL.md) (what can be attacked), [`DESIGN.md`](DESIGN.md) §14 (the guardrail table), [`NETWORKING.md`](NETWORKING.md) (egress topology).

---

## 1. Position

The tool exists to answer **"is this service reachable from here, and does that differ from what it should be?"**

It does not exist to:

- prove that a service is broken for everyone,
- get past a control that is deliberately refusing it,
- exercise a server's capacity,
- enumerate a host's services,
- reproduce content.

Those four things are what separate a monitor from an attack tool. Every design decision below follows from refusing them.

### The evasion line

A target that returns `403`, a CAPTCHA, a JS challenge, or `429` is **not down**, and the tool records that as `BLOCKED` and moves on.

It does not rotate User-Agents, spoof browser TLS fingerprints, solve challenges, use residential proxies, or retry with a different identity. `CLASSIFIER.md` §12 lists these as anti-patterns; `QUALITY.md` makes the relevant tests fail if a fingerprint-mimicking client is introduced.

This is a deliberate product limitation. A monitor that evades bot protection produces data nobody can interpret, because it can no longer distinguish "the service is down" from "the service noticed me."

---

## 2. What actually goes on the wire

Every instrument is a single request/response exchange with a fixed shape. There are no payloads, no fuzzing, no state changes, and no authenticated requests.

| Instrument | Wire behaviour | Port | Typical bytes |
|---|---|---|---|
| `dns` | One standard recursive query (`A`, `AAAA`, or `SOA`) to the configured resolver | 53/udp, TCP on truncation | ~100–200 |
| `doh` | One HTTPS `GET` to a fixed DoH endpoint with a fixed query parameter | 443 | ~1.5 KB |
| `tcp` | TCP handshake, then close | 80/443 | ~100 |
| `tcpping` | TCP handshake, measure time to SYN-ACK, close | 80/443 | ~100 |
| `tls` | TCP + TLS handshake, read the certificate, close. No application data | 443/853 | ~4 KB |
| `http` | `GET` or `HEAD` on one fixed path, read at most 32 KB, close | 80/443 | ~5–7 KB with handshake |
| `icmp` | One echo request | — | ~100 |
| `ntp` | One client-mode request to a fixed pool server | 123/udp | ~100 |
| `trace` | TTL-limited probes, on demand only, ≤10/hour globally | — | ~30 KB/run |
| `corridor` | Same as `tcp`/`icmp` to a fixed exchange endpoint | 80/443 | ~100 |

Properties that hold for all of them:

- **Methods:** `GET` and `HEAD` only. Never `POST`, `PUT`, `DELETE`, `PATCH`, `OPTIONS`, or `TRACE`.
- **Paths:** a fixed path from config (`/`, `/robots.txt`, `/.well-known/security.txt`, a status JSON). Never crawled, never enumerated, never derived from page content.
- **Redirects:** followed up to 5 hops, and each hop is re-validated (scheme, host, resolved address, port). Never to a private address.
- **No auth:** no credentials, cookies, API keys, or session tokens are ever sent to a probed target.
- **No bodies stored:** the response body is read to a 32 KB cap, matched against signatures, and discarded. One 200-byte summary line is kept.
- **No JS:** nothing is executed. A JS shell is classified `BLOCKED`, not rendered.
- **TLS:** verified. Certificate errors are recorded, not bypassed.

---

## 3. Hard limits

These are the limits a reviewer should check first. Each is enforced at the stated layer, not merely documented.

| Limit | Value | Enforced in |
|---|---|---|
| Minimum interval per host | 30s | Scheduler, clamped after jitter; config cannot go lower |
| Per-host concurrency | 1 | Per-registrable-domain semaphore |
| Global request ceiling | 20 req/s default | Agent step pacing: one probe at a time, spaced by `1/rate`; the transport adds no second limiter |
| Outbound ports | 80, 443, 53, 123, 853 | Config load **and** dial time |
| Inbound ports | 2600 only | Single listener; startup assertion |
| Methods | GET, HEAD | Request builder; no generic method parameter |
| Max targets | 2000 | Config/seed validation, reject not truncate |
| Max redirects | 5 | Redirect policy |
| Max body read | 32 KB | `io.LimitReader` + decompression cap |
| Per-stage deadline | DNS 5s total, TCP connect 5s, TLS handshake 5s, TTFB 10s, total 15s | Instrument contexts and transport timeouts. One DNS timeout retry is enabled by default inside the same 5s window (`NETWEATHER_DNS_RETRIES`, 0–2; `NETWEATHER_DNS_RETRY_DELAY_MS`, 0–200). Only a timeout is retried — negative answers, policy blocks, and malformed replies are final — so lost UDP datagrams do not flip a healthy anchor while request volume stays bounded |
| Traceroute | ≤10 runs/hour, on demand only | Rate limiter, no scheduler path |
| Tor vantages | ≤25 targets and ≥2s spacing per exit, daily MB budget per exit | Vantage policy |
| Daily egress budget | 2 GB default, hard stop | Byte counter in the dialer. **Implemented:** `NETWEATHER_DAILY_MB` (default 2048, `0` disables) wraps the direct vantage's dialer, so every probe and self-check counts; once spent, the agent stops until UTC midnight and the board carries a banner naming the limit |
| ICMP | Echo request only, never the sole up/down signal | Instrument policy |
| Private address ranges | RFC1918, loopback, link-local, CGNAT, ULA, metadata IP | Dialer, after DNS and after every redirect |

The daily egress budget is the backstop for everything above it. A misconfiguration that produces 20 req/s forever stops at the budget instead of at the operator's bandwidth bill or the target's abuse desk.

---

## 4. Volume

At the full 1000-target field-trial fleet with its generated intervals (`DESIGN.md` §6.1):

| Tier | Targets | Interval | Probes/day |
|---|---|---|---|
| 0 — reference | 10 | 600s | 1,440 |
| 1 — critical | 109 | 120s | 78,480 |
| 2 — service | 297 | 300s | 85,536 |
| 3 — anchors | 584 | 120s | 420,480 |
| **Total** | **1000** | | **~586,000** |

That is ~6.8 requests/second on average, and **not** a burst: the dispatcher spreads due targets across its 1-second tick and jitter is additive.

Bandwidth is dominated by TLS handshakes, not by bodies:

| Traffic | Estimate |
|---|---|
| Anchor DNS probes (420k × ~0.15 KB) | ~63 MB/day |
| HTTP/TLS probes (~165k × ~6 KB) | ~1.0 GB/day |
| **Total at full scale** | **~1.1 GB/day ≈ 32 GB/month** |

At the shipped 14 targets this is roughly 50 MB/day, dominated by the 30-second reference target and the three criticals; the rest of the list is on the 300-second tier. The fleet number is the honest cost of 1000 independent measurements and is worth knowing before seeding a metered VPS.

Two mitigations are built in:

- The reference tier should use a cheap instrument (`tcpping` or `dns`) rather than `http` where the target allows it. Twenty targets every 30 seconds is 57k handshakes a day for a signal that a SYN-ACK answers just as well.
- `NETWEATHER_DAILY_MB` stops probing at the limit and shows a banner. Raising it is a deliberate act.

Per host, the ceiling is one request every 30 seconds — 2,880 per day in the pathological case where an operator pins a host at tier 0. No target in the default list is treated that way; tier 0 is the reference set, not the interesting set.

---

## 5. Target selection rules

A target is acceptable if **all** of these hold:

1. It is a **public** service or endpoint. Private, internal, and CGNAT addresses are rejected by the dialer regardless of config.
2. It is reachable **without authentication** and without accepting terms on a per-request basis.
3. The probe path is a **fixed, public** resource: `/`, a status endpoint, `robots.txt`, `security.txt`.
4. The host is not on an explicit opt-out list the project maintains.
5. The port is in the allowlist.
6. The target is not an individual person's site, a forum, a pastebin, a file host, or anything whose availability is not a public-interest signal.

A target is **not** acceptable if it is: a login page behind credentials, an admin panel, a payment endpoint, a search endpoint with query strings, anything that mutates state, anything that requires solving a challenge, or anything the operator does not have a legitimate reason to observe.

Country anchors are ccTLD authoritative nameservers, queried with a `SOA` for the zone apex. That is a query the DNS was built to answer, not a probe of a website.

---

## 6. Prohibited uses

The following are outside the licence and outside the design. Pull requests that add them are rejected without discussion (`§9`).

| Prohibited | Why |
|---|---|
| Load testing, stress testing, "how many RPS can it take" | Not a measurement; causes the outage it claims to detect |
| Port scanning or service enumeration | The tool knows one port per target by construction |
| Credential testing, login attempts, session probing | Attacking, not observing |
| WAF evasion: UA rotation, TLS fingerprint spoofing, CAPTCHA solving, residential proxy pools | Destroys the meaning of `BLOCKED` and is an access-control bypass |
| Scraping, crawling, content reproduction | The tool reads one fixed path and keeps a 200-byte summary |
| Using scraped or discovered proxy lists | Unreliable, non-consensual, and often someone else's compromised host |
| Running Tor as an exit relay, bridge, or onion service | A legal and ethical event the project will not cause |
| Monitoring individuals, dissidents, or private groups | Surveillance, not internet weather |
| Circumventing sanctions or national blocks for access | The tool records a block; it does not defeat one |
| Presenting output as an SLA, a contractual uptime figure, or a safety-of-life signal | It is one vantage point with a confidence model, not an authority |
| Publishing a target list that reveals a third party's private infrastructure | The page is the disclosure |

---

## 7. Abuse handling

### Identification

Probe requests carry a `User-Agent` of the form:

```
netweather/<version> (+https://github.com/<owner>/netweather; abuse: <contact>)
```

The contact must be set by the operator (`NETWEATHER_CONTACT`). **Implemented:** `probe.BuildUserAgent` builds `netweather/<version> (...; abuse: <contact>)` once at startup, the HTTP instrument sends it on every probe and self-check, and the Tor vantage uses the same string. An unset contact logs `event=contact_missing` at `WARN` and sends the literal `<contact>` placeholder, because an unattributed monitor is indistinguishable from a scanner and a silent default would hide the omission.

### If someone complains

1. Stop probing that host immediately: `netweather disable <target>` (takes effect on the next tick; no restart). **Implemented:** `netweather disable <host-or-id>` resolves the target through the API, calls `POST /api/v1/targets/{id}/disable`, and the running scheduler drops it; the row and its history stay, and a config reload can bring it back.
2. Reply with the configured contact and the request pattern from the logs: method, path, interval, source IP/ASN, and the instrument used.
3. If the complaint is about volume rather than existence, widen that target's interval or move it to `UNMEASURABLE` and note why.
4. If the complaint is about a target that was added by a seed, remove it from the seed and record the removal.

The tool keeps enough history to answer "what exactly did you send, and when" — that is what makes a complaint resolvable instead of a fight.

### Opt-out

A host owner who asks not to be probed is removed. The project maintains an opt-out list for seeded targets; an operator removes locally seeded ones. There is no technical mechanism that can enforce this across forks, which is why it is stated as a norm rather than a feature.

### Rate-limit responses

`429` and `Retry-After` are honoured per target: the interval is extended to at least the requested delay, capped at 1 hour, and the state is `BLOCKED` (not `DOWN`). Repeated `429` moves the target toward `UNMEASURABLE`.

---

## 8. Tor and other vantages

A Tor vantage is **corroboration**, never the primary measurement. It exists to answer "does this look different from another country?". The base Compose app enables the unpinned `tor` vantage; pinned `tor-<cc>` sidecars stay idle until `NETWEATHER_TOR_VANTAGES` is set.

Rules:

- TCP-based instruments only; no ICMP, no traceroute, no NTP. **Implemented:** the Tor agent is built from HTTP/TLS/TCP only, and DNS targets are excluded rather than silently failing.
- `socks5h` only. `socks5://` is rejected because it resolves the name locally and leaks the query. **Implemented:** `netx.Socks5` uses x/net/proxy, which sends domain names with `ATYP=3`; a test asserts the proxy sees the hostname.
- Exit-country pinning is required for a country-labelled vantage; an unpinned exit is not a country measurement. **Implemented:** set `NETWEATHER_TOR_EXIT_COUNTRY` for one vantage, or `NETWEATHER_TOR_VANTAGES` to a comma-separated `CC=host:port` list for several. Each code names a separate client-only Tor instance whose torrc pins that exit; the monitor fetches every exit IP through its own SOCKS hop, asks that same Tor process's internal control port for the country from Tor's GeoIP database, uses Cymru only for the ASN, and labels the vantage `tor-<cc>` only when the observed country matches. A mismatch, an unverifiable exit, or a malformed code refuses that one vantage rather than fabricating a country measurement; the other vantages keep running. Without either variable the vantage stays labelled `tor`, which is the shipped default. The field trial pins DE, JP, AU, SG, CL, and MX, giving five continents of corroboration.
- Expect 40-70% `BLOCKED`. That is the normal result of using Tor, not a fault to be fixed by evasion.
- Volume caps are lower than the direct vantage by design (≤25 targets, ≥2s spacing, daily MB budget). **Implemented:** `NETWEATHER_TOR_MAX` (default 25), a 2-second pace between probes, a 10-minute interval floor, and a daily byte budget (`NETWEATHER_TOR_DAILY_MB`, default 50 MB, `0` disables it). Each pinned exit gets its own caps, so one exhausted country does not pause the others; the board shows the summed allowance. The budget is counted on the SOCKS connection itself, so a large body or a retry cannot slip past it; once spent, that agent stops probing until UTC midnight and logs one warning. It is not the operator's kill switch, so `Paused()` stays false.
- The sidecar must be client-only: `ExitPolicy reject *:*`, `BridgeRelay 0`, `PublishServerDescriptor 0`, and `ClientOnly 1`. **Implemented:** the shipped profile is built from this repository's `Dockerfile.tor`, with those four directives baked into `torrc` and pinned by a unit test, so the operator does not have to trust an arbitrary image. `netweather doctor` verifies only what it can, the SOCKS endpoint answering; the app cannot read the sidecar's config, and `socks5h` ensures no name leaks locally.

The seven Tor services are available only through `docker compose --profile tor up -d`.
They build `netweather-tor:local` and the six pinned
`netweather-tor-de/jp/au/sg/cl/mx:local` images. No sidecar publishes a port.
The pinned services stay idle until `NETWEATHER_TOR_VANTAGES` names them:
`DE=tor-de:9050,JP=tor-jp:9050,AU=tor-au:9050,SG=tor-sg:9050,CL=tor-cl:9050,MX=tor-mx:9050`.
The app's unpinned vantage uses `NETWEATHER_TOR=true` and `tor:9050`; country
labels remain untrusted until the real exit is verified through the matching
proxy.

A user-supplied SOCKS5 endpoint is allowed if the operator owns it or has permission to use it. Scraped lists are not. **Implemented:** `NETWEATHER_TOR_SOCKS` accepts any `host:port`, and the only shipped default is the local loopback sidecar.

Only the primary vantage may declare `DOWN`. **Implemented by construction:** Tor probes are stored under `tor` or `tor-<cc>`, while the board, incidents, anomalies, and coverage all read the `local` vantage. A Tor failure is therefore evidence, never a verdict. The target drawer and API expose each corroborating vantage's verified country and continent so a regional disagreement can be read as such.

### Provider status and BGP evidence feeds

The field-trial overlay enables three verified public Statuspage JSON endpoints
(Twilio, Telnyx, and Verizon) and the bounded RIPEstat BGP-updates poller for
nine public operator ASNs. Both are read-only GETs to published endpoints; no
numbers, SIMs, CPaaS credentials, webhooks, SMS, or voice traffic are involved.
Statuspage rows are stored as `provider-reported` evidence: the operator's own
statement about the components on that page. RIPEstat rows are stored as
`ip-routing-corroboration`: an IP network's prefixes changed in BGP. Neither is
a measured telephony health signal and neither can declare a target `DOWN`.
Operator scope stays with the telecom fleet targets; unconfigured operators
stay unconfigured rather than being guessed from a hostname.

Verizon's publicly served Statuspage page currently labels its components as
examples, so it is not mapped to a target's `status_url`; the target drawer only
shows a Provider status panel where an operator-supplied page is declared. The
real-time RIS WebSocket remains opt-in and is not enabled by the field trial.

### RIPE Atlas

The optional `ripe-atlas` profile runs RIPE's software probe. That probe generates its own measurements for RIPE's network, from the operator's IP, under RIPE's terms - not netweather's target list and not netweather's rate limits. It is a separate trust domain and a separate agreement. Turning it on does not change anything in §2 or §3; turning it off does not affect probing.

The `atlas` feed plugin now ships as a read-only adapter (`internal/feeds/atlas`):
it polls the results of the measurement ids the operator names
(`NETWEATHER_ATLAS_MEASUREMENTS`), with an optional operator key
(`NETWEATHER_ATLAS_KEY`) for private measurements. It reads only the results
endpoint, which costs no credits, and never creates a measurement. An Atlas
result is a measurement from RIPE's probes, not an outage verdict about one
of this monitor's targets, and it carries no target id to map onto, so it
appears as evidence in the feed strip and corroboration lists rather than as
a verdict. Corridors do not depend on it (they are configuration-driven).

---

## 9. Contributing

Changes are judged against this document, not only against the tests.

**Rejected on sight:**

- Anything that makes the tool harder for a target to identify as a monitor
- Anything that increases request volume without a measurement justification
- New outbound ports, new HTTP methods, or a generic "send this request" escape hatch
- Scraping, crawling, or proxy-list discovery
- Anything that stores a response body

**Required with any change to limits:**

- A test proving the limit holds at the new value
- A line in `DESIGN.md` §14
- A note in this document if the change alters what leaves the container

The guardrail tests in `QUALITY.md` §5 are the enforcement mechanism. A PR that raises the 30-second floor without touching them will fail CI, which is the intended outcome.

---

## 10. Change control

| Version | Change |
|---|---|
| 0.1 (design) | Initial position: classification not evasion; allowlisted ports; 30s floor; no bodies stored |

Material changes to this document require a version bump and a note in the changelog. The limits in §3 are code, not prose; this document explains them and can lag a release, but the code is authoritative.
