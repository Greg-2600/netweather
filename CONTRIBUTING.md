# Contributing

Read `docs/ACCEPTABLE_USE.md` first. This project probes third-party infrastructure, so a change that looks like a feature to you can be an abuse vector to everyone else. Changes are judged against that document, not only against the tests.

## Setup

- Go **1.27+** (`go version`), Docker for container tests
- No C compiler is needed: the build is `CGO_ENABLED=0` and SQLite is pure Go

```sh
git clone https://github.com/Greg-2600/netweather
cd netweather
go build ./...
go test ./...
```

## Build and test

One entry point, identical on Windows, macOS, Linux, and CI:

```sh
go run ./internal/devtool verify   # fmt -check, modules, vet, lint, test, coverage gates, build
go run ./internal/devtool lint-all # Go + both Dockerfiles + full-history gitleaks (Docker required)
go run ./internal/devtool test     # race (when cgo is available) + shuffle + count=1
go run ./internal/devtool cover    # coverage profile + per-package floors
go run ./internal/devtool build    # dist/netweather with version, commit, date
go run ./internal/devtool repro    # build twice, compare SHA-256
go run ./internal/devtool snapshot # five platform binaries + SHA256SUMS
go run ./internal/devtool ui       # Playwright suite (browser required)
```

Formatting and tools are pinned in `go.mod` as `tool` directives, so there are no global installs and no version drift:

```sh
go tool gofumpt -w .
go tool gci write -s standard -s default .
go tool govulncheck ./...
```

CI runs exactly these commands; if `devtool verify` passes locally it should pass there.

Optional pre-commit hook: `git config core.hooksPath .githooks`. It runs `fmt -check`, `vet`, and the test suite.

`go test -race` needs a C compiler. On Windows without cgo, the devtool skips the race flag and CI still runs it on Linux.

### Browser tests

The UI suite is opt-in so the default loop stays fast:

```sh
go run github.com/mxschmitt/playwright-go/cmd/playwright@v0.6401.0 install chromium
go test -tags=uitest ./internal/web/uitest -timeout 10m
```

No `sleep` and no class-based selectors: use `data-testid` and Playwright's auto-waiting assertions. A failure writes a screenshot to `testdata/out/`. See `docs/QUALITY.md` §5.8.

## Rules that are not negotiable

1. **No evasion.** No User-Agent rotation, TLS fingerprint impersonation, CAPTCHA solving, or residential proxy pools. `BLOCKED` is a valid, useful answer.
2. **No new outbound ports or HTTP methods** without a measurement justification and a matching guardrail test.
3. **No response bodies stored.** One summary line, 200 bytes.
4. **Every limit is enforced in code and covered by a test.** A limit that exists only in documentation will be treated as a bug.
5. **New dependency?** Justify it in the PR. The dependency set is deliberately small.

## Commits

- One logical change per commit; say *why* in the message, not just what.
- Sign off your commits (`git commit -s`) to certify the Developer Certificate of Origin.
- Add a line to `docs/DESIGN.md` §14 if you change a guardrail, and to `docs/ACCEPTABLE_USE.md` §3 if you change what leaves the container.

## Tests

- Classifier changes need a recorded fixture (`docs/CLASSIFIER.md` §13). Do not test against a live site.
- Bug fixes need a test that fails before the fix.
- Coverage gates are in `docs/QUALITY.md` §7: classifier 95%, correlation 90%, scheduler 85%, everything else 70%.

## Security

Do not open a public issue for a vulnerability. See `SECURITY.md`.
