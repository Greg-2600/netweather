# netweather — Container image

> The base image decision, the two-image strategy, and the hardening that actually matters.

---

## 1. Decision

**Use the Chainguard / Wolfi family.**

| Image | Role |
|---|---|
| `cgr.dev/chainguard/static` | **Release base.** No shell, no package manager, no libc tooling. CA certs + tzdata only. |
| `ghcr.io/<owner>/netweather:<version>` | **Release image**, built from `cgr.dev/chainguard/static`. The default a deployment should run. |
| `ghcr.io/<owner>/netweather:<version>-debug` | **Debug variant**, built from `cgr.dev/chainguard/wolfi-base`: glibc, `apk`, a shell, and network tools for troubleshooting. Same binary, different base and tag; never the default. **Published and signed** by the release job. |
| `ghcr.io/<owner>/netweather:main` and `:sha-<commit>` | **CI image** from the newest commit on `main`, built by the `image-publish` job in `.github/workflows/ci.yml`. `linux/amd64`, not signed, no SBOM. For the k3s path that needs a fresh build; see §6.1. A user who wants a verified image runs a release tag. |
| `ghcr.io/<owner>/netweather-tor` | **Tor sidecar.** Built on Alpine, not Wolfi: Chainguard's Wolfi base does not package `tor`. Alpine 3.20 packages it, is small, and keeps the `torrc` baked in and the rebuild cadence ours. **Published and signed** by the release job; `compose.yaml` builds the same Dockerfile locally by default. |

Why Wolfi rather than a pentest distro:

- It is an actual Linux distribution **built for containers** — minimal by design, not minimal by stripping
- **Daily rebuilds** upstream, so base-image CVEs are patched without you cutting a release
- **glibc**, not musl — avoids resolver and threading differences if any C code ever enters the stack
- Signed images and SBOMs by default
- `apk` is available in the debug variant, so `dig`, `traceroute`, and `tcpdump` exist exactly where you need them and nowhere else

The alternative we rejected — a security *tooling* distro — is discussed in §2.

---

## 2. Why not Kali / Parrot / BlackArch

This is the obvious instinct for a security-adjacent project, and it is the wrong trade.

| Image | Rough size | Shell | Rebuild cadence | Verdict |
|---|---|---|---|---|
| `cgr.dev/chainguard/static` | ~2–10 MB | ❌ | daily | ✅ release |
| `gcr.io/distroless/static-debian12` | ~2–10 MB | ❌ | periodic | ✅ fallback |
| `alpine:3.20` | ~7 MB | ✅ | periodic | ⚠️ fallback, musl |
| `cgr.dev/chainguard/wolfi-base` | ~25 MB | ✅ | daily | ✅ debug |
| `debian:12-slim` | ~75 MB | ✅ | periodic | ⚠️ fallback, boring but correct |
| `kalilinux/kali-rolling` | ~120 MB *before tools* | ✅ | rolling | ❌ |
| `parrotsec/security` | >1 GB | ✅ | rolling | ❌ |
| `blackarchlinux/blackarch` | several GB | ✅ | rolling | ❌ |

The reasoning, in order of importance:

1. **Attack surface.** A pentest distro ships thousands of packages we will never execute. Every one is a CVE feed into an image whose only job is to make HTTP requests and write SQLite rows.
2. **The Kali project itself discourages using it as a general-purpose server OS.** It is a live-assessment platform. Basing a long-running service on it is fighting the design.
3. **Signal.** A public tool that monitors the internet, shipped on an offensive distro base, invites "is this an attack tool?" scrutiny — and we have already documented dual-use concerns (§14 of `DESIGN.md`). The base image should not add to that.
4. **Size and cold-start.** 120 MB of unused tooling, on an image that would otherwise be under 10 MB.
5. **Reproducibility.** Rolling-release distros make it hard to pin and rebuild identically.

**The important point:** distro choice is the *least* significant part of container security. Read-only rootfs, non-root UID, dropped capabilities, no shell, no package manager, and a signed SBOM matter far more. A hardened `distroless` image beats a `kali` image with a shell in it, every time.

If you want the security *aesthetic*, it belongs in the `:debug` tag — never the release image.

---

## 3. Build

