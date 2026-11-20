# netweather — Docker networking

> A network measurement tool that runs on a NAT'd bridge with a 1500-byte MTU and Docker's embedded DNS has three silent failure modes that all look like outages. This document exists so they don't.

---

## 1. Decision

**Default to a user-defined bridge network. Treat host networking as an opt-in measurement profile, not the default.**

| Mode | Verdict | Why |
|---|---|---|
| User-defined bridge | ✅ **default** | Isolation from host loopback and host services; per-service DNS; no published ports needed internally |
| `network_mode: host` | ⚠️ opt-in | Real IPv6 and honest traceroutes; but the app can now reach the host's own `127.0.0.1` services, and `ports:` is ignored |
| `network_mode: service:tailscale` | ⚠️ conditional | Convenient for tailnet-only access, but **an exit node silently changes your vantage** (§7.2) |
| Overlay (Swarm) | ❌ | 1450 MTU, cross-node egress, and you are measuring the overlay as much as the internet |
| `macvlan` / `ipvlan` | ⚠️ advanced | A real LAN IP with true L2 — best ICMP/trace fidelity — but many Wi-Fi APs drop the second MAC, and the container cannot talk to its own host |

The isolation argument is the one that matters most. Inside a bridge network, a malicious or misconfigured target that resolves to `127.0.0.1` reaches *the container*, which has nothing on it. Under host networking, the same target reaches the host's SSH agent, its local databases, and its admin panels. That converts a config mistake into a real SSRF. `THREAT_MODEL.md` depends on this default.

---

## 2. Three planes

Keep egress, UI, and the Tor sidecar on separate networks. They have different trust levels.

```
                    ┌─────────────────────────────────────┐
   host :2600 ──────┤ ui       (published, 0.0.0.0)       │
                    │   └── netweather:2600                │
                    ├─────────────────────────────────────┤
                    │ egress   (no published ports)       │
                    │   ├── netweather                     │
                    │   └── tor:9050                      │
                    └─────────────────────────────────────┘
```

| Plane | Network | Published | Purpose |
|---|---|---|---|
| UI | `ui` | `2600` | Inbound only. Nothing else attaches here. Auth required (§7.1). |
| Egress | `egress` | none | Probe traffic, Tor SOCKS, feed fetches. |
| Proxy (optional) | `ui` + `egress` | `443` | TLS termination; needs egress for ACME |

The Tor container is on `egress` **with no published ports**, so the host cannot reach `9050` and neither can anything on `ui`. That is the whole access control for the sidecar.

```yaml
networks:
  ui:
  egress:
    driver_opts:
      com.docker.network.driver.mtu: 1400   # see §4
```

---

## 3. What the network does to each instrument

The honest table. Every row is a way the tool can lie.

| Instrument | Bridge behaviour | Risk | Mitigation |
|---|---|---|---|
| `dns` | Container → `127.0.0.11` (Docker DNS) → host resolvers | Measures the host's resolver, not the target's | Query ccTLD authoritative servers **directly**, bypassing embedded DNS (§5) |
| `doh` | Plain HTTPS egress | none | — |
| `tcp` / `tls` / `http` | NAT'd, fine | MTU black-holing on large handshakes (§4) | MSS/MTU clamp; PMTU self-check |
| `icmp` | Needs `NET_RAW` or `net.ipv4.ping_group_range` | The instrument is unavailable, not the target | Report the socket error; `doctor` names it as an optional capability. There is no silent `tcpping` substitution, because that would change what the anchor measures. |
| `tcpping` | NAT'd TCP SYN; `RST` from a closed port still proves reachability | none | — |
| `ntp` | UDP 123 out through NAT | Some networks block outbound 123 | Report `EGRESS_BLOCKED_123`, never `DOWN` |
| `trace` | First hop is the bridge gateway; ICMP time-exceeded is conntrack-translated | Truncated traces look like a broken path | Label hop 0; treat "stopped after hop 1" as a container artifact (§8) |
| `corridor` | Two in-region endpoints, both egress from the same box | Corridor is a path *from you*, not a cable measurement | Already stated in `DESIGN.md` §9 |
| Tor `socks5` | Container → `tor:9050` over TCP | A `127.0.0.1:9050` torrc is unreachable across containers | Bind `0.0.0.0:9050` inside the sidecar (§7) |

---

## 4. MTU — the highest-value false positive

