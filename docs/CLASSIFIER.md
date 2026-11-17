# netweather — Classifier

> The rule table that decides what a response *means*. This is the difference between a useful monitor and a machine that cries wolf.
>
> Read `DESIGN.md` §2–§3 first.

---

## 1. Contract

```
classify(target, response) -> (state, confidence, reason)
```

- **Deterministic.** Same input, same output. No network calls inside the classifier.
- **Total.** Every response maps to a state. No `nil`, no "unknown error."
- **Explainable.** `reason` is a human sentence shown in the target drawer.
- **Testable offline.** Inputs are recorded fixtures (§13). The classifier never touches the network.

The classifier does **not** decide `DOWN` vs `BLOCKED` on its own authority — it emits a candidate state. Confirmation (§7.4 of `DESIGN.md`) promotes candidates over multiple probes.

---

## 2. Precedence

Order is load-bearing. Applying these out of order is the most common way to build a monitor that lies.

```
1. Transport failure        -> error class            (§3)
2. Challenge signature      -> BLOCKED                (§6)
3. Status in expected set   -> UP / UP_REACHABILITY / DEGRADED   (§5, §7)
4. Config-error statuses    -> CONFIG_ERROR, not DOWN (§5.2)
5. Rate limit / legal       -> BLOCKED                (§5.3)
6. Origin 5xx               -> DOWN                   (§5.2)
7. Fallback                 -> DEGRADED, low confidence
```

Why challenge before status: a WAF commonly returns `503` or `403`. If the config expects `[200, 503]`, checking status first would mark a bot-wall as healthy. If the config expects `[200]`, checking 5xx first would mark it as an outage. Both are wrong.

### 2.1 Vantage precedence

Classification runs **per vantage**, and the results are combined afterwards:

- Only the `primary` vantage may produce `DOWN`.
- A corroborating vantage that disagrees produces `REGIONAL` — never `DOWN`, never `UP`.
- `BLOCKED` from any vantage is `BLOCKED`: it is evidence about that path, not about the service.
- Never average or merge latencies across vantages.

A lone failure from a corroborating vantage is not even a signal. Tor exits are blocklisted widely enough that a failure there is the expected outcome, not news.

---

## 3. Transport error classes

### 3.1 DNS

| Class | Condition | Candidate state | Notes |
|---|---|---|---|
| `dns_nxdomain` | RCODE 3 | DOWN | **Must** be corroborated (§3.2) |
| `dns_nodata` | RCODE 0, 0 answers | DOWN | Apex exists but no A/AAAA |
| `dns_servfail` | RCODE 2 | DOWN | Could be resolver, not target |
| `dns_refused` | RCODE 5 | DOWN | |
| `dns_timeout` | no response in 3s | DOWN | Retry once with TCP/53 |
| `dns_divergence` | answer outside that resolver's own baseline | **DNS_ANOMALY**; `TAMPER` only with a second signal | See §3.2 |

### 3.2 DNS divergence — the tamper detector

Three resolvers: local, DoH `1.1.1.1`, DoH `9.9.9.9`. **They are not compared to each other.**

GeoDNS means a CDN target legitimately returns a different edge to each resolver, in each location, at each hour. Cross-resolver comparison therefore flags every large site in the list, continuously — "DoH disagree with each other" is the *normal* case, not an anomaly. The earlier consensus rule (`local != DoH consensus`) was wrong and is removed.

Compare **each resolver against its own baseline for that target**:

```
baseline(resolver, target) = {
    answer_set:   { A/AAAA seen in the last 7d, capped at 32 },
    asn_set:      { origin ASN per answer },
    cname_suffix: { registrable suffix of the final CNAME, e.g. "cloudfront.net" },
    delegation:   NS set at the parent, from the authoritative query
}
```

Signals, ascending by confidence:

| Signal | Meaning | Strength |
|---|---|---|
| New address inside the baseline `asn_set` and `cname_suffix` | CDN edge rotation — **normal** | ignore |
| New address outside the baseline `asn_set` | New hosting, or interception | weak |
| `SERVFAIL`/`REFUSED` where it answered within 7d | Resolver-side or path-side | medium |
| `NXDOMAIN` where it answered within 7d | Interception or bad delegation | strong |
| Changed NS delegation at the parent | Delegation hijack | strong |
| Different answer from **one** resolver only | Local resolver tampering | strong |

The ladder:

```
1 strong signal, one resolver                        -> DNS_ANOMALY   (advisory)
1 strong + 1 medium on the same target               -> TAMPER
strong signal on the same target from >= 2 vantages,
  or from 2 distinct resolvers                       -> TAMPER       (high confidence)
```

