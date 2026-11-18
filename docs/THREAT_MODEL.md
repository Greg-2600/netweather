# netweather — Threat model

> This tool is a public-facing web service that makes outbound requests to third parties from the operator's network. Both halves are attack surface. This document states what is being protected, from whom, by what, and what is knowingly left exposed.

Companion documents: [`NETWORKING.md`](NETWORKING.md) (topology and reachability), [`IMAGE.md`](IMAGE.md) (hardening), [`QUALITY.md`](QUALITY.md) (scanning and tests), [`ACCEPTABLE_USE.md`](ACCEPTABLE_USE.md) (what the tool may be pointed at).

---

## 1. Scope

### In scope

| Surface | Why it matters |
|---|---|
| The dashboard and its HTTP handlers | Internet-reachable by default (`0.0.0.0:2600`) |
| The probe engine | Turns configuration into requests from the operator's IP |
| The target list and seed ingestion | Untrusted input that selects those requests |
| The SQLite database and backups | Contains the target list, egress identity, and a timeline |
| The container runtime and image | Runs as a non-root process on the operator's host |
| Dependencies and build pipeline | Published binary consumed by strangers |
| The optional Tor sidecar | A proxy that must never become a relay |
| External feed clients | Outbound fetches to third-party APIs with credentials |

### Out of scope

- A host that is already root-compromised. Container hardening raises the cost of escape; it does not survive a hostile kernel or a hostile container runtime.
- The operator's browser and workstation.
- Physical access.
- Denial of service by a party who can saturate the operator's uplink. The tool degrades; it does not defend the link.
- Legal advice. `ACCEPTABLE_USE.md` describes intent, not a jurisdiction-by-jurisdiction opinion.

### Method

Assets → adversaries → trust boundaries → threats per boundary → mitigation → residual risk → the invariant that makes the mitigation testable. Every mitigation listed as "enforced" has a corresponding test in `QUALITY.md` §5 or a startup assertion; a mitigation that is only a sentence in a document is listed as *policy*, not *enforced*.

**The tables describe the finished system.** The status column is a specification of what the code must enforce, not a report on today's binary. §1.1 is the report.

### 1.1 Implementation status

What exists in code today, and therefore what "enforced" currently means:

| Area | Status | Evidence |
|---|---|---|
| Startup guardrail (non-loopback + no token) | **implemented** | `internal/api/config.go`, `config_test.go` |
| Refuse to run as root | **implemented** | `internal/startup`, `startup_test.go` |
| API bearer auth, constant-time compare | **implemented** | `internal/api/middleware.go`, `api_test.go` |
| Per-token rate limit (60/min, burst 20) | **implemented** | `internal/api/ratelimit.go` |
| Session cookies, CSRF, login rate limit | **implemented** | `internal/web/auth.go`, `web_test.go` |
| Security headers, request IDs, JSON errors, panic recovery | **implemented** | `internal/api` |
| Robots/noindex (`robots.txt`, `X-Robots-Tag`) | **implemented** | `internal/web`, `internal/api` |
| Token file permission check (POSIX) | **implemented** | `internal/api/config.go` |
| Handler timeouts and header limits | **implemented** | `cmd/netweather/main.go` |
| Log redaction, ring, non-blocking sink | **implemented** | `internal/logging`, `redact_test.go`, `async_test.go` |
| Target escaping in HTML | **implemented** | `internal/web/web_test.go` |
| Probe dialer, SSRF rules, port allowlist | **implemented** | `internal/netx`, `dialer_test.go`, `policy_test.go` |
| Store, migrations, retention, backups | **implemented** | `internal/store`, `docs/SCHEMA.md` |
| Container image, compose, capabilities, read-only rootfs | **implemented** | `Dockerfile`, `compose.yaml`, `docs/IMAGE.md`, `docs/NETWORKING.md` |
| Feeds, Tor sidecar, RIPE Atlas profile | **implemented** | `internal/feeds`, `compose.yaml` profiles, `docs/DESIGN.md` §8 |

The SSRF rules in §5.2 are properties of the running binary: every instrument dials through `internal/netx`, which resolves, checks the address and port against the policy, and only then dials the checked IP (name resolution for the Host/SNI is preserved). The table describes code that exists; where a capability is optional (raw ICMP, traceroute), the degradation is explicit rather than silent.