```dockerfile
# syntax=docker/dockerfile:1.7

# ---------- build ----------
FROM golang:1.23-alpine AS build
WORKDIR /src

# Dependency layer first: cached unless go.mod/go.sum change
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/netweather ./cmd/netweather

# ---------- release ----------
FROM cgr.dev/chainguard/static:latest AS release
COPY --from=build /out/netweather /netweather
USER 65532:65532
EXPOSE 2600
ENTRYPOINT ["/netweather"]

# ---------- debug ----------
FROM cgr.dev/chainguard/wolfi-base:latest AS debug
USER 0
# The base points at Chainguard's repo, which does not carry these tools; add
# Wolfi's own repository first. The shipped build logs this line so the image is
# reproducible from the Dockerfile alone.
RUN echo 'https://packages.wolfi.dev/os' >> /etc/apk/repositories \
 && apk add --no-cache \
      ca-certificates-bundle tzdata \
      bind-tools iputils tcpdump curl netcat-openbsd
COPY --from=build /out/netweather /netweather
USER 65532:65532
EXPOSE 2600
ENTRYPOINT ["/netweather"]
```

Notes:

- `CGO_ENABLED=0` — the binary is fully static, which is why the release image needs no libc at all. SQLite is `modernc.org/sqlite` (pure Go) precisely so this stays true.
- `-trimpath` and pinned digests in production for reproducibility.
- The release image must contain **CA certificates and tzdata** — TLS verification and `TZ` both depend on them. Verify in CI rather than assuming.
- Two targets, two tags: `netweather:1.2.3` and `netweather:1.2.3-debug`. The release job builds, pushes, and signs both, and the plain tag stays the default: the debug tag is the one with a shell.

Network topology (planes, MTU, DNS, access posture) is specified separately in [`NETWORKING.md`](NETWORKING.md) — the compose file below is the shape, that document is the reasoning.

### Compose

```yaml
services:
  netweather:
    image: ghcr.io/<owner>/netweather:latest
    ports: ["2600:2600"]        # UI on 2600 — unprivileged, so the non-root UID can bind it
    volumes:
      - ./data:/data
      - ./config:/config:ro
    read_only: true
    tmpfs: [/tmp]
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "/netweather", "healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3

  # Optional: Tor vantage (docs/DESIGN.md §5.7). Built from this repository's
  # Dockerfile.tor so the client-only torrc is baked in, not mounted.
  tor:
    build:
      context: .
      dockerfile: Dockerfile.tor
    image: netweather-tor:local
    profiles: ["tor"]
    read_only: true
    tmpfs: [/tmp]
    cap_drop: [ALL]
    restart: unless-stopped
```

---

## 4. Hardening posture

The list that actually matters:

| Control | Setting |
|---|---|
| Root filesystem | `read_only: true` |
| User | `65532:65532` (nonroot), never root |
| Capabilities | `cap_drop: ALL`, add back only what is justified (§5) |
| Privilege escalation | `security_opt: ["no-new-privileges:true"]` |
| Shell in release image | none |
| Package manager in release image | none |
| Writable paths | `/data` (volume) and `/tmp` (tmpfs) only |
| Inbound | UI port only, bound to loopback by default |
| Outbound | 80/443/53/123 only |
| Seccomp | runtime default (do not disable) |
| Telemetry | none |

Two consequences worth stating plainly:

- **`netweather` must refuse to start as root.** It should exit with an explanation, not run. This is the inverse of `proxy_scan.sh`'s `check_root`.
- **`read_only: true` means the binary cannot write next to itself.** All state lives in `/data`, all config is read-only. A config reload writes nothing to disk.

---

## 5. Capabilities for ICMP and traceroute

ICMP and traceroute need raw sockets. Two options, in preference order (`DESIGN.md` §5.5):

**Option A — unprivileged ICMP (preferred):**

```yaml
sysctls:
  net.ipv4.ping_group_range: "0 2147483647"
```

No capability added. Works with `golang.org/x/net/icmp` datagram sockets. Enough for `icmp` probes, **not** enough for `trace`.

**Option B — `CAP_NET_RAW` (only when tracing is enabled):**

```yaml
# compose.trace.yaml - a second file, never the default
services:
  netweather:
    cap_add: [NET_RAW]
    environment:
      NETWEATHER_TRACE_ENABLED: "true"
```

