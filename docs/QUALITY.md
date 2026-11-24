# netweather — Code quality, security scanning, and testing

> Python gives you `black` + `isort` + `pylint` + `mypy` + `bandit` + `pytest`. This is the Go equivalent, and in two places it is strictly better.

---

## 1. Start with the thing Python cannot give you

**`mypy` has no equivalent here because Go does not need one.** The compiler is a whole-program static type checker that runs on every build. There is no untyped runtime, no `AttributeError` at 3am, no `None` where a `Target` was expected. Roughly a third of the bug classes `mypy` exists to catch are impossible by construction.

That changes the tooling budget. The effort moves from *"is the type right?"* to *"what the compiler cannot see"*:

| Compiler cannot catch | Covered by |
|---|---|
| Data races across the probe path (step, ring, board) | `go test -race` |
| Unclosed HTTP response bodies | `bodyclose`, `go vet httpresponse` |
| Goroutine and timer leaks | `goleak`, `go vet lostcancel` |
| A `switch` missing a new state | `exhaustive` |
| A swallowed error | `errcheck`, `errorlint` |
| Decompression bombs from gzip bodies | `gosec G110` |
| SSRF via a user-supplied URL | `gosec G107` + runtime guard |

That last row is the point: this codebase has a real attack surface and real concurrency. The tooling should be aimed there.

---

## 2. Parity table

| Python | Purpose | Go equivalent | Notes |
|---|---|---|---|
| `black` | formatting | **`gofmt`** | Canonical, zero config. Go has one style; there is nothing to argue about. |
| `isort` | import order | **`goimports`** + `gci` | Also removes unused imports |
| `pylint` / `flake8` | lint | **`staticcheck`** + `go vet` | `staticcheck` is the de-facto standard and covers ~150 checks |
| `ruff` | fast all-in-one lint | **`golangci-lint`** | Aggregator; runs the below in one pass |
| `mypy` | static types | **the compiler** | Plus `nilaway` for nil-panic analysis |
| `bandit` | security lint | **`gosec`** | Same class of rules (G1xx–G6xx) |
| `pip-audit` / `safety` | dependency CVEs | **`govulncheck`** | Call-graph aware — only reports vulns you actually reach |
| `pytest` | tests | **`go test`** | Table-driven, subtests, `-race`, `-fuzz` all built in |
| `pytest-cov` | coverage | `-coverprofile` + `go tool cover` | |
| `hypothesis` | property tests | **native fuzzing** + `rapid` | Built into the toolchain |
| `pytest-benchmark` | benchmarks | `testing.B` + `benchstat` | |
| `tox` / `nox` | matrix | GitHub Actions matrix | |
| `pre-commit` | hooks | `lefthook` / `pre-commit` | Both work fine with Go |
| `vulture` | dead code | `unused` (staticcheck `U1000`), `deadcode` | |
| `radon` | complexity | `gocyclo`, `gocognit`, `maintidx` | |
| `pyupgrade` | modernize | `gopls` modernize analyzers | |
| `interrogate` | doc coverage | `revive`, `godot` | |
| `liccheck` | licenses | `go-licenses` | |
| `mutmut` | mutation testing | `gremlins` | Optional; see §5.7 |

Nothing in the Python list is missing a counterpart. Four Go tools have no Python equivalent at all: **`-race`**, **native fuzzing**, **`exhaustive`**, and **`bodyclose`**.

---

## 3. Linter stack

`golangci-lint` is the single entry point. Enabled set, chosen for *this* codebase rather than for maximum count:

```yaml
# .golangci.yml
version: "2"
linters:
  default: none
  enable:
    # correctness
    - errcheck
    - govet
    - staticcheck
    - ineffassign
    - unused
    - bodyclose          # HTTP bodies must be closed - critical at ~5 probes/s
    - rowserrcheck
    - sqlclosecheck
    - contextcheck
    - noctx              # every outbound request needs a context
    - errorlint          # errors.Is/As, not ==
    - nilerr
    - nilnil
    - exhaustive         # every state in every switch
    - durationcheck
    - copyloopvar
    - forcetypeassert
    - wastedassign

    # security
    - gosec

    # resource hygiene
    - prealloc
    - makezero
    - perfsprint

    # style and structure
    - revive
    - gocritic
    - misspell
    - godot
    - usestdlibvars
    - goconst
    - gocyclo
    - funlen
    - dupl
    - unparam

    # tests
    - paralleltest
    - tparallel
    - thelper
    - testpackage
    - testifylint

  settings:
    exhaustive:
      default-signifies-exhaustive: false   # force explicit cases
    gocyclo:
      min-complexity: 15
    funlen:
      lines: 80
      statements: 50
    gosec:
      excludes: [G404]                      # we want math/rand for jitter, not crypto/rand
    revive:
      rules:
        - name: exported
        - name: error-return
        - name: context-as-argument
        - name: indent-error-flow

formatters:
  enable:
    - gofmt
    - goimports
    - gci
    - gofumpt
```