`DNS_ANOMALY` never colours the board. It appears in the drawer and in a collapsed "DNS observations" strip. A single resolver disagreeing with its own history is usually a resolver upgrade, a CNAME migration, or a bad day at the recursive resolver — and calling that state-level interference is exactly the overclaim that gets a monitor ignored.

`TAMPER` requires the second signal because it is the highest-severity claim the tool can make.

Baselines are per-resolver, per-target, rebuilt on every answer, and reset when `VANTAGE_CHANGED` fires (`DESIGN.md` §7.5).

**Do not** classify `dns_nxdomain` as DOWN when the other resolvers resolve the name. That is divergence, not an outage.

### 3.3 TCP

| Class | Condition |
|---|---|
| `tcp_refused` | RST on connect |
| `tcp_timeout` | no SYN-ACK within timeout |
| `tcp_reset` | RST mid-stream |
| `tcp_unreachable` | ICMP unreachable |

All → DOWN.

### 3.4 TLS

| Class | Condition | Candidate state |
|---|---|---|
| `tls_handshake` | generic handshake failure | DOWN |
| `tls_cert_expired` | `NotAfter` in the past | DOWN |
| `tls_cert_not_yet_valid` | `NotBefore` in the future | DOWN |
| `tls_cert_mismatch` | SAN does not cover host | DOWN |
| `tls_cert_untrusted` | chain does not validate to system roots | DOWN |
| `tls_protocol` | no shared version/cipher | DOWN |
| `tls_reset_after_hello` | RST immediately after ClientHello, repeatedly | **BLOCKED_TLS** (low confidence) |
| `tls_issuer_unexpected` | issuer outside learned set (§3.5) | **TAMPER** |

`tls_reset_after_hello` is ambiguous: it is either JA3-based blocking or a middlebox. Only classify as `BLOCKED_TLS` when the same host answers other probe types successfully and the reset is reproducible. Otherwise treat as DOWN.

### 3.5 Certificate pinning — the right way

**Do not pin the leaf SPKI.** Leaf certificates rotate every 60–90 days, and pinning them produces constant false TAMPER alarms.

Pin the **issuer set** instead:

1. On first success, record `(issuer DN, intermediate SPKI, chain length)`.
2. Maintain a learned set of issuers per target (a target may legitimately rotate between two CAs).
3. Alert `tls_issuer_unexpected` when the issuer validates against system roots **but is not in the target's learned set**.
4. Record leaf changes as informational only.

That rule catches the real threat — a locally trusted CA being used to intercept a specific host — without firing on ordinary renewals. Chain-length changes and validity periods > 398 days are also informational flags.

### 3.6 ICMP and NTP

| Class | Condition | Candidate state |
|---|---|---|
| `icmp_timeout` | no echo reply | DOWN **only** for `icmp`-class targets (anchors, corridors) |
| `icmp_unreachable` | ICMP unreachable / admin prohibited | DOWN |
| `icmp_loss_partial` | < 5 of 5 replies | `DEGRADED` — **never** DOWN |
| `ntp_unreachable` | no reply from ≥2 of 4 servers | self-health warning only |
| `ntp_offset` | offset > 2s | self-health warning only |

Two hard rules:

1. **ICMP loss is never `DOWN`.** Partial loss is a path-quality signal. It belongs to corridors, not to service targets.
2. **An ICMP timeout on a service target is meaningless.** CDNs, clouds, and banks drop ICMP by policy. Only `icmp`-class targets — ones chosen *because* they answer ICMP — may be classified from it.

NTP never produces a target state at all. It feeds `DESIGN.md` §7.5 self-health and can only warn.

---

## 4. Response normalization

Before classification:

- Follow redirects up to `max_redirects` (default 5), re-running the private-IP guard on each hop
- Record every hop: `(status, location, challenge_signature)`
- Read at most 32 KB of body, decompressed
- Lowercase headers for matching; preserve original for display
- Body matching is case-insensitive on the first 32 KB, on decoded text (not gzip bytes)

If any **hop** in the redirect chain carried a challenge signature, that is a `BLOCKED` — the final page is irrelevant.

---

## 5. HTTP status mapping

### 5.1 Default expected set

```yaml
expect:
  status: [200, 203, 204, 301, 302]   # 3xx only if redirects exhausted
```

### 5.2 Status table

