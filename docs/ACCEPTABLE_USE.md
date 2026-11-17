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
| Global request ceiling | 20 req/s default | Token bucket in the dialer |
| Outbound ports | 80, 443, 53, 123, 853 | Config load **and** dial time |
| Inbound ports | 2600 only | Single listener; startup assertion |
| Methods | GET, HEAD | Request builder; no generic method parameter |
| Max targets | 2000 | Config/seed validation, reject not truncate |
| Max redirects | 5 | Redirect policy |
| Max body read | 32 KB | `io.LimitReader` + decompression cap |
| Per-stage deadline | DNS 3s, TCP 5s, TLS 5s, TTFB 5s, total 15s | Dialer and probe contexts |
| Traceroute | ≤10 runs/hour, on demand only | Rate limiter, no scheduler path |
| Tor vantage | ≤25 targets, ≥2s spacing, daily MB budget | Vantage policy |
| Daily egress budget | 2 GB default, hard stop | Byte counter in the dialer |
| ICMP | Echo request only, never the sole up/down signal | Instrument policy |
| Private address ranges | RFC1918, loopback, link-local, CGNAT, ULA, metadata IP | Dialer, after DNS and after every redirect |

The daily egress budget is the backstop for everything above it. A misconfiguration that produces 20 req/s forever stops at the budget instead of at the operator's bandwidth bill or the target's abuse desk.

---

## 4. Volume

At the full 750-target fleet with the default tiers (`DESIGN.md` §6.1):

| Tier | Targets | Interval | Probes/day |
|---|---|---|---|
| 0 — reference | 20 | 30s | 57,600 |
| 1 — critical | 80 | 120s | 57,600 |
| 2 — service | 320 | 450s (midpoint) | 61,400 |
| 3 — anchors | 350 | 120s | 252,000 |
| **Total** | **750** | | **~430,000** |

That is ~5 requests/second on average, and **not** a burst: the dispatcher spreads due targets across its 1-second tick and jitter is additive.

Bandwidth is dominated by TLS handshakes, not by bodies:

| Traffic | Estimate |
|---|---|
| Anchor DNS probes (252k × ~0.15 KB) | ~38 MB/day |
| HTTP/TLS probes (~119k × ~6 KB) | ~700 MB/day |
| **Total at full scale** | **~0.7 GB/day ≈ 20 GB/month** |

At the shipped default of ~10 targets this is ~15 MB/day. The fleet number is the honest cost of 750 independent measurements and is worth knowing before seeding a metered VPS.

Two mitigations are built in:

- The reference tier should use a cheap instrument (`tcpping` or `dns`) rather than `http` where the target allows it. Twenty targets every 30 seconds is 57k handshakes a day for a signal that a SYN-ACK answers just as well.
- `probe.daily_mb_budget` stops probing at the limit and shows a banner. Raising it is a deliberate act.

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

The contact must be set by the operator (`probe.user_agent_contact`). Shipping with the placeholder is a startup warning, because an unattributed monitor is indistinguishable from a scanner.

### If someone complains

1. Stop probing that host immediately: `netweather disable <target>` (takes effect on the next tick; no restart).
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

A Tor vantage is **corroboration**, never the primary measurement. It exists to answer "does this look different from another country?" and it is off by default.

Rules:

- TCP-based instruments only; no ICMP, no traceroute, no NTP. **Implemented:** the Tor agent is built from HTTP/TLS/TCP only, and DNS targets are excluded rather than silently failing.
- `socks5h` only. `socks5://` is rejected because it resolves the name locally and leaks the query. **Implemented:** `netx.Socks5` uses x/net/proxy, which sends domain names with `ATYP=3`; a test asserts the proxy sees the hostname.
- Exit-country pinning is required for a country-labelled vantage; an unpinned exit is not a country measurement. **Specified:** the shipped profile is unpinned and therefore labelled `tor`, not a country.
- Expect 40-70% `BLOCKED`. That is the normal result of using Tor, not a fault to be fixed by evasion.
- Volume caps are lower than the direct vantage by design (≤25 targets, ≥2s spacing, daily MB budget). **Implemented:** `NETWEATHER_TOR_MAX` (default 25), a 2-second pace between probes, and a 10-minute interval floor. The daily MB budget is specified.
- The sidecar is client-only. `ExitPolicy reject *:*`, `BridgeRelay 0`, `PublishServerDescriptor 0`, and `ClientOnly 1` are verified at startup. **Specified:** the shipped image is a third-party client-only proxy, and the operator is expected to verify its torrc; the monitor only speaks SOCKS5 to it.

A user-supplied SOCKS5 endpoint is allowed if the operator owns it or has permission to use it. Scraped lists are not. **Implemented:** `NETWEATHER_TOR_SOCKS` accepts any `host:port`, and the only shipped default is the local loopback sidecar.

Only the primary vantage may declare `DOWN`. **Implemented by construction:** Tor probes are stored under vantage `tor`, while the board, incidents, anomalies, and coverage all read the `local` vantage. A Tor failure is therefore evidence, never a verdict.

### RIPE Atlas

The optional `ripe-atlas` profile runs RIPE's software probe. That probe generates its own measurements for RIPE's network, from the operator's IP, under RIPE's terms — not netweather's target list and not netweather's rate limits. It is a separate trust domain and a separate agreement. Turning it on does not change anything in §2 or §3; turning it off does not affect probing.

The `atlas` feed plugin is read-mostly: it reads measurements the operator created (or public anchor data) with the operator's API key, and it spends credits only when asked.

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