Why these specifically:

- **`bodyclose`** — a prober that leaks response bodies exhausts file descriptors and sockets within hours. This is the single highest-value linter in the list.
- **`exhaustive`** — the state machine has 7 states and several switch sites. Adding `TAMPER` and forgetting a case should be a build failure, not a runtime default.
- **`noctx` + `contextcheck`** — every probe must be cancellable, or a hung target blocks a worker forever.
- **`gosec G107`** — flags HTTP requests with variable URLs, which is precisely our SSRF surface. Expect to annotate it deliberately at the one place it is intentional, with a comment explaining the runtime guard.
- **`gosec G110`** — decompression bombs. We read gzip bodies with a 32 KB cap; the cap must apply *after* decompression.
- **`testpackage`** — tests live in `_test` packages, so they can only use the public API. Keeps the seams honest.

Deliberately **not** enabled: `wsl`, `nlreturn`, `varnamelen`, `lll`. They generate style churn without catching bugs, and they make contributors fight the tool instead of the problem.

---

## 4. Security scanning

Four layers, because they catch different things.

| Layer | Tool | Gate |
|---|---|---|
| Source rules | `gosec` (via golangci-lint) | blocking |
| Dependency CVEs | `go tool govulncheck ./...` | blocking, call-graph aware |
| Container | `grype` on the built image | blocking on `HIGH`/`CRITICAL` |
| Secrets | full-history `gitleaks` | blocking |
| Dockerfile | `hadolint` on both files | blocking |

The workflows themselves are pinned by `TestWorkflowFilesParse`, which parses every workflow and the Dependabot config with the project's YAML library. That catches a file GitHub would reject; it is not a substitute for `actionlint`, which is not wired in.

Decisions the linters cannot express stay review rules:

```
- http.Get(...)                         → use the instrument's context-aware client
- tls.Config{InsecureSkipVerify: true}  → forbidden (gosec also refuses it)
- os.Getenv("...")                      → runtime settings come from env by design;
                                          a new one needs a README row and a test
- math/rand                             → crypto/rand for anything security-relevant
- fmt.Errorf($X) without %w             → errorlint (enabled) blocks it
```

`govulncheck` deserves a note: unlike `pip-audit`, it builds a call graph, so a CVE in a package you import but never call does not fail the build. Fewer false positives, higher signal.

---

## 5. Testing strategy

Six layers run by default. Each answers a question the layer below cannot. A seventh, opt-in browser layer (§5.8) proves the UI wiring and is deliberately outside the default loop.

```
        ┌──────────────────────────────┐
   E2E  │ real endpoints, 6 targets    │  "does it work on the actual internet?"
        ├──────────────────────────────┤
   E2E  │ stubbed internet, real binary│  "does the whole system behave?"
        ├──────────────────────────────┤
   Ctr  │ compose topology, real image │  "does Docker break it?"
        ├──────────────────────────────┤
  Integ │ component + fake dialer      │  "do the pieces fit?"
        ├──────────────────────────────┤
   Fuzz │ classifier, parser           │  "is it total and deterministic?"
        ├──────────────────────────────┤
   Unit │ pure functions, tables       │  "is the logic right?"
        └──────────────────────────────┘
```

### 5.1 Unit tests

Table-driven, `t.Parallel()`, no network, no disk, no sleep.

