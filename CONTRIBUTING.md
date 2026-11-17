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

## Before you open a PR

```sh
gofmt -l .            # must print nothing
go vet ./...
CGO_ENABLED=0 go build ./...
go test -race ./...
```

CI additionally runs `golangci-lint`, `gosec`, and `govulncheck`. Run them locally if you can; a failing security scanner is a review blocker, not a style note.

`go test -race` needs a C compiler. On Windows without cgo, `go test ./...` is still valid; CI runs the race detector on Linux.

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
