# netweather — Design

> Working codename. Single container, single binary, self-hosted.
> Status: design complete enough to implement. Open questions in §16.

---

## 1. Purpose

Answer three questions, in this order of importance:

1. **Is something large happening on the internet right now?** (cyberwar, mass outage, cable cut, BGP hijack, national shutdown)
2. **Is a specific service or sector down?** (banks, airlines, cloud, socials)
3. **Is it me?** (my ISP, my DNS, my egress, my container)

Most monitoring tools answer only #2, and answer it badly — they report `curl != 200` as "down." That produces so many false alarms that the tool becomes noise.

The ordering matters. #3 is checked first, because if your egress is broken, every other answer is meaningless. #1 is the headline, because a mass event is the only thing worth waking up for.

### Non-goals

- Not a Downdetector clone. You are one vantage point; the UI must never claim global truth.
- Not a homelab service monitor. Use Uptime Kuma or Gatus for "is my NAS up."
- Not a scraper, not a load tester, not a port scanner. See `ACCEPTABLE_USE.md`.
- Not an uptime SLA system. No paging, no on-call, no SLOs.
- Not a replacement for official status pages. It cross-checks them.

### Design principles

| # | Principle | Consequence |
|---|---|---|
| P1 | **Be honest about what you cannot see** | `BLOCKED`, `UNKNOWN`, and `UNMEASURABLE` are first-class states |
| P2 | **Classify bot protection, never evade it** | No TLS impersonation, no headless browser by default |
| P3 | **One vantage point cannot prove an outage** | Every verdict carries a confidence and a corroboration source |
| P4 | **Breadth beats depth** | 350 ASNs across 100 countries is worth more than 1000 brand domains |
| P5 | **Silence is a failure mode** | Every panel shows staleness; a dead feed is visible, not blank. **Implemented:** the board carries a feed-health strip (`stale`, `never`, consecutive failures), and a missing or failed feed source says so rather than rendering as healthy. |
| P6 | **Never cry wolf** | Baseline-relative thresholds; `BLOCKED` excluded from health |
| P7 | **Boring, small, portable** | Go, SQLite, one binary, ~20MB image, read-only rootfs |

---

## 2. What "outage" means here

A naive monitor conflates these. This tool separates them:

| Observation | Naive says | netweather says |
|---|---|---|
| `503` from origin | DOWN | **DOWN** |
| `403` + `cf-mitigated: challenge` | DOWN | **BLOCKED** — site is fine, we are not welcome |
| `200` with a JS shell | UP | **UP (reachability only)** — low confidence, stated as such |
| `429` + `Retry-After` | DOWN | **BLOCKED** — rate limited |
| All 40 targets fail at once | 40 outages | **LOCAL** — your egress is broken |
| 8 targets fail, 7 behind Akamai | 8 outages | **INCIDENT — Akamai edge** |
| DNS NXDOMAIN, DoH agrees | DOWN | **DOWN** (domain gone) |
| DNS NXDOMAIN, DoH disagrees | DOWN | **TAMPER** — possible DNS interference |
| Cert issuer changed | UP | **TAMPER** — possible interception |
| 404 on probe path | DOWN | **CONFIG_ERROR** — your config is wrong, not their service |

The whole product is this table executed correctly, with confidence levels attached.

---

## 3. State model

### 3.1 Per-target state

Six values. No more.

| State | Meaning | Counts as |
|---|---|---|
| `UP` | Passed the probe and any configured semantic check | healthy |
| `UP_REACHABILITY` | Responded correctly, but no semantic check configured | healthy, low confidence |
| `DEGRADED` | Reachable, but semantic check failed, or latency ≫ its own baseline | healthy, flagged |
| `DOWN` | Confirmed failure (see confirmation rules) | unhealthy |
| `BLOCKED` | Deliberate refusal: WAF, challenge, auth wall, rate limit | excluded |
| `UNKNOWN` | No recent data: paused, disabled, or not yet probed | excluded |

### 3.2 Per-target classification (orthogonal to state)

| Flag | Meaning |
|---|---|
| `measurability: ok` | Has produced a real measurement in the last 7 days |
| `measurability: unmeasurable` | The "mostly blocked" condition: `BLOCKED` > 80% of probes over 7 days. Excluded from health and coverage. |
| `confidence: high` | Transport resolved and a semantic assertion passed, or an unambiguous challenge / origin failure |
| `confidence: medium` | Single-signature challenge, semantic mismatch, or a DNS-derived `DOWN` pending corroboration |
| `confidence: low` | Reachability only, a JS shell, a flapping target (>6 transitions/24h), or a single observation |

**UNMEASURABLE targets are excluded from the health index and collapsed into their own board section.** This is the single most important calibration decision: it prevents 200 permanently-bot-walled banks and airlines from making the dashboard look permanently on fire.

**Implemented:** `store.RefreshMeasurability` recomputes the rolling share every maintenance tick. It reads `rollup_5m` rather than raw probes, because raw rows are pruned at 72 hours and a "seven-day" share computed from them would silently be a three-day share. A target with at least 10 probes in the window and a BLOCKED share above 80% is set to `measurability = unmeasurable`; when the rolling share falls back to 80% or below it returns to `ok`, so a lifted wall is not permanent. The board moves a target whose measurability is `unmeasurable` into the quiet section even while its last confirmed state is `BLOCKED`, and the scope, coverage, and verdict queries already excluded that value from health and from the judgeable denominator.

### 3.3 Global verdict

Exactly one, shown in the top bar.

| Verdict | Trigger | UI behaviour |
|---|---|---|
| `OK` | Health within baseline band | Calm, one line of text |
| `INCIDENT` | Health below baseline, or attribution found a dominant cluster | Name the cluster |
| `LOCAL` | ≥2 of ≥3 reference groups failing | **Suppress per-target red.** One banner. |
| `REGIONAL` | Same target `DOWN` from one vantage, `UP` from another | Show both; never call it global |
| `TAMPER` | DNS anomaly **plus** a second independent signal, or a certificate anomaly confirmed | Distinct, high-severity styling |
| `STALE` | No probe cycle completed in 3× the reference interval | Prominent warning |
| `STARTING` | < 1 full cycle completed | Skeleton UI |
| `LEARNING` | < 28 weekly samples for the current hour-of-week bucket | Health shown, no `INCIDENT`; "learning 12/28 samples" |

`DNS_ANOMALY` is deliberately **not** a global verdict. A single resolver disagreeing with its own history is advisory: it appears in the drawer and a collapsed observations strip, never on the top bar. Promoting it to `TAMPER` requires a second signal (`CLASSIFIER.md` §3.2).

`LOCAL` does not overwrite per-target state - targets keep `DOWN` internally. While the verdict holds, the board renders a `DOWN` tile muted with a "your connection" label instead of red. This preserves history while preventing panic. `target_state.attributed_to` is reserved for a stored attribution and stays `NULL`: the mute is presentation, not a state write, so a read never mutates the database.

**Implemented:** `internal/verdict` is the table as a pure function, and the board computes it per request from signals that are already on the page: the newest confirmed probe (`STALE`), the confirmed target count (`STARTING`), the newest high-confidence DNS `TAMPER` verdict in the last hour, reference-class failures (`LOCAL`, two of three or more), a target `DOWN` on one vantage and up on another (`REGIONAL`), open incidents (`INCIDENT`), and the global scope's usable baseline samples (`LEARNING`). The badge is in the top bar with the sentence as its title, `STALE` also raises a banner, and `/api/v1/board` carries the same value and sentence in `meta.verdict`.

Priority is explicit because two signals can be true at once: `STALE` outranks every interpretation (a blind monitor cannot speak), `TAMPER` outranks an outage, `LOCAL` outranks the cluster it may be casting, and `INCIDENT` outranks `LEARNING`. A failed read is an absent signal, never evidence: the builder does not invent a failure, a tamper, or a local-path claim from a broken query.

Two limits are stated rather than hidden. The `TAMPER` verdict accepts the DNS ladder's `TAMPER`, which is already the second-signal verdict from `CLASSIFIER.md` §3.2; a certificate anomaly alone remains a drawer signal. And `LOCAL`'s "suppress per-target red" is a presentation rule: while the verdict holds, a `DOWN` tile keeps its confirmed state but renders muted with a "your connection" label, so the operator reads the path before the service; nothing about the stored state or the history changes.

---

## 4. Target classes

Four classes, each with a different job. This is the fix for bad ASN distribution.

| Class | Default count | Job | Typical ASNs |
|---|---|---|---|
| **Service** | 400 | "Is Chase / Delta / AWS down?" | concentrated (CDNs) |
| **Country anchor** | 250 | "Is country X still on the internet?" | ~250 distinct, in-country |
| **Network anchor** | 100 | "Is the DNS/peering fabric sane?" | root letters, resolvers, IXPs |
| **Reference** | 10–100 | "Is it me?" | deliberately diverse, must be stable |

External feeds (§8) are a fifth data class but are not probes.

### 4.1 Service targets

Sector taxonomy, two levels. The folder path *is* the taxonomy (§13.2).

| Sector | Sub-sectors |
|---|---|
| Financial | banking, payments, brokerage-crypto |
| Travel | airlines, hotels, otas, rail |
| Cloud & Infra | hyperscalers, cdn-edge, devtools |
| Commerce | marketplaces, retail |
| Social | networks, messaging |
| Media | streaming, news |
| Productivity | workspace, saas |
| Comms | isps, dns, email |
| Government | gov-services |
| Health | providers, pharmacies |
| Reference | baseline, corridors |

25–35 targets per sector. Depth beyond that adds no signal — the third-largest bank in a country does not fail independently of the first.

### 4.2 Country anchors — the key idea

Probe each country's **ccTLD authoritative nameservers**.

Why this works:

- Every ccTLD has stable, published NS records (from the IANA root zone)
- They are designed to be queried by anyone, at any rate a resolver would
- They are mostly in-country or regionally anycast
- A DNS probe costs ~100 bytes and one UDP round trip
- ~250 ccTLDs ≈ 250 country-level probes for less bandwidth than 10 HTTP probes

Auto-seed: fetch `https://www.internic.net/domain/root.zone`, extract ccTLD NS records, emit one target per country/territory.

**Implemented:** `netweather seed --country-anchors` performs that one fetch and writes the anchors. The parser (`internal/seed.ParseCountryAnchors`) accepts the master-file shapes that matter for NS records, keeps only two-letter TLDs and IDN ccTLDs (`xn--...`), ignores `com`/`arpa`/multi-label names, and picks the lexicographically first nameserver per territory so two runs against the same zone produce the same list. Each anchor is a `dns` target on port 53 whose query is the TLD, class `country`, sector `country`, tier 3. The fetch is an explicit command rather than a startup side effect, because it is an outbound request to a third party; `NETWEATHER_ROOT_ZONE_URL` points it at a mirror and `NETWEATHER_CONTACT` identifies it. The command refuses an empty parse instead of reporting success with no targets. A nameserver that is not a hostname is skipped, and a seed list over the 2000-target cap is refused wholesale rather than truncated (THREAT_MODEL.md T33/T34). The fetch itself is HTTPS-only; `file://` names a local mirror and no other scheme or host is accepted, every redirect hop is re-checked so a 302 cannot move the fetch to plaintext, userinfo in the URL is refused, and a payload larger than 8 MiB is an error rather than a truncated zone that would parse into a smaller anchor list. Before any row is written, every entry is validated: a lower-case slug id, a known class and tier, a supported probe with an allowlisted port, a hostname (or an IP literal only for a non-service class, so a CDN edge is never pinned), no wildcard or userinfo, a DNS question for `dns`/`doh`, an HTTPS URL when one is given, and no duplicate id or natural key. A failure names the entry and refuses the whole apply rather than writing part of it.

Limitation to document honestly: a DNS probe from your house measures *the path from your house to that country's DNS*, which conflates your ISP's peering with that country's health. Mitigations:

- These anchors are reachability measurements by construction: a DNS probe has no content assertion, so its confidence is the classifier's reachability path rather than `high`. There is no separate `path: transit` field; the drawer and `CLASSIFIER.md` name the transit-path limitation instead, and the confidence value is what a reader can filter on.
- Cross-check with IODA (§8.1), which has real in-country vantage points
- Optional: RIPE Atlas (§8.6) for genuine in-country measurement

### 4.3 Network anchors

- 13 root server letters (`a.root-servers.net` … `m.root-servers.net`) — TCP/53 + DNS query
- Major public resolvers (1.1.1.1, 8.8.8.8, 9.9.9.9, Quad9, OpenDNS)
- IXP route servers where publicly queryable
- A handful of large ISP resolvers per region

Purpose: detect DNS-layer and peering-layer events that service probes cannot see.

### 4.4 Reference targets — the "is it me?" set

Grouped, and the grouping is what the LOCAL verdict keys on:

| Group | Example | Probes |
|---|---|---|
| `ref-doh` | 1.1.1.1 DoH, dns.google DoH | HTTPS JSON |
| `ref-dns` | 1.1.1.1:53, 9.9.9.9:53 | UDP DNS |
| `ref-static` | Cloudflare/Fastly static asset | HTTPS GET |
| `ref-http` | example.com, a neutral well-known site | HTTPS GET |

