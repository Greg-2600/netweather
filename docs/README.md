# Design documentation

Project name: **netweather** (decided — see `DESIGN.md` §16).

| Doc | Status | Covers |
|---|---|---|
| [`DESIGN.md`](DESIGN.md) | ✅ | Architecture, target classes, scheduler, state model, correlation/attribution, external feeds, sectors/ASN/country, UI, storage, deployment, guardrails, roadmap |
| [`CLASSIFIER.md`](CLASSIFIER.md) | ✅ | The rule table that turns a response into a state. Error classes, challenge signatures, precedence, confidence, hard-target recipes, golden fixtures |
| [`IMAGE.md`](IMAGE.md) | ✅ | Base image decision (Chainguard/Wolfi), two-image strategy, hardening, capabilities, supply chain, Tor sidecar |
| [`NETWORKING.md`](NETWORKING.md) | ✅ | Docker network topology, MTU/PMTU, embedded DNS, SSRF reachability, Tailscale and proxy access, Tor sidecar wiring, platform matrix |
| [`QUALITY.md`](QUALITY.md) | ✅ | Linters, security scanning, the six test layers, fuzz targets, the fake internet, coverage gates, CI |
| [`SCHEMA.md`](SCHEMA.md) | ✅ | SQLite tables, pragmas and pools, incremental rollups, retention jobs, query shapes, backup and export, sizing |
| [`LOGGING.md`](LOGGING.md) | ✅ | Levels and volume, the decision-record field schema, redaction, sinks, backpressure, the in-memory ring, debug bundle |
| [`THREAT_MODEL.md`](THREAT_MODEL.md) | ✅ | Assets, adversaries, trust boundaries, SSRF and exposure threats, supply chain, invariants, residual risk, operator runbook |
| [`ACCEPTABLE_USE.md`](ACCEPTABLE_USE.md) | ✅ | Wire-level behaviour, hard limits, volume math, target selection, prohibited uses, abuse handling, Tor policy, contribution rules |

## Reading order

1. `DESIGN.md` §1–§3 — why this exists and what "outage" means here
2. `DESIGN.md` §4–§7 — how probes and verdicts work
3. `CLASSIFIER.md` — the part that determines whether the tool is useful or noise
4. `DESIGN.md` §8–§12 — external data, UI, storage, deployment
5. `SCHEMA.md` — if you are touching storage, rollups, or retention
6. `THREAT_MODEL.md` and `ACCEPTABLE_USE.md` — before you expose it or point it at anyone
7. `NETWORKING.md` and `IMAGE.md` — if you are going to actually run it
8. `LOGGING.md` — when a target flips and you need to know why
9. `QUALITY.md` — if you are going to change it

## Core position

This is **not** a Downdetector clone and not a homelab uptime monitor.

It is a **private vantage point that is honest about what it can and cannot see**, which classifies bot protection instead of evading it, and which uses breadth (sectors, ASNs, countries) plus external feeds to distinguish *"a service is down"* from *"my connection is down"* from *"something large is happening."*
