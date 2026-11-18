# Changelog

Behaviour changes are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
`<major>.<minor>.<patch>` tags from the release job. The detailed record of
every commit is the git history; this file exists so an operator can read what
changed between two images without it.

## [Unreleased]

### Added

- `state_change` at `INFO`: one line per confirmed transition, written after the
  batch commits, with the same reason string the drawer reads.
- Per-stage `TRACE` records (`event=probe_stage`) from the guarded dialer
  (`dns`, `tcp`) and the HTTP instrument (`http`), selected by a time-boxed
  debug scope; nothing is emitted at the normal level.
- `netweather debug enable --until <RFC 3339>`, alongside `--for`.
- A recorded-response classifier fixture set and its parser, covering the
  challenge, block, rate-limit, legal, origin-error, configuration-error,
  reachability, semantic-pass, semantic-mismatch, and JS-shell branches.
- Target provenance in the API detail and the drawer: the seed or config that
  produced a row and the SHA-256 of that payload.
- A config-reload banner that names a rejected edit and the reason; the previous
  target set stays live.
- The optional RIPE Atlas software-probe profile and its README; netweather
  never reads from it.

### Changed

- `/api/v1/meta` capabilities are computed from the wired dependencies, so a
  disabled action (trace, target control, a reader) is not advertised.
- The dashboard's pause and annotation audit lines carry the request id, like
  the API's.
- The compose Tor client points at the sidecar service (`tor:9050`); the
  binary's own default remains `127.0.0.1:9050`.
- An HTTP target may name a request `path`, which is how the robots.txt recipe is
  configured rather than silently rewritten.

### Fixed

- The session and CSRF cookies are `Secure` whenever the browser's effective
  scheme is HTTPS, including a trusted proxy's `X-Forwarded-For`/`Proto`
  (`NETWEATHER_UI_SECURE_COOKIES` remains an override).
- The content-security policy is sent by the shared middleware, so a static
  asset, icon, or method refusal cannot miss it.
- A CSRF failure returns `403`, matching the documented invariant.
- The Tor vantage bounds each probe at 30s, so a silent circuit cannot stall the
  sequential probe step.
- The shipped `torrc` enables `IsolateSOCKSAuth` and keeps a circuit for an hour.
- A disabled target returns when its config loads again.
- The browser surface renders one generic error page with a request id; internal
  detail stays in the log line that carries the same id.

## Classifier

- No classification rule changed. The recorded fixtures pin the existing table:
  a challenge signature outranks status, a plain 403/429/451 is `BLOCKED`, an
  origin 5xx is `DOWN`, a 404/405/407/410 is `CONFIG_ERROR`, and a status the
  contract admits with a missing body marker is `DEGRADED` rather than `DOWN`.