Diversity is the point: different ASNs, different protocols. If two of these four groups fail simultaneously, it is not the internet — it is you.

### 4.5 ASN enrichment

Every target gets `asn`, `as_country`, `prefix` via **Team Cymru DNS** (`origin.asn.cymru.com`, `asn.cymru.com`) — no GeoIP database in the image, two cached DNS queries per target. Refreshed daily and on IP change.

**Implemented (migration-era phase 0):** `internal/enrich` resolves the target through the guarded dialer, asks `origin.asn.cymru.com` / `origin6.asn.cymru.com` over DNS to `1.1.1.1:53`, and records the ASN and covering prefix. It also asks `AS<n>.asn.cymru.com` for the registered organisation name and country (`as_name`, `as_country`), and maps the CNAME chain to a known CDN suffix (Cloudflare, Akamai, CloudFront, Fastly, Azure, Google, CDN77). A direct answer leaves `cdn` empty rather than guessing, and a missing organisation answer leaves the labels empty rather than inventing them. `NETWEATHER_ENRICH=false` disables the loop; it runs once at startup and every six hours, paced at 150 ms per target.

Implemented, including the daily recheck: a learned target older than 24 hours is re-resolved, and if its address leaves the learned prefix the ASN, prefix, CDN, and AS labels are refreshed. If it is still inside the prefix, the row is only touched, so a no-op update cannot blank the labels. PeeringDB exchange labels are learned from the same pass and cached per ASN (§8.5), so the drawer can answer "which exchanges does this network sit at?" without a request per target. Empty enrichment in the drawer reads "not learned yet", so a gap is visible instead of fabricated.

This powers the coverage panel (§10.6) and the attribution engine (§7.3).

---

## 5. Probes and instruments

For HTTP targets, four stages run in order, recording which stage failed. Cheap, and it converts "red square" into a diagnosis. Other target classes use a subset of these — see §5.1.

```
DNS ──► TCP ──► TLS ──► HTTP ──► semantic
 │       │       │        │
 │       │       │        └─ status, headers, body marker
 │       │       └─ handshake, chain, issuer, SPKI pin
 │       └─ connect :443 (or :80 / :53 per probe type)
 └─ resolve A/AAAA; compare against DoH consensus
```

### 5.1 Probe types

Protocol choice is a **diagnostic** choice, not a coverage choice. Each instrument answers a different question with the fewest packets and the least ambiguity. Use the cheapest instrument that answers the question — and never infer more than it can tell you.

| Type | Protocol | Answers | Cost | Used for |
|---|---|---|---|---|
| `dns` | UDP/53 → TCP/53 | "does the name resolve?" | ~100 B | country anchors, network anchors |
| `doh` | HTTPS + JSON | "does the name resolve *honestly*?" | ~1 KB | reference, tamper checks (§3.2) |
| `tcp` | TCP connect | "is the port accepting?" | ~300 B | reachability-only targets |
| `tls` | TLS handshake | "is the edge serving a valid cert?" | ~2–5 KB | banks, airlines, cert monitoring |
| `http` | HTTP(S) GET | "is the app serving the right thing?" | ~1–3 KB | service targets |
| `icmp` | ICMP echo | "is the host answering, and how fast?" | 32 B | **anchors and corridors only** |
| `tcpping` | TCP connect timing | same, where ICMP is filtered | ~300 B | corridors, default RTT instrument |
| `ntp` | UDP/123 | "is my clock right?" | 48 B | self-health (§7.5) |
| `trace` | ICMP/UDP TTL walk | "**where** is it broken?" | ~90 pkts | on anomaly or on demand only |
| `quic` | UDP/443 | "is UDP/443 filtered?" | ~1 KB | phase 3, middlebox detection |
| `corridor` | 5× icmp/tcpping pair | "did an intercontinental path step?" | ~1.5 KB | §9 |

Not every target needs HTTP. A bank that bot-walls you is better measured with `tls` + `robots.txt` than with a fake browser (§5.3).

**Implemented:** `dns`, `doh`, `dot`, `quic`, `tcp`, `tcpping`, `tls`, `http`, `icmp`, `ntp`, and on-demand `trace` are instruments in `internal/probe`, dispatched by `agent.Instruments`. The `doh` instrument is one HTTPS `GET` to a JSON endpoint (`?name=…&type=A`) through the same guarded dialer as every other probe; it requires `qname`, accepts an explicit `url` because providers disagree on the path (Google `/resolve`, Cloudflare `/dns-query`, and the default `https://host:port/dns-query` otherwise), refuses a plaintext endpoint, does not follow redirects, and reports NXDOMAIN as a real DOWN while SERVFAIL, a malformed answer, and a missing A record are UNKNOWN rather than an outage. The `dot` instrument (DNS over TLS, port 853) reuses the DNS wire format over a TLS connection and reports the same RCODEs. The `quic` instrument is a UDP/443 middlebox reachability probe (not a full QUIC handshake). `corridor` is not a per-target instrument but the corridor sampler in §9.

### 5.2 HTTP probe specifics

- **`GET`, not `HEAD`.** Many sites 405/403 `HEAD`. Use `GET` with `Range: bytes=0-2047` when no body marker is configured. **Implemented:** the agent asks for the 2 KiB range only when the target has no content assertion and its status contract admits `206`; a compliant server answers `206`, and `PartialOK` admits it as a success. A target with a body or header assertion keeps the full body, and a named status list without `206` disables the range so a partial response cannot masquerade as the code the operator asked for.
- Identifying headers, but **no fingerprint spoofing**: the operator `User-Agent` (see below) and `Accept: */*`. No browser-specific `Accept-Language`, and no header order tricks. The transport negotiates `Accept-Encoding: gzip` and decompresses, so the classifier sees text rather than gzip bytes.
- **Identify yourself.** `User-Agent: netweather/0.1 (+https://github.com/<owner>/netweather)`. This is both polite and a deliberate anti-evasion stance: operators can see who you are and block you if they want.
- Follow ≤5 redirects. Re-run the private-IP check **on every hop** (§14).
- HTTP/2 where offered. No TLS session-ticket tricks beyond the standard client cache.
- 15s end-to-end ceiling, shared by the Tor vantage (its 2s probe spacing is the slower constraint). Body read cap 32 KB, or 2 KiB when only status and headers are needed.

### 5.3 The robots.txt trick

For hard targets (banks, airlines, socials), the most bot-friendly path on the internet is `/robots.txt`. It is small, static, cacheable, explicitly published for automated clients, and rarely behind a challenge.

Default recipe for targets that are mostly blocked:

1. `GET /robots.txt` → expect `200` or `404` (both prove the origin is serving)
2. If blocked, fall back to `TLS`-only probe
3. If that fails, mark `UNMEASURABLE` and stop wasting requests

This measures **reachability** honestly instead of pretending to measure content.

**Implemented as operator guidance, not an automatic rewrite.** netweather does not silently change a target's URL or instrument: the calibration marks a permanently blocked target `unmeasurable` (so it stops dragging health down), and an operator who wants the robots or TLS recipe configures it as an override (`DESIGN.md` §13.2). For the first step that is `path: /robots.txt` on an `http` target; `path` is http-only, must start with a single `/`, and may not carry a scheme, authority, query, fragment, or whitespace. For the second it is `probe: tls`. Rewriting the probe would change what the target measures without saying so, which is the kind of quiet reinterpretation the project rejects.

### 5.4 What each instrument cannot tell you

Every protocol has a blind spot, and most false alarms come from forgetting them.

| Instrument | Blind spot |
|---|---|
| **ICMP** | Dropped or deprioritized by most CDNs, many ISPs, and default cloud security groups. Inflated RTT is normal because ICMP queues below data traffic. **Never the sole up/down signal for a service target.** |
| **TCP connect** | Proves the port answered, not that the app works. A load balancer answering is not a healthy origin. |
| **TLS** | Proves the edge serves a certificate. Terminating CDNs answer happily while the origin is dead. |
| **HTTP status** | Proves *a* response. Says nothing about whether the app behind it functions — hence semantic checks (§7 of `CLASSIFIER.md`). |
| **Traceroute** | A `*` hop is not loss; routers rate-limit and deprioritize TTL-exceeded. ECMP makes traces non-deterministic beyond a few hops. Treat as a hint, never as proof. |
| **NTP** | Tells you about your clock, not about the internet. |
| **DoH** | Bypasses your local resolver, so it cannot tell you your resolver is broken — only that the name resolves elsewhere. |

The rule: **a probe type may only move a target to `DOWN` if the protocol was chosen for that purpose.** ICMP and traceroute can raise a suspicion; they cannot conclude an outage.

### 5.5 Container capabilities

ICMP and traceroute need raw sockets, which containers do not have by default. Two options, in preference order:

1. **Unprivileged ICMP** — set `net.ipv4.ping_group_range` to include the container's GID. Works with `golang.org/x/net/icmp` datagram sockets, no `CAP_NET_RAW`.
2. **`CAP_NET_RAW`** — required for traceroute, and for ICMP on some platforms.

Degradation is mandatory:

- `netweather doctor` reports which instruments are available
- `icmp` probes fall back to `tcpping` transparently
- `trace` is disabled with a visible reason, not an error
- **The monitor must never fail to start because of a missing capability**

**Implemented:** `probe.ICMP` sends five echoes, reports replies, loss and mean RTT, and classifies `icmp_timeout` / `icmp_loss_partial` / `icmp_unreachable` / `icmp_unavailable`. Only a target marked `icmp_anchor` can be classified DOWN from a timeout; partial loss is `DEGRADED` and a missing capability is `UNKNOWN`, because a visibility failure is not an outage. A hostname resolves through the guarded dialer.

`probe.Trace` walks IPv4 ICMP TTLs and reports each hop or a star. The API action `POST /api/v1/targets/{id}/trace` is capped at ten runs per hour across the instance and answers `503` with the socket error when the capability is missing - the process itself starts normally. `compose.trace.yaml` adds `CAP_NET_RAW` as an overlay; the default file grants no capabilities. A target not answering is `200` with `reached: false`, not an error.

**Implemented:** `netweather doctor` reports data-directory writability, resolver reachability, TCP egress, TLS, HTTP, clock offset, and whether raw ICMP and traceroute sockets can be opened. Optional capabilities are warnings; only a fatal capability (unwritable data directory, no resolver, no TCP egress, a broken allowlist) exits non-zero.

`tcpping` is the portable RTT instrument: it reuses the TCP connect path, needs no raw socket, and is selected explicitly with `probe: tcpping`. It is not an automatic fallback from `icmp`, because silently connecting to a guessed port on an anchor would fabricate a measurement; an `icmp` target on a host without raw sockets reports `UNKNOWN` / `icmp_unavailable` instead. Windows and macOS hosts differ enough that this must be documented rather than guessed.

### 5.6 Vantage points

A vantage point is **where a probe originates**. Today there is exactly one (your container). That is the project's biggest limitation: one box cannot distinguish "down" from "down for me."

Vantages make that explicit, and let you add more without shipping a second container to a friend's house.

```yaml
vantages:
  - id: local
    type: direct
    role: primary
    timeout: 8s
    instruments: [dns, doh, tcp, tls, http, icmp, tcpping, ntp, trace]

  - id: tor-de
    type: socks5
    proxy: socks5h://tor:9050   # sidecar service name; 127.0.0.1:9050 only if sharing a netns
    exit_country: de
    role: corroboration
    timeout: 25s
    instruments: [tcp, tls, http, doh]
    max_targets: 25
    min_interval: 120s
    daily_budget_mb: 40
```

Rules:

- **Exactly one `primary`.** Only the primary can move a target to `DOWN`. Everything else corroborates.
- **Baselines are per-vantage.** Never mix latencies from different paths.
- `REGIONAL` is emitted when primary and a corroborating vantage disagree about the same target in the same window (§7.1).
- Instruments are declared per vantage, because a vantage may not support all of them (§5.7).
- **Timeouts are per-vantage.** A Tor handshake routinely exceeds the 8s that is generous on a direct path.

**Implemented, the shipped configuration surface:** the target file has no `vantages:` block. The direct vantage is implicit (`local`, `direct`, `primary`), and the Tor vantage is enabled by environment: `NETWEATHER_TOR=true`, `NETWEATHER_TOR_SOCKS` (default `tor:9050`), an optional verified `NETWEATHER_TOR_EXIT_COUNTRY`, `NETWEATHER_TOR_MAX` (default 25 targets), and `NETWEATHER_TOR_DAILY_MB` (default 50 MB). The agent applies the 2s pace and ten-minute interval floor from §5.7. The YAML above is the intended general shape, not input the binary accepts today: a second source of truth for the same knobs is the drift `DESIGN.md` §13.2 rejects. The rules still hold — a non-primary vantage is stored under its own vantage id and can never move a target to `DOWN`.

#### The abstraction: one dialer