| Status | Classification | Rationale |
|---|---|---|
| 200–299 | evaluate semantic | |
| 300–399 | follow, up to max | |
| 401 | `BLOCKED` (auth wall) | We are not authenticated; service is fine |
| 403 | `BLOCKED` after challenge check | Never an outage |
| **404** | **`CONFIG_ERROR`** | Your probe path is wrong. **Not** an outage. |
| 405 | retry with `GET`; then `CONFIG_ERROR` | Often HEAD-only rejection |
| 407 | `CONFIG_ERROR` | Proxy auth |
| 408 | `DOWN` | Origin gave up |
| **410** | **`CONFIG_ERROR`** | Path gone |
| **429** | **`BLOCKED`** | Rate limited. Honour `Retry-After`. |
| **451** | **`BLOCKED`** | Unavailable for legal reasons |
| 500 | `DOWN` | |
| 502 | `DOWN` | |
| 503 | challenge check first, else `DOWN` | WAFs love 503 |
| 504 | `DOWN` | |
| 520–527 | `DOWN` | Cloudflare origin errors (real failures) |
| 530 | `DOWN` | Cloudflare origin DNS failure |
| **999** | **`BLOCKED`** | LinkedIn's non-standard block |
| other 4xx | `CONFIG_ERROR` | |
| other 5xx | `DOWN` | |

`CONFIG_ERROR` is a distinct, visible state — the target renders with a wrench badge and appears in a "config problems" list. It never contributes to health. This single rule eliminates the most common source of false alarms in naive monitors: a 404 on a path that used to exist.

### 5.3 Redirect signatures

A redirect target matching any of these means the chain was challenged:

| Pattern | Vendor |
|---|---|
| `/__cf_chl`, `/cdn-cgi/challenge-platform` | Cloudflare |
| `/sorry/`, `google.com/sorry` | Google |
| `/incapsula/` | Imperva |
| `/ddm/`, `captcha-delivery.com` | DataDome |
| `/px/`, `/px/captcha` | PerimeterX / HUMAN |

---

## 6. Challenge signatures

`BLOCKED` requires **at least one** signature. Header match is strong; body match is strong; cookie presence alone is weak and must not be used alone.

### 6.1 Headers

| Header / value | Vendor |
|---|---|
| `cf-mitigated: challenge` | Cloudflare |
| `x-amzn-waf-action: challenge` \| `captcha` | AWS WAF |
| `x-iinfo` present | Imperva |
| `x-datadome` present | DataDome |
| `x-px-*` present | PerimeterX / HUMAN |
| `x-kpsdk-*` present | Kasada |
| `x-sucuri-block` present | Sucuri |
| `x-vercel-mitigated: challenge` | Vercel |
| `server: AkamaiGHost` **+** status 403 | Akamai |

### 6.2 Body markers

Match case-insensitively in the first 32 KB.

| Marker | Vendor |
|---|---|
| `Just a moment...` | Cloudflare |
| `Checking your browser before accessing` | Cloudflare / generic |
| `Enable JavaScript and cookies to continue` | Cloudflare |
| `Attention Required! \| Cloudflare` | Cloudflare |
| `cf-error-details` **+** `challenge` | Cloudflare |
| `Access Denied` **+** `Reference #` | Akamai |
| `Reference #18.` / `Reference #9.` | Akamai |
| `Request unsuccessful. Incapsula incident ID` | Imperva |
| `_Incapsula_Resource` | Imperva |
| `captcha-delivery.com` | DataDome |
| `px-captcha` | PerimeterX |
| `Please verify you are a human` | generic |
| `Pardon Our Interruption` | Distil |
| `unusual traffic from your computer network` | Google |
| `The requested URL was rejected` | F5 ASM |
| `Radware Bot Manager Captcha` | Radware |
| `Are you a robot?` | generic |
| `Checking if the site connection is secure` | generic |

### 6.3 Cookies

| Cookie | Meaning | Use |
|---|---|---|
| `cf_clearance` | Cloudflare challenge **passed** | Informational |
| `_abck`, `bm_sz` | Akamai sensor cookies | Informational |
| `visid_incap_*` | Imperva issued a session | Informational |
| `datadome`, `_px3`, `pxcts` | Challenge issued | Informational |

Cookie presence indicates the site uses protection. It is **not** evidence of a block, and must never be the sole signature.

### 6.4 JS-shell detection (200 but not meaningful)

These are not `BLOCKED` — they are "cannot assess content":

| Marker | Result |
|---|---|
| `<noscript>` **+** `enable JavaScript` | `UP_REACHABILITY`, low confidence |
| Body < 2 KB, contains `<script src=`, no target marker | `UP_REACHABILITY`, low confidence |
| Empty SPA root (`<div id="root"></div>`) | `UP_REACHABILITY`, low confidence |

Never report these as `DEGRADED`. We did not fail to observe a problem; we failed to observe anything.