`CAP_NET_RAW` is a meaningful privilege - it permits arbitrary raw packet construction. It is enabled only by merging `compose.trace.yaml`, which also flips `NETWEATHER_TRACE_ENABLED`; the default compose keeps `cap_drop: ALL` literally true. Traceroute stays on-demand regardless: there is no schedule that can reach it, and the API and the target drawer share one ten-per-hour governor.

Either way: if neither is available, `icmp` reports the socket error as an unavailable instrument (never as `DOWN`), `trace` is disabled with a visible reason, and **the container still starts**. There is no silent `tcpping` substitution: it would change what the anchor measures.

---

## 6. Supply chain

Cheap for a Go project, and expected of a tool that asks people to run it:

| Item | Tooling | Status |
|---|---|---|
| Multi-arch build | `docker buildx` — `linux/amd64`, `linux/arm64` | **Implemented:** `.github/workflows/release.yml`, on every `v*` tag |
| Image signing | `cosign sign` on every release tag | **Implemented:** keyless, through GitHub OIDC |
| SBOM | `syft` → attached as an attestation | **Implemented:** buildx attaches an SPDX SBOM, and the release job attests a syft SPDX file with `cosign attest` |
| Provenance | SLSA build provenance via GitHub Actions | **Implemented:** `provenance: mode=max` on the pushed manifest list |
| Vulnerability scan | `govulncheck` for the module graph, `grype` for the image | **Implemented:** `govulncheck` blocks CI; `grype` scans the release image, advisory, with SARIF uploaded |
| Dependency updates | Dependabot, grouped weekly | **Implemented:** `.github/dependabot.yml`, one grouped PR per ecosystem |
| Pin by digest | `image@sha256:…` in the documented production compose | **Documented:** the README verify snippet and `compose.yaml` both say to pin the digest |
| Base image currency | Upstream rebuilds, pulled deliberately | **Implemented:** every `FROM` is pinned by digest (`cgr.dev/chainguard/*` for the main image, `alpine` for the Tor sidecar, which Wolfi does not package), and Dependabot opens a weekly digest update. A moving `:latest` tag is a dependency the release cannot reproduce, so `TestDockerfilesPinBaseImages` fails if one returns. |

The README carries the `cosign verify` one-liner, so a user can check the signature instead of trusting a tag. A release that fails to sign does not finish: publishing an unverifiable image is worse than publishing nothing.

### 6.1 CI image from `main`

`.github/workflows/ci.yml` builds the CI image after every blocking gate passes (format, lint, vuln, secrets, Dockerfile lint, build, browser, container smoke, and compose). It uses the same `Dockerfile` and the same `VERSION`, `COMMIT`, and `DATE` build arguments as the release.

| Event | Job | Pushes? | Tags |
|---|---|---|---|
| Pull request | `image-build` | No. Build only, so a fork can never publish | none (local `netweather:pr-<number>`) |
| Push to `main` | `image-publish` | Yes, to `ghcr.io/<owner>/netweather` | `sha-<full commit SHA>` (immutable) and `main` (moving) |
| `v*` tag | `release.yml` (unchanged) | Yes, multi-arch, signed, SBOM | `<version>`, `latest`, `<version>-debug` |

Tag rules:

- `sha-<commit>` names one commit and is never pointed at another build. Deploy this tag, or better its digest, when you need to know exactly what is running. A re-run of the same commit's job pushes the same tag again with an equivalent build.
- `main` always moves to the newest successful build on `main`. It suits a test instance that should follow `main`. Do not use it where you need a rollback target.
- `latest` and `<version>` stay release-only. The CI image never pushes them.
- The binary inside reports `main-<first 12 characters of the SHA>` from `netweather version`, so a running pod shows which commit it came from.

Permissions: only `image-publish` has `packages: write`, together with `contents: read`, and it logs in with the automatic `GITHUB_TOKEN`. No personal access token or other secret is used. `TestCIImagePublishRules` in `cmd/netweather/workflow_test.go` fails if a pull request job could push, if another job gains `packages`, or if the workflow reads any secret other than `GITHUB_TOKEN`. A concurrency group (`ghcr-main-image`) queues two `main` runs instead of letting them race on the `main` tag.

The published package appears under the repository's **Packages** sidebar. The `org.opencontainers.image.source` label links it to this repository. The CI image is a convenience build, not a release: it is unsigned and single-architecture, so pin its digest and use a release tag for anything you need to verify.