The pattern worth borrowing from `proxychains` is not the tool — it's that the transport is **transparent to everything above it**. Copy that as an internal interface:

```go
type Dialer interface {
    DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}
```

Every instrument takes a `Dialer`. A vantage is nothing more than a dialer plus a policy (instruments, rate, budget, timeout, baseline). `direct` wraps `net.Dialer`; `socks5` wraps a SOCKS5 dialer. No probe type contains Tor-specific code.

Four deliberate constraints:

- **Never chain proxies.** `proxychains` supports `chain_len=3`; for measurement that is poison — it multiplies latency and variance and tells you nothing about any single path.
- **Dial by hostname through the proxy**, never a pre-resolved IP. Otherwise DNS leaks to your resolver and the measurement becomes local, not remote.
- **`socks5h://` only.** `socks5://` resolves locally and defeats the vantage; config validation rejects it.
- **Address the sidecar by service name** (`tor:9050`), never `127.0.0.1:9050` — that is the sidecar's own loopback. See `NETWORKING.md` §9.

### 5.7 Tor / SOCKS5 vantages

Tor is the cheapest way to get a second vantage point, and the only one that lets you choose the country you appear to come from.

Why it matters here:

- A different path proves the outage is not yours
- Exit-country pinning (`ExitNodes {de}`, `StrictNodes 1`) approximates "probe as if from Germany" — the only single-box answer to "is this regional?"
- It separates ISP-level blocking from site-level failure

Hard constraints, all enforced in code:

| Constraint | Reason |
|---|---|
| **TCP only** — no ICMP, traceroute, NTP, or raw DNS | Tor carries streams, not packets |
| **Dial by hostname, never a pre-resolved IP** | Otherwise DNS leaks and the measurement is local |
| **Separate baseline** | Tor RTT is 300ms–3s with huge variance; mixing corrupts every threshold |
| **Expect 40–70% `BLOCKED`** | Exits are widely blocklisted; Cloudflare, banks, airlines refuse them outright |
| **Never `primary`** | Tor cannot be a health authority |
| **Target cap (default 25)** | 750 probes through exits is abusive to exit operators |
| **Rate cap (default 1 probe / 2s)** | "Slowly" is a correctness requirement here, not politeness |
| **Long timeouts (15–30s)** | Default 8s fails on slow circuits and produces false `DOWN`. The Tor runner bounds each probe at 30s, and the sequential loop means an unbounded circuit would stall every later target |
| **Daily bandwidth budget, hard stop** | Prevents a runaway loop from becoming an incident |
| **`IsolateSOCKSAuth` on the SOCKS port** | Separate circuits by SOCKS credentials without touching the control port. Tor accepts arbitrary credentials as an isolation key; the shipped sidecar enables the flag. The env surface configures one Tor vantage, so per-vantage credentials are a protocol capability here, not a shipped setting |
| **`MaxCircuitDirtiness` 3600s** | Comparable measurements across time: the same circuit does not rotate mid-incident |

#### Deployment: don't embed Tor

Point at **any** SOCKS5 endpoint — Tor, an SSH tunnel, a VPN, a corporate proxy. Ship a compose profile with a Tor sidecar:

```yaml
  tor:
    build:
      context: .
      dockerfile: Dockerfile.tor
    image: netweather-tor:local
    profiles: ["tor"]
    restart: unless-stopped
```

This keeps the main image small, keeps Tor security updates independent of the release cycle, and lets users reuse a proxy they already run. The sidecar is built from this repository's `Dockerfile.tor` so the client-only `torrc` is baked in rather than mounted: a forgotten mount cannot turn the proxy into an exit relay. An all-in-one image with `tor` + `s6-overlay` is acceptable as an optional variant, but it is not the default.

#### Client-only torrc

A misconfigured Tor turns a user into an exit relay and earns them abuse complaints. Ship a torrc that cannot do that:

```conf
SocksPort 0.0.0.0:9050     # sidecar: 127.0.0.1 would be unreachable across containers
ClientOnly 1
ExitPolicy reject *:*
BridgeRelay 0
PublishServerDescriptor 0
IsolateSOCKSAuth 1        # distinct SOCKS creds = distinct circuits
MaxCircuitDirtiness 3600  # stable path for comparable measurements
NewCircuitPeriod 3600
```

`ClientOnly 1` + `ExitPolicy reject *:*` is non-negotiable. They are baked into the shipped `torrc` (not a mount an operator can forget) and pinned by a test. `netweather doctor` proves the SOCKS endpoint answers; it cannot read the sidecar's effective config, so it does not claim to audit the policy. (This is the one thing `proxy_scan.sh` gets exactly right: `ExitPolicy reject *:*`, `BridgeRelay 0`, `PublishServerDescriptor 0`.)

The `0.0.0.0` bind is safe only because the sidecar publishes no ports and lives on an internal-only network — see `NETWORKING.md` §9.

#### Determinism over anonymity

For a measurement tool, a **stable path beats a fresh path**. A rotating circuit gives every sample a different baseline, which destroys the per-vantage baselines that make `REGIONAL` meaningful. Long dirtiness, one circuit per vantage, explicit reset only on failure.

#### Bootstrap by polling, never `sleep`

A fixed `sleep 90` is the wrong pattern — too short on a slow network, wasted time on a fast one. Poll the SOCKS port until a test connection succeeds, expose a `BOOTSTRAPPING` state, and time out with a clear error.

#### No scraped proxy lists

`proxy_scan.sh` can scrape free-proxy-list.net and us-proxy.org. **Do not copy that pattern.** Public open proxies are mostly dead, frequently MITM'd, and using them is unreliable and ethically dubious — and a tool that ships a proxy scraper gets flagged as a scanner.

Instead: **bring your own SOCKS5.** A user-supplied list of endpoints they control is the only supported source. No scraping, no discovery, no pools.

#### Abuse posture

This is the feature most likely to earn the project a bad reputation, so the limits are defaults, not suggestions:

- Off by default
- Small target cap, low rate, hard budget
- Never the full target list — a "corroboration set" only
- `ACCEPTABLE_USE.md` states plainly: this verifies your own view; it does not evade access controls
- The UI shows Tor bandwidth consumed and remaining budget

**Implemented:** `newTorAgent` returns the Tor `bytecap.Budget`, and the board shows `Tor budget: <used> of <allowance> used (<left> left)` whenever Tor is configured; when the allowance is spent the line says the vantage is paused until 00:00 UTC. The direct vantage keeps its own separate banner, because the two allowances protect different peers. A zero allowance means the cap is disabled and the line is omitted rather than claiming a budget of zero.

---

## 6. Scheduler

### 6.1 Tiers

| Tier | Interval | Jitter | Targets |
|---|---|---|---|
| 0 — reference | 30s | +0–10% | reference set |
| 1 — critical | 120s | +0–20% | top service targets, corridors |
| 2 — service | 300–600s | +0–20% | bulk of service targets |
| 3 - anchors | 120s | +0-20% | country + network anchors (cheap probes) |

Hard floor: **30s per host, enforced in code, not configurable below.**

Anchors sit at 120s rather than 60s on purpose. National shutdowns and cable cuts are measured in hours; halving the anchor cadence doubles the largest single block of traffic (~350 targets) to buy a detection latency improvement nobody can use. At 120s the whole 750-target fleet lands near 5 probes/s, which is also the number the storage sizing in §12 assumes.

```
20  @ 30s   = 0.67/s
80  @ 120s  = 0.67/s
320 @ 450s  = 0.71/s      # tier 2 midpoint
350 @ 120s  = 2.92/s      # anchors
                ----
                4.97/s  ≈ 430k probes/day
```

Jitter is **additive only**: `interval + rand(0, pct·interval)`, never subtractive. Two reasons, both load-bearing:

- Tier 0 sits exactly on the 30s floor. `±10%` would produce 27s intervals and violate the one guardrail that has to be literally true.
- One-sided jitter still de-aligns the herd, without ever pushing a target below its own configured minimum.

The floor is applied **after** jitter, as a final clamp.

### 6.2 Concurrency and pacing

- A 1-second dispatcher tick calls one deterministic `Step`; probes run one at a time, in due order, with per-host serialization and the global ceiling below
- **Deliberately not a worker pool.** The earlier sketch had 64 workers and a `probe.workers` setting. The shipped loop is a single step so the scheduler rules are testable with an injected clock and no sleeping, and 750 targets at their intervals need a few probes per second, well inside the ceiling; a pool would add nondeterminism without buying throughput the workload needs
- **Per-host semaphore = 1**, keyed by registrable domain, so `chase.com` and `www.chase.com` share a slot
- **Global token bucket** ceiling (default 20 req/s) - a bad config cannot melt your connection or look like an attack. **Implemented:** the agent spaces probes by `1/rate`, with the rate from `schedule.RampRate`; `NETWEATHER_RATE_LIMIT` sets the ceiling (fractional values allowed, malformed or non-positive refuses startup), the first probe starts immediately, and a probe slower than the gap simply lowers the effective rate
- **Single-flight per target** — never two probes of the same target concurrently
- Missed ticks are dropped, never queued (no thundering herd after a suspend)

### 6.3 Adaptive politeness

- `BLOCKED` twice in a row multiplies that target's interval x2, up to x8, and logs it. **Implemented:** `event=interval_adjusted` at INFO, and any other result resets the streak
- `DOWN` keeps the interval, but schedules a **recovery watch** at 30s for the first 5 minutes. **Implemented:** the watch ends five minutes after the first down result
- `UNMEASURABLE` drops to tier 3 regardless of configured interval. **Implemented:** the anchor cadence, 120s
- Honour `Retry-After` on `429` exactly. **Implemented:** seconds or an HTTP date, no jitter, capped at one hour so a hostile header cannot park a target

### 6.4 Probe budget visibility

`netweather doctor` prints, and `/metrics` exports:

```
targets: 750 (service 400, country 250, network 100, reference 10)
effective rate: 4.1 probes/s
blocked share: 11.2%   unmeasurable: 38
asn coverage: 341   countries: 96   top-ASN share: 14.3% (Cloudflare)
```

This exists because at 750 targets, **interval choice is the whole ballgame**, and users will get it wrong.

**Implemented:** `netweather doctor` opens the database read-only (no migration, no write pool) and prints a three-line fleet block: target count by class, scheduled probes per second (sum of 1/interval), blocked share, unmeasurable count, distinct ASN and country coverage, and the most common ASN with its share. A host with no database yet says so instead of failing. `/metrics` exports the same numbers as `netweather_targets_class{class=...}`, `netweather_probe_rate_per_second`, `netweather_blocked_share`, `netweather_unmeasurable_targets`, `netweather_asn_coverage`, `netweather_countries`, `netweather_top_asn_share`, and a single `netweather_top_asn{asn,name}` series.

The sketch's `peak concurrency` is deliberately absent: the shipped probe loop is one step at a time, so the number would always be 1. `projected egress` is absent from doctor and metrics because a stopped process has no byte counter; the running service reports the day's usage on the board banner and as `egress` in `/api/v1/meta`, and a monthly projection is that measured number times 30.

### 6.5 Cold start

Two problems, both solved at `t=0`.

**No burst.** 750 targets due at the same instant is a 750-probe spike: it saturates the pool, blows a minute of egress budget, and is the single most scan-like thing this tool ever does. Initial scheduling is spread uniformly across the first interval:

```
first_run(t) = now + rand(0, 0.95 * interval(t))
```

The global token bucket additionally ramps from 2 req/s to the configured ceiling over the first 10 minutes. A restart does not repeat the ramp - only a cold database does. **Implemented:** the ramp is measured from `MIN(target.created_at)`, so a restart resumes at the current rate; the cold-start reference is read once at startup and an empty database uses the process start.

**No false confidence.** An hour-of-week bucket needs 28 samples, and since one week contributes exactly one sample to a given hour-of-week bucket, that is **28 weeks**. Until then the baseline does not exist, and pretending it does is how a monitor earns its first false alarm:

| Age | Mode | Detection | UI |
|---|---|---|---|
| < 1 full cycle | `STARTING` | none | skeleton |
| 1 cycle – 28 weeks | `LEARNING` | absolute floor only: global health < 90%, sector < 75%, 2 consecutive buckets | "learning — 12/28 samples, no incidents yet" |
| 28 weeks | normal | baseline-relative (7.2) | normal |

**Implemented:** the absolute-floor row is real, not aspirational. `internal/anomaly` runs the floor whenever a scope's history is below the 21-of-28 usability threshold: a global health share under 0.90 or a sector under 0.75 on two consecutive evaluations records an anomaly with kind `health_floor` and the floor as its baseline. Country and ASN scopes get no floor, because the design gives them none, and the detector stays silent rather than inventing a number. It remains an anomaly, never an `INCIDENT` or an opened incident, which is what the next paragraph requires.

`LEARNING` never emits `INCIDENT`. It can still emit `STALE`, `LOCAL`, and `TAMPER`, because none of those depend on a baseline. The mode is tracked **per scope**, not globally: a newly added sector is in `LEARNING` while the rest of the board is not, and the confidence badge follows the scope, not the process.

---