---

## 7. Semantic checks

Configured per target. All optional.

| Check | Passes when |
|---|---|
| `status_in` | status ∈ set |
| `body_contains` | substring present (case-insensitive) |
| `body_not_contains` | substring absent |
| `body_regex` | regex matches |
| `title_contains` | `<title>` contains |
| `header_present` | header exists |
| `header_value` | header equals |
| `json_path` | JSON path resolves to expected value |
| `min_body_bytes` | body length ≥ n |

Outcomes:

- No semantic check configured → `UP_REACHABILITY`, confidence **low**
- All configured checks pass → `UP`, confidence **high**
- Any check fails → `DEGRADED`, confidence **medium**
- Body was a JS shell (§6.4) → `UP_REACHABILITY`, confidence **low**, semantic skipped

---

## 8. Decision function

```go
func classify(t Target, r Response) (State, Confidence, Reason) {
    // 1. transport
    if r.Err != nil {
        return transportClass(r.Err), ConfidenceFor(r.Err), r.Err.Error()
    }

    // 2. challenge — before status, before semantic
    if sig := challengeSignature(r); sig != "" {
        return Blocked, Medium, sig
    }

    // 3. expected status
    if in(r.Status, t.Expect.Status) {
        if isJSShell(r.Body) {
            return UpReachability, Low, "no assessable content"
        }
        if !t.Expect.HasSemantic() {
            return UpReachability, Low, "reachability only"
        }
        if ok, why := runSemantic(t.Expect, r); !ok {
            return Degraded, Medium, why
        }
        return Up, High, "semantic ok"
    }

    // 4. auth / bot wall without a signature (§5.2)
    if r.Status == 401 || r.Status == 403 {
        return Blocked, High, fmt.Sprintf("blocked: %d", r.Status)
    }

    // 5. config errors, never outages
    if r.Status == 404 || r.Status == 410 || r.Status == 405 ||
       (r.Status >= 400 && r.Status < 500 &&
        r.Status != 408 && r.Status != 429 && r.Status != 451) {
        return ConfigError, High, fmt.Sprintf("%d on probe path", r.Status)
    }

    // 6. rate limit / legal
    if r.Status == 429 || r.Status == 451 || r.Status == 999 {
        return Blocked, High, fmt.Sprintf("blocked: %d", r.Status)
    }

    // 7. origin failure
    if r.Status >= 500 {
        return Down, High, fmt.Sprintf("origin %d", r.Status)
    }

    return Degraded, Low, "unclassified"
}
```

---

## 9. Confidence

| Level | When |
|---|---|
| **high** | Transport resolved and semantic passed; or unambiguous challenge / origin 5xx |
| **medium** | Single-signature challenge; semantic mismatch; a DNS-derived `DOWN` (needs corroboration) |
| **low** | No semantic check (reachability only), JS shell, flapping (>6 transitions/24h), single observation |

Confidence propagates: a `DOWN` derived from a `low`-confidence probe is not promoted to `INCIDENT` on its own — it needs a second observation.

---

## 10. Overrides

Per-target escape hatches, all explicit:

```yaml
classify:
  treat_403_as: blocked        # or down (rare, for APIs)
  treat_404_as: up             # for endpoints where 404 is a valid answer
  ignore_challenge: false      # never set true for service targets
  expected_issuers:            # learned automatically if omitted
    - "DigiCert"
    - "Let's Encrypt"
  max_body_bytes: 32768
```

`ignore_challenge: true` exists only for API targets that return a challenge-looking body legitimately. It must be logged at warn level and shown in the UI as "classifier override active."

---

## 11. Hard-target recipes

Banks, airlines, socials, and marketplaces will block a container forever. Measure them honestly:

| Class | Recipe | Expected |
|---|---|---|
| **Retail bank** | `probe: tls` + `GET /robots.txt` | Usually `BLOCKED` on HTTP → falls back to TLS → `UP` if the edge is serving |
| **Airline** | `GET /robots.txt` on the static/asset host, not `www` | Often `UP`; `www` will be Akamai-blocked |
| **Social** | `GET /robots.txt` | Reliably `200`; the most bot-friendly path on the internet |
| **Hyperscaler** | Status API + a static object from their CDN | `UP` with high confidence |
| **CDN-fronted SaaS** | `GET /.well-known/security.txt` | Small, static, rarely challenged |
| **Government** | `probe: tcp` + `GET /robots.txt` | `UP_REACHABILITY` |
| **Anything >80% blocked** | Downgrade to `probe: tls`, tier 3, then `UNMEASURABLE` | Excluded from health |