| Package | What is tested |
|---|---|
| `internal/classify` | Every rule in `CLASSIFIER.md` — one case per row, with the expected value beside the input |
| `internal/probe` | The ladder: which stage failed, with a fake dialer; the transport's per-host cap and TLS floor |
| `internal/schedule` | Tiering, jitter bounds, per-host semaphore, backoff, missed ticks (fake clock) |
| `internal/correlate` | LOCAL, attribution scoring, anomaly thresholds (fixture datasets) |
| `internal/verdict` | Every board verdict; the store test covers REGIONAL's primary-down/corroborator-up rule |
| `internal/region` | Country-to-continent mapping, filter normalisation, and unknown-code handling |
| `internal/config` | TXT/YAML parsing, sector inference from paths, validation errors, reload-state clearing |
| `internal/calendar` | Strict date and region parsing, the absent-file case, and the exclusion's reach |
| `internal/store` | Migrations, pre-migration backup, retention pruning, rollup math, provenance and re-enable |
| `internal/tracebudget` | The fixed window, its reset, a zero limit, and the retry hint |

Stable serialized forms are pinned rather than golden-filed: JSON API responses are asserted field by field in `internal/api` (envelope, error codes, pagination shape), pages are pinned by their `data-testid` contract and the Playwright suite, and classifier decisions are one table row per `CLASSIFIER.md` rule, kept in code so the expected value sits beside the input. Feed payloads are the one captured-file set (`testdata/feeds/`), because a third party's shape is the thing under test.

### 5.2 Property and fuzz tests

Go's native fuzzing is the best fit for the classifier contract, which states that classification is **total** and **deterministic**:

```go
func FuzzClassify(f *testing.F) {
    f.Add([]byte("HTTP/1.1 200 OK\r\n\r\n"))
    f.Fuzz(func(t *testing.T, raw []byte) {
        r := parseResponse(raw)          // must never panic
        s1, _, _ := classify(target, r)
        s2, _, _ := classify(target, r)
        if s1 != s2 {
            t.Fatalf("non-deterministic: %v vs %v", s1, s2)
        }
        if !s1.Valid() {
            t.Fatalf("invalid state: %v", s1)
        }
    })
}
```

Fuzz targets, implemented:

- `FuzzClassify` (`internal/classify`) — totality, determinism, no panic on arbitrary status, header, body, and final-URL bytes. The classifier reads the response in place, so this also covers response parsing: there is no separate `parseResponse`.
- `FuzzParse` (`internal/config`) — arbitrary YAML/TXT must produce a validated file or an error, never a panic. `config.Parse` exists so the parser can be fuzzed without the filesystem.
- `FuzzRank` (`internal/correlate`) — share stays in [0,1], failures never exceed members, scores are finite and non-negative, ordering is deterministic, and a winner always clears the gates.
- `FuzzScrubStringIsIdempotent` and `FuzzParseLevel` (`internal/logging`) — redaction is idempotent, and a level parse either succeeds or errors.

Each runs for a short time in CI (`.github/workflows/ci.yml`) and locally with `go test -run=^$ -fuzz=<name> ./<package>`.

`rapid` (property-based) is not needed for the attribution scorer: `TestRankRecountsEveryCluster` (`internal/correlate`) generates random failure distributions with the standard library and checks every candidate against an independent recount — `Failing`, `Total`, and `Share` are recomputed from the observations rather than trusted — and checks that a winner exists exactly when some candidate clears the gates and is the highest-scoring one that does. It deliberately does not assert that the winner contains the most failures, because that is false: a 5-of-5 cluster outscores a 6-of-10 cluster while containing fewer failures, and the documented score is the ranking.

### 5.3 Integration — the fake internet

This is the highest-value harness in the project, because the classifier is only as good as the responses it is tested against.

A single test binary that stands up a complete, deterministic, offline internet:

```
testdata/internet/
  scenarios.yaml      # per-host behaviour
  certs/              # test CA + leaf certs
```

```yaml
- host: chase.test
  tls: valid
  http:
    - status: 403
      headers: { cf-mitigated: challenge }
      body: "<html>Just a moment...</html>"

- host: akamai.test
  http:
    - status: 403
      body: "Access Denied<br>Reference #18.abc123"

- host: slow.test
  delay: 30s

- host: loop.test
  http: [{ status: 302, location: /a }]

- host: expired.test
  tls: expired

- host: ipv6only.test
  aaaa: true
```

It provides, on `127.0.0.1` with ephemeral ports:

- an HTTPS server per scenario, using a **local CA** injected into the client's root pool
- a UDP DNS server answering from `scenarios.yaml`
- a SOCKS5 server that forwards to the fake hosts (exercises the Tor vantage path)
- a controllable clock and a controllable "egress IP" endpoint

Then run the **real binary** against it, drive it through its HTTP API, and assert on rendered state. No mocks of our own code — the seams are `Dialer` and `Clock`, which are already design requirements.

This catches the bugs unit tests cannot: config → scheduler → probe → classify → store → correlate → SSE → template.

**Implemented now:** `internal/e2e` wires the real store, agent, classifier, board source, and HTTP API together with a scripted runner at the probe seam. It asserts that a scripted Cloudflare challenge reaches the board as `BLOCKED`, a 503 as `DOWN`, a 200 as `UP_REACHABILITY`, that coverage counts `DOWN` but treats `BLOCKED` as a blind spot, and that `/targets/{id}` and `/targets/{id}/probes` agree with the tile. It runs in `devtool verify` and in CI.

**Implemented now, the fake internet:** `internal/fakeinternet` with `testdata/internet/scenarios.yaml` stands up a local CA, one HTTPS listener that selects a certificate by SNI and a scenario by `Host`, a UDP DNS responder that answers from the same file (NXDOMAIN, NODATA, and A records all real RCODEs), a SOCKS5 CONNECT forwarder, and controllable clock and egress-IP endpoints. `internal/fakeinternet` tests run the **real** `probe` instruments over the real `crypto/tls` stack and classify the results: a challenge page and a plain 403 are `BLOCKED`, a 503 is `DOWN`, a 200 is `UP_REACHABILITY`, a semantic marker is `UP`, expired/mismatched/untrusted leaves are `DOWN` with the exact class, a delayed host is `http_timeout`, NODATA and NXDOMAIN are their own DNS classes, and the Tor-style SOCKS hop carries TCP and HTTPS traffic. The clock check and the egress-identity check are driven from the fake endpoints rather than from injected values.

Still specified: driving the **compiled binary** through the fake internet via its HTTP API. Strictly, it cannot be done safely as specified: the real binary resolves the fake hostnames to `127.0.0.1`, and its SSRF policy rejects loopback and private addresses by design (`THREAT_MODEL.md` T16), so the process boundary would only be reachable through a test-only egress bypass. Such a bypass is exactly the hole the policy exists to close, so the harness stops at the seam instead and covers every layer below it - dialer, TLS roots, DNS wire, SOCKS, clock, egress - with the real code.

### 5.4 E2E — real endpoints, as small as possible

Six targets. Not 1000. Runtime under ~10 seconds. Loose assertions, because the internet is not deterministic.

| Target | Probe | Expectation |
|---|---|---|
| `https://www.cloudflare.com/` | `http` | a judgeable state; a bot wall is a measurement |
| `https://www.wikipedia.org/` | `http` | a judgeable state |
| `https://github.com/` | `http` | a judgeable state |
| `https://www.chase.com/` | `http` | a judgeable state; `BLOCKED` is the honest answer |
| `https://www.united.com/` | `http` | a judgeable state; `BLOCKED` is the honest answer |
| `https://example.com/` | `http` | a judgeable state |

The set is deliberately mixed: one reference, one documentation endpoint, two
sites that commonly wall automated clients, and two services. The suite does not
assert that any of them is up; it asserts that each produced a state the
classifier understands and that at least four of six were judgeable. A separate
step asks a public resolver for a name, because a broken resolver path blinds
every hostname target at once.

Assertions are set-membership, never equality:

```go
// Wrong: the internet is not a fixture.
if state != UP { t.Fatal(...) }

// Right: we care that it did not lie.
if !state.In(UP, UP_REACHABILITY, DEGRADED, DOWN, BLOCKED) { t.Fatal(...) }
```

Rules:

- Behind `//go:build e2e`, skipped under `-short`
- **Never blocking on PRs** — a flaky network must not block a merge. It runs on a schedule and on release tags.
- One retry, then report. A second failure is worth investigating, not retrying again.
- Assertions on *behaviour*, not on third parties being healthy.

