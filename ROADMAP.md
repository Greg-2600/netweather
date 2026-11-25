# Roadmap

netweather is a self hosted internet weather station that answers three questions in order: is something large happening right now, is a specific service or sector down, and is it me. It probes a list of real targets from one vantage point, classifies bot protection instead of evading it, and never lets a single vantage claim to be a global truth. The goal is an honest, low volume signal that a volunteer can run in a small container and read at a glance, with external feeds as corroboration rather than a second detector. This file records where the project stands and the next pieces of work, judged against `docs/ACCEPTABLE_USE.md` and `docs/QUALITY.md`.

Legend for size: S is a small change, M is a moderate change, L is a large change.

## Current state

Phases 0 to 3 are shipped. The binary probes HTTP, TLS, TCP, DNS, DoH, DoT, QUIC UDP reachability, NTP, and ICMP on a tiered scheduler, classifies each result, writes to SQLite, and serves a store backed board over server rendered HTML and SSE. The default install now includes the full 1000 target field trial fleet and one unpinned Tor corroboration sidecar. Shipped surfaces include incident timelines, baseline anomalies, coverage, corridors, the Internet Health Index, feeds and enrichment (IODA, Radar, Statuspage, AWS, GCP, Azure, Atlas, RIS Live), DNS divergence, certificate pins, idempotent actions, on demand traceroute, remote vantage agents, the versioned API, and the board, map, atlas, hubs, world, corridors, coverage, incidents, feeds, and diagnostics pages. Containers ship a hardened release image plus Tor, Tailscale, Atlas probe, trace, and IPv6 overlays; deployment tooling includes Compose, Kubernetes manifests, a Helm chart, a systemd unit, and Azure Terraform.

Verification on this tree: `go run ./internal/devtool verify` is green with library coverage at 87.3 percent and every per package gate passing, and the Go linter reports zero issues. The latest `ci` and `azure k3s` runs on `main` at `dfb5c41` both succeeded. The scheduled real endpoint run, the weekly browser run across three engines, and the advisory mutation job complement the blocking pull request gates. There is no continuous image publish from `main` and no automated deploy yet; both are tracked below.

Stated limits: one shared token with no named users, anomaly detection is floor based until roughly 28 weeks of hour of week history exist, no multi day soak has been recorded, QUIC is UDP reachability only, the DF bit path MTU probe is Linux only, IPv6 measurement needs host networking, and RIS Live is a poller by default with a WebSocket overlay.

## Now, next 2 to 4 weeks

- Land open PR #15, the Atlas screenshot gallery for the README, so the documentation matches the shipped atlas view (S).
- Publish a continuous integration image to GHCR on pushes to `main` using an immutable commit SHA tag and a moving `main` tag, with build only on pull requests, closes issue #8, so the k3s path from PR #7 can pull a fresh image (M).
- Bring the Helm chart to parity with `deploy/k8s`, adding startup, readiness, and liveness probes on `/healthz`, a named container port, and a Secret reference with no token value in values, closes issue #9, so one deployment path is canonical (M).
- Record the Azure Terraform review from PR #7 in `deploy/azure/REVIEW.md` and apply any correctness fixes, closes issue #10, so no cloud apply happens against unreviewed network exposure, state, or cloud init secret handling (M).

## Next, 1 to 3 months

- Add a deploy workflow that promotes the SHA tagged image to k3s with a rollout wait, a `/healthz` gate, and rollback, closes issue #11, so `main` reaches the running instance without a manual apply (L).
- Add a Prometheus ServiceMonitor, a small alert rule set, a scheduled no credential uptime workflow, and a documented query set, closes issue #12, so the k3s instance is observable and failures are visible off site (M).
- Write `docs/ARCHITECTURE.md` and `docs/RUNBOOK.md` and link them from both READMEs, closes issue #13, so a new volunteer can answer a health, rollout, certificate, disk, metrics, uptime, or token incident without guessing (M).
- Run the documented 72 hour soak at the default fleet and record counters and growth in `docs/SOAK.md`, so the current no soak gap is closed with evidence rather than an estimate (M).

## Later