## 7. Correlation and verdicts

All correlation runs in **5-minute rolling windows** with **30-second buckets**, so "simultaneous" is well-defined despite one box.

### 7.1 LOCAL verdict ("is it me?")

```
fail_groups = { g in reference_groups : g.state in (DOWN, UNKNOWN) }
if |fail_groups| >= 2 of |reference_groups| >= 3  ->  LOCAL
```

Secondary heuristic, used when reference targets are themselves flaky:

```
if failures in one 30s bucket span >= 5 sectors AND >= 10 ASNs  ->  LOCAL_SUSPECT
```

`LOCAL_SUSPECT` suppresses red styling but does not assert `LOCAL`; the UI says "possibly your connection" and shows both interpretations.

**Implemented:** the primary rule uses the reference class (`ReferenceStates`): two of three or more failing reports `LOCAL`. The secondary heuristic counts the distinct sectors and origin ASNs among confirmed `DOWN` targets (`store.FailureSpread`); five sectors and ten networks at once reports `LOCAL` with the detail "failures span N sectors and M ASNs; possibly your connection", and the tile mute applies. It is a confirmed-state count rather than a literal 30-second bucket, because that is the durable signal the rest of the pipeline reads; the wording stays qualified because a genuine mass outage produces the same pattern.

Third signal - the one that makes this precise: **vantage comparison.** If the primary vantage says `DOWN` but a corroborating vantage reaches the same target, the path is the problem, not the service. That is `REGIONAL`, and it is the only evidence that distinguishes *your ISP* from *your country* from *the service*.

**Implemented:** `store.TargetVantages` reads every vantage's confirmed state for one target, primary first. The target drawer shows the rows with each vantage's state and since time, and when the primary is `DOWN` while any corroborating vantage is `UP`/`UP_REACHABILITY` it says in words that this is a regional path problem rather than a service outage; agreeing vantages leave the note empty. `GET /api/v1/targets/{id}` carries the same rows in `vantages`, omitted when no reader is configured. A failed read is a missing panel, never a missing target.

### 7.2 Anomaly detection (the actual mass-outage detector)

Per scope — `global`, `sector`, `country`, `asn`:

```
judgeable  = enabled - unmeasurable        # structural: what this scope can ever see
measurable = up + degraded + down          # current: a real observation exists now
health     = (up + degraded) / measurable
coverage   = measurable / judgeable        # current observation completeness
share      = judgeable / enabled           # structural visibility, coverage panel only
```

`BLOCKED` and `UNKNOWN` are excluded from `health`; `UNMEASURABLE` targets are excluded from `health` **and** from the `coverage` denominator, because a target already classified as permanently blind cannot make a scope blind *again* every five minutes. It still counts in `share`, so the coverage panel keeps showing the structural gap (`DESIGN.md` §3.2).

That distinction is the whole point. The obvious formula, `health = (up+degraded)/(up+degraded+down)`, reports **100%** for a sector where 90 of 100 targets are bot-walled and the remaining 10 are fine. The tool would be blind and green at the same time.

So:

| Condition | Health | Board |
|---|---|---|
| `coverage >= 0.80` and `measurable >= 20` | computed | normal |
| `0.50 <= coverage < 0.80`, or `measurable < 20` | computed, flagged `low_coverage` | hatched tile, band widened ×1.5 (a small denominator is noisy) |
| `coverage < 0.50`, or `measurable < 8` | **`UNKNOWN`** | grey, "not enough visibility", excluded from the anomaly detector |

The absolute floors matter as much as the ratio. A scope of 10 targets at `coverage = 1.0` has a perfect ratio and almost no information; `measurable < 8` is not a healthy scope, it is a scope that cannot support a verdict.

**Implemented, the low-coverage band:** when `measurable < 20` or `coverage < 0.80`, the health detector's band is widened 1.5x before the trigger is evaluated, exactly as the middle row says. The widening is applied after the atypical-day widener and before `Detector.Push`, so a small or partly measurable scope needs a larger drop before it can alarm; the floors below still stop detection entirely. The coverage panel separates the two reasons: a row below 80% measurable carries a `low` badge and a hatch, while an adequately measured row with fewer than 20 measurable targets carries a quieter `small` badge instead. A 100%-measured five-target sector is thin, not weak; calling it `low` made the panel lie about what the detector was worried about.

A collapse in coverage is itself an event: if `coverage` for a scope falls below `median - 3σ` of its own baseline, emit `COVERAGE_DROP` as an **advisory**, never `INCIDENT`. A mass bot-wall rollout and a mass outage look identical in the health number, and only the coverage line tells them apart.

**Implemented:** `internal/anomaly` runs a second detector over the coverage series with its own detectors, day-widening, and cooldown, so a visibility drop and a health drop cannot suppress each other. It runs **before** the health floors, because the case that matters — a scope that has become mostly unmeasurable — is exactly where health detection must stay silent. The row is `anomaly.kind = 'coverage_drop'` with the observed coverage and its baseline, it never opens an incident, `/api/v1/anomalies` lists it, and the board shows the newest one inside six hours as an advisory banner that says the loss is visibility rather than an outage. `store.ScopeCoverageHistory` reads the `coverage` column from the same hour-of-week `scope_sample` window as the health history.

Baseline: same **hour-of-week**, rolling 28 weeks (28 samples, one per week at that hour), robust statistics. **Implemented:** the lookback is `7 * baseline.Window` days for exactly that reason: a 28-day lookback held about four points per bucket, below the 21-sample floor, so the detector could never leave `LEARNING`.

```
trigger when:  health < median - 3 * 1.4826 * MAD
         and:  absolute drop >= 2 percentage points of scope size
         for:  2 consecutive 5-minute buckets
```

#### Resolution and cadence

Probe cadence and detection cadence are different things, and conflating them is the bug this section exists to prevent.

Tier 2 targets are probed every 300–600s. If scope health were computed by counting probes that landed inside a 5-minute bucket, a 600s target would contribute one observation to one bucket and zero to the next, and health would oscillate purely from sampling. At 750 targets that produces a sawtooth that looks exactly like a slow-motion incident.

So:

| Concern | Cadence | Source |
|---|---|---|
| Probe | per tier, 30s–600s | scheduler (§6.1) |
| Scope health | fixed 5 min, UTC-aligned | `target_state`, freshness-weighted |
| Baseline sample | 1 per hour-of-week slot | latest evaluation inside the hour (upsert); the 28-sample median/MAD is the robust layer |
| Detection | 2 consecutive 5-min evaluations | the trigger above |

**Scope health reads last-known state, not bucket counts.** A target counts as an observation when `now - last_probe_at <= freshness`, where `freshness = min(2 × interval + 30s, 15 min)`. A target probed once per 10 minutes is a low-resolution sensor, not a missing one; its state persists between probes.

Stale targets drop out of both numerator and denominator, and therefore lower `coverage`. That is deliberate: a stalled scheduler is a visibility failure, not an outage, and it should surface as a hatched board plus a `COVERAGE_DROP` advisory rather than as mass `DOWN`.

**Implemented:** incident detection and the anomaly detector apply exactly that window (`internal/incidents/freshness`, `store.ScopeStatesFresh`). A target with no probe time, or one outside its window, is dropped from both numerator and denominator rather than counted as healthy or failing. Reconciliation refuses to close an incident when every observation is stale: blind is not clear. Coverage deliberately uses the unfiltered query, because a stalled scheduler is precisely the blind spot it reports.

Consequences, stated plainly:

- "Simultaneous" means *current within the freshness window at the evaluation instant*, not *observed in the same 5 minutes*.
- A tier-2 `DOWN` needs its own debounce (2 failures, up to 20 min). Acceptable: mass-outage latency is carried by tier 0/1 and the anchors, not by the bulk tier.
- `rollup_5m` is kept for per-target history, the drawer, and coverage trends. It is **not** the correlation input.
- `probe.bucket_30s` survives because attribution uses it to show two fast-tier failures shared a 30-second window; slow-tier targets are attributed by state, never by bucket.

Why baseline-relative: with 750 public targets, **3–10 are genuinely unreachable at any moment.** A fixed threshold screams daily. A Tuesday 14:00 dip is not a Saturday 03:00 dip.

A bucket needs at least **21 of 28** samples before it is usable; below that the scope stays in `LEARNING` (§6.5). Two consecutive missing buckets (a restart, a suspend) are tolerated by dropping the bucket, not by imputing it.

#### Atypical days

Hour-of-week is a good prior and a terrible absolute. Christmas, New Year, Ramadan, national holidays, and major events change both traffic and availability enough to trip a 3σ band on exactly the days someone is watching.

- **Calendar.** A config-provided `config/calendar.yaml`: dates plus optional region tags. Never hardcoded, never fetched at runtime. A date with no entry is a normal day.
- **Both-direction rule.** If the first three buckets of a day fall outside the band in *both* directions, the day is atypical and the band widens to `median ± 6·MAD` for the rest of that day. A one-directional excursion is an incident, not a holiday — this is what stops the rule from becoming a way to ignore real outages.
- **Atypical days are excluded from baseline samples.** Otherwise the band learns the anomaly it is supposed to catch.
- **DST.** Bucket keys are UTC, and there is no configured offset: a DST transition cannot duplicate or skip a bucket.

**Implemented:** `NETWEATHER_CALENDAR` (or a `calendar.yaml` beside the target config) is parsed strictly at startup. Each entry is `date: YYYY-MM-DD` plus optional two-letter `regions:`. Every listed date is written to `atypical_day` and its already-recorded `scope_sample` rows are deleted; `SampleScopes` skips that day from then on, so the exclusion is not only a one-time delete. A region narrows the exclusion to that country's baseline; an entry with no regions applies to every scope. The both-direction rule is `baseline.DayWidener`, and when it first marks a day atypical the evaluator calls back to record the day with source `detector`, deleting the morning's samples and skipping the rest, once per UTC day. A malformed calendar refuses startup; an absent one is a normal deployment.

The calendar is advisory, not authoritative: an operator who never touches it gets slightly more sensitive detection on holidays, not a broken one.

### 7.3 Attribution ("why?")

When an anomaly fires, cluster the failing targets by each candidate dimension and pick the best explanation.

```
candidates = group failures by: cdn | asn | prefix_24 | country | sector | stage
score(g)   = (failing_in_g / total_in_g) * log(1 + failing_in_g)
winner     = argmax(score) where failing >= 3 AND share >= 0.60
```

Emit:

```json
{ "kind": "cdn", "value": "akamai", "failing": 17, "total": 21,
  "share": 0.81, "confidence": "high",
  "headline": "17 of 21 Akamai-fronted targets unreachable" }
```

CDN inference comes free from response headers even on failure:

| Signal | CDN |
|---|---|
| `cf-ray`, `server: cloudflare` | Cloudflare |
| `x-amz-cf-id`, `x-amz-cf-pop` | CloudFront |
| `x-akamai-*`, `server: AkamaiGHost`, `Reference #` | Akamai |
| `x-fastly-request-id`, `x-served-by`, `via: varnish` | Fastly |
| `x-azure-ref`, `x-msedge-ref` | Azure Front Door |
| `x-vercel-id`, `x-nf-request-id` | Vercel / Netlify |
| `x-iinfo`, `visid_incap` | Imperva |

Attribution is the highest-value output of the whole system. "8 sites down" is noise; "17 of 21 Akamai targets down" is an incident report.

### 7.4 Confirmation rules

| Class | → DOWN | → recover | Debounce |
|---|---|---|---|
| Reference | 2 of 3 fails | 2 consecutive passes | 30s |
| Service | 3 consecutive fails | 2 consecutive passes | 90s |
| Anchors | 2 consecutive fails | 1 pass | 120s |
| `BLOCKED` | 2 consecutive, same signature | 1 non-challenge response | none |

Never transition on a single observation. Never transition twice inside the debounce window.