---

## 7. Tor sidecar image

Build it ourselves rather than depending on a third-party Tor image, for three reasons:

1. **We control the `torrc`.** `ClientOnly 1` and `ExitPolicy reject *:*` must be non-negotiable, not a mount the user might forget (`DESIGN.md` §5.7).
2. **Rebuild cadence.** Tor is a security-sensitive component, so the sidecar base is pinned by digest and updated deliberately rather than following whatever a vendor tag points at today.
3. **Size.** An Alpine + `tor` image is tens of MB, not hundreds.

```dockerfile
FROM alpine:3.20@sha256:<digest>
RUN apk add --no-cache tor ca-certificates-bundle
COPY --chmod=0644 torrc /etc/tor/torrc
USER 65532:65532
EXPOSE 9050
ENTRYPOINT ["tor", "-f", "/etc/tor/torrc"]
```

The `torrc` is baked in at build time (`torrc`, copied by `Dockerfile.tor`), and `compose.yaml`'s `tor` service builds that image rather than pulling a third-party proxy. The same Dockerfile is published as `ghcr.io/<owner>/netweather-tor` by the release job, signed with cosign, so a deployment that pulls images can use the signed sidecar instead of building locally. The non-negotiable lines are `ClientOnly 1`, `ExitPolicy reject *:*`, `BridgeRelay 0`, and `PublishServerDescriptor 0`; the SOCKS port carries `IsolateSOCKSAuth` so credentials, when a client presents them, select a circuit, and `MaxCircuitDirtiness 3600` keeps one circuit for comparable measurements. A unit test reads `torrc` and asserts every one of those lines, and another asserts the compose service builds `Dockerfile.tor` and publishes no port.

A plain `docker compose up -d` builds the six pinned images,
`netweather-tor-de/jp/au/sg/cl/mx:local`. The optional `--profile tor` also
builds the unpinned `netweather-tor:local`. No Tor service publishes a host
port, and every service stays on the `egress` network.

The app enables Tor with `NETWEATHER_TOR=true` and `NETWEATHER_TOR_VANTAGES` set
to `DE=tor-de:9050,JP=tor-jp:9050,AU=tor-au:9050,SG=tor-sg:9050,CL=tor-cl:9050,MX=tor-mx:9050`.
The optional unpinned sidecar at `tor:9050` stays idle while that list is set.
A `tor-<cc>` label is trusted only after the app verifies the real exit through
that proxy.

`netweather doctor` adds a `tor socks (optional)` check when the Tor vantage is enabled: it proves the SOCKS port answers. It does **not** audit the effective exit policy, because the app container cannot read the sidecar's config, and claiming a runtime policy check would imply a control channel the deployment does not have. The policy is guaranteed by the image, which is why the image is built here.

The app's UI token file is a separate startup prerequisite. Manual Compose
deployments must set `secrets/ui_token` to mode `600` and owner UID/GID
`65532:65532`; otherwise the container uid cannot read it and the app
crash-loops. The deploy scripts copy it into a secret volume with those
permissions. Docker Desktop bind mounts cannot carry Unix ownership, so use a
volume or secret for the manual Windows path.

---

## 8. Fallback if Chainguard is unavailable

Some environments cannot reach `cgr.dev`, and Chainguard's free tier is a moving target. The documented fallback, in order:

1. `gcr.io/distroless/static-debian12:nonroot` (release) + `debian:12-slim` (debug)
2. `alpine:3.20` (both), accepting musl and a busybox shell in the release image
3. `scratch` + a statically linked binary + copied CA bundle (release), `debian:12-slim` (debug)

The Dockerfile keeps the build stage separate from the runtime stage specifically so swapping the runtime base is a one-line change.

---

## 9. Open questions

1. **Do we publish the debug image at all?** It is useful, but it is also the image someone will run in production by mistake. Leaning yes, with a `-debug` suffix and a warning banner in the UI when it detects a shell is present.
2. **Should the release image include `tzdata`?** Needed for correct local-time display; adds ~2 MB. Currently yes.
3. **Wolfi vs. Alpine for the Tor sidecar - resolved: Alpine.** Chainguard's Wolfi base does not package `tor`, so the sidecar would not build there; Alpine 3.20 does, is small, and is pinned by digest like the main image.