---

## 2. Assets, ranked by what an attacker actually wants

| # | Asset | Why it is valuable | Worst realistic outcome |
|---|---|---|---|
| A1 | **The target list** | A curated map of banks, airlines, clouds, and country anchors, with intervals and vantages. That is reconnaissance someone else did for free | Disclosure of which institutions the operator considers critical, and of coverage gaps |
| A2 | **The egress identity** | Public IP, ASN, geolocation, and (if Tor is on) exit countries. Shown on the dashboard by design | The operator is identified and their measurement is correlated to them |
| A3 | **The probe engine as a primitive** | Anything that makes the process issue requests from the operator's IP is SSRF, and at scale is a scanner | The operator's IP is the source of abusive traffic |
| A4 | **The container as a foothold** | It sits inside the operator's network, often on a NAS or a VPS with other services | Lateral movement into the operator's estate |
| A5 | **The database** | History, incident annotations (free-text operator notes), feed tokens | Disclosure of notes and credentials, or silent history tampering |
| A6 | **The build** | The image is pulled by strangers | Supply-chain compromise at scale |
| A7 | **The Tor sidecar** | A misconfigured `tor` is an exit relay, and an exit relay is a legal event | Traffic attributed to the operator; abuse complaints; ISP termination |

The ranking drives the design: A1–A3 are why authentication is mandatory and why the SSRF rules are enforced in the dialer rather than in each probe.

---

## 3. Adversaries

| ID | Adversary | Capability | Motivation |
|---|---|---|---|
| X1 | Opportunistic scanner | Port scans, path brute force, known-CVE probes | Whatever is open |
| X2 | Curious or hostile visitor | Reads the dashboard if it is reachable | Learn A1/A2 |
| X3 | Credential attacker | Password/token guessing, session theft, CSRF | A1, A4 |
| X4 | Malicious target or MITM on the path | Returns hostile responses, redirects, malformed TLS, huge bodies, DNS answers pointing at private ranges | A3, A4 |
| X5 | Poisoned target list | A seed URL, a contributed `targets.yaml`, or a compromised upstream list adds hosts under the attacker's control | A3 |
| X6 | Compromised dependency | Typosquat, hijacked module, malicious transitive dep | A4, A6 |
| X7 | Local network attacker | Can reach `2600` on the LAN, can spoof DNS | A1, A2, A3 |
| X8 | Abused third party | A target that objects to being probed | Availability, legal |
| X9 | Operator error | A typo in config, an over-broad CIDR, a public bind with auth off | All of the above |

X9 is listed as an adversary on purpose. Most real incidents in a tool like this are self-inflicted configuration, not attacks.

---

## 4. Trust boundaries

```
                       untrusted internet
                              |
             +----------------+----------------+
             |                                 |
      [ B1 dashboard ]                  [ B2 probe egress ]
             |                                 |
   ===== trust boundary =====        ===== trust boundary =====
             |                                 |
      +------+------+                   +------+------+
      |  handlers   |                   |   dialer    |
      +------+------+                   +------+------+
             |                                 |
             +-----------+   +-----------------+
                         |   |
                   [ B3 store ]  [ B4 config/seed ]
                         |              |
                         +------+-------+
                                |
                        ===== trust boundary =====
                                |
                        [ B5 container boundary ]
                                |
                        [ B6 host / kernel ]
```

| Boundary | Crossing | Rule |
|---|---|---|
| B1 | Browser → handlers | Authenticated, CSRF-protected, rate-limited, no state change on GET |
| B2 | Dialer → internet | Only public addresses, only allowlisted ports, only GET/HEAD, always revalidated |
| B3 | Handlers/probes → SQLite | One writer, readers `query_only`, no SQL string concatenation |
| B4 | Config/seed → scheduler | Untrusted; validated, capped, provenance-recorded |
| B5 | Process → host | Non-root, read-only rootfs, no capabilities, no docker socket |
| B6 | Container → host kernel | Out of scope; documented residual |

The critical property is that **B2 is the only place a connection is created**. No handler, feed client, or plugin opens a socket directly. That is what makes the SSRF rules auditable: one function, one test suite, one review surface.

---

## 5. Threats and mitigations

### 5.1 Exposure and authentication (B1)