**Implemented:** `internal/confirm` is the rule table as a pure function, and `store.Record` applies it in the same transaction that writes the probe, so the board, the incident detector, and the metrics all read a confirmed state. A probe that is held is still recorded: the observation is real, the state did not change. The FIRST observation of a target adopts its candidate immediately, because establishing a baseline is not a transition; after that, the class row above decides. `BLOCKED` matches on the challenge signature (error class plus the classifier's reason), so two different walls do not confirm each other. `UNMEASURABLE`, `UNKNOWN`, `CONFIG_ERROR`, and `DEGRADED` are adopted immediately: holding a visibility signal would hide a blind vantage, which is worse than a fast transition. The class comes from the target's config class (`reference`; `country`/`network` anchors; everything else is a service target), so there is no second setting that can disagree with the role.

### 7.5 Self-health — vantage point integrity

Every correlation depends on assumptions that can silently break. Check them.

| Check | Interval | Why it matters |
|---|---|---|
| **Clock offset** | 10 min | Correlation is timestamp-based. Warn above 2s; suppress correlation above 10s. **Implemented:** the `clock` check compares the local clock with one HTTP Date header, which works on networks that block UDP/123; a skew above 2s fails it and the detail names the skew. The `clock-sync` check uses the same source at 10s and gates incident correlation: while it is failing and fresh, the incident loop neither opens nor closes an incident and logs `event=correlation_suppressed`, then `event=correlation_resumed` when the skew clears. The NTP instrument still measures the offset of configured NTP targets, but the self-check does not depend on four NTP servers: a blocked clock port would otherwise suppress correlation forever. |
| **Egress IP** | 10 min | If your ISP re-IPs you or you move to a VPN, your ASN, geo, and every baseline change. Record `(ip, asn, country)`. **Implemented:** the `egress-ip` check reads the public address from Cloudflare's trace endpoint and enriches it with the Cymru ASN and country. Maintenance compares consecutive successful observations and logs `event=vantage_changed` when the tuple moves; a failed or missing observation is not treated as a change. On a move it also drops the learned DNS baselines, because the same resolver reached from a different address is a different path; the recorded divergence verdicts are kept. |
| **Resolver identity** | 10 min | DHCP handing out a different resolver changes every DNS result. Record the resolver set. **Implemented:** the `resolver-identity` check reads the `nameserver` lines from `/etc/resolv.conf`, sorts and deduplicates them, and records the set. A change logs `event=resolver_changed` with both sets and resets the learned DNS baselines, because the same name answered by a different resolver is not the same history. The check is omitted on a platform without that file rather than reported as a failure. |
| **Disk headroom** | 10 min | SQLite full = silent data loss. Warn at 85%. **Implemented:** the `disk` check writes and syncs 64 KiB into the data directory, then measures the free share of the volume holding it (`syscall.Statfs` on Unix, `GetDiskFreeSpaceEx` on Windows). Below 15% free the check fails with the share in the value and detail; a platform that cannot measure free space keeps the write result and says so rather than inventing a number. |
| **Probe heartbeat** | continuous | Feeds the `STALE` verdict (3.3). **Implemented:** the verdict reads `MAX(target_state.last_probe_at)` on every render, so silence is noticed at the cadence of the slowest reference target rather than by a separate heartbeat process. |
| **Config validity** | on reload | A rejected config keeps the previous one live (§13.3). |
| **All of the above** | shared tick | **Implemented:** every check in this table other than the heartbeat runs at startup and then on the maintenance loop's ten-minute tick, so the intervals column is the effective cadence rather than a separate scheduler. A slower check would need its own timer; none does today. |
| **Path MTU** | 10 min | Docker bridges default to 1500; a VPN uplink of 1420 black-holes large handshakes and fakes `DOWN`. See `NETWORKING.md` §4. **Implemented:** the `mtu` self-check reports the smallest MTU among up, non-loopback interfaces and warns below a 1400-byte floor. It measures the local interface, not the path: a lower path MTU needs a DF-bit probe and raw sockets. A Linux-only implementation ships as `internal/probe/pmtu_linux.go`; other platforms fall back to this interface check, and the detail says so rather than implying a path measurement. While that check is failing and fresh, the agent tells the classifier, so an `http_timeout`, `tcp_timeout`, or `tls_handshake` becomes `UNKNOWN` with the reason "egress MTU is below the floor" instead of faking `DOWN`. A `tcp_refused` stays `DOWN`: the SYN arrived, so this handshake was not black-holed. |
| **Egress ports** | 10 min | Port 53 blocked turns 250 country anchors red for no reason. Blocked → `UNMEASURABLE`, never `DOWN`. See `NETWORKING.md` §10. **Implemented:** the `port53` self-check proves UDP/53 and TCP/53 to a public resolver and names the blocked transport. A fresh failure feeds a gate in the agent: while egress port 53 is blocked, `dns_timeout` and `dns_error` classify `UNMEASURABLE` with the blocked port in the reason, so one firewall cannot turn every anchor red. A stale gate fails open (the classifier runs normally) and an NXDOMAIN is still `DOWN`, because that is target truth rather than a blocked path. |

`VANTAGE_CHANGED` is not an outage — it invalidates baselines, and the UI must say so plainly:

> Your egress changed (ASN 7922 → 9009, US → NL). Baselines reset; expect ~24h of reduced anomaly sensitivity.

That kind of admission is what keeps the tool trustworthy. A monitor that silently keeps comparing against a baseline from a different network is worse than no monitor.

**Implemented:** `store.RecordVantageChange` appends the previous and current egress tuples when maintenance's `egress-ip` comparison sees a move, and `store.LatestVantageChange` reads the newest row. The board shows the sentence above for 24 hours after the move, with the ASN and country but not the literal address; an expired change, a failed read, and an unwired reader each omit the banner rather than inventing one. The values are the same tuples the self-check already stores, so a restart does not lose the fact that the baselines belong to a different path.

**Diagnostics.** Self-health failures are only useful if you can act on them. The logging model — levels, the canonical decision record, redaction, the in-memory ring, backpressure, and the debug bundle — is specified in [`LOGGING.md`](LOGGING.md). Two invariants from that document matter here: a stalled log sink must never stall the scheduler (drops are counted, not blocked), and the `reason` string on a transition is identical in the log line and in `state_change.reason`.

---

## 8. External intelligence

Your probes cannot detect a cyberwar: one box, one path, anycast edges. Detection must come from outside. Each source is a **plugin** with its own cache, backoff, and staleness display. **The core must be fully functional with zero feeds enabled** — degraded, not broken.

### 8.1 IODA — build first

Georgia Tech's Internet Outage Detection and Analysis. Country-level outage scores from BGP, active probing, and darknet traffic. Free, public API.

- `GET /v2/outages` — active and recent outage events with scores
- `GET /v2/signals/...` — raw signals per country/datasource
- **This is the use case, already solved.** Do not reimplement it.
- Poll every 5 minutes. Cache aggressively. Attribution required.

**Implemented:** two separate adapters. `internal/feeds.IODA` polls `/v2/outages/events` on the five-minute cycle and stores country evidence with the event's real score as severity (`NETWEATHER_FEEDS=ioda`). `internal/feeds.IODA.FetchLevels` polls the ranked `/v2/outages/summary` once per cycle (`NETWEATHER_FEEDS=ioda-level`) and stores at most `NETWEATHER_IODA_LEVEL_LIMIT` (default 25, hard max 100) countries in `country_level` (migration `0025`). IODA's `overall` score is an unbounded magnitude, not a probability, so the stored `level` is a **relative** value: `log10(1+score) / log10(1+top_score)` within the same window. The raw score, event count, and rank are stored beside it and shown with it, so a tile never claims an absolute outage probability. The world page reads the stored map with one query; it never calls IODA on a page view. A stale observation cannot replace a newer one, and a missing, empty, or failed snapshot states its case on the page.

### 8.2 Cloudflare Radar — build second

Free with a Cloudflare account token. The BGP feeds are the cyberwar signal.

- `/radar/bgp/hijacks/events` - named hijack events with confidence
- `/radar/bgp/leaks/events` - route leaks
- `/radar/traffic_anomalies` - traffic deviations by country/ASN
- `/radar/annotations/outages` - curated outage annotations
- Bring your own token. Ship no shared key. Poll every 5-15 minutes.

**Implemented:** `internal/feeds.Radar` polls hijacks, leaks, traffic anomalies, and curated outage annotations with the operator's `NETWEATHER_RADAR_TOKEN`, normalises their severity vocabularies, and stores them as `hijack`, `leak`, `traffic_anomaly`, and `outage_annotation` evidence. Annotations map `outage_type` (nationwide/regional/local) to severity and take the first location code from their `locations` list. Rows without an id get a stable synthetic key so re-polls stay idempotent. A missing token disables the feed with a startup warning rather than polling unauthenticated. All four documented endpoints are implemented.

### 8.3 RIPE RIS Live — feed plugin (BGP)

RIS Live is RIPE's real-time BGP update stream. It is powerful and very noisy,
and it is only useful once the baseline machinery (§7.2) is proven. This
project ships **two adapters**: a bounded RIPEstat polling adapter that is the
default, and an opt-in real-time WebSocket stream over RIS Live itself.

- `internal/feeds/rislive` polls the RIPEstat bgp-updates endpoint for the ASNs
  the operator names (`NETWEATHER_RIS_ASNS`), with an optional endpoint override
  (`NETWEATHER_RIS_URL`). It maps each update to an event: a withdrawal is
  severity 1.0, an announcement 0.0, both carrying the ASN and the prefix.
- An event is evidence, not a verdict: it appears in the feed strip and the
  drawer's corroboration list. The IHI's BGP component scores on events whose
  kind names a hijack or a leak; the adapter's `bgp_withdrawal` kind is a
  routing change, not a hijack verdict, so a dead feed drops the component
  with "feed is not enabled" rather than fabricating routing health.
- The adapter is read-only and spends no credits; it only reads RIPEstat.
- A contract test with a recorded payload pins the wire shape.

**Real-time stream (opt-in).** `internal/feeds/rislive_ws.go` reads RIS Live at
`wss://ris-live.ripe.net/v1/ws/?client=netweather` and subscribes to `UPDATE`
messages for the same `NETWEATHER_RIS_ASNS` list. It is enabled by
`NETWEATHER_FEEDS=ris-live-ws` or `NETWEATHER_RIS_STREAM=true`; an endpoint
override (`NETWEATHER_RIS_WS_URL`) must be `wss`, and a plaintext override is
ignored with `event=feed_url_insecure`. It is additive to the poller: a
persistent socket is a different network commitment from a bounded REST read,
so the polling adapter keeps working where a WebSocket cannot traverse.
`ris_message` updates are mapped into the same `bgp_update` / `bgp_withdrawal`
evidence shape (feed name `ris-live-ws`) with host, peer, path, and prefixes in
the payload; application-level `ping` frames are answered so a quiet
subscription is not dropped. Frames are batched (≤128, or a ten-second flush)
into one store write, the TCP dial goes through `netx` so the egress policy
still applies, and a disconnect is recorded as feed failure then reconnected
with exponential backoff capped at one minute. A contract test with a scripted
socket pins the subscribe frame, ping/pong, event mapping, and reconnect.

### 8.4 Statuspage family

One adapter covers a huge number of companies:

```
https://<slug>.statuspage.io/api/v2/summary.json
https://status.<company>.com/api/v2/summary.json
```

Plus AWS (`status.aws.amazon.com/rss/all.rss`), GCP (`status.cloud.google.com/incidents.json`), Azure status feed.

**Implemented:** `internal/feeds.Statuspage` polls any number of configured summary endpoints (`NETWEATHER_STATUSPAGE_URLS`, a base URL or a full `summary.json` URL) and stores one `statuspage` event per incident with its impact mapped to a severity, its start time, and its `resolved_at` when present. An empty incident list is a successful poll: "operational" is information, not absence. Update history is dropped from the stored payload because it is long and adds nothing to corroboration. `internal/feeds.AWSStatus` does the same for AWS service status RSS (`NETWEATHER_FEEDS=aws-status`), dropping "operating normally" items and mapping disruption/error-rate/degraded/maintenance wording to severity. `internal/feeds.GCPStatus` reads Google Cloud incidents JSON (`NETWEATHER_FEEDS=gcp-status`), maps `status_impact` and keeps the affected locations in the payload. `internal/feeds.NewAzureStatus` reuses the RSS adapter (`NETWEATHER_FEEDS=azure-status`); resolved and healthy items are dropped because RSS has no end time, and service-issue wording maps to 0.8. All four provider shapes named here are implemented.

Used for **corroboration**, producing the most interesting sentence in the product:

> **You:** unreachable · **Official:** operational → likely you, your region, or a bot-wall.

**Implemented:** when a target's state is `DOWN`, `DEGRADED`, or `BLOCKED` and its declared status page answered successfully with no incident in the last 24 hours, the target drawer prints that sentence with the actual state. It is drawn only when the provider query succeeded: an unconfigured reader, a failed query, or a page with an incident says its own case, because the asymmetry is evidence only when both sides are known. The feed-corroboration panel stays separate: a matching country or ASN event is a scope observation, not the provider's own report.

### 8.5 PeeringDB - enrichment only

ASN → organisation, country, IX membership. Cached locally, refreshed weekly. Labels the ASN view; never a monitoring signal.

**Implemented:** `internal/enrich.PeerDB` reads `/netixlan?asn=N` for internet-exchange membership, dedupes and sorts the names, and caps the list so the drawer row stays readable. The client caches per ASN for the process lifetime, because hundreds of targets share a handful of networks and one request per target would be both slow and rude. `NETWEATHER_PEERINGDB=false` disables it without disabling the rest of enrichment, and `NETWEATHER_PEERINGDB_URL` overrides the API root.

The labels are stored on `target.ix_names` (migration `0012`), written only by `UpdateEnrichment`, and shown as "Internet exchanges" in the drawer and as `ix_names` on the target API. An empty value means "not learned yet", never "not at an exchange", because the two are different claims. Freshness follows the enrichment loop (startup, then every six hours) rather than a separate weekly timer; the earlier "refreshed weekly" line described an intent that a single loop now satisfies more often, not less.

### 8.6 RIPE Atlas — optional profile

The only honest way to measure another country. A free software probe earns credits, and the built-in anchor mesh is publicly readable.

Ships as an **optional compose profile** (`ripe-atlas`), not a default service:

- The profile runs RIPE's official probe image. It is RIPE's daemon, measuring for RIPE's network; `netweather` never talks to its socket and never wraps it.
- **Implemented:** the `ripe-atlas` profile in `compose.yaml` and `NETWORKING.md`. Nothing in netweather reads from it; turning the profile on or off changes no probe, feed, or page.
- **Implemented:** the `atlas` feed plugin (`internal/feeds/atlas`) is a read-only adapter over the RIPE Atlas results API. It polls the measurement ids the operator names (`NETWEATHER_ATLAS_MEASUREMENTS`), with an optional operator key (`NETWEATHER_ATLAS_KEY`) for private measurements, and an optional endpoint override (`NETWEATHER_ATLAS_URL`). It reads only the results endpoint, which costs no credits; it never creates a measurement. An Atlas result is a measurement from RIPE's probes, not an outage verdict about one of this monitor's targets: it carries no target id, so it appears as evidence in the feed strip and corroboration lists rather than as a verdict. Corridors do not depend on it — they are configuration-driven (§9), so the fallback is the shipped behavior, not a degraded one. The plugin stays read-mostly: credits are the operator's and finite, so it never spends them on a timer.

A probe sidecar is a second host relationship: RIPE sees the operator's IP and uptime. That is stated in [`docs/RIPE_ATLAS.md`](RIPE_ATLAS.md) and in `THREAT_MODEL.md` §5.7, not buried in a compose file.

### 8.7 Degradation contract

Every feed plugin returns `(data, observed_at, err)`. The UI shows `Radar: 4m ago` or `Radar: stale (2h)` - never a silent blank. If a feed is stale, it is dropped from the composite index and the remaining weights renormalise.

**Implemented:** `feed_state` records the last success, the last error, and consecutive failures per feed, and the board's strip derives `ok`, `stale`, `never`, or `failing` from those columns on every render. The index reads the same states: `External outage severity` requires a healthy `ioda` feed and `BGP stability` requires a healthy `radar` feed. A feed that has never succeeded, is older than 30 minutes, or is failing drops its component with the reason attached, rather than being scored as "nothing happened". A feed that is not configured at all is a different statement and drops the component with `feed is not enabled`.

---

## 9. Corridors (undersea cables)

There is no live API for cable health. Infer it.

Ten corridors, each a pair of in-region endpoints:

```
NYC↔LON  LON↔SIN  SIN↔TYO  LAX↔SYD  MRS↔BOM
MIA↔SAO  FRA↔NYC  SYD↔SIN  JNB↔LON  SFO↔TYO
```

Per corridor, every 5 minutes: RTT (median of 5) and loss (%).

On a step change, capture **one** traceroute and attach it to the incident. Do not schedule traces — they walk ~30 third-party routers per run and are the most scanner-like thing this tool does (§14).

### 9.1 Trace policy

**Prefer a second vantage over a trace.** A trace shows you the path; a vantage proves the answer. Traces are expensive, noisy, and the most scanner-like thing in the tool, so they are the last resort, not the first.

Allowed triggers — and only these:

| Trigger | Why a trace is justified |
|---|---|
| Corridor step change | Localisation: which hop or exchange changed |
| Primary and corroborating vantage disagree, neither `BLOCKED` | Path vs. destination |
| User explicitly requests it from the drawer | Human judgement |
| Every other instrument exhausted | Literally no other option |

Not allowed:

- Scheduled or periodic traces
- Traces on ordinary `DOWN` transitions — a second vantage answers that better
- Traces to private ranges
- More than 10 runs/hour, globally
- Any trace without `CAP_NET_RAW` (§5.5)

Results attach to the incident. They are never stored as a time series.

Detect a **step change**, not an absolute value:

```
baseline = rolling median over 6h
alert when |rtt - baseline| > 3σ  OR  loss jumps from ~0 to >= 5%
and it persists 2 consecutive samples
```

A cut shows as one corridor stepping while neighbours stay flat — that localisation is the value. Cross-check with BGP: a cable cut usually reroutes prefixes, so corridor event + BGP path change on the same ASNs = high confidence.

Endpoints: prefer RIPE Atlas anchors (stable, distributed). Fallback: country anchor NS hosts, or public looking-glass targets.

**Implemented, configuration-driven:** a `corridors:` section in the target file defines the pairs (`id`, `from`, `to`, and exactly two `host:port` endpoints on 80/443). The runner samples each endpoint five times with TCP every five minutes — no raw sockets, so it works in the shipped container — and records the median RTT per endpoint, their mean, and the failure rate. Detection is the documented rule: a six-hour rolling baseline, a move beyond 3σ **and** a material 20 ms / 25% change, or loss moving from ~0 to ≥5%, and it must persist for two consecutive samples. A step opens one `corridor_event`; recovery closes it. Corridors never open target incidents. On a step change the runner captures exactly one traceroute to the far endpoint and stores it with the event — the one trigger §9.1 allows, captured once rather than scheduled. A walk that cannot start (no `CAP_NET_RAW`, an unresolvable endpoint) is logged and the event still opens without a trace.

**Deliberately not shipped: a default ten-city list.** There is no honest endpoint list to ship: the endpoints are the measurement, and inventing ten `host:port` pairs would produce ten fabricated paths with real-looking numbers. The `corridors:` section is the interface, and `config/targets.example.yaml` documents its shape. An operator who wants the ten-city view supplies ten pairs they can defend.

---

## 10. UI


Four screens plus a drawer. Server-rendered HTML + SSE; a small `EventSource` script applies one state snapshot on connect and then deltas. No JS framework, no build step. Served on `:2600` - port and bind rationale in `NETWORKING.md` §7.1. HTMX is optional and has not earned its place yet: the board needs one stream and no partial swaps.

The same state is exposed machine-readably under `/api/v1`; the contract, envelope, error codes, and stability tiers are in [`API.md`](API.md). The UI is a client of the store, not of its own API - but the two must never disagree about a state, so both read the same rows.

### 10.1 Overview

```
┌──────────────────────────────────────────────────────────────┐
│ ● Internet healthy        Health 98.7%  (normal 97.9–99.2%)  │
│   last cycle 12s ago · 750 targets · 38 unmeasurable          │
├──────────────────────────────────────────────────────────────┤
│ IHI 94  ▁▂▁▃▂▁▁▂▃▁▁▂▁▁▂▁▁▂▁▁▂▁▁▂▁▁▂▁▁▂▁▁▂▁▁▂ 24h           │
├──────────────────────────────────────────────────────────────┤
│ SECTOR            HEALTH              DOWN  BLOCKED           │
│ Financial         ████████░░ 96%        2      11             │
│ Travel            ██████████ 99%        0       6             │
│ Cloud & Infra     ███████░░░ 88%  ⚠     5       0             │
│ ...                                                          │
├──────────────────────────────────────────────────────────────┤
│ WHAT'S HAPPENING                                             │
│ ⚠ 5 cloud targets unreachable — 4 behind CloudFront          │
│ ⚠ IODA: elevated outage score, Iran (0.71, 6m ago)           │
│ ✓ DNS answers consistent across 3 resolvers                  │
└──────────────────────────────────────────────────────────────┘
```

**Implemented, the overview line:** the board's summary carries `Health 98.7% (normal 97.9-99.2%)`, the age of the newest confirmed probe, the enabled target count, and the `UNMEASURABLE` count. Health is `(up + up_reachability + degraded) / (up + up_reachability + degraded + down)`, the same definition the hourly scope sample uses, so the band and the number are comparable. The band is the global hour-of-week baseline at 3 sigma; while it has fewer than 21 of 28 weekly samples the line says "learning n/28" rather than drawing a range, and when the measurable share is below 0.80 or the scope is under 20 targets the band is widened 1.5x exactly as the detector widens it, with the page saying so. A fleet with no judgeable targets states that instead of rendering 0%.

**Implemented, "what's happening":** below the overview line the board lists the newest three open incidents, each as its stored headline linked to the incident page, with the scope and open time. The section is deliberately shallow: attribution and evidence stay on `/incidents/{id}`. An unconfigured incident reader, a failed query, and a configured log with no open incidents are three different statements and each says its own, rather than rendering an empty list that reads as calm.

### 10.2 Board

The wall of favicons. Grouping is a toggle: **sector (default)** · state · CDN · ASN · country · failure stage.

**Implemented:** the toggle is a query parameter (`/?group=cdn`) with one link per lens; the chosen lens is marked active and focus mode is preserved when switching. Ordering inside a group and between groups is the same severity order the flat board uses, so "worst first" survives every lens. An unknown lens falls back to sector rather than returning an error. Empty values get their own explicit bucket ("ASN not learned yet", "No stage recorded") instead of being folded into a real one.

**Implemented:** tiles render a generated square icon per target (`/icon/{id}.svg`): the name's initial on a colour derived from the host. It is generated rather than fetched, because pulling `/favicon.ico` from every target would add requests with no measurement justification (`ACCEPTABLE_USE.md`). The icon is session-protected like the board, and each tile keeps its state colour, text, corner stage badge, and accessible name.

- **Compact** (default at 750+): 40-48px tiles, ring colour + corner stage badge (`DNS`/`TLS`/`403`/`429`). ~760 per screen, so 750 targets is about one screen - no virtualisation needed.
- **Comfortable**: 96px tiles, name + 24h sparkline. Paginated by group, never all at once.

**Implemented:** both densities exist as a toggle (`/?density=comfortable`). Compact is the default; comfortable widens the grid and enlarges the tile, name, and icon. Every toggle link preserves the other view state (grouping lens, focus mode, country filter, density), so switching one control cannot silently reset another. An unknown density falls back to compact rather than returning an error. Comfortable density also draws a 24-hour availability shape per tile, from one batch rollup query for the visible set rather than one query per target; a target with no history, a failed query, or a deployment without the batch reader simply omits the shape instead of fabricating one. It is decorative and hidden from assistive technology, because the state text already carries the meaning. Comfortable density pages each group: the first page renders 60 tiles, and a group with more says how many are behind it with "Show more" and "Show all" links. The page sizes travel in `?show=<group>:<n>,…`, so any group can be paged independently and every link preserves the other view controls; a malformed value falls back to the first page rather than returning an error, and compact never pages because it is the lens designed to fit.
- Healthy groups collapsed by default. Problems expanded.
- The feed-health strip shows each external feed as `ok`, `stale`, `failing`, or `never`; a stale or never-succeeded feed adds a banner, because a dead corroboration source must be visible on the board, not only in the API (P5).
- Focus mode: hide everything not `DOWN`/`DEGRADED`. **Implemented:** `/?focus=problems` keeps only those two states, and `TAMPER` is deliberately not part of it: `TAMPER` is a global verdict and a drawer signal, not a tile state (`internal/state` has no tamper state), so a focus mode that named it would hide nothing extra. The summary counts still describe the whole fleet, so the page does not claim the hidden targets are healthy.
- `UNMEASURABLE` collapsed into its own muted section at the bottom. **Implemented:** those targets are removed from their sector group and rendered last in a quiet, collapsed section, so a permanent blind spot cannot keep a healthy sector looking broken; the summary counts still include them.

### 10.3 World

One tile per country: ccTLD code + flag, coloured by reachability, with an IODA score overlay when available. This is the view that makes a cyberwar legible, and it reuses the same visual language as the board.

**Implemented:** `/world` renders one tile per learned country, worst state first, with the target count and per-state chips; the code, state text, and glyph carry the meaning alongside colour. A target with no learned country is counted separately as "unplaced", never folded into a real code. Clicking a tile opens the board filtered to that country (`/?group=country&country=US`), and the board shows the active filter with a clear link that preserves the grouping lens and focus mode. A country tile also carries the highest-severity feed event for that country from the last 24 hours (`ioda outage 0.87`), with one query for the whole page; a missing reader, an empty window, and a failed query each state their case instead of leaving the tile looking quiet. The tile also carries the regional-indicator flag for the code, decorative only: the code is always rendered as text, so a platform without flag glyphs loses nothing.

**Geographic projection (presentation only).** Below the tiles, the same country
aggregates are placed on a quiet equirectangular land silhouette served from
`internal/web/static/world-projection.svg` (simplified public-domain Natural
Earth 110m land geometry; no script or third-party data fetch). Circle
positions are precomputed from the geographic centres in the public
`mledoze/countries` dataset; diamond positions come from Natural Earth 10m city
centres matched to the existing `City, ST hub — site` name convention. They are
placements for the eye, not measurements: a circle is a country aggregate, a
diamond is a label anchor, and neither claims that a target is hosted at that
coordinate or that CDNs anycast to it. The projection reuses each row's state
class, glyph, count, feed reason, level, and drill-down link, so it adds no new
schema field and no second source of truth. A bucket or hub without a public
anchor is counted in a note rather than dropped or pinned to a guessed place.

**Live per-country level (bounded, server-side).** The earlier objection to a
live IODA overlay was the cost of IODA's per-entity signal endpoints: a page
view would become an API-scraping loop. That is still true, so the world page
does **not** fetch IODA. Instead the `ioda-level` feed polls the single ranked
`/v2/outages/summary` endpoint every five minutes and stores at most 25
countries; `/world` reads that stored map with the same one-query discipline as
the event overlay. IODA's `overall` is an unbounded magnitude, not a 0–1
probability, so the tile shows a clearly named **relative level** beside the
raw score and event count, with the source (`ioda-summary`) on the badge. The
event overlay and the level snapshot have separate notes: an event feed can be
healthy while the level feed is not, and neither silence is allowed to look
like "no level".

### 10.3a US hubs

City/state detail is metadata, not a schema column. The field-trial generator
names its 42 US hub targets as `City, ST hub — site` in sector `us/hub`, so
`/hubs` can group them without a migration: the stored schema stays at version
25. The view reuses the `/map` treemap one level down: each city is a sector
block, each public endpoint is a tile, and the same severity × duration area
and diverging health colour make unhappy hubs dominate the picture. Cities and
the sites inside them are packed and linked worst-first. A malformed name is
reported as unplaced rather than grouped under a guessed city, and a
non-`us/hub` target never appears. Each site tile keeps its identity, state,
and status reason in its title and hover card and opens its target drawer;
clicking the city header opens the board's sector grouping. The view is a naming
convention for large US hubs, not a claim of city-wide outage truth from one
vantage.

### 10.4 Corridors

Ten rows: RTT sparkline, loss bar, baseline band, step-change highlight.

**Implemented:** `/corridors` lists one row per configured corridor with its current RTT, its 24-hour baseline (the median successful RTT in the window the step rule compares against), a 24-hour RTT sparkline, a five-cell loss bar beside the exact percentage, sample time, state (`normal`, `no reply`, `step change`, or `waiting for a sample`), event kind, detail, and when the change started. The corridor column leads with the operator's readable `from → to` label and keeps the stable id underneath; the path column shows the configured endpoint hosts from the target file, with the full `host:port` pair in its hover text. Both cells carry the same explanation in hover text and an accessible label: endpoints are the measurement, city labels are approximate because CDNs anycast. With no configured endpoints the page keeps the labels and says hosts are unavailable, rather than guessing a host. The shapes are text glyphs rather than an image or a chart library, and the sparkline is omitted when there are fewer than two successful samples: a fabricated shape is worse than an empty cell. A failed history query is stated on the page and does not remove the current sample. A step-change row also carries the one traceroute captured when the event opened, collapsed behind a "trace" summary; a silent hop renders as `*` rather than as loss. Under the sparkline, the baseline band is drawn as markers on the same buckets: `·` inside the six-hour band (median ± max(3σ, 10%)), `▲` above it, `▼` below it. The newest bucket is excluded from its own band, mirroring the detector's persistence rule, so a spike cannot widen the band that is meant to catch it.

**Projection:** the same page can place the configured `from`/`to` labels on the
self-contained equirectangular silhouette used by `/world`. Coordinates are
approximate city centres from public Natural Earth populated places; only codes
already present in that bounded anchor table are placed, and unmapped labels are
counted rather than pinned to invented geography. A straight line joins each
known pair and is split at the antimeridian when the shorter visual link crosses
the Pacific edge. It carries the corridor id, label pair, current state, and
configured `host:port` endpoints in its hover/accessible text, followed by the
explicit caveat that labels are not measured locations and the line is not the
physical or undersea-cable path. The base image is a same-origin static asset;
there is no script, third-party map service, or runtime data fetch.

### 10.5 Target drawer

Click any tile:

- **Implemented:** state + why (the classifier's reason), the host, the 24-hour hourly strip, and the last 20 probes with stage, status, ms, error class and the detail line.
- **Implemented:** the 24-hour strip's bar shows the healthy share per hour and each cell's title carries the counts and EWMA latency. It is not a p50/p95 chart: the rollups hold EWMAs, not percentiles (`SCHEMA.md` §6.2), and calling the smoothing a percentile would be the quiet lie the project avoids. A true percentile chart waits on the histogram.
- **Implemented:** the metadata block (class, sector, country, probe, port, tier, interval, ASN, prefix, CDN, measurability), the leaf certificate issuer and expiry from the last TLS observation, and the last 20 state transitions with their stored reason. Empty enrichment reads "not learned yet" rather than an empty cell; a non-TLS target states "not a TLS probe" for the certificate rows.
- **Implemented:** certificate issuer, expiry, and pin status. Issuer and expiry are recorded per TLS probe (migration `0011`). The agent learns each target's issuer set (migration `0013`, `cert_pin`): the first issuer is a baseline, a new issuer is accepted on first sight and flagged once as `tls_issuer_unexpected`, and only the issuer DN's hash is stored. The drawer says "issuer learned" rather than implying the certificate is certified.
- **Implemented:** the drawer's Feed corroboration panel lists events from the last 24 hours that match the target's country or ASN, highest severity first. It is a scope match, not a provider match: the panel says "events in this target's country or ASN", never "the operator says it is down". An empty panel, an unconfigured reader, a target with no learned scope, and a failed query each say so explicitly.
- **Implemented, provider mapping:** a target may declare `status_url:` — the operator's status page in the same shape the Statuspage feed accepts. Its drawer then gains a **Provider status** panel listing that page's incidents from the last 24 hours; it is a provider match, so the panel can say "their report", and it repeats that an incident there is the provider's own. The match key is the page's host, which is what the statuspage adapter stores in `external_id`, so a full `summary.json` URL still matches its host. A target with no declared page has no provider panel at all, because netweather does not guess which company owns a hostname. An unconfigured reader, an empty window, and a failed query each state their case.
- **Implemented, provenance:** the drawer's **Source** panel lists the seed or config that produced the row and the SHA-256 of that payload, so a poisoned list can be traced back to the input (`THREAT_MODEL.md` T35). It shows the first 12 characters of the hash with the full value in the title, and the load time. An unwired reader omits the panel; a failed read and an empty list each state their case rather than leaving a blank table.
- **Implemented, traceroute:** when `NETWEATHER_TRACE_ENABLED=true` and the capability is present, the drawer offers a **Run traceroute** button. The action runs one walk and renders a small result page (destination, reached, hop table, and the container caveat) instead of rewriting the target page, and it spends from the same instance-wide ten-per-hour budget as the API, so the ceiling is a property of the process rather than of each surface. A disabled capability, an exhausted budget, and an unresolvable host each state their case instead of returning a bare error.
- **Implemented, disable:** when runtime target control is wired, the drawer offers a **Disable this target** button behind a CSRF-checked form. It calls the same TargetControl the API uses, logs vent=target_disabled with the request id, and redirects to the board. The row and its history stay; the scheduler drops the target on the next tick, and a later config reload can bring it back. An unwired control omits the button rather than offering one that cannot work.
- DNS divergence is implemented too: the newest verdict is a drawer row, and the board carries a collapsed observations strip (§3.2 of `CLASSIFIER.md`). The enrichment passes also carry PeeringDB exchange labels (migration `0012`).

### 10.6 Coverage panel

The blind-spot report. Required, not optional:

```
ASNs: 341    Countries: 96
Top-ASN share: 14.3% (Cloudflare)  ✓
Top-10 share:  41.2%               ✓
Measurable:    94.9% (712/750)     ✓
⚠ 12 countries have a single anchor — no redundancy
⚠ 38 targets are unmeasurable (bot-walled)
⚠ 2 sectors below 50% measurable — health UNKNOWN
```

**Implemented:** `/coverage` carries the concentration block above the tables: distinct ASNs and countries, the top ASN with its share and the top-10 share, a count of countries with exactly one country anchor, a count of sectors below 50% measurable, and the unmeasurable total. `store.Coverage` computes it in the same read as the rest of the panel (one query for the distinct counts, one for the ten largest ASNs, one for single-anchor countries), so the page cannot show a total that disagrees with the concentration beside it. `GET /api/v1/coverage` exposes the same block as `concentration`.

### 10.7 Accessibility

Every state carries a **shape or glyph** as well as colour: ring style (solid/dashed/hollow), corner badge, and a pattern in compact mode. Colour alone is not sufficient.

**Implemented:** every state has a distinct ring style in `app.css` — solid 1px for `UP` and `UP_REACHABILITY`, dashed for `DEGRADED` and `UNKNOWN`, solid 2px for `DOWN`, dotted for `BLOCKED` and `UNMEASURABLE`, double for `CONFIG_ERROR` — and compact density adds a diagonal fill pattern to `DOWN` and `BLOCKED`. The state glyph and text remain inside every tile, and the tile's `aria-label` carries the state text, so colour is never the only carrier. A Playwright test reads the computed ring style and width for three states and fails if they are identical, and checks the compact pattern is present.

### 10.8 Live updates

SSE stream of one state snapshot per connection followed by deltas - not repeated full pages. The UI consumes it at `GET /events` with the session cookie; API clients use `/api/v1/events` with a bearer token (`API.md` §7.2). The API does not accept the UI's session cookie: the two surfaces share the token but not the session store.

The snapshot exists because the page render and the stream are not atomic: a change in that gap would otherwise sit inside the baseline and never be sent. It is bounded by connections, not by changes, so a single delta still cannot amplify into a whole grid. The API stream's matching mechanism is `Last-Event-ID` replay (`API.md` §6.17).

Reconnect is automatic: the server sends `retry: 3000`, and the badge distinguishes `live` from `retrying`. There is no separate polling fallback - a second code path for a rare case is more likely to break than to help, and a page reload always recovers. If `EventSource` is unavailable the badge says so instead of pretending to be live.

---

## 11. Internet Health Index (IHI)

A single 0-100 number, with its components always shown underneath. Never the only thing on screen.

| Component | Weight | Source |
|---|---|---|
| Own health (live judgeable share) | 0.35 | §7.2 |
| External outage severity | 0.25 | highest feed severity in 24h |
| BGP stability | 0.15 | Radar hijack/leak severity |
| Corridor stability | 0.15 | §9 |
| Tamper signals | 0.10 | DNS divergence verdicts (`CLASSIFIER.md` §3.2); cert pins are shown, not scored |

Rules:

- Stale components are dropped and remaining weights renormalised
- **The own-health component is weighted by `coverage`.** A scope below 50% measurable is dropped entirely, exactly like a stale feed; a scope at 60% measurable contributes at 60% weight. Without this, a bot-wall rollout silently raises the index - the tool reads *more* healthy as it sees *less*
- IHI never triggers anything on its own - it is a summary, not a detector
- The components are always one click away, with their timestamps

**Implemented:** `internal/ihi` computes the index per request, so it cannot go stale between ticks. The board carries an `IHI nn` chip linking to `/ihi`, which lists every scored component with its score, effective weight, and evidence time, then every dropped component with the reason it was dropped; `GET /api/v1/ihi` serves the same shape.

- **Own health** is the share of judgeable targets that are up (`up` and `up_reachability` count 1, `degraded` counts 0.5, `down` counts 0; `blocked`, `unknown`, `config_error`, and `unmeasurable` are not evidence) times the measurable share of the fleet. It is a live health share, not a stored baseline comparison; the baseline detector remains `anomaly` (7.2) and this component deliberately does not duplicate it.
- **External outage severity** is the highest severity among feed events in the last 24 hours, scored `100 x (1 - severity)`; no event is a current 100 only when the IODA feed is healthy, because a running feed that reports nothing is a current answer. A feed that has never succeeded, is stale beyond 30 minutes, or is failing drops the component with that reason instead.
- **BGP stability** is the same computation restricted to events whose kind names a hijack or a leak, gated on the Radar feed's health. The two components are gated separately so a dead IODA feed does not silence routing evidence.
- **Corridor stability** is the share of configured corridors with no open step change. A deployment with no corridors drops the component rather than scoring 100.
- **Tamper signals** starts at 100 and loses 20 per DNS divergence verdict (`TAMPER` or `DNS_ANOMALY`) in the last 24 hours, floored at 0.
- The component timestamp is when its evidence was read, except the corridor component, which carries the newest sample so a dead sampler drops it as stale. The TTL is one hour.
- **Deliberately not included: cert-issuer changes.** They are detected, pinned, and shown in the drawer, but they are stored as pins rather than as a dated signal series, so counting them over a window would be inventing a history the database does not keep. The tamper component measures DNS divergence only, and this paragraph is the reason.
- The component weights live in `internal/ihi` and sum to 1; a change there changes the documented table too.


## 12. Storage and retention

SQLite (WAL), `modernc.org/sqlite` (pure Go, no CGO).

Every probe and rollup row carries `vantage_id`, and rollups are keyed `(target_id, vantage_id, bucket)` — paths are never mixed, because a Tor RTT and a direct RTT are not the same measurement.

| Tier | Retention | Est. size |
|---|---|---|
| Raw probes | 72h | ~170 MB |
| Incident-pinned raw probes | 1y | small (bounded by incident count) |
| 5-minute rollups | 7d | ~80 MB |
| Hourly rollups | 30d | ~32 MB |
| Daily rollups | 1y | ~16 MB |
| Scope samples (the hour-of-week baseline) | 28 weeks | ~140 MB |
| Incidents | forever | small |
| Anomalies, external events, egress moves | 90d | small |
| Self-checks | 30d | small |
| Atypical days (baseline calendar) | forever | one row per day per scope |

Steady state ≈ **480 MB** for 750 targets at ~430k probes/day, with a hard ceiling well under 1 GB. The maintenance tick (ten minutes) folds and prunes in batches of ≤5000 rows, and **one write transaction per dispatcher tick** — never one fsync per probe.

**Implemented, the prune:** `store.Prune` deletes raw probes in 5,000-row batches, then the archive at one year, then the three rollup tiers at 7/30/365 days, then `anomaly`, `external_event`, and `vantage_change` at 90 days, `self_check` at 30 days, and `scope_sample` at the 28-week baseline window. Feed events are aged on `observed_at`, so an event a feed is still reporting keeps refreshing and a long outage is not deleted while it is current; an anomaly is aged on its own timestamp, and an egress move on `changed_at`. Incidents and the atypical-day calendar are never pruned.

**Rollups are incremented, never recomputed.** Each tick upserts into the current 5-minute bucket; hourly is folded from 5-minute at bucket close; daily from hourly. Recomputing a 5-minute rollup by scanning raw probes is the one query that would make this design slow at 750 targets.

Rollup latency columns are **EWMA, not percentiles**, and are named `ewma50_ms` / `ewma95_ms` in the schema. Calling an EWMA a p95 is the kind of quiet lie this project exists to avoid.

Raw probes within ±1 hour of a confirmed incident are copied to `probe_archive` before pruning, so a drawer opened a month later can still show the probes around the event. Retention without that is 72 hours of memory.

**Implemented:** `store.OpenIncident` pins the ±1 hour window in the same transaction that writes the incident, keyed by `(target_id, vantage_id, ts)` so a retry or a second incident over the same window cannot duplicate or steal rows. `store.RecentProbes` unions the raw and archived tables, `store.IncidentProbes` reads one incident's samples oldest first, `Prune` deletes the archive after one year, and the incident page and `GET /api/v1/incidents/{id}` show the samples with their stage, error class, and latency.

Full schema, PRAGMAs, retention jobs, and query shapes in [`SCHEMA.md`](SCHEMA.md).

---

## 13. Configuration and deployment

### 13.1 Container

```yaml
services:
  netweather:
    image: ghcr.io/<owner>/netweather:latest
    networks: [ui, egress]            # see NETWORKING.md
    ports: ["2600:2600"]              # UI on 2600, all interfaces; auth required
    volumes:
      - ./data:/data
      - ./config:/config:ro
      - ./secrets/ui_token:/run/secrets/ui_token:ro
    environment:
      NETWEATHER_LOG_LEVEL: info
      NETWEATHER_UI_LISTEN: 0.0.0.0:2600
      NETWEATHER_UI_TOKEN_FILE: /run/secrets/ui_token
      TZ: America/Denver
    read_only: true
    tmpfs: [/tmp]
    user: "65532:65532"
    cap_drop: [ALL]
    sysctls:
      net.ipv4.ping_group_range: "0 2147483647"   # unprivileged ICMP
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "/netweather", "healthcheck"]

networks:
  ui:
  egress:
    driver_opts:
      com.docker.network.driver.mtu: 1400
```

- ~20 MB image, ~80 MB RSS at 750 targets
- Multi-arch: linux/amd64, linux/arm64
- Egress only; no inbound required except the UI port (2600)
- **Ships with 14 targets.** `netweather seed` generates sector/country lists for users who want scale

Base image, hardening, capabilities, and supply chain: see [`IMAGE.md`](IMAGE.md). Network topology, MTU, DNS, SSRF reachability, port choice, and access posture: see [`NETWORKING.md`](NETWORKING.md).

### 13.2 Config layout

`NETWEATHER_CONFIG` may point at a single YAML file or at a directory. The directory form is for a large list: the sector is the path, so adding a sector means adding a file, not repeating a tag on every line.

```
config/
  netweather.yaml          # optional: version, extra targets, corridors
  sites/
    financial/banking.txt      # one domain per line
    financial/payments.txt
    travel/airlines.txt
    cloud/hyperscalers.txt
    ...
  overrides/
    chase.yaml            # only for targets needing real checks
    delta.yaml
  corridors.yaml
```

The path under `sites/`, without the extension, **is** the sector/sub-sector: `financial/banking.txt` produces sector `financial/banking`.

**Implemented, reload failures:** the config loop polls the path every 30 seconds and reconciles on a change. A rejected edit leaves the previous target set live and records the rejection in a shared state; the board then shows a `config-reload` banner naming the time and the reason, so the typo is visible where the operator is looking rather than only in the log (`THREAT_MODEL.md` T36). A later successful reload clears it.

`sites/*.txt` defaults: `http` probe on 443, `interval: 300s`, any 2xx/3xx status (`expect.status_min: 200`, `expect.status_max: 399`), or reachability only (no content assertion). Blank lines and `#` comments are ignored. The single-file `targets.yaml` form in the README stays the default; the directory form is loaded by the same `config.Load`, so the existing file schema keeps working.

`netweather.yaml` may carry `targets:` and `corridors:` that do not belong to a sector file. It does **not** carry the listen address, auth, log settings, or feed tokens: those stay environment variables, because a second source of truth for them is how two configs drift.

```yaml
# overrides/chase.yaml
target: chase.com          # must match a host from sites/ or netweather.yaml
name: Chase
sector: financial/banking
probe: tls                 # reachability only; HTTP is bot-walled
interval: 600s
status_url: https://www.chasestatus.com/
expect:
  status: [200, 301, 302]
  max_redirects: 3          # 0-20; zero uses the instrument's default of 5
```

An override is a **change to a target that already exists**, not a way to add one: an unmatched `target` is a startup error, so a file that silently edits nothing cannot ship. The supported keys are the ones above plus `qname` and `url` for a `doh` override (the question and the HTTPS endpoint, which providers locate differently), `path` for an `http` override (the request path, which is how the robots.txt recipe in §5.3 is expressed), plus `expect`; unknown keys, including a `tags` list the schema does not have, are rejected rather than ignored. `status_min`/`status_max` and a `status` list may coexist; a status admitted by either is accepted.

### 13.3 Hot reload

Config changes apply without a restart. The path is polled every 30 seconds rather than watched, because config edits are rare, the check is one walk, and inotify does not follow into every bind mount. For a directory the stamp is the newest mtime, total size, and file count across the tree, so editing `sites/financial/banking.txt` is seen even though the directory's own mtime did not move. Invalid changes are rejected, logged, and the previous config stays live. A config error must never take the monitor down.

---

## 14. Guardrails (enforced in code)

| Guardrail | Value |
|---|---|
| Minimum interval per host | 30s, not configurable lower |
| Per-host concurrency | 1 |
| Global rate ceiling | 20 req/s default (`NETWEATHER_RATE_LIMIT`), ramped from 2 req/s over ten minutes from the database's first target; **implemented** in the agent's Step pacing |
| Allowed ports (outbound) | 80, 443, 53 (DNS), 123 (NTP, fixed allowlist), 853 (DoT, implemented). **22 and everything else excluded.** |
| Inbound port | `2600` only; no other listener. Non-loopback bind requires auth (`NETWORKING.md` §7.1) |
| API rate limit | 60 req/min per token, burst 20, `429` with `Retry-After` (`API.md` §8) |
| Allowed probe methods | GET, HEAD. The API's POST actions (pause, resume, annotate, trace, disable) are not outbound probes |
| ICMP | Echo request only, never to private ranges, never the sole up/down signal |
| Traceroute | On-demand or anomaly-triggered only; ≤10 runs/hour; never to private ranges |
| NTP | Fixed allowlist of clock sources only - never an arbitrary host. **Implemented:** config refuses an `ntp` target outside `pool.ntp.org` (including numbered members), `time.cloudflare.com`, `time.google.com`, `time.apple.com`, and `time.windows.com` |
| Tor | Never an exit relay: `ClientOnly 1` + `ExitPolicy reject *:*` are baked into the shipped `torrc` and pinned by a test. `doctor` checks that the SOCKS endpoint answers, not the peer's config, which it cannot read |
| Proxy sources | User-supplied SOCKS5 endpoints only — no scraped or discovered proxy pools |
| Tor volume | Off by default; ≤25 targets per vantage; ≥2s spacing; daily MB budget with hard stop |
| Private ranges | RFC1918, loopback, link-local, CGNAT `100.64.0.0/10`, `169.254.169.254`, IPv6 ULA - **blocked after DNS resolution and after every redirect**. In Docker this includes `172.17.0.1` (the host) and `host.docker.internal` — see `NETWORKING.md` §6 |
| Max targets | 2000 |
| Max redirects | per target, 0-20, default 5 (`expect.max_redirects`); exhaustion returns the last 3xx rather than an error |
| Max body read | 32 KB |
| Daily egress budget | 2 GB default, hard stop with a banner - the backstop for every rate above. **Implemented:** `NETWEATHER_DAILY_MB` wraps the direct dialer's connections, suspends the agent at the limit until UTC midnight, and shows the banner |
| Default bind | `0.0.0.0:2600`. Non-loopback listen requires auth; `ui.auth: none` + non-loopback refuses to start unless `ui.allow_public: true` is also set (`NETWORKING.md` §7.1) |
| Container runtime | `/var/run/docker.sock` is never mounted; host networking is never the default |
| Telemetry | none, ever |

Full rationale in `THREAT_MODEL.md`, `ACCEPTABLE_USE.md`, and `NETWORKING.md` §6.

---

## 15. Roadmap

The living plan — what is now, next, later, and deliberately not planned — is
[`ROADMAP.md`](../ROADMAP.md). This table is the phase record.

| Phase | Scope | Proves | Status |
|---|---|---|---|
| **0** | Service targets, probe ladder, classifier (incl. per-resolver DNS baseline), SQLite, board + overview, reference/LOCAL, baseline health | It is not noise | Implemented |
| **1** | Country anchors, ASN enrichment, coverage panel, attribution, world view, corridors, optional RIPE Atlas profile, Atlas feed plugin (read-only) | It sees more than a favicon grid | Implemented. The RIPE Atlas **software-probe profile** ships (`docs/RIPE_ATLAS.md`), and the Atlas **feed plugin** now ships as a read-only adapter (`internal/feeds/atlas`) that reads measurements the operator created and never spends credits |
| **2** | IODA + Radar plugins, Statuspage corroboration, IHI, drawer depth | It detects large events | Implemented |
| **3** | RIS Live, cert pinning, multi-instance agent, live IODA overlay | It detects tampering and mass events | Cert pinning, both RIS Live adapters (RIPEstat poller plus opt-in real-time WebSocket), the bounded live IODA level overlay, and federation-lite remote agents implemented. Vantages are first-class (direct, Tor, remote); an agent runs the same instruments and reports to `/api/v1/vantages/{id}/reports`. mTLS and signed batches are a hardening item. |

Phase 0 is the whole thesis in miniature. If phase 0 is noisy, nothing later saves it.

---

## 16. Decisions and open questions

**Resolved:**

- **Name — `netweather`.** Neutral, descriptive of what it reports, no trademark collision. "Downdetector" is an Ookla trademark and is never used as a name.
- **RIPE Atlas — optional compose profile** (§8.6), off by default.
- **Traceroute - on-demand only, via the opt-in `compose.trace.yaml` override** that adds `CAP_NET_RAW`; the default image has no capabilities.
- **Tailscale — compose profile only** (`NETWORKING.md` §7); the default compose publishes exactly one port.
- **BGP - v1 ships IODA and Radar summaries only (phase 2).** RIS Live ships as both a bounded RIPEstat poller (`internal/feeds/rislive`) and an opt-in real-time WebSocket (`internal/feeds/rislive_ws.go`); the poller stays the default where a persistent socket is not wanted.

**Open:**

1. **Federation.** Tor vantages (§5.7) provide a cheap second path today; true multi-operator federation (a public map) is post-v1 and off by default — the privacy cost is real, and a federated instance leaks its target list to its peers.
2. **Default seed.** Ship a small curated list, or ship a `seed` command that pulls from public lists? Currently: both — 14 curated targets, plus the generator for operators who want scale.
3. **Corridor endpoints.** With the Atlas profile available, Atlas anchors become the default when it is on; country NS hosts remain the self-contained fallback. Which is authoritative when both exist?
4. **`DEGRADED` thresholds.** Latency-based degradation needs a per-target EWMA baseline; exact multiplier (3× p50? 2× p95?) needs real data before it is codified.
5. **Dashboard exposure.** Resolved in `NETWORKING.md` §7.1: bind `0.0.0.0:2600`, require auth, and put it on the tailnet via the optional sidecar profile when it should not be on the LAN at all.
6. **IPv6 as a probe family.** Docker's IPv6 networking is fiddly enough that it is currently host-networking-only (`NETWORKING.md` §14).