If a target is `UNMEASURABLE`, say so in the UI and stop spending requests on it. Do not let 38 bot-walled targets make the board look permanently on fire.

---

## 12. Anti-patterns

These are prohibited, with reasons. Each one has caused a real false-alarm epidemic in some monitoring tool.

| Anti-pattern | Why it is wrong |
|---|---|
| Treating `404` as DOWN | The path changed; the service is fine |
| Treating `403` as DOWN | Bot protection is not an outage |
| Treating `429` as DOWN | Rate limiting is not an outage |
| Treating `HEAD` `405` as DOWN | Many servers reject HEAD |
| Treating `NXDOMAIN` as DOWN without DoH check | Hides DNS tampering and misdiagnoses resolver failures |
| Treating a single failure as DOWN | One packet is not an outage |
| Relying on `User-Agent` to avoid blocks | WAFs fingerprint TLS, not headers |
| Evading bot protection | Violates P2; produces invalid data |
| Pinning leaf certificate SPKI | Fires on every legitimate renewal |
| Following redirects without re-checking private IPs | SSRF |
| Counting `BLOCKED` in the health index | Makes the board permanently red |
| Marking JS shells as `DEGRADED` | We observed nothing; that is not a fault |
| **Using ICMP as the sole up/down signal** | Most CDNs and clouds drop it by policy |
| **Treating traceroute `*` hops as loss** | Routers rate-limit TTL-exceeded; stars are not evidence |
| **Scheduling traceroute** | It is a 90-packet walk across third-party routers; on-demand only |
| Using `tcpping` success as proof the app works | A load balancer answering is not a healthy origin |
| Treating a Tor-vantage failure as an outage | Exits are blocklisted by design; a failure there is expected |
| Averaging latency across vantages | Different paths are different measurements |
| Chaining proxies | Multiplies latency and variance, and tells you nothing about any single path |
| Comparing resolvers to each other for a GeoDNS target | Different edges are normal; compare each resolver to its own history |
| Raising `TAMPER` on one resolver signal | Highest-severity claim; needs a second, independent signal |

---

## 13. Golden fixtures

The classifier test suite is a directory of recorded responses plus expected outcomes. No network access in CI.

```
testdata/
  001-plain-200-semantic-pass.http
  002-plain-200-no-semantic.http
  003-cf-challenge-403.http
  004-cf-challenge-503.http
  005-cf-1020-waf.http
  006-akamai-reference-403.http
  007-imperva-incident-403.http
  008-datadome-redirect-302.http
  009-perimeterx-px-captcha.http
  010-aws-waf-challenge.http
  011-origin-502.http
  012-cf-520-origin.http
  013-rate-limit-429.http
  014-legal-451.http
  015-linkedin-999.http
  016-not-found-404.http
  017-head-405.http
  018-js-shell-200.http
  019-empty-spa-200.http
  020-semantic-mismatch.http
  021-redirect-loop.http
  022-tls-cert-expired.txt
  023-tls-issuer-unexpected.txt
  024-dns-nxdomain-doh-agrees.txt
  025-dns-nxdomain-doh-disagrees.txt
  026-tcp-refused.txt
  027-tls-reset-after-hello.txt
  028-retry-after-429.txt
  029-503-without-challenge.http
  030-multi-hop-challenge-in-chain.http
  031-icmp-timeout-anchor.txt
  032-icmp-partial-loss-corridor.txt
  033-ntp-offset-warning.txt
  034-traceroute-star-hops.txt
  035-tor-exit-blocked.txt
  036-vantage-disagreement-regional.txt
  037-tor-dns-leak-detected.txt
  038-geodns-edge-rotation-not-divergence.txt
  039-dns-anomaly-single-strong-signal.txt
  040-tamper-two-independent-signals.txt
  041-learning-mode-no-incident.txt
```

Each fixture is a small file:

```http
# expect: BLOCKED
# confidence: medium
# reason: cloudflare challenge (header)
--- request
GET / HTTP/1.1
Host: example.com
--- response
HTTP/1.1 403 Forbidden
cf-mitigated: challenge
content-type: text/html

<html>...
```

Adding a WAF signature without a fixture is not allowed to merge. That rule is what keeps the classifier honest over time.

---

## 14. Changing this document

Classifier changes are behaviour changes and need:

1. A new or updated fixture in `testdata/`
2. A note in `CHANGELOG.md` under "Classifier"
3. A re-run of the corpus against 7 days of recorded production probes, with the diff in state counts shown in the PR

Rule 3 matters: a signature tweak that reclassifies 4% of your corpus as `BLOCKED` is a big deal, and should be visible before merge, not discovered a week later.