| ID | Threat | Mitigation | Status |
|---|---|---|---|
| T1 | Dashboard reachable with no auth | Non-loopback listen + `ui.auth: none` **refuses to start** unless `ui.allow_public: true` is also set | Enforced |
| T2 | Token compared with `==` | `subtle.ConstantTimeCompare` on fixed-length hashes | Enforced |
| T3 | Token in URL, logs, or referrer | Accepted only via `Authorization` header or login form POST; never a query parameter; redacted in logs | Enforced |
| T4 | Session theft via XSS | `HttpOnly`, `SameSite=Lax`, `Secure` when the request arrived over TLS; no token in `localStorage`; a strict CSP (`default-src 'self'`, no inline script or style, no framing) set by the shared web middleware on every browser response, so a static asset or method refusal cannot miss it; no third-party assets | Enforced |
| T5 | CSRF on state-changing routes | All mutations are `POST` with a per-session token; `SameSite=Lax` alone is not treated as sufficient | Enforced |
| T6 | Password/token brute force | Per-client and process-wide token buckets on `/login` (5/minute per client, 30/minute overall), then `429` with `Retry-After`; no username enumeration (single account, identical error and timing). The per-client key is the peer unless the peer is a configured trusted proxy, in which case the rightmost untrusted `X-Forwarded-For` entry is used, so a spoofed header cannot escape the bucket. There is no exponential delay in the handler, because a sleeping handler is its own denial of service; the buckets are the control. | Enforced |
| T7 | Session fixation | A fresh signed session is issued on login; the idle window is 12h and slides on use at most once per 5 minutes, while the absolute cap is 7d from login and is carried in the signed payload, so a refresh cannot extend it | Enforced |
| T8 | No transport encryption | The binary does **not** terminate TLS. Startup logs `event=plaintext_transport` once, and every page (including login) draws an "insecure transport" badge when the browser's own connection is plaintext. A TLS-terminating proxy suppresses it with `X-Forwarded-Proto: https`, believed only from `NETWEATHER_TRUSTED_PROXIES`, and the same signal marks the session and CSRF cookies `Secure` so a browser on HTTPS never sends them over plaintext. `NETWEATHER_UI_SECURE_COOKIES=true` forces `Secure` where the proxy cannot be detected. TLS itself is the reverse proxy's job. | Enforced (display) |
| T9 | Open redirect in `?next=` | Only same-origin relative paths accepted | Enforced |
| T10 | Handler-level DoS | `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout`, `MaxHeaderBytes`, an 8 KiB action-body cap (`http.MaxBytesReader` on the login form and the JSON action bodies), SSE excluded from `WriteTimeout`, and an SSE connection cap | Enforced |
| T11 | Clickjacking | `X-Frame-Options: DENY` + `frame-ancestors 'none'` | Enforced |
| T12 | MIME sniffing | `X-Content-Type-Options: nosniff`, explicit `Content-Type` on every response | Enforced |
| T13 | Token file readable by others | On POSIX, refuse to start if the token file is group- or world-readable (`chmod 600`); the release image runs one UID and the compose file mounts the token read-only. Windows has no POSIX mode to test, so the file's ACL is the operator's responsibility and the binary does not pretend to check it. | Enforced (POSIX) |
| T14 | Secrets in the image or env | Token from a file or secret mount is preferred; an inline env token is accepted and logged once as `event=auth_token_env`, because anything that can read the process environment can read it; `.dockerignore` and full-history `gitleaks` in CI | Enforced |
| T15 | Error pages leaking paths/SQL | Every server failure on the browser surface renders one generic page with the request id and a pointer to the log, and the id is also on `X-Request-ID`; the API returns the `{error:{code,message,request_id}}` envelope. Internal detail, paths, and SQL stay in the log line, which carries the same id. | Enforced |

Residual: a public bind with `ui.allow_public: true` and a weak token is the operator's choice. The startup banner states it once, loudly, and the badge never goes away.

### 5.2 Probe egress — SSRF and scanning (B2)

This is the highest-severity area, because a mistake here turns the operator's IP into the attacker's.