- Add OIDC with viewer and operator roles while keeping the shared token as break glass, so the audit trail names a subject at deployment scale and the single team model stays usable (L).
- Add incident subscriptions with opt in delivery and unsubscribe tokens, so an operator can follow one incident without polling the board (M).
- Add maintenance windows with affected components and planned and completed states, so planned work is not reported as an outage (M).
- Add bounded historical uptime from retained rollups with missing and unknown explicitly excluded, so a public status view stays honest (M).
- Add an accessibility rules pass on the board, login, and detail pages, so keyboard and screen reader use is checked in CI after a triage policy exists (S).

## Deliberately not planned

- WAF evasion, fingerprint spoofing, scraping, load testing, and port scanning, because `BLOCKED` is a valid answer and acceptable use forbids them.
- Running a Tor exit relay, telemetry, and a multi tenant service, because the shipped design is a single team, self hosted instance with client only Tor.
- A generated `openapi.yaml`, because `docs/API.md` and `/api/v1/meta` are the contract and a partial generated spec would mislead.
- A full QUIC handshake, a DF bit probe outside Linux, and default corridor endpoints, because the shipped reachability probe, the interface MTU check, and operator supplied pairs already cover those cases honestly.

## Quality and DevOps

- Tests: six backend layers run on every change, unit with race and shuffle, classifier fixtures, the fake internet, API and integration, the compose network plane, and short fuzz runs; Playwright covers the browser on Chromium per pull request and on all three engines weekly; real endpoint runs are weekly and on demand and never block a merge.
- Coverage: per package floors are enforced in `internal/devtool/main.go` and documented in `docs/QUALITY.md` section 7, with 95 percent for the classifier and core logic down to 72 percent for the web package, while the overall number is reported and not gated.
- CI: `.github/workflows/ci.yml` blocks on format, modules, vet, lint, tests, coverage gates, fuzz, `govulncheck`, `gitleaks`, `hadolint`, static build, reproducible build, container smoke, and compose checks; `.github/workflows/e2e.yml`, `.github/workflows/release.yml`, and `.github/workflows/azure_k3s.yml` cover scheduled real tests, tagged releases, and Terraform and manifest validation.
- Security scanning: `gosec` runs through the Go linter, `govulncheck` is call graph aware, `gitleaks` scans full history, `hadolint` checks both Dockerfiles, and the release image gets an advisory `grype` scan; tagged images are signed keyless with `cosign` and carry an SBOM and provenance attestation.
- Containers: the release image is a small static Chainguard based image with no shell, running as UID 65532 with a read only root filesystem and all capabilities dropped, publishing only port 2600, with a client only Tor sidecar and opt in Compose overlays.
- Releases: tags publish multi arch images for the app, a debug variant, and the Tor sidecar, with digest pinning and a `cosign verify` recipe in `docs/IMAGE.md`; the gap is continuous publish from `main` (issue #8) and automated deploy (issue #11).

## Known risks

- One shared token means no named users and no per person audit trail; OIDC is deferred until a deployment needs it.
- No multi day soak has been recorded, so memory growth and dropped counters over a week are estimates from short runs; the 72 hour run is an operator step.
- Detection is conservative until about 28 weeks of baseline history exist, so early anomaly results can under report slow changes.
- The Azure default VM size is too small for the full fleet plus Tor, the memory note in `deploy/azure/README.md` calls for at least 4 GiB, and cloud apply, DNS changes, and secret creation remain maintainer actions.
- Terraform state and the generated UI token are sensitive; remote state is only commented out and cloud init must never print the token.
- Feed adapters depend on third party JSON shapes; recorded fixtures pin the current shape but a live change can silently drop events until the fixture is refreshed.
- Until issues #8 and #11 land, the running k3s instance can drift from `main` and there is no automated rollback outside the Compose deploy scripts.
- QUIC is UDP reachability only, the DF bit MTU probe is Linux only, IPv6 measurement needs host networking, and RIS Live is a poller by default, so some claims are deliberately narrower than the instrument names suggest.

## How work is accepted

1. `go run ./internal/devtool verify` is green.
2. A behaviour change has a test that fails without it and the doc that describes it changes in the same commit.
3. Container visible changes extend `devtool compose` and browser visible changes extend the Playwright suite.
4. Every commit is signed off and a user visible change adds a `CHANGELOG.md` line.