**Implemented:** `internal/e2e/real_test.go` runs six public endpoints through the real HTTP instrument and classifier, retries once, and asserts set-membership (a bot wall is a successful measurement, not a failure). It also proves the resolver path works. It is double-gated: `//go:build e2e` and `NETWEATHER_E2E=1`, and `go run ./internal/devtool e2e` is the entry point. It is not part of `devtool verify`.

The scheduled workflow exists as `.github/workflows/e2e.yml` (weekly and on demand, never on pull requests).

A second real-endpoint test covers the **Tor vantage**: `internal/e2e/tor_real_test.go` builds a `socks5h` dialer against the configured proxy (default `127.0.0.1:9050`), proves the proxy answers, and then runs one TCP probe. It is double-gated on the `e2e` tag and `NETWEATHER_TOR_E2E=1`, and skips with a message when the proxy is absent. A blocked or slow exit is logged as a normal Tor outcome; only an unclassified failure fails the test.

### 5.5 Container smoke test — the network plane

The stub E2E above runs in-process and therefore proves nothing about Docker. A separate compose-based suite runs the real image in the real topology and asserts only the things a container can break: DNS bypass to authoritative servers, MTU black-holing classified as `UNKNOWN`, Tor reachable at `tor:9050` and not at `127.0.0.1:9050`, no published 9050, SSRF refusal of `172.17.0.1` and `169.254.169.254`, read-only rootfs, and port-53 blocking producing `UNMEASURABLE` rather than `DOWN`.

**Implemented now:** the CI `container` job builds the real image and asserts the container-level invariants: `/healthz` answers, the process runs as `65532:65532`, the rootfs is read-only, and there is no shell in the image. The same sequence was run locally against the release image. `netweather doctor` additionally reports what the container can and cannot do: the ICMP check passes with an unprivileged ping socket (the shipped compose sysctl) or with `CAP_NET_RAW`, and is a warning rather than a fatal error when neither is available; traceroute remains an opt-in capability and appears as a warning when it is off.

**Implemented now, the network plane:** `internal/e2e/compose_test.go`, behind `//go:build compose`, builds `netweather:compose-test`, creates both a bridge network with a 1280-byte MTU and an `--internal` network, and runs five scenarios. It needs a Docker daemon, so `go test ./...` never touches it; `go run ./internal/devtool compose` runs it locally and the CI `compose` job runs it with a 20-minute timeout.