| ID | Threat | Mitigation | Status |
|---|---|---|---|
| T16 | Target resolves to a private address | Resolve first, reject RFC1918, loopback, link-local, CGNAT (`100.64/10`), multicast, unspecified, IPv6 ULA, `::1`, `169.254.169.254`, and IPv4-mapped v6 forms | Enforced; a refused destination is `UNMEASURABLE` with `policy_blocked`, never `DOWN` |
| T17 | DNS rebinding between check and connect | Dial the **validated IP literal**, not the hostname; keep SNI and `Host` header as the original name; revalidate on every new connection, including pool reuse after TTL | Enforced |
| T18 | Redirect to a private address or a new port | Every hop revalidated: scheme, host, resolved IPs, port allowlist; max 5 hops; cross-host redirects re-resolve | Enforced |
| T19 | Redirect to a different port | Port allowlist applies after redirect, not just to the configured target | Enforced |
| T20 | Arbitrary port probing | Outbound allowlist: 80, 443, 53, 123, 853. `22` and everything else rejected at config load **and** at dial time | Enforced |
| T21 | Hostile response body | 32 KB read cap, decompression cap, no JS execution, no HTML rendering, signature matching only, no `Content-Length` trust | Enforced |
| T22 | Malicious TLS | Verification always on. No `InsecureSkipVerify`, no custom `VerifyPeerCertificate` that returns nil. Cert parse limits via `crypto/x509` | Enforced |
| T23 | Slowloris from the target side | Per-stage deadlines (DNS, TCP, TLS, TTFB, total) and a body-read deadline; no unbounded `io.ReadAll` | Enforced |
| T24 | Connection exhaustion | Per-host semaphore 1, one probe at a time in the step, `MaxConnsPerHost`, idle pool capped, global token bucket | Enforced |
| T25 | Using the tool as a scanner | One port per target, no port sweep, max 2000 targets, 30s floor per host, GET/HEAD only, no payloads | Enforced |
| T26 | Using the tool as a load generator | Global 20 req/s default, per-host 1 concurrent, 30s floor, kill switch (`netweather pause`) | Enforced |
| T27 | Amplification via DNS/NTP | DNS: normal recursive queries to configured resolvers only. NTP: fixed pool allowlist, single client request, no `monlist`, no broadcast | Enforced |
| T28 | ICMP as a covert channel or scan | Echo request only, unprivileged datagram ICMP where possible, never to private ranges, never the sole up/down signal | Enforced |
| T29 | Traceroute abuse | On-demand only, ≤10/hour globally, no private ranges, never scheduled. The API and the dashboard spend from one governor, so the ceiling is per instance; both resolve through the guarded dialer, so a private destination is refused; no schedule can reach it. | Enforced |
| T30 | Proxy chaining hides the source of abuse | Never chain proxies; a target has exactly one vantage; SOCKS5 is `socks5h` only, so DNS happens at the proxy | Enforced |
| T31 | Environment proxy vars reroute traffic | `http.Transport.Proxy = nil` explicitly; `HTTP_PROXY` is not honored | Enforced |
| T32 | Feed URL as an SSRF vector | Feed endpoints are compiled-in constants; a user-supplied feed URL is opt-in, must be HTTPS, and goes through the same dialer. A plaintext URL is dropped (or an override ignored) with `event=feed_url_insecure`, and every redirect is re-checked so a chain cannot downgrade to HTTP. | Enforced |

Residual: a public host that later *becomes* a private address after the operator's DNS is compromised is caught at dial time, not before. A target that is genuinely public but hostile can still return 32 KB of garbage — that is bounded, not prevented.

### 5.3 Untrusted configuration and seeds (B4)

| ID | Threat | Mitigation | Status |
|---|---|---|---|
| T33 | Poisoned seed adds attacker-controlled hosts | Seed payloads are untrusted: hostname syntax and IDN/punycode normalization, no IP literals for service targets, no wildcards, no userinfo, no non-allowlisted ports, dedupe, hard cap 2000 | Enforced |
| T34 | Poisoned seed adds thousands of hosts | Cap enforced before scheduling; a seed that exceeds it is rejected wholesale, not truncated silently | Enforced |
| T35 | No provenance for a bad target | `target_source` records the source string and SHA-256 of the payload, written by every seed and config load; the target drawer and `GET /api/v1/targets/{id}` show the source and hash, so a poisoned list can be traced rather than merely recorded; `netweather seed --diff` shows additions before apply | Enforced |
| T36 | Config typo takes the monitor down | Invalid reload is rejected; previous config stays live; error surfaced in the UI | Enforced |
| T37 | Config points at the operator's own services | Documented: the private-range block will refuse them; local monitoring is explicitly not a use case | Policy |
| T38 | YAML bomb / alias expansion | Size cap on config files, alias expansion limit, no custom tags | Enforced |
| T39 | Path traversal in config paths | Config root is fixed; names are validated; no `..` | Enforced |
| T40 | Seed fetched over HTTP | Seed URLs must be HTTPS (or `file://` for local); redirects revalidated | Enforced |