This is the one that will waste a weekend if it isn't handled on day one.

Docker's bridge MTU defaults to 1500. If the host's uplink is lower — PPPoE at 1492, WireGuard at 1420, some VPNs at 1280 — the container will happily emit 1500-byte packets that are silently dropped, **and PMTUD often fails through NAT when ICMP is filtered**. The symptom is ugly and misleading:

- Small `GET /` succeeds → `UP`
- TLS handshake with a large certificate chain stalls → `DOWN`
- The same target is `UP` on one probe and `DOWN` on the next

A prober that reports outages caused by its own MTU is worse than useless.

**Fix, in order:**

1. Set the compose network MTU to match the real path (`1400` is a safe default for anything VPN'd). No capability required.
2. The `mtu` self-check reports the smallest MTU among up, non-loopback interfaces at startup and with maintenance, and warns below 1400. While it is failing and fresh (twenty minutes, twice the maintenance interval), `http_timeout`, `tcp_timeout`, and `tls_handshake` are classified `UNKNOWN` with the reason "egress MTU is below the floor" instead of faking `DOWN`; a `tcp_refused` stays `DOWN`, because the SYN arrived.
3. Never add `NET_ADMIN` to run `iptables --clamp-mss-to-pmtu`. Fix the network, don't patch the container.

Deliberately not implemented on non-Linux: a DF-marked path probe (1400 bytes, then 1280). Linux builds ship `internal/probe/pmtu_linux.go` using `IP_MTU_DISCOVER`; other platforms fall back to the interface-MTU self-check, which covers the documented failure mode. Raw sockets or platform-specific path-MTU discovery are not granted to the shipped image. `DESIGN.md` §7.5 states the same limitation in the self-check row.

---

## 5. DNS — three separate problems

### 5.1 Embedded DNS is a proxy, not a resolver

Docker writes `nameserver 127.0.0.11` and `options ndots:0` into the container's `/etc/resolv.conf`. That address is the daemon's DNS proxy: it answers container names from its own registry and **forwards everything else** to the host's resolvers.

Consequences:

- The `dns` instrument, as normally implemented, measures *the host's resolver path*, which is exactly the bias that made `global_packet_throwing` wrong.
- The resolver-identity self-health check (`DESIGN.md` §7.5) reads `127.0.0.11` and learns nothing. It must read the **effective upstream set**, which the app discovers by querying a control name or reading the daemon's configured DNS. Record the set, not the address.
- `ndots:0` means a bare `chase` is treated as an FQDN. Good for us; note it so config validation doesn't add search-domain magic.

### 5.2 The systemd-resolved trap

If the host's `/etc/resolv.conf` contains only `127.0.0.53` (systemd-resolved) or `127.0.0.1`, **the Docker daemon discards it as a loopback address and falls back to `8.8.8.8` / `8.8.4.4`.** The container then resolves through Google, not through the operator's resolver — with different geo-answers and different filtering. Nobody notices until a target is "down" for one user and up for another.

Mitigation: `netweather doctor` compares the effective upstream against the configured `dns.resolvers`. Mismatch → `RESOLVER_UNEXPECTED` warning in the UI, not an error. Users may legitimately want the fallback; they should just know.

### 5.3 Country anchors bypass all of it

The country-anchor probe (`DESIGN.md` §4.2) sends a `<tld>. SOA` query **directly to that ccTLD's authoritative nameserver**, on port 53, with the embedded DNS out of the path entirely. That is the point — it is the only measurement here that leaves your resolver behind.

Which makes port 53 egress load-bearing. Many corporate networks and some consumer ISPs block outbound 53 to arbitrary destinations. If they do:

- Every country anchor fails identically
- That is **not** 250 outages, it is one blocked port

So: at startup and hourly, probe port 53 to a known authoritative server. On failure, country anchors become `UNMEASURABLE` with reason `EGRESS_BLOCKED_53`, and the board shows a banner. Never `DOWN`.

---

## 6. What is reachable from inside the container

The SSRF guard in `DESIGN.md` §14 blocks private ranges after DNS and after every redirect. In Docker, the "private" list is longer than the RFC1918 list:

| Address | What it is | Block |
|---|---|---|
| `127.0.0.0/8` | The container's own loopback | ✅ |
| `172.16.0.0/12` | **`172.17.0.1` is the host**, and user-defined bridges live here too | ✅ |
| `10.0.0.0/8`, `192.168.0.0/16` | LAN, other compose projects, host services | ✅ |
| `169.254.169.254` | Cloud instance metadata — credentials | ✅ hard block, always |
| `169.254.0.0/16` | Link-local | ✅ |
| `100.64.0.0/10` | CGNAT, Tailscale, some VPNs | ✅ |
| `fd00::/8`, `fe80::/10` | ULA and link-local IPv6 | ✅ |
| `host.docker.internal` | Only exists if `extra_hosts` adds it — but then it maps to the host | ✅ resolve-and-block |
| `0.0.0.0`, `::` | Ambiguous | ✅ |
| Multicast, broadcast | — | ✅ |

Two rules that follow:

- **Never mount `/var/run/docker.sock`.** An SSRF plus a socket mount is root on the host. Not a hardening nicety — an absolute.
- **Resolve, check, then dial the checked IP.** Dialing by hostname lets a rebinding attacker pass the check and then answer differently. Direct vantages therefore dial by IP with `Host`/SNI preserved.

The Tor vantage cannot follow that rule — it must dial by hostname or DNS leaks to your resolver (`DESIGN.md` §5.7). So a Tor vantage *can* be pointed at a private address by a malicious config. Accepted and documented: the target list is operator-supplied and read-only, and Tor vantages only ever probe the ≤25-target corroboration set.

---

## 7. The UI plane

### 7.1 Port 2600, and the bind posture

The UI listens on **2600/tcp**.

| Candidate | Why it looks right | Why it is not |
|---|---|---|
| **2600** ✅ | 2600 Hz — phreaking, *2600: The Hacker Quarterly*, the same lineage as `censor_sensor` and `proxy_scan`. Unassigned by IANA, memorable, above 1024 | — |
| 179 | BGP — the literal protocol of internet outages, hijacks, and route leaks | Privileged; collides with any real BGP daemon on the host; browsers refuse it as unsafe. Use it as a *reference target* instead |
| 6667 | IRC — the classic botnet C2 port, maximum cyberwar aesthetic | **Chrome and Firefox refuse to load it** (`ERR_UNSAFE_PORT`; the IRC range 6665–6669 is blocklisted). The dashboard would be unreachable in a browser |
| 31337 | "elite", Back Orifice | Reads as malware to every reviewer and corporate proxy. This tool already has a dual-use problem; do not add to it |
| 1337 | leetspeak | Same problem, more overused |
| 1984 | Orwell, surveillance state | Politically loaded, and `1984/tcp` is already used by niche tooling |
| 451 | Censorship — matches the `censor_sensor` theme | Collides with HTTP 451, which the classifier already treats as a meaningful status |
| 8080 | Familiar, never blocked | No meaning, and the most-scanned port on the internet |

The technical constraint that decides it: **the app runs as UID 65532**, so it cannot bind below 1024 without `NET_BIND_SERVICE`, and it should not have that capability. That rules out 179, 53, 443, and 80 immediately. 2600 is unprivileged, unassigned, and not on any browser blocklist.

#### Binding: `0.0.0.0` is fine, anonymous `0.0.0.0` is not

`0.0.0.0` is the default. It is not the risk.

| Posture | App binds | Published | Auth |
|---|---|---|---|
| Loopback | `127.0.0.1:2600` | `127.0.0.1:2600:2600` | optional |
| **LAN** | `0.0.0.0:2600` | `2600:2600` | **mandatory** |
| Behind proxy | `0.0.0.0:2600` on `ui` | nothing — proxy publishes 443 | mandatory, or terminated by the proxy |
| Tailnet | `0.0.0.0:2600` on `ui` | nothing — sidecar serves it on the tailnet | tailnet identity (§7.2) |

The guardrail moves from the address to the credential:

- Non-loopback listen + `ui.auth: none` → **refuse to start**, unless `ui.allow_public: true` is also set. Two deliberate flags, so it cannot happen by accident.
- Auth is a token file or a single operator account, compared in constant time, with login rate-limited per IP. No token in the query string — it ends up in logs and `Referer` headers.
- Cookies are `HttpOnly` + `SameSite=Lax`, and `Secure` whenever the effective scheme is HTTPS (proxy TLS or `X-Forwarded-Proto: https`). Plaintext deployments get the cookie *without* `Secure` - a browser will not send a `Secure` cookie over HTTP, so setting it unconditionally would simply break login - plus a persistent "insecure transport" badge on every page; a TLS-terminating proxy suppresses it with `X-Forwarded-Proto: https`, believed only from `NETWEATHER_TRUSTED_PROXIES` (§7.3), and startup logs `event=plaintext_transport` once. Every state-changing route requires the auth cookie; there is no "read-only so it's safe" mode, because the dashboard exposes mutations (pause/resume, incident annotation, sign-out, on-demand traceroute), and the API shares the same token for trace and target disable. A `?next=` destination is accepted only as a same-origin path: an absolute URL, a protocol-relative `//host`, a backslash, or a control character falls back to the board (`THREAT_MODEL.md` T9).
- **There is no TLS in the binary.** A bearer token over plaintext HTTP is only as private as the network it crosses. On a LAN you trust, that is acceptable. On anything else, terminate TLS at a proxy (§7.3) or put the whole thing on the tailnet (§7.2).
- `X-Forwarded-For` is trusted only when the immediate peer is inside `NETWEATHER_TRUSTED_PROXIES` (§7.3). With no configured proxies the header is ignored and the peer address is used.

Why the credential matters more here than on a normal dashboard: **the page itself is the disclosure.** A public netweather publishes your target list, your egress IP and ASN, your resolver set, and your incident history. That is a map of what you can see and what you cannot — precisely the reconnaissance worth having before an operation. The HTML is not the secret; the coverage panel is.

Anonymous mode exists for a lab and for screenshots. It prints a banner on every page saying the instance is unauthenticated and reachable from the network.

### 7.2 Tailscale — the right answer to "private or public?"

Decided: **profile only**. The sidecar ships behind the `tailscale` profile (`docker compose --profile tailscale up -d`), never in the default stack. For anything beyond a trusted LAN, the answer is: **neither public nor LAN. Put it on the tailnet.**

Preferred: run Tailscale as a sidecar and let it proxy to the app, without sharing a network namespace:

```yaml
  tailscale:
    image: ghcr.io/tailscale/tailscale:latest
    profiles: ["tailscale"]
    environment:
      TS_AUTHKEY_FILE: /run/secrets/ts_authkey
      TS_STATE_DIR: /var/lib/tailscale
      TS_USERSPACE: "true"            # no /dev/net/tun, no host device mount
    volumes:
      - ./data/tailscale:/var/lib/tailscale
      - ./secrets/ts_authkey:/run/secrets/ts_authkey:ro
    cap_drop: [ALL]
    cap_add: [NET_ADMIN]
    networks: [ui]
    # no ports: `tailscale serve --bg http://netweather:2600` exposes it on the tailnet only

  netweather:
    networks: [ui, egress]
    # binds 0.0.0.0:2600 on the ui network, no ports published
```

Rejected alternative: `network_mode: "service:tailscale"`. It is simpler, but it puts netweather in Tailscale's network namespace, which means **if that Tailscale node has an exit node configured, every probe egresses through the exit node** — your primary vantage silently becomes someone else's network. The app would detect it (`VANTAGE_CHANGED`) but the user would have no idea why. Do not couple the measurement plane to the access plane.

If you do use namespace sharing, be aware that the app cannot detect it from inside: the exit node is configured on the Tailscale side, and nothing in the netweather container can see that. What it can see is the consequence — the egress IP and ASN change, and maintenance logs `event=vantage_changed` with the previous and new tuple (`DESIGN.md` §7.5). That is a detection of the symptom, not a refusal to start, and the docs say so rather than promising a guardrail the container cannot implement. The default compose file does not use namespace sharing.

### 7.3 Reverse proxy details

- Trust `X-Forwarded-For` **only** from a configured source: `NETWEATHER_TRUSTED_PROXIES` is a comma-separated list of CIDRs or bare addresses. An empty list is the default and trusts nobody. When the immediate peer is trusted, the chain is walked right to left and the first address that is not itself a trusted proxy is the client: a caller can only prepend forged entries, so the rightmost untrusted address is the one a proxy actually saw. A malformed entry reached by the walk falls back to the peer, and a malformed configured CIDR refuses startup rather than silently trusting nobody. Without this, `X-Forwarded-For` is a rate-limit bypass and a log-poisoning vector.
- Docker does not set `X-Forwarded-*`. Your proxy must.
- Healthcheck (`/netweather healthcheck`) hits `127.0.0.1:2600` inside the container and needs no egress. Do not make it check the internet — a healthcheck that fails when your ISP blips will restart-loop the container for no reason.

---

## 8. Traceroute through NAT

A trace from a container shows an extra hop, and often only an extra hop:

```
 1  172.17.0.1     0.08 ms   ← Docker bridge gateway (the host)
 2  192.168.1.1    1.1 ms    ← your router
 3  * * *
```

Three things to know:

- **Hop 1 is not a network hop.** Label it `bridge` in the UI, or users will report "my ISP has a new router" as an incident.
- **ICMP time-exceeded traverses conntrack** because it is "related" to a tracked flow. Some iptables/nftables configurations still drop it, and rootless/slirp4netns setups differ again. A trace that stops after hop 1 is a **container artifact**, not a path failure.
- Because of the above, `trace` is the least trustworthy instrument in a container. That is a second, independent reason for the policy already in `DESIGN.md` §9.1: **prefer a second vantage over a trace.**

**Implemented:** the API's `note` on `POST /api/v1/targets/{id}/trace` states both caveats when they apply: a private first hop is "likely the local gateway rather than a wide-area hop", and a walk that stops after that hop is a "NAT/conntrack artifact, not a path failure". The corridor event's captured trace appends the same artifact note, and the drawer's trace page renders it beside the hop table. A tile never carries a trace, because a trace is a hint about a path rather than a target's state.

If honest traces matter more than isolation, that is exactly what the opt-in host-networking profile is for. Then §6's guard list becomes mandatory rather than defensive.

---

## 9. Tor sidecar

The single networking mistake here is copying `SocksPort 127.0.0.1:9050` from a desktop torrc. Across containers that address is the sidecar's own loopback, and the connection is refused.

```conf
# in the sidecar — binds its container interface, not its loopback
SocksPort 0.0.0.0:9050
ClientOnly 1
ExitPolicy reject *:*        # still mandatory
BridgeRelay 0
PublishServerDescriptor 0
IsolateSOCKSAuth 1
MaxCircuitDirtiness 3600
NewCircuitPeriod 3600
```

Safety properties this preserves:

- `0.0.0.0` inside the sidecar's namespace is reachable **only** from the `egress` network. No published port, so not from the host, not from the LAN.
- `ClientOnly 1` + `ExitPolicy reject *:*` means the bind cannot be turned into a relay. They are baked into the image's `torrc` (not a mount an operator can forget) and pinned by a test. `netweather doctor` proves the SOCKS endpoint answers; it cannot read the sidecar's effective config, so it does not claim to audit the policy.
- `IsolateSOCKSAuth` works over TCP and does not require the control port. Distinct SOCKS username/password per vantage = distinct circuit, no `SIGNAL NEWNYM`.

For a country-labelled vantage, one Tor process cannot be steered per stream: `ExitNodes {cc}` is a process-wide setting. The field trial therefore runs six separate client-only sidecars (`tor-de`, `tor-jp`, `tor-au`, `tor-sg`, `tor-cl`, `tor-mx`), each with its own baked torrc and DataDirectory. `NETWEATHER_TOR_VANTAGES` maps each code to that sidecar's SOCKS endpoint. The monitor fetches the exit IP through that hop, then asks the same Tor process's internal control port (`GETINFO ip-to-country/<ip>`) where that address is. Tor's own GeoIP database is authoritative for the label; Cymru still supplies the ASN but its registrar country can disagree with the relay location. The control port stays on `egress` and is never published. One exit with no healthy circuits is skipped; the other continents keep probing.

And one thing to verify, not assume: **DNS must resolve at the exit.** `socks5h://` (remote DNS) is required; `socks5://` resolves locally and defeats the entire vantage. Config validation rejects `socks5://` for a vantage - only `socks5h://` is accepted. The `socks5h` dialer is what guarantees the name is sent to the proxy rather than resolved here; `netweather doctor` does not separately observe the local resolver, because that assertion would need a resolver the doctor controls, and the dialer's form is the enforceable part.

Bootstrap is a poll, not a sleep (`DESIGN.md` §5.7): connect to `tor:9050`, send a SOCKS greeting, retry with backoff, expose `BOOTSTRAPPING`, time out at 180s with a clear message. `depends_on` is not a readiness check.

---

## 10. Port 53, 123, and the rest

Egress allowlist, and what happens when a network blocks it:

| Port | Use | If blocked |
|---|---|---|
| 80, 443 | HTTP(S), TLS, DoH | Fatal — the tool cannot work. Detect at startup, exit with a clear message. |
| 53 | Direct authoritative DNS (country anchors) | Country anchors → `UNMEASURABLE`, banner, not `DOWN` |
| 123 | NTP clock-offset check | Clock check `UNKNOWN`; correlation confidence reduced |
| 853 | DoT (optional) | Instrument disabled |
| 9050 | Tor SOCKS (to sidecar, not the internet) | Tor vantage → `UNAVAILABLE`, primary unaffected |

Startup runs all five and prints a table. A user whose corporate network blocks 53 should learn that in one line, not by watching 250 tiles turn red.

---

## 11. Platform matrix

| Runtime | ICMP | Trace | IPv6 | Notes |
|---|---|---|---|---|
| Docker, bridge | ✅ with `ping_group_range` or `NET_RAW` | ⚠️ conntrack-dependent | ❌ unless `enable_ipv6` | Default |
| Docker, host net | ✅ | ✅ honest | ✅ | Loses isolation (§1) |
| Docker Desktop (macOS/Windows) | ✅ | ⚠️ | ❌ | `host` means **the Linux VM**, not your Mac — host networking does not give you the host's network |
| Rootless Docker | ⚠️ via ping socket | ❌ | ⚠️ | Slirp/pasta MTU differs from the host; treat MTU as 1400 |
| Podman rootless | ⚠️ | ❌ | ⚠️ | Same, plus no `NET_RAW` by default |
| Kubernetes | ✅ (default caps include `NET_RAW`) | ⚠️ | ⚠️ | `hostNetwork: true` for trace fidelity; watch NetworkPolicy egress |

In every case where an instrument is unavailable, the container **still starts**, the instrument is marked unavailable with a reason, and the affected targets report `UNMEASURABLE` rather than `DOWN`. Degrade visibly, never silently.

---

## 12. Testing the network plane

The stub E2E (`QUALITY.md` §5.3) runs the fake internet on `127.0.0.1` inside the test process, so it needs no container networking at all — that is the primary harness, and it stays fast.

The containerized smoke test is the one that catches networking bugs, so it must exercise the real topology:

```yaml
# test/compose.e2e.yaml
services:
  fake-internet:
    build: ./testdata/internet
    networks: [egress]
  netweather:
    image: netweather:test
    networks: [egress, ui]
    volumes: ["./test/config:/config:ro"]
    read_only: true
  tor:
    image: netweather-tor:test
    profiles: ["tor"]
    networks: [egress]
```

Then assert the things only a container can break. The suite is `internal/e2e/compose_test.go` behind `//go:build compose`; the names below are its subtests.

| Scenario | Asserts |
|---|---|
| `the image is the hardened release image` | uid 65532, a read-only rootfs, and no shell |
| `a low MTU is reported, not hidden` | on a 1280-byte bridge the `mtu` self-check fails and names 1280, while `port53` still passes with working egress |
| `dns probes the configured resolver and refuses private ones` | `1.1.1.1:53` reaches `UP_REACHABILITY`; `127.0.0.11`, `172.17.0.1`, and `169.254.169.254` are `UNMEASURABLE`, so there is no silent fallback to the host resolver and the SSRF policy is not mistaken for an outage |
| `tor is reachable by service name, not on loopback` | `tor.nw:9050` answers from the app's network after a readiness wait, `127.0.0.1:9050` does not, and `docker port` shows no published 9050 |
| `port 53 blocked is unmeasurable, not down` | on an `--internal` network the `port53` self-check fails and the DNS target becomes `UNMEASURABLE` |
| `backup produces a usable copy` | the live container runs `/netweather backup` and a helper container opens the copy with `sqlite3` and runs `PRAGMA integrity_check`; the release image has no shell, so the binary is the only tool available |

The SSRF scenario is the one worth writing first: it is the only test that proves the guard list matches the *actual* container topology rather than the RFC list. A probe black-holed by a low MTU cannot be asserted here for the same SSRF reason (the only reachable endpoint is a private address), so the gate is covered by unit tests instead; `QUALITY.md` §5.5 states both.

---

## 13. Full compose

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
      NETWEATHER_LOG_LEVEL: info
      NETWEATHER_UI_LISTEN: 0.0.0.0:2600
      NETWEATHER_UI_TOKEN_FILE: /run/secrets/ui_token
      NETWEATHER_TRACE_ENABLED: "false"
      TZ: America/Denver
    read_only: true
    tmpfs: [/tmp]
    user: "65532:65532"
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    sysctls:
      net.ipv4.ping_group_range: "0 2147483647"
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "/netweather", "healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3

  # Optional corroboration vantage. Never primary. See §9 and DESIGN.md §5.7.
  tor:
    image: ghcr.io/<owner>/netweather-tor:latest
    profiles: ["tor"]
    networks: [egress]
    read_only: true
    tmpfs: [/tmp]
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    restart: unless-stopped

  # Optional tailnet access. Proxies to the UI; it never becomes the vantage. See §7.
  tailscale:
    image: ghcr.io/tailscale/tailscale:latest
    profiles: ["tailscale"]
    networks: [ui]
    environment:
      TS_AUTHKEY_FILE: /run/secrets/ts_authkey
      TS_STATE_DIR: /var/lib/tailscale
      TS_USERSPACE: "true"
    volumes:
      - ./data/tailscale:/var/lib/tailscale
      - ./secrets/ts_authkey:/run/secrets/ts_authkey:ro
    read_only: true
    tmpfs: [/tmp]
    cap_drop: [ALL]
    cap_add: [NET_ADMIN]
    security_opt: ["no-new-privileges:true"]
    restart: unless-stopped

  # Optional RIPE Atlas software probe (docs/RIPE_ATLAS.md). RIPE's daemon, not
  # ours: host networking and raw sockets are what it needs to measure from the
  # host address. Off by default, and netweather never reads from it.
  ripe-atlas:
    image: ghcr.io/ripe-atlas/ripe-atlas-probe:latest   # verify the tag against RIPE's docs
    profiles: ["ripe-atlas"]
    network_mode: host
    cap_drop: [ALL]
    cap_add: [NET_RAW, NET_ADMIN]
    security_opt: ["no-new-privileges:true"]
    volumes:
      - ./data/ripe-atlas:/var/atlas-probe
    restart: unless-stopped

networks:
  ui:
  egress:
    driver_opts:
      com.docker.network.driver.mtu: 1400
```

Traceroute is the one capability that cannot live behind a service profile, because the trace runs inside the main process. It ships as a small override file instead:

```yaml
# compose.trace.yaml - docker compose -f compose.yaml -f compose.trace.yaml up -d
services:
  netweather:
    cap_add: [NET_RAW]
    environment:
      NETWEATHER_TRACE_ENABLED: "true"
```

That keeps `cap_drop: ALL` literally true for every default install, and makes enabling raw sockets a visible second file rather than a line someone uncomments.

Deliberately absent from the default file: `ports:` on `tor`, `tailscale`, or `ripe-atlas`; `/var/run/docker.sock`; `network_mode: host` on `netweather`; and any capability on `netweather` itself. The Atlas profile is the only place `network_mode: host` appears, and it is a third-party daemon with its own trust domain - documented rather than hidden.

Present and deliberate: `2600:2600` published on all host interfaces, with a token file that makes the app refuse to start anonymously (§7.1). If you want the loopback posture instead, publish `127.0.0.1:2600:2600` and the token becomes optional.

---

## 14. Open questions

1. **Should `netweather doctor` rewrite the MTU?** It cannot without `NET_ADMIN`, and it should not have `NET_ADMIN`. Current answer: detect, warn, and document the one-line compose fix.
2. **IPv6 as a first-class probe family.** Docker's IPv6 support is real but fiddly (per-network `enable_ipv6`, gateway mode, ip6tables). Implemented: `compose.ipv6.yaml` uses host networking so the container shares the host's IPv6 stack; the `family: 6` target flag and AAAA queries ship. Targets report `SKIPPED_NO_IPV6` rather than `DOWN` when no IPv6 is available.
3. **macvlan profile.** Best measurement fidelity of any mode, worst portability. Worth a documented `profiles: ["lan"]` block if there is demand.
4. **Per-vantage network namespace.** A second `netweather` container on a different host is a better vantage than any proxy. That is federation (`DESIGN.md` §16), post-v1.