- **The image is the hardened release image.** The running container is `65532:65532`, its rootfs is read-only, and `docker exec … sh -c true` fails because there is no shell.
- **A low MTU is reported, not hidden.** On the 1280-byte network the `mtu` self-check fails and names 1280, and the `port53` self-check passes on that same network with working egress.
- **DNS probes the configured resolver and refuses private ones.** A DNS target pointed at `1.1.1.1:53` reaches `up_reachability` while `127.0.0.11` (Docker's embedded DNS), `172.17.0.1`, and `169.254.169.254` are `unmeasurable`: the instrument queries the configured server directly rather than falling back to the host resolver, and the SSRF policy refuses the private destinations instead of calling them `DOWN`.
- **Tor is reachable by service name, not on loopback.** A stand-in TCP listener publishes nothing, answers at `tor.nw:9050` from the app's network after a readiness wait, and refuses `127.0.0.1:9050`.
- **Port 53 blocked is unmeasurable, not down.** On the `--internal` network the `port53` self-check fails, and a DNS target in that container becomes `unmeasurable`. Because Docker Desktop does not forward host traffic into an internal network, this scenario is observed through an `alpine` container beside the app rather than a published port; that is a harness detail, not a product behaviour.

Still specified: a probe whose payload is black-holed by a low MTU asserted at the container level as `UNKNOWN`. It cannot be done inside this harness: the only endpoint a low-MTU container can reach is another container on a private address, and the SSRF policy refuses that address before any handshake — the same reason the fake-internet suite stops at the seam. The gate itself is covered by unit tests in `classify` and `agent`, and the container asserts the `mtu` self-check fact that drives it. The DNS no-fallback claim is also covered by a unit test (`probe.TestDNSNeverFallsBackToTheHostResolver`) rather than in the container, because the instrument's dial seam is where a fallback would appear. It is a different harness from the image smoke test, and the docs should not conflate them.

### 5.6 Contract tests for external feeds

IODA and Radar will change their JSON. `testdata/feeds/` holds one recorded payload per adapter — IODA, Radar, Statuspage, GCP, AWS RSS, and Azure RSS — and `internal/feeds/contract_test.go` runs three checks per feed:

- parse the shipped shape → assert the fields the product depends on (id, kind, severity, country or ASN, start and end times, and the bounded payload)
- parse a mutated shape with a required field removed → assert the event is skipped and the other events survive, with no error and no panic
- parse a truncated body → assert the feed returns an error so the poller records `stale` rather than "no outages"

Recorded, never live, in CI. The fixtures are captured shapes rather than live calls: `testdata/feeds/README.md` says how to refresh one, and a refresh that moves a field fails the test on that field's name.

**Implemented:** all six adapters above, with the mutations chosen per feed (a missing id for IODA, Statuspage, and GCP; a missing start time for Radar; an unparseable `pubDate` for the RSS pair). The truncation case also pins the error path the poller turns into a stale feed.

### 5.7 Mutation testing (optional, high value)

`gremlins` mutates the classifier and checks whether the fixture suite notices. This is the only way to prove the tests are not vacuous - a rule table with 40 fixtures can still have 20 fixtures that assert nothing.

Scope it to `internal/classify` only; it is slow and the classifier is where correctness lives. Advisory in CI, not blocking.

**Implemented:** `go run ./internal/devtool mutate` runs `gremlins` v0.6.0 (pinned in the task, so no `tool` directive is needed for something this slow) against `internal/classify` with two workers, a ten-times timeout coefficient, and a machine-readable `mutation.json`. The CI `mutate` job is `continue-on-error: true` with a 25-minute timeout and uploads the report, so a surviving mutant is a prompt to look rather than a merge block.

### 5.8 Browser UI tests

The layers above never execute JavaScript, never apply CSS, and never press Tab. A page can pass every HTTP test and still show a blank grid because a `querySelector` broke or a CSP header blocked an asset. This layer exists for that gap only.

**Tool: Playwright for Go** (`github.com/mxschmitt/playwright-go`), not Selenium.

| | Playwright | Selenium/WebDriver |
|---|---|---|
| Driver management | one bundled driver, `playwright install` | a driver binary per browser, version-matched to the installed browser |
| Waiting | auto-waiting assertions (`ToHaveAttribute`, `ToBeVisible`) with retry | explicit waits; the classic source of flakes |
| Failure evidence | screenshot, trace, video, console log capture built in | assembled by hand |
| Selectors | `data-testid`, role, accessible name | same, but no accessibility assertions |
| Multi-browser | chromium, firefox, webkit from one API | one driver each |

The deciding factor is the assertion model: `expect(locator).ToHaveAttribute("data-state", "up")` replaces `sleep(500); assert(getAttribute())`, which is how browser suites become untrustworthy.

**Opt-in by build tag.** The suite lives in `internal/web/uitest` and starts with `//go:build uitest`.

```sh
go run github.com/mxschmitt/playwright-go/cmd/playwright@v0.6401.0 install chromium
go test -tags=uitest ./internal/web/uitest -timeout 10m
```

Reasons for the tag: `go test ./...` stays under a second per package, contributors without a browser are not blocked, and the release image never gains a test dependency. CI runs the tagged suite as its own job.

What it asserts, and nothing more:

| Test | Proves |
|---|---|
| `TestLoginRejectsWrongTokenThenAccepts` | the form errors in place and still works after a failure |
| `TestBoardRendersStatesAccessibly` | one tile per target, correct `data-state`, accessible name contains the state, every state has a glyph |
| `TestLiveUpdateArrivesOverSSE` | a server-side change reaches the DOM over SSE, the label and the summary chip update, without reload |
| `TestKeyboardReachesTiles` | tiles are in the tab order |
| `TestTargetDetailNavigation` | tile → detail → back works |
| `TestMobileViewportDoesNotOverflow` | no horizontal overflow at 390 px |
| `TestAtlasMapFirstHoverAndPin` | `/atlas` renders the combined projection above every detail section, the server-rendered hover card appears on hover, click-to-pin fills the detail panel, unpin restores it, and the page has only same-origin scripts and no external refs |

It also fails on any `console.error` or uncaught page error, with one documented exception: the deliberate `401` from a failed login, which is asserted by the error box instead.

**Selector contract.** Tests address elements by `data-testid`, never by CSS class or DOM position:

| Test id | Element |
|---|---|
| `login-token`, `login-submit`, `login-error` | login form |
| `board` | grid container |
| `tile` | one target tile (`data-target`, `data-state`) |
| `count-<state>` | summary chip |
| `connection` | live/paused indicator (`data-state=live\|retrying`) |
| `target-detail`, `target-state` | detail page |
| `empty-board` | no targets configured |
| `atlas`, `atlas-projection`, `atlas-pin`, `atlas-unpin` | combined map-first page and pin panel |
| `atlas-map-country`, `atlas-map-hub`, `atlas-map-corridor`, `atlas-map-line` | markers and links sharing the hero projection |
| `atlas-group`, `atlas-coverage`, `atlas-incident`, `atlas-feed`, `atlas-ihi`, `atlas-diagnostics` | the detail sections that must stay on one page |

Renaming a test id is a deliberate act; changing a class is not. That asymmetry is the point.

**Flake policy.** No `sleep` anywhere in the suite; Playwright retries assertions, not tests. Locally: zero retries, so a flake is visible. CI: one retry of the whole job, and a retry that passes is logged as a known flake, not silently accepted. A test that needs a retry three runs in a row is quarantined or deleted, because a browser suite nobody trusts is worse than none.

**Artifacts.** On failure, a screenshot goes to `testdata/out/<TestName>.png` (gitignored), and CI uploads that directory. Traces are off by default because they are large; enable them locally with `PWDEBUG=1` or a tracing option when chasing something specific.

**Not tested here.** Classification, formatting, boundary logic, API envelopes, and anything that does not need a DOM. Those belong in §5.1–§5.3. Browser tests are slow and brittle by nature; putting logic in them is how a suite becomes a 20-minute gate people skip.

**Cross-browser.** Chromium is the per-PR gate. **Implemented:** `NETWEATHER_UI_BROWSER` selects the engine (chromium, firefox, or webkit; an unknown value fails loudly), `go run ./internal/devtool ui-all` runs the suite once per engine, and the weekly `browsers` job in `.github/workflows/e2e.yml` installs all three and runs it. A per-PR matrix would triple the gate for near-zero signal on a UI that uses no exotic APIs; a browser-specific bug earns a permanent matrix entry.

**Accessibility.** The suite already asserts accessible names and glyph presence; the plan is `axe-core` via `page.AddScriptTag` for a rules pass on board, login, and detail. Not in the first cut: axe findings need a triage policy, and an un-triaged accessibility gate gets muted.

**Cost.** A few seconds locally for the suite, ~40 s in CI including browser install. Acceptable for what it catches.

---

## 6. Testability requirements

These are design constraints, not test conveniences. They are already in `DESIGN.md`; restating them because the test strategy depends on them.

| Seam | Why |
|---|---|
| **`Dialer` interface** (`DESIGN.md` §5.6) | Every probe goes through it. Tests inject the fake internet; production injects direct or SOCKS5. |
| **`Clock` interface** | Scheduler, jitter, baselines, and retention must be deterministic. `time.Now()` is banned outside `internal/clock`. |
| **`Recorder`** | `--record` captures live responses into fixtures, so every production surprise becomes a regression test. |
| **No globals** | `gochecknoglobals` keeps state injectable. |
| **`TestMain` network guard** | Any test that dials a real address fails loudly. Prevents accidental network dependence from creeping in. |

That last one is worth writing explicitly:

```go
func TestMain(m *testing.M) {
    net.DefaultResolver.PreferGo = true
    defaultDialer = dialerFunc(func(ctx context.Context, n, a string) (net.Conn, error) {
        panic("test attempted a real network connection to " + a)
    })
    os.Exit(m.Run())
}
```

---

## 7. Coverage policy

100% coverage is a bad target; it rewards assertion-free tests. Instead:

| Scope | Gate |
|---|---|
| `internal/classify`, `internal/baseline`, `internal/confirm`, `internal/dnsdiv`, `internal/ihi`, `internal/netx`, `internal/verdict` | **95%** |
| `internal/anomaly`, `internal/api`, `internal/board`, `internal/config`, `internal/correlate`, `internal/corridor`, `internal/metrics`, `internal/probe`, `internal/state`, `internal/streamlimit`, `internal/bytecap` | **90%** |
| `internal/agent`, `internal/doctor`, `internal/enrich`, `internal/feeds`, `internal/incidents`, `internal/schedule`, `internal/seed`, `internal/selfcheck`, `internal/tracebudget` | **85%** |
| `internal/logging`, `internal/store` | **80%** |
| `internal/fakeinternet`, `internal/startup` | **75%** |
| `internal/web` | **72%** |
| `cmd/netweather` | **30%** (wire-up; the container smoke test owns it) |
| `internal/devtool` | **15%** (profile parser and ldflags tests) |
| `internal/version` | **70%** |
| overall | reported, not gated |

The floors live in `internal/devtool/main.go`, which is the thing that enforces them; this table is the readable copy, and the two are checked together when either changes.

Always `-race -covermode=atomic`. Coverage is uploaded per-PR and diffed, so a PR that drops `classify` coverage is visible even if the global number stays flat.

---

## 8. CI pipeline

| Stage | Runs | Blocking |
|---|---|---|
| format | `devtool fmt -check` (gofumpt + gci) | ✅ |
| vet | `go vet ./...` | ✅ |
| lint | `go tool golangci-lint run` (pinned in `go.mod`) | ✅ |
| modules | `go mod verify` + `go mod tidy -diff` (no uncommitted tidy) | ✅ |
| test | `devtool test` (`-race`, `-shuffle=on`, `-count=1` for library packages; normal shuffled/count=1 for `internal/web`) | ✅ |
| coverage | `devtool cover`: profile + the per-package floors in §7 (the same numbers `internal/devtool/main.go` enforces) | ✅ |
| ui test (browser) | Playwright + chromium, `-tags=uitest` | ✅ (every push and PR) |
| fuzz (short) | `go test -run=^$ -fuzz=FuzzScrubStringIsIdempotent -fuzztime=20s` | ✅ |
| mutation (advisory) | `devtool mutate`: gremlins on `internal/classify`, `mutation.json` uploaded | advisory; job-level `continue-on-error` |
| vuln | `go tool govulncheck ./...` (pinned in `go.mod`) | ✅ |
| secrets | `gitleaks` over full history, pinned in the workflow | ✅ |
| build | `devtool build` (static, `-trimpath`, version stamped via ldflags) | ✅ |
| reproducibility | `devtool repro`: two builds of the same revision, identical SHA-256 | ✅ |
| release | `devtool snapshot`: five platforms + `SHA256SUMS`, tags only | ✅ (tags) |
| image scan | `grype` on the release image; `hadolint` on both Dockerfiles (`.hadolint.yaml` explains its two ignores) | ✅ |
| sbom + sign | `syft`, `cosign` | ✅ |
| integration | fake internet | ✅ |
| container smoke | compose topology, 7 network tests | ✅ (every push and PR) |
| e2e real | 6 targets | ❌ scheduled + release only |
| mutation | `gremlins` on `classify` | ❌ advisory |

Pre-commit hook runs format + vet + test. Install it once per clone with `git config core.hooksPath .githooks`; the hook itself is `.githooks/pre-commit`. Everything else is CI. Go tool versions live in `go.mod`; external scanners (gitleaks, hadolint, Playwright) are pinned to an explicit version in the workflow, never a floating tag.

For the same external lint surface locally:

```sh
go run ./internal/devtool lint-all
```

It runs pinned `golangci-lint`, Hadolint for both Dockerfiles, and full-history
Gitleaks using `.gitleaks.toml`. Docker is required for the two Hadolint
checks. It is separate from `verify` because Docker is intentionally optional
for the Go development loop.

---

## 9. Deliberately not doing

- **No style linters that fight contributors** (`wsl`, `lll`, `varnamelen`). Bug-catching rules only.
- **No coverage badge as a quality claim.** A high number with weak assertions is worse than a lower honest one.
- **No live third-party calls in CI.** Feeds are recorded; E2E-real is separate and non-blocking.
- **No mocking our own packages.** If a package needs a mock, its seam is wrong — fix the seam.
- **No `-short` in CI for the integration layer.** The fake internet is fast; skipping it hides the bugs that matter.