### 5.4 Storage (B3)

| ID | Threat | Mitigation | Status |
|---|---|---|---|
| T41 | SQL injection | Only `?` placeholders; no string-built SQL; `query_only` readers make an accidental write a hard error | Enforced |
| T42 | Handler writes to the DB | Readers are `query_only=1`; write access exists only in the store package | Enforced |
| T43 | Backup copied while WAL is live → corrupt file | Only `VACUUM INTO`; documentation forbids copying the live file | Enforced |
| T44 | Restored DB with stale `-wal` | Restore procedure deletes `-wal`/`-shm` siblings (`SCHEMA.md` §14). Startup logs `event=db_wal_present` with the WAL size and repeats the restore rule; SQLite recovers a WAL left by a crash, and the binary cannot tell a foreign WAL from a crashed one, so it does not claim to detect it. | Enforced (procedure) |
| T45 | Database readable by other users on the host | File created `0600`, directory `0700`; container runs as one UID; volume ownership documented | Enforced |
| T46 | Operator annotations leak sensitive context | Free text, stored as written, never sent anywhere; the export command is explicit | Policy |
| T47 | Downgrade silently drops columns | Refuse to start if `schema_version` is newer than the binary | Enforced |
| T48 | Disk fill from a runaway writer | Retention jobs, per-table caps, and the `self_check` disk warning at 85% used (15% free) with the measured share in the value. There is no probe hard stop: stopping the writer does not free the volume, retention does, and a store write failure is already logged. | Enforced (warning) |

Residual: encryption at rest is the host's responsibility. The document says so instead of pretending a SQLite file is encrypted.

### 5.5 Container and host (B5)

| ID | Threat | Mitigation | Status |
|---|---|---|---|
| T49 | Container escape via a kernel bug | Non-root UID 65532, `read_only: true`, `cap_drop: ALL`, `no-new-privileges`, default seccomp, no shell or package manager in the release image | Enforced |
| T50 | Docker socket abuse | Never mounted. Not a config option | Enforced |
| T51 | Host networking widens SSRF to the host | Bridge is the default; host networking is opt-in and the docs say exactly what it exposes | Enforced (default) |
| T52 | Reaching the Docker API or metadata service | `172.17.0.1`, `host.docker.internal`, and `169.254.169.254` are in the private-range block | Enforced |
| T53 | Privileged mode for ICMP | Not used. The ping instrument prefers an unprivileged ping socket (`net.ipv4.ping_group_range`, set by the shipped compose file) and falls back to a raw socket only when the host grants `CAP_NET_RAW`; on-demand traceroute still needs the capability and is an opt-in profile | Enforced (default) |
| T54 | Writable rootfs persistence | `read_only`, `/tmp` tmpfs, `/data` the only volume | Enforced |
| T55 | Host path mount exposes secrets | Compose mounts exactly three paths; the image runs as one UID and cannot read what that UID cannot read | Enforced |
| T56 | Tor sidecar becomes a relay | The sidecar image is built here (`Dockerfile.tor`) with `ClientOnly 1`, `ExitPolicy reject *:*`, `BridgeRelay 0`, and `PublishServerDescriptor 0` baked into `torrc`, and a unit test fails if any is missing. The app checks that the SOCKS endpoint answers, not the peer's config, because it cannot read it | Enforced by the image; reachability checked |
| T57 | Tor control port exposed | Control port disabled or cookie-authenticated on a private plane only; `SocksPort` never published to the host | Enforced |

### 5.6 Supply chain (B5 → build)

| ID | Threat | Mitigation | Status |
|---|---|---|---|
| T58 | Malicious or hijacked dependency | `go.sum` + checksum DB, a deliberately small direct-dependency set, `govulncheck` over the module graph in CI, `grype` on the release image, and a review rule for new direct dependencies | Enforced (process) |
| T59 | Typosquatted module | Direct dependencies are reviewed in `go.mod` and kept small; a new module is a review item rather than an automatic accept. There is no automated domain allowlist: the lint toolchain itself pulls a large transitive graph, and a rule broad enough to pass it would not catch a typosquat in a domain it already allows. | Policy |
| T60 | Tampered image | Images referenced by digest in docs and compose examples; cosign signature + SBOM published with releases | Enforced (release) |
| T61 | Compromised base image | Chainguard static/Wolfi for the main image and Alpine for the Tor sidecar (Wolfi does not package `tor`); every `FROM` is digest-pinned, Dependabot updates the digest weekly, and `grype` gates the release image on HIGH/CRITICAL. `TestDockerfilesPinBaseImages` fails if an unpinned `FROM` returns. | Enforced (CI) |
| T62 | Secrets in the build | `gitleaks` over full history in CI on every push and PR; no build args carry secrets; `.dockerignore` keeps the data directory and token files out of the build context | Enforced |
| T63 | Dockerfile mistakes | `hadolint`, no `ADD` from URLs, no `latest`, explicit non-root `USER`, no secrets in layers | Enforced |
| T64 | Malicious contribution | Review rules: PRs adding evasion, scraping, or new outbound ports are rejected on sight (`ACCEPTABLE_USE.md` §9). The mechanical cases are covered by the linters that run: `gosec` for TLS and unsafe calls, `errorlint` for wrapped errors, and the config tests for new environment variables. There is no custom semgrep rule set. | Policy |

### 5.7 Privacy and disclosure of the operator

| ID | Threat | Mitigation | Status |
|---|---|---|---|
| T65 | Dashboard reveals egress ASN/IP | Deliberate: the tool's honesty depends on showing its vantage. `NETWEATHER_REDACT_EGRESS=true` hides the literal address on `/api/v1/selfcheck` while keeping the ASN and country; the stored tuple stays exact, because `VANTAGE_CHANGED` must compare it. Display-only: logs keep the address for debugging. | Enforced (opt-in display) |
| T66 | Target list reveals the operator's interests | Auth-gated; not in any public endpoint; no telemetry | Enforced |
| T67 | Telemetry or analytics | None. Every outbound request is a probe, a feed poll, a self-check, or opt-in enrichment (ASN/prefix/CDN and PeeringDB); nothing is sent to a telemetry endpoint, and there is no analytics code in the binary | Enforced |
| T68 | Logs contain tokens or full URLs with credentials | Structured logs redact `Authorization`, cookies, query strings on `/login`, and feed tokens | Enforced |
| T69 | Public instance indexed by search engines | `robots.txt` disallows everything and `X-Robots-Tag: noindex` is set | Enforced |
| T70 | RIPE Atlas profile is a third-party daemon on host networking | Ships only as an opt-in profile; netweather never connects to it; `docs/RIPE_ATLAS.md` states what RIPE sees (operator IP, uptime); its traffic is RIPE's, not netweather's, and is governed by RIPE's terms | Policy |

---

## 6. Invariants (each one is a test)

These are the properties the security model reduces to. They are listed as testable statements because a threat model that cannot fail a build is a mood board.

1. No direct `net.Dial*` call exists outside `internal/netx` — `TestOutboundDialsGoThroughNetx` scans production files and fails on one. Callers that need a connection take a dial function, and production passes the guarded dialer. The linter does not enforce this rule; the test does.
2. `Dialer` rejects every address in the private-range table, including IPv4-mapped IPv6 — table-driven test with ~40 cases.
3. `Dialer` connects to the validated IP (`TestDialDialsTheValidatedIPNotTheHostname`), and the HTTP instrument keeps SNI equal to the configured name while the dial goes elsewhere (`TestTransportPreservesSNIWhenTheDialGoesElsewhere`).
4. A redirect to a private address, a blocked port, or a non-HTTP scheme fails closed — `TestRedirectToABlockedDestinationFailsClosed` drives each hop through a dial function that refuses it, and the client itself refuses the scheme.
5. Outbound port allowlist is applied at config load and at dial time, and `22` fails both.
6. `http.Transport.Proxy` is nil even with `HTTP_PROXY` set in the environment.
7. The binary refuses to start when: non-loopback + `auth: none` + `allow_public` unset; token file is world-readable; `schema_version` is newer than the binary (`TestOpenRefusesANewerSchema`); it is running as UID 0.
8. Every response carries CSP, `nosniff`, `X-Frame-Options`, and `Referrer-Policy`.
9. A state-changing request without a CSRF token returns 403.
10. Login compares the single token in constant time, so a response time does not reveal how much of it matched. There is no username, hence no account to enumerate; the per-client and process-wide buckets are the brute-force control (T6), not an artificial sleep in the handler.
11. The shipped `torrc` contains the four client-only directives, and a unit test fails if any is missing; `netweather doctor` proves the SOCKS endpoint answers.
12. No compose example mounts `/var/run/docker.sock` or publishes anything except `2600` — `TestComposePublishesOnlyTheDashboard` parses every compose file and fails on either.
13. The release image has no shell: `docker run --rm image sh` fails.
14. `probe.detail` is truncated at 200 bytes and control characters are stripped.
15. A 10 MB decompression bomb is cut at 32 KB decompressed.

---

## 7. Residual risk register

| Risk | Likelihood | Impact | Accepted? | Note |
|---|---|---|---|---|
| Operator publishes a weak token on a public bind | Medium | High | Operator's decision | Startup refuses only the *unauthenticated* case; it cannot refuse a bad password |
| Public target turns hostile and returns 32 KB of junk | High | Low | Yes | Bounded, never rendered |
| Operator's DNS is compromised and returns a public IP for an internal name | Low | Medium | Yes | Private block catches private answers; a public answer for an internal name is indistinguishable from a real target |
| Kernel-level container escape | Low | High | Yes | Out of scope; hardening raises cost |
| Abuse complaint from a probed third party | Medium | Medium | Mitigated | Contact UA, `pause`, target removal, `ACCEPTABLE_USE.md` §7 |
| Legal exposure in an unfriendly jurisdiction | Low | High | Documented | Not legal advice; operator decides |
| Tor exit used by the operator's vantage is itself blocked | High | Low | Yes | `BLOCKED` is a normal state; the Tor vantage is never `primary` |
| Free-text annotation contains something the operator later regrets | Low | Medium | Yes | Never transmitted; export is explicit |
| Dependency compromise between scans | Low | High | Partly | Pinning, SBOM, cosign, weekly rebuild |
| RIPE Atlas probe compromised → host-network access | Low | High | Operator opt-in | Runs host-networked per RIPE's requirements; isolated from netweather, not from the host |

---

## 8. Operator runbook

**Rotate the token.** Replace the file contents, restart or `SIGHUP`; all sessions are invalidated because session IDs are derived from a token hash.

**Suspect compromise.** `netweather pause` stops all probing immediately. Then: stop the container, keep the volume for forensics, do not reuse the token, check `self_check` and the egress IP history, and assume the target list is disclosed.

**Receive an abuse complaint.** Stop probing the host (`netweather disable <target>`), reply with the contact in `ACCEPTABLE_USE.md` §7, and record the interval and method used. The logs can produce the exact request pattern.

**Publish an instance.** Read `NETWORKING.md` §7.1 first. The page is the disclosure: target list, egress ASN, coverage gaps.

**Vulnerability reports.** `SECURITY.md` is the intake path: GitHub private vulnerability reporting, with the in-scope list naming the SSRF guard, auth, and container hardening as urgent. Do not open a public issue for a suspected vulnerability.

---

## 9. Open questions

1. **Single account vs. OIDC.** A single token is right for self-hosting; OIDC is right for teams. Ship the token, leave the interface open (`ui.auth: oidc` reserved, not implemented).
2. **`allow_public: true` should probably require an explicit acknowledgement string**, not a boolean, so it cannot be enabled by a stray line in a copied compose file.
3. **Read-only mode for public demos** — a sanitized board with no target names would let people evaluate the tool without publishing their list. Not v1.
4. **Per-target `robots.txt` handling.** The tool is not a crawler, but a courtesy check on the first probe of a new target may be worth the extra request. Leaning no: it doubles first-contact traffic and robots.txt is not a security control.
5. **Memory zeroing for the token.** Go cannot guarantee it. Constant-time compare and process isolation are the practical mitigations; say so rather than pretending.
