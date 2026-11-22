package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/agent"
	"github.com/Greg-2600/netweather/internal/bytecap"
	"github.com/Greg-2600/netweather/internal/config"
	"github.com/Greg-2600/netweather/internal/feeds"
	"github.com/Greg-2600/netweather/internal/logging"
	"github.com/Greg-2600/netweather/internal/probe"
	"github.com/Greg-2600/netweather/internal/seed"
	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
)

func TestRunVersion(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	if err := run([]string{"version"}, &out); err != nil {
		t.Fatalf("run(version) = %v, want nil", err)
	}
	if got := out.String(); !strings.Contains(got, "netweather") {
		t.Fatalf("version output %q does not identify the binary", got)
	}
}

func TestRunHealthcheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("NETWEATHER_HEALTHCHECK_URL", srv.URL+"/healthz")

	if err := run([]string{"healthcheck"}, io.Discard); err != nil {
		t.Fatalf("run(healthcheck) = %v, want nil", err)
	}
}

func TestRunVersionJSON(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	if err := run([]string{"version", "--json"}, &out); err != nil {
		t.Fatalf("run(version --json) = %v", err)
	}

	var env struct {
		Data struct {
			Version string `json:"version"`
			Commit  string `json:"commit"`
			Built   string `json:"built"`
		} `json:"data"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("output is not JSON: %v: %s", err, out.String())
	}
	if env.Error != nil {
		t.Fatalf("envelope carried an error: %s", out.String())
	}
	if env.Data.Version == "" || env.Data.Commit == "" || env.Data.Built == "" {
		t.Fatalf("envelope = %+v", env.Data)
	}
}

func TestRunHealthcheckJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("NETWEATHER_HEALTHCHECK_URL", srv.URL+"/healthz")

	var out bytes.Buffer
	if err := run([]string{"healthcheck", "--json"}, &out); err != nil {
		t.Fatalf("run(healthcheck --json) = %v", err)
	}
	var ok struct {
		Data struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &ok); err != nil {
		t.Fatalf("output is not JSON: %v: %s", err, out.String())
	}
	if ok.Data.Status != "ok" {
		t.Fatalf("status = %q", ok.Data.Status)
	}

	// A failed probe still writes the error envelope to stdout, because stdout
	// is data; the exit code and the stderr line are unchanged.
	t.Setenv("NETWEATHER_HEALTHCHECK_URL", "http://127.0.0.1:1/healthz")
	out.Reset()
	if err := run([]string{"healthcheck", "--json"}, &out); err == nil {
		t.Fatal("a failed healthcheck returned nil")
	}
	var bad struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &bad); err != nil {
		t.Fatalf("failure output is not JSON: %v: %s", err, out.String())
	}
	if bad.Error.Code != "unhealthy" || bad.Error.Message == "" {
		t.Fatalf("error envelope = %+v", bad.Error)
	}
}

func TestVersionRejectsAnUnknownFlag(t *testing.T) {
	t.Parallel()

	if err := run([]string{"version", "--nope"}, io.Discard); err == nil {
		t.Fatal("an unknown flag was accepted")
	}
	if err := run([]string{"version", "extra"}, io.Discard); err == nil {
		t.Fatal("a positional argument was accepted")
	}
}

func TestRunUnknownCommandIsAnError(t *testing.T) {
	t.Parallel()

	err := run([]string{"definitely-not-a-command"}, io.Discard)
	if err == nil {
		t.Fatal("run(unknown) = nil, want an error")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("error %q does not name the problem", err)
	}
	// main turns this sentinel into exit 2, the documented usage code.
	if !errors.Is(err, errUnknownCommand) {
		t.Fatalf("error %q is not marked as a usage error", err)
	}
}

func TestRunServeRefusesPublicBindWithoutToken(t *testing.T) {
	// Not parallel: it mutates the process environment.
	t.Setenv("NETWEATHER_UI_LISTEN", "0.0.0.0:2600")
	t.Setenv("NETWEATHER_UI_TOKEN", "")
	t.Setenv("NETWEATHER_UI_TOKEN_FILE", "")
	t.Setenv("NETWEATHER_UI_ALLOW_PUBLIC", "")

	err := run([]string{"serve"}, io.Discard)
	if err == nil {
		t.Fatal("a public bind without a token must refuse to start")
	}
	if !strings.Contains(err.Error(), "without auth") {
		t.Fatalf("error %q does not explain the refusal", err)
	}
}

func TestRunServeRejectsBadListenAddress(t *testing.T) {
	t.Setenv("NETWEATHER_UI_LISTEN", "not-an-address")
	t.Setenv("NETWEATHER_UI_ALLOW_PUBLIC", "true")

	if err := run([]string{"serve"}, io.Discard); err == nil {
		t.Fatal("an unparseable listen address must fail before binding")
	}
}

func TestSeedTargetsDisabled(t *testing.T) {
	t.Setenv("NETWEATHER_SEED", "none")

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "seed.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	got, err := seedTargets(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("targets = %v, want none", got)
	}
}

func TestSeedTargetsSeedsTheDefaultList(t *testing.T) {
	t.Setenv("NETWEATHER_SEED", "")

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "seed.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	want := len(seed.Default())
	if want == 0 {
		t.Fatal("the default seed list is empty")
	}

	got, err := seedTargets(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != want {
		t.Fatalf("agent targets = %d, want %d", len(got), want)
	}
	for _, tg := range got {
		if tg.Interval <= 0 || tg.URL == "" || tg.Vantage != "local" {
			t.Errorf("incomplete agent target: %+v", tg)
		}
	}

	rows, err := st.TargetsWithState(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != want {
		t.Fatalf("stored targets = %d, want %d", len(rows), want)
	}
	for _, r := range rows {
		if r.State != state.Unknown {
			t.Errorf("%s state = %s before any probe, want unknown", r.ID, r.State)
		}
	}
}

func TestTargetURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		port int
		path string
		want string
	}{
		{443, "", "https://example.com:443/"},
		{80, "", "http://example.com:80/"},
		{443, "/robots.txt", "https://example.com:443/robots.txt"},
		{80, "/health", "http://example.com:80/health"},
	}
	for _, tc := range tests {
		if got := targetURL("example.com", tc.port, tc.path); got != tc.want {
			t.Errorf("targetURL(%d, %q) = %q, want %q", tc.port, tc.path, got, tc.want)
		}
	}
}

func TestLoadTargetsFromConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "targets.yaml")
	yaml := `version: 1
targets:
  - name: Example
    host: example.com
    port: 443
    probe: http
    class: reference
    tier: 0
    expect:
      status: [200]
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NETWEATHER_CONFIG", path)

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "load.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	got, err := loadTargets(ctx, st)
	if err != nil {
		t.Fatalf("loadTargets: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("targets = %d", len(got))
	}
	if got[0].URL != "https://example.com:443/" {
		t.Errorf("URL = %q", got[0].URL)
	}
	if len(got[0].Expect.Status) != 1 || got[0].Expect.Status[0] != 200 {
		t.Errorf("expectation not carried: %+v", got[0].Expect)
	}

	rows, err := st.TargetsWithState(ctx, "local")
	if err != nil || len(rows) != 1 {
		t.Fatalf("stored rows = %d, err = %v", len(rows), err)
	}
}

func TestLoadTargetsAcceptsADNSAnchor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dns.yaml")
	yaml := `version: 1
targets:
  - name: Root
    host: 198.41.0.4
    port: 53
    probe: dns
    qname: example.com
    class: network
    tier: 3
    interval_s: 120
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NETWEATHER_CONFIG", path)

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "dns.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	got, err := loadTargets(ctx, st)
	if err != nil {
		t.Fatalf("loadTargets: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("targets = %d", len(got))
	}
	target := got[0]
	if target.Instrument != "dns" || target.Address != "198.41.0.4:53" || target.Query != "example.com" {
		t.Fatalf("dns target = %+v", target)
	}
	if target.URL != "" {
		t.Fatalf("dns target should not carry an HTTP URL: %q", target.URL)
	}
}

func TestLoadTargetsAcceptsICMPAnchor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "icmp.yaml")
	yaml := `version: 1
targets:
  - name: Gateway
    host: 192.0.2.1
    port: 0
    probe: icmp
    class: network
    tier: 3
    interval_s: 120
    icmp_anchor: true
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NETWEATHER_CONFIG", path)

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "icmp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	got, err := loadTargets(ctx, st)
	if err != nil {
		t.Fatalf("loadTargets: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("targets = %d", len(got))
	}
	target := got[0]
	if target.Instrument != "icmp" || target.Address != "192.0.2.1" || target.URL != "" {
		t.Fatalf("icmp target = %+v", target)
	}
	if !target.ICMPAnchor || !target.Expect.ICMPAnchor {
		t.Fatalf("icmp_anchor was not carried into the expectation: %+v", target)
	}
}

func TestLoadTargetsAcceptsNTP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ntp.yaml")
	yaml := `version: 1
targets:
  - name: Cloudflare time
    host: time.cloudflare.com
    port: 123
    probe: ntp
    class: reference
    tier: 0
    interval_s: 600
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NETWEATHER_CONFIG", path)

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "ntp-ok.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	got, err := loadTargets(ctx, st)
	if err != nil {
		t.Fatalf("loadTargets: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("targets = %d", len(got))
	}
	if got[0].Instrument != "ntp" || got[0].Address != "time.cloudflare.com:123" {
		t.Fatalf("ntp target = %+v", got[0])
	}
}

func TestSelectTorTargets(t *testing.T) {
	t.Parallel()

	base := []agent.Target{
		{ID: "dns", Instrument: "dns", Interval: time.Minute, Vantage: "local"},
		{ID: "c", Instrument: "http", Interval: time.Minute, Vantage: "local"},
		{ID: "a", Instrument: "tls", Interval: 30 * time.Minute, Vantage: "local"},
		{ID: "b", Instrument: "tcp", Interval: 5 * time.Minute, Vantage: "local"},
	}

	got := selectTorTargets(base, 2)
	if len(got) != 2 {
		t.Fatalf("selected = %d, want the cap of 2", len(got))
	}
	// Sorted by id, so "a" then "b": deterministic across restarts.
	if got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("order = %s,%s", got[0].ID, got[1].ID)
	}
	for _, tg := range got {
		if tg.Vantage != "tor" {
			t.Errorf("%s vantage = %q", tg.ID, tg.Vantage)
		}
		if tg.Interval < minTorInterval {
			t.Errorf("%s interval = %v, want the Tor floor", tg.ID, tg.Interval)
		}
	}
	// A long interval is preserved, not shortened.
	if got[0].Interval != 30*time.Minute {
		t.Errorf("a interval = %v, want it preserved", got[0].Interval)
	}

	// A zero cap falls back to the default cap rather than selecting nothing.
	if n := len(selectTorTargets(base, 0)); n != 3 {
		t.Fatalf("default cap selected %d, want the three tcp-based targets", n)
	}

	// Non-TCP instruments are never selected.
	for _, tg := range selectTorTargets(base, 10) {
		if tg.Instrument == "dns" {
			t.Fatal("dns target selected for Tor; DNS does not run over this dialer")
		}
	}
}

func TestNewTorAgentIsOffByDefault(t *testing.T) {
	t.Setenv("NETWEATHER_TOR", "")

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "tor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	lg := &logging.Logger{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ag, _, err := newTorAgent(ctx, st, nil, lg, "netweather/test")
	if err != nil {
		t.Fatalf("disabled tor: %v", err)
	}
	if ag != nil {
		t.Fatal("tor vantage was built without being enabled")
	}
}

func TestNewTorAgentRejectsBadProxy(t *testing.T) {
	t.Setenv("NETWEATHER_TOR", "true")
	t.Setenv("NETWEATHER_TOR_SOCKS", "not-a-proxy")

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "tor-bad.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	lg := &logging.Logger{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if _, _, err := newTorAgent(ctx, st, nil, lg, "netweather/test"); err == nil {
		t.Fatal("an invalid SOCKS address must fail loudly")
	}
}

func TestNewTorAgentBuildsOne(t *testing.T) {
	t.Setenv("NETWEATHER_TOR", "true")
	t.Setenv("NETWEATHER_TOR_SOCKS", "127.0.0.1:9050")
	t.Setenv("NETWEATHER_TOR_MAX", "2")

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "tor-on.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	direct := []agent.Target{
		{ID: "a", Instrument: "http", Interval: time.Minute, Vantage: "local"},
		{ID: "b", Instrument: "tls", Interval: time.Minute, Vantage: "local"},
		{ID: "c", Instrument: "tcp", Interval: time.Minute, Vantage: "local"},
	}

	lg := &logging.Logger{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ag, _, err := newTorAgent(ctx, st, direct, lg, "netweather/test")
	if err != nil {
		t.Fatalf("newTorAgent: %v", err)
	}
	if ag == nil {
		t.Fatal("expected a Tor agent")
	}
	if ag.TargetCount() != 2 {
		t.Fatalf("targets = %d, want the cap", ag.TargetCount())
	}
	if ag.Pace != torPace {
		t.Fatalf("pace = %v, want %v", ag.Pace, torPace)
	}

	states, err := st.FeedStates(ctx) // unrelated table, but proves the store is usable
	if err != nil {
		t.Fatal(err)
	}
	_ = states
}

func TestParseTorSpecsMultipleContinents(t *testing.T) {
	t.Setenv("NETWEATHER_TOR_VANTAGES",
		"DE=tor-de:9050, JP = tor-jp:9050 ,AU=tor-au:9050,SG=tor-sg:9050,CL=tor-cl:9050,MX=tor-mx:9050")

	specs, err := parseTorSpecs()
	if err != nil {
		t.Fatalf("parseTorSpecs: %v", err)
	}
	want := []torSpec{
		{Country: "DE", Socks: "tor-de:9050", Control: "tor-de:9051", ID: "tor-de"},
		{Country: "JP", Socks: "tor-jp:9050", Control: "tor-jp:9051", ID: "tor-jp"},
		{Country: "AU", Socks: "tor-au:9050", Control: "tor-au:9051", ID: "tor-au"},
		{Country: "SG", Socks: "tor-sg:9050", Control: "tor-sg:9051", ID: "tor-sg"},
		{Country: "CL", Socks: "tor-cl:9050", Control: "tor-cl:9051", ID: "tor-cl"},
		{Country: "MX", Socks: "tor-mx:9050", Control: "tor-mx:9051", ID: "tor-mx"},
	}
	if len(specs) != len(want) {
		t.Fatalf("specs = %d, want %d", len(specs), len(want))
	}
	for i := range want {
		if specs[i] != want[i] {
			t.Errorf("spec %d = %+v, want %+v", i, specs[i], want[i])
		}
	}
}

func TestParseTorSpecsRejectsBadEntries(t *testing.T) {
	for _, raw := range []string{
		"DE", "de=tor:9050", "USA=tor:9050", "DE=", "DE=tor:9050,DE=tor2:9050", ",",
	} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("NETWEATHER_TOR_VANTAGES", raw)
			if _, err := parseTorSpecs(); err == nil {
				t.Fatalf("accepted %q", raw)
			}
		})
	}
}

func TestCombinedBudgetWaitsForEveryExit(t *testing.T) {
	spent := bytecap.New(100)
	spent.Add(100)
	fresh := bytecap.New(100)
	b := combinedBudget{spent, fresh}
	if b.Used() != 100 || b.Allowance() != 200 {
		t.Fatalf("used=%d allowance=%d", b.Used(), b.Allowance())
	}
	if b.Exhausted() {
		t.Fatal("one exhausted exit must not pause the other vantages")
	}
	if !(combinedBudget{spent}).Exhausted() {
		t.Fatal("all exits exhausted must report exhausted")
	}
}

func TestNewTorAgentRequiresTcpTargets(t *testing.T) {
	t.Setenv("NETWEATHER_TOR", "true")
	t.Setenv("NETWEATHER_TOR_SOCKS", "127.0.0.1:9050")

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "tor-none.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	lg := &logging.Logger{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, _, err = newTorAgent(ctx, st, []agent.Target{{ID: "dns", Instrument: "dns"}}, lg, "netweather/test")
	if err == nil {
		t.Fatal("a Tor vantage with only DNS targets should fail, not run empty")
	}
}

func TestReloadTargetsDisablesRemoved(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	cfg := filepath.Join(dir, "targets.yaml")

	write := func(yaml string) {
		t.Helper()
		if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write(`version: 1
targets:
  - name: A
    host: a.example
    port: 443
    probe: http
    class: service
  - name: B
    host: b.example
    port: 443
    probe: http
    class: service
`)
	t.Setenv("NETWEATHER_CONFIG", cfg)
	t.Setenv("NETWEATHER_DATA", dataDir)

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dataDir, "netweather.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ag := agent.New(nil, agent.Instruments{}, st)

	if err := reloadTargets(ctx, st, ag, logger); err != nil {
		t.Fatalf("initial reload: %v", err)
	}
	if ag.TargetCount() != 2 {
		t.Fatalf("targets = %d, want 2", ag.TargetCount())
	}

	// Remove A from the file.
	write(`version: 1
targets:
  - name: B
    host: b.example
    port: 443
    probe: http
    class: service
`)
	if err := reloadTargets(ctx, st, ag, logger); err != nil {
		t.Fatalf("second reload: %v", err)
	}
	if ag.TargetCount() != 1 {
		t.Fatalf("targets = %d, want 1", ag.TargetCount())
	}

	rows, err := st.TargetsWithState(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Host != "b.example" {
		t.Fatalf("stored board = %+v", rows)
	}
	if exists, _ := st.TargetExists(ctx, "a-example-http"); exists {
		t.Fatal("removed target is still enabled")
	}

	// An invalid edit is rejected without touching the live set.
	write("version: 1\ntargets:\n  - name: x\n    host: x.com\n    port: 22\n    probe: tcp\n    class: service\n")
	if err := reloadTargets(ctx, st, ag, logger); err == nil {
		t.Fatal("invalid config should be rejected")
	}
	if ag.TargetCount() != 1 {
		t.Fatal("a rejected reload changed the live target set")
	}
}

func TestSeedCommand(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "targets.yaml")
	yaml := `version: 1
targets:
  - name: Example
    host: example.com
    port: 443
    probe: http
    class: reference
    tier: 0
`
	if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NETWEATHER_CONFIG", cfg)
	t.Setenv("NETWEATHER_DATA", filepath.Join(dir, "data"))

	var out bytes.Buffer
	if err := run([]string{"seed"}, &out); err != nil {
		t.Fatalf("run(seed): %v", err)
	}
	if !strings.Contains(out.String(), "seeded 1 targets") {
		t.Fatalf("output = %q", out.String())
	}

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "data", "netweather.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	rows, err := st.TargetsWithState(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Host != "example.com" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestSeedCommandRequiresConfig(t *testing.T) {
	t.Setenv("NETWEATHER_CONFIG", "")
	t.Setenv("NETWEATHER_DATA", t.TempDir())

	if err := run([]string{"seed"}, io.Discard); err == nil {
		t.Fatal("seed without NETWEATHER_CONFIG should fail")
	}
}

func TestSeedCommandRejectsBadConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "targets.yaml")
	if err := os.WriteFile(cfg, []byte("version: 1\ntargets:\n  - name: x\n    host: x.com\n    port: 22\n    probe: tcp\n    class: service\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NETWEATHER_CONFIG", cfg)
	t.Setenv("NETWEATHER_DATA", filepath.Join(dir, "data"))

	if err := run([]string{"seed"}, io.Discard); err == nil {
		t.Fatal("seed should reject an invalid target file")
	}
}

func seedForExport(t *testing.T, dataDir string, base time.Time) {
	t.Helper()

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dataDir, "netweather.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	if err := st.EnsureVantage(ctx, "local", "direct", "primary"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertTarget(ctx, store.Target{
		ID: "t1", Host: "chase.com", Port: 443, Probe: "http",
		Name: "Chase", Class: "service", Tier: 2, IntervalS: 300,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Record(ctx, []store.Probe{{
		TargetID: "t1", VantageID: "local", TS: base,
		State: state.Up, Stage: "http", Confidence: "high",
	}}); err != nil {
		t.Fatal(err)
	}
}

func TestExportCommandCSV(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	t.Setenv("NETWEATHER_DATA", dataDir)

	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	seedForExport(t, dataDir, base)

	var out bytes.Buffer
	err := run([]string{
		"export",
		"--from", base.Add(-time.Minute).Format(time.RFC3339),
		"--to", base.Add(time.Minute).Format(time.RFC3339),
	}, &out)
	if err != nil {
		t.Fatalf("run(export): %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want header + one row:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], "target_id") || !strings.Contains(lines[1], "chase.com") {
		t.Fatalf("csv = %q", out.String())
	}
}

func TestExportCommandJSONL(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	t.Setenv("NETWEATHER_DATA", dataDir)

	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	seedForExport(t, dataDir, base)

	var out bytes.Buffer
	err := run([]string{
		"export", "--format", "jsonl",
		"--from", base.Add(-time.Minute).Format(time.RFC3339),
		"--to", base.Add(time.Minute).Format(time.RFC3339),
	}, &out)
	if err != nil {
		t.Fatalf("run(export): %v", err)
	}

	line := strings.TrimSpace(out.String())
	var row map[string]any
	if err := json.Unmarshal([]byte(line), &row); err != nil {
		t.Fatalf("jsonl is not one object: %v\n%s", err, line)
	}
	if row["host"] != "chase.com" || row["state"] != "up" {
		t.Fatalf("row = %v", row)
	}
}

func TestExportCommandRejectsBadRange(t *testing.T) {
	t.Setenv("NETWEATHER_DATA", t.TempDir())

	err := run([]string{
		"export",
		"--from", "2026-11-17T13:00:00Z",
		"--to", "2026-11-17T12:00:00Z",
	}, io.Discard)
	if err == nil {
		t.Fatal("a reversed range must be rejected")
	}
}

func TestExportCommandRejectsBadFormat(t *testing.T) {
	t.Setenv("NETWEATHER_DATA", t.TempDir())

	if err := run([]string{"export", "--format", "xml"}, io.Discard); err == nil {
		t.Fatal("an unknown format must be rejected")
	}
}

func TestHealthcheckFailsWithoutAServer(t *testing.T) {
	t.Setenv("NETWEATHER_HEALTHCHECK_URL", "http://127.0.0.1:1/healthz")

	if err := healthcheck(); err == nil {
		t.Fatal("healthcheck should fail when nothing is listening")
	}
}

func TestHealthcheckChecksTheStatus(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))

		t.Setenv("NETWEATHER_HEALTHCHECK_URL", srv.URL+"/healthz")
		err := healthcheck()
		srv.Close()

		if status == http.StatusOK && err != nil {
			t.Errorf("200 should pass: %v", err)
		}
		if status != http.StatusOK && err == nil {
			t.Errorf("%d should fail", status)
		}
	}
}

func TestHealthcheckDerivesTheURLFromListen(t *testing.T) {
	t.Setenv("NETWEATHER_HEALTHCHECK_URL", "")
	t.Setenv("NETWEATHER_UI_LISTEN", "not-an-address")

	if err := healthcheck(); err == nil {
		t.Fatal("an unparseable listen address should fail")
	}
}

func TestHealthcheckRefusesNonLoopback(t *testing.T) {
	t.Setenv("NETWEATHER_HEALTHCHECK_URL", "http://example.com/healthz")

	if err := healthcheck(); err == nil {
		t.Fatal("a non-loopback healthcheck URL must be refused")
	}
}

func TestLoadTargetsAcceptsTCPPing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tcpping.yaml")
	yaml := `version: 1
targets:
  - name: Portable RTT
    host: example.com
    port: 443
    probe: tcpping
    class: network
    tier: 3
    interval_s: 120
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NETWEATHER_CONFIG", path)

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "tcpping.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	got, err := loadTargets(ctx, st)
	if err != nil {
		t.Fatalf("loadTargets: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("targets = %d", len(got))
	}
	if got[0].Instrument != "tcpping" || got[0].Address != "example.com:443" {
		t.Fatalf("tcpping target = %+v", got[0])
	}
}

// pinTransport answers the exit trace request with a fixed body so the pin
// decision is testable without a proxy or a network.
type pinTransport struct {
	status int
	body   string
}

func (p pinTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	status := p.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(p.body)),
		Request:    req,
	}, nil
}

func TestTorGeoIPCountry(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "GETINFO ip-to-country/") {
				_, _ = io.WriteString(conn, "250-ip-to-country/203.0.113.9=de\r\n250 OK\r\n")
				return
			}
		}
	}()

	if got := torGeoIPCountry(context.Background(), ln.Addr().String(), "203.0.113.9"); got != "DE" {
		t.Fatalf("country = %q, want DE", got)
	}
	if got := torGeoIPCountry(context.Background(), "", "203.0.113.9"); got != "" {
		t.Fatalf("empty control returned %q", got)
	}
}

func TestTorExitCountry(t *testing.T) {
	t.Parallel()

	usLookup := func(context.Context, netip.Addr) (int, string) { return 64500, "US" }
	usBody := "ip=203.0.113.9\n"

	// A verified exit returns the recorded identity tuple.
	got, err := torExitCountry(context.Background(),
		probe.NewHTTPWithTransport(pinTransport{body: usBody}), usLookup, "us")
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if got != "203.0.113.9,AS64500,US" {
		t.Fatalf("value = %q", got)
	}

	// A country label that does not match the exit is refused, and the error
	// names both, because that is what the operator has to fix.
	nlLookup := func(context.Context, netip.Addr) (int, string) { return 64500, "NL" }
	_, err = torExitCountry(context.Background(),
		probe.NewHTTPWithTransport(pinTransport{body: usBody}), nlLookup, "US")
	if err == nil || !strings.Contains(err.Error(), "NL") || !strings.Contains(err.Error(), "US") {
		t.Fatalf("mismatch error = %v", err)
	}

	// An exit whose country cannot be learned is not labelled either.
	unknownLookup := func(context.Context, netip.Addr) (int, string) { return 0, "" }
	_, err = torExitCountry(context.Background(),
		probe.NewHTTPWithTransport(pinTransport{body: usBody}), unknownLookup, "US")
	if err == nil || !strings.Contains(err.Error(), "could not be learned") {
		t.Fatalf("unknown country error = %v", err)
	}

	// A malformed label is caught before any request.
	if _, err := torExitCountry(context.Background(),
		probe.NewHTTPWithTransport(pinTransport{body: usBody}), usLookup, "USA"); err == nil {
		t.Fatal("a three-letter code was accepted")
	}

	// A failed trace is not a verification.
	_, err = torExitCountry(context.Background(),
		probe.NewHTTPWithTransport(pinTransport{status: 502, body: usBody}), usLookup, "US")
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("trace failure error = %v", err)
	}
}

func TestTorExitCountryUsesGeoIPWhenRegistrarHasNoASN(t *testing.T) {
	t.Parallel()

	lookup := func(context.Context, netip.Addr) (int, string) { return 0, "DE" }
	got, err := torExitCountry(context.Background(),
		probe.NewHTTPWithTransport(pinTransport{body: "ip=203.0.113.9\n"}), lookup, "DE")
	if err != nil {
		t.Fatalf("country-only lookup failed: %v", err)
	}
	if got != "203.0.113.9,,DE" {
		t.Fatalf("value = %q", got)
	}
}

func TestVerifyTorExitDoesNotRetryMismatch(t *testing.T) {
	t.Parallel()

	calls := 0
	lookup := func(context.Context, netip.Addr) (int, string) {
		calls++
		return 64500, "NL"
	}
	_, err := verifyTorExitWithRetry(context.Background(),
		probe.NewHTTPWithTransport(pinTransport{body: "ip=203.0.113.9\n"}), lookup, "US")
	if err == nil {
		t.Fatal("mismatch was accepted")
	}
	if calls != 1 {
		t.Fatalf("mismatch retried %d times", calls)
	}
}

func TestFormatTraceHops(t *testing.T) {
	t.Parallel()

	res := probe.TraceResult{
		Hops: []probe.TraceHop{
			{TTL: 1, Addr: netip.MustParseAddr("192.0.2.1"), RTT: 4 * time.Millisecond},
			{TTL: 2}, // a silent hop is a star, not a loss claim
			{TTL: 3, Addr: netip.MustParseAddr("198.51.100.9"), RTT: 22 * time.Millisecond},
		},
		Destination: netip.MustParseAddr("198.51.100.9"),
		Reached:     true,
	}
	got := formatTraceHops(res)
	for _, want := range []string{
		" 1  192.0.2.1", "4ms",
		" 2  *",
		" 3  198.51.100.9", "22ms",
		"destination 198.51.100.9 reached=true",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("trace missing %q:\n%s", want, got)
		}
	}

	// A walk that never resolved a destination still renders, rather than
	// printing an invalid address.
	res = probe.TraceResult{Hops: []probe.TraceHop{{TTL: 1}}}
	got = formatTraceHops(res)
	if !strings.Contains(got, "destination unresolved reached=false") {
		t.Fatalf("unresolved trace = %q", got)
	}
}

func TestDirectDailyMB(t *testing.T) {
	cases := []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{"", 2048, false},
		{"0", 0, false},
		{"512", 512, false},
		{"-1", 0, true},
		{"many", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv("NETWEATHER_DAILY_MB", tc.raw)

			got, err := directDailyMB()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%q was accepted as %d", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("%q = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

// A disabled budget must be absent, not a zero-limit budget that stops on the
// first byte.
func TestNewDirectBudgetDisabled(t *testing.T) {
	t.Setenv("NETWEATHER_DAILY_MB", "0")

	var out bytes.Buffer
	lg, err := logging.New(logging.DefaultConfig(), &out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lg.Close() }()

	budget, err := newDirectBudget(lg)
	if err != nil {
		t.Fatal(err)
	}
	if budget != nil {
		t.Fatalf("budget = %+v, want nil when disabled", budget)
	}

	t.Setenv("NETWEATHER_DAILY_MB", "2048")
	budget, err = newDirectBudget(lg)
	if err != nil {
		t.Fatal(err)
	}
	if budget == nil || budget.Allowance() != 2048<<20 {
		t.Fatalf("budget = %+v, want 2 GB", budget)
	}
}

func TestTorSocksCheck(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()

	detail, err := torSocksCheck(ln.Addr().String())(context.Background())
	if err != nil {
		t.Fatalf("a listening proxy reported %v", err)
	}
	if !strings.Contains(detail, "answered") {
		t.Fatalf("detail = %q", detail)
	}

	// A closed proxy is a warning for the doctor, not a panic: the check
	// reports an error and the runner decides.
	addr := ln.Addr().String()
	_ = ln.Close()
	if _, err := torSocksCheck(addr)(context.Background()); err == nil {
		t.Fatal("a closed proxy was accepted")
	}
}

func TestFormatFleet(t *testing.T) {
	t.Parallel()

	got := formatFleet(store.FleetStats{
		ByClass:            map[string]int{"service": 6, "country": 2},
		Total:              8,
		EffectivePerSecond: 0.5,
		Blocked:            2,
		Unmeasurable:       1,
		ASNCount:           3,
		CountryCount:       4,
		TopASN:             13335,
		TopASNName:         "Cloudflare, Inc.",
		TopASNCount:        4,
		TopASNShare:        0.5,
	})
	for _, want := range []string{
		"fleet: 8 target(s)",
		"(country 2, service 6)",
		"effective rate 0.5 probes/s, blocked 25.0%, unmeasurable 1",
		"ASN coverage 3, countries 4",
		"top ASN AS13335 (Cloudflare, Inc.) 50.0%",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("fleet block missing %q:\n%s", want, got)
		}
	}

	// An empty fleet is a zero answer, not a missing top ASN line.
	empty := formatFleet(store.FleetStats{})
	if !strings.Contains(empty, "fleet: 0 target(s)") {
		t.Fatalf("empty fleet = %q", empty)
	}
	if strings.Contains(empty, "top ASN") {
		t.Fatalf("empty fleet invented a top ASN: %q", empty)
	}
}

func TestConfigStampSeesANestedConfigEdit(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "sites", "financial")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	targets := filepath.Join(nested, "banking.txt")
	if err := os.WriteFile(targets, []byte("chase.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	first, err := configStamp(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Editing a file under the tree must move the stamp even though the root
	// directory's own mtime does not change. The explicit timestamp keeps the
	// test independent of filesystem mtime resolution.
	if err := os.WriteFile(targets, []byte("chase.com\nunited.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(targets, future, future); err != nil {
		t.Fatal(err)
	}

	second, err := configStamp(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("a nested config edit did not change the stamp")
	}

	// A file path stamps its own mtime and size.
	fileStamp, err := configStamp(targets)
	if err != nil {
		t.Fatal(err)
	}
	if fileStamp == "" || fileStamp == first {
		t.Fatalf("file stamp = %q", fileStamp)
	}
}

func TestRateLimitFromEnv(t *testing.T) {
	cases := []struct {
		raw     string
		want    float64
		wantErr bool
	}{
		{"", 20, false},
		{"50", 50, false},
		{"0.5", 0.5, false},
		{"0", 0, true},
		{"-1", 0, true},
		{"many", 0, true},
		{"2000", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv("NETWEATHER_RATE_LIMIT", tc.raw)

			got, err := rateLimitFromEnv()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%q was accepted as %v", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("%q = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestSeedCommandCountryAnchors(t *testing.T) {
	dir := t.TempDir()
	zone := `; fixture
.			172800	IN	NS	a.root-servers.net.
ad.			172800	IN	NS	ns1.nic.ad.
uk.			172800	IN	NS	nsa.nic.uk.
com.			172800	IN	NS	a.gtld-servers.net.
`
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("the root-zone fetch carried no User-Agent")
		}
		_, _ = io.WriteString(w, zone)
	}))
	t.Cleanup(srv.Close)
	// The production client trusts the system roots and refuses plaintext; the
	// test server has its own certificate, so the fetch uses this one. The
	// redirect policy has its own test.
	restore := rootZoneClient
	rootZoneClient = func() *http.Client { return srv.Client() }
	t.Cleanup(func() { rootZoneClient = restore })

	t.Setenv("NETWEATHER_CONFIG", "")
	t.Setenv("NETWEATHER_DATA", filepath.Join(dir, "data"))
	t.Setenv("NETWEATHER_ROOT_ZONE_URL", srv.URL)

	var out bytes.Buffer
	if err := run([]string{"seed", "--country-anchors"}, &out); err != nil {
		t.Fatalf("run(seed --country-anchors): %v", err)
	}
	if !strings.Contains(out.String(), "added 2 ccTLD anchors") {
		t.Fatalf("output = %q", out.String())
	}

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "data", "netweather.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	ids, err := st.TargetIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(ids))
	for _, id := range ids {
		got[id] = true
	}
	if !got["cctld-ad"] || !got["cctld-uk"] {
		t.Fatalf("ids = %v, want the two ccTLD anchors", ids)
	}
	if got["cctld-com"] {
		t.Fatal("a generic TLD was seeded as a country anchor")
	}

	// The row is a real DNS target, not an HTTP one with a country label.
	rows, err := st.TargetsWithState(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.ID != "cctld-ad" {
			continue
		}
		found = true
		if row.Probe != "dns" || row.Port != 53 || row.Class != "country" {
			t.Fatalf("cctld-ad = %+v, want a DNS country anchor", row.Target)
		}
	}
	if !found {
		t.Fatalf("cctld-ad is not in %+v", rows)
	}
}

func TestSeedCommandCountryAnchorsRejectsAnEmptyZone(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "; no delegations\ncom.\t172800\tIN\tNS\ta.gtld-servers.net.\n")
	}))
	t.Cleanup(srv.Close)
	restore := rootZoneClient
	rootZoneClient = func() *http.Client { return srv.Client() }
	t.Cleanup(func() { rootZoneClient = restore })

	t.Setenv("NETWEATHER_CONFIG", "")
	t.Setenv("NETWEATHER_DATA", filepath.Join(dir, "data"))
	t.Setenv("NETWEATHER_ROOT_ZONE_URL", srv.URL)

	err := run([]string{"seed", "--country-anchors"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no ccTLD delegations") {
		t.Fatalf("error = %v, want an empty-zone refusal", err)
	}
}

func TestBuildFeedPollersHonoursTheDocumentedInterval(t *testing.T) {
	// No t.Parallel: this test sets environment variables, which the testing
	// package forbids in a parallel test.

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "netweather.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	t.Setenv("NETWEATHER_FEEDS", "ioda")
	lg, err := logging.New(logging.DefaultConfig(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	pollers := buildFeedPollers(st, lg)
	if len(pollers) != 1 || pollers[0].Feed != "ioda" {
		t.Fatalf("pollers = %+v, want one ioda poller", pollers)
	}
	// DESIGN 8.1 says IODA polls every five minutes.
	if pollers[0].Every != 5*time.Minute {
		t.Fatalf("ioda interval = %v, want 5m", pollers[0].Every)
	}

	// Radar without its token is a stated skip, not an unauthenticated call.
	t.Setenv("NETWEATHER_FEEDS", "radar")
	t.Setenv("NETWEATHER_RADAR_TOKEN", "")
	if got := buildFeedPollers(st, lg); len(got) != 0 {
		t.Fatalf("radar without a token produced %+v", got)
	}
}

func TestBuildLevelPollersIsOptInAndBounded(t *testing.T) {
	// No t.Parallel: this test sets environment variables.
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "netweather.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	lg, err := logging.New(logging.DefaultConfig(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("NETWEATHER_FEEDS", "ioda")
	if got := buildLevelPollers(st, lg); len(got) != 0 {
		t.Fatalf("level feed without opt-in produced %+v", got)
	}

	t.Setenv("NETWEATHER_FEEDS", "ioda-level")
	t.Setenv("NETWEATHER_IODA_LEVEL_LIMIT", "7")
	got := buildLevelPollers(st, lg)
	if len(got) != 1 || got[0].Feed != "ioda-level" {
		t.Fatalf("level pollers = %+v, want one ioda-level poller", got)
	}
	if got[0].Every != 5*time.Minute {
		t.Errorf("level interval = %v, want 5m", got[0].Every)
	}
	source, ok := got[0].Source.(*feeds.IODA)
	if !ok {
		t.Fatalf("level source = %T, want *feeds.IODA", got[0].Source)
	}
	if source.LevelLimit != 7 {
		t.Errorf("level limit = %d, want 7", source.LevelLimit)
	}
	if source.SummaryEndpoint != feeds.DefaultIODASummaryEndpoint {
		t.Errorf("summary endpoint = %q, want the compiled default", source.SummaryEndpoint)
	}

	// An out-of-range limit is a warning plus the bounded default, never an
	// unbounded scrape of every country IODA knows.
	t.Setenv("NETWEATHER_IODA_LEVEL_LIMIT", "9999")
	source = buildLevelPollers(st, lg)[0].Source.(*feeds.IODA)
	if source.LevelLimit != feeds.DefaultIODALevelLimit {
		t.Errorf("out-of-range limit = %d, want default %d",
			source.LevelLimit, feeds.DefaultIODALevelLimit)
	}
}

func TestBuildRISStreamsIsOptInAndBounded(t *testing.T) {
	// No t.Parallel: this test sets environment variables.
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "netweather.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	lg, err := logging.New(logging.DefaultConfig(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("NETWEATHER_FEEDS", "ris-live")
	t.Setenv("NETWEATHER_RIS_STREAM", "")
	t.Setenv("NETWEATHER_RIS_ASNS", "15169")
	if got := buildRISStreams(st, lg); len(got) != 0 {
		t.Fatalf("stream without opt-in produced %+v", got)
	}

	t.Setenv("NETWEATHER_RIS_STREAM", "true")
	got := buildRISStreams(st, lg)
	if len(got) != 1 {
		t.Fatalf("streams = %d, want one", len(got))
	}
	if got[0].URL != feeds.DefaultRISLiveWSURL || len(got[0].ASNs) != 1 || got[0].ASNs[0] != 15169 {
		t.Fatalf("stream = %+v, want the compiled endpoint and one ASN", got[0])
	}

	// A plaintext override is ignored, not silently honoured.
	t.Setenv("NETWEATHER_RIS_WS_URL", "ws://ris-live.ripe.net/v1/ws/")
	if again := buildRISStreams(st, lg); again[0].URL != feeds.DefaultRISLiveWSURL {
		t.Errorf("plaintext override changed the stream URL to %q", again[0].URL)
	}

	// No ASNs means no stream: an unbounded subscription is not a configuration.
	t.Setenv("NETWEATHER_RIS_ASNS", "not-a-number")
	if got := buildRISStreams(st, lg); len(got) != 0 {
		t.Fatalf("stream with no usable ASNs produced %+v", got)
	}

	if !feedURLIsWSS("wss://ris-live.ripe.net/v1/ws/") || feedURLIsWSS("ws://ris-live.ripe.net/v1/ws/") {
		t.Error("feedURLIsWSS accepted a plaintext stream URL")
	}
}

func TestSeedCommandDiffPreviewsWithoutWritingTargets(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "targets.yaml")
	writeConfig := func(port int) {
		t.Helper()
		body := fmt.Sprintf(`version: 1
targets:
  - name: One
    host: one.example
    port: %d
    probe: http
    class: service
    tier: 2
    interval_s: 300
`, port)
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig(443)
	t.Setenv("NETWEATHER_CONFIG", cfg)
	t.Setenv("NETWEATHER_DATA", filepath.Join(dir, "data"))

	var out bytes.Buffer
	if err := run([]string{"seed", "--diff"}, &out); err != nil {
		t.Fatalf("seed --diff: %v", err)
	}
	if !strings.Contains(out.String(), "+ one-example-http") ||
		!strings.Contains(out.String(), "1 addition(s), 0 change(s)") ||
		!strings.Contains(out.String(), "no targets were written") {
		t.Fatalf("diff output = %q", out.String())
	}

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "data", "netweather.db"))
	if err != nil {
		t.Fatal(err)
	}
	ids, err := st.TargetIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if len(ids) != 0 {
		t.Fatalf("a preview wrote %v", ids)
	}

	// Apply, then the same diff is empty.
	if err := run([]string{"seed"}, io.Discard); err != nil {
		t.Fatalf("seed: %v", err)
	}
	out.Reset()
	if err := run([]string{"seed", "--diff"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "0 addition(s), 0 change(s)") {
		t.Fatalf("diff after apply = %q", out.String())
	}

	// A port change keeps the derived id, so it is a change rather than an
	// addition.
	writeConfig(80)
	out.Reset()
	if err := run([]string{"seed", "--diff"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "~ one-example-http") ||
		!strings.Contains(out.String(), "1 change(s)") {
		t.Fatalf("diff after a port change = %q", out.String())
	}
}

func TestStaleWALNotice(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	db := filepath.Join(dir, "netweather.db")
	if err := os.WriteFile(db, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}

	// No WAL beside a clean database.
	if size, ok := staleWAL(db); ok || size != 0 {
		t.Fatalf("no WAL = (%d, %v)", size, ok)
	}

	// A non-empty WAL is reported with its size; SQLite will recover it, and
	// the log line names the restore rule.
	wal := db + "-wal"
	if err := os.WriteFile(wal, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if size, ok := staleWAL(db); !ok || size != 5 {
		t.Fatalf("WAL = (%d, %v), want (5, true)", size, ok)
	}

	// An empty WAL is not worth a line.
	if err := os.WriteFile(wal, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := staleWAL(db); ok {
		t.Fatal("an empty WAL was reported")
	}
}

func TestFeedURLsMustBeHTTPS(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"https://status.example.com",
		"https://status.example.com/api/v2/summary.json",
		"HTTPS://STATUS.EXAMPLE.COM",
	} {
		if !feedURLIsHTTPS(raw) {
			t.Errorf("%q was rejected", raw)
		}
	}
	for _, raw := range []string{
		"http://status.example.com",
		"",
		"not a url",
		"ftp://status.example.com",
		"https://",
	} {
		if feedURLIsHTTPS(raw) {
			t.Errorf("%q was accepted", raw)
		}
	}
}

func TestFeedClientRefusesNonHTTPSRedirects(t *testing.T) {
	t.Parallel()

	client := feedClient()
	https, err := http.NewRequest(http.MethodGet, "https://status.example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CheckRedirect(https, nil); err != nil {
		t.Fatalf("an HTTPS redirect was refused: %v", err)
	}

	plain, err := http.NewRequest(http.MethodGet, "http://status.example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CheckRedirect(plain, nil); err == nil {
		t.Fatal("an HTTP redirect was followed")
	}

	via := make([]*http.Request, 5)
	if err := client.CheckRedirect(https, via); err == nil {
		t.Fatal("six hops were followed")
	}
}

func TestBuildFeedPollersRejectsInsecureURLs(t *testing.T) {
	// No t.Parallel: this test sets environment variables.

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "netweather.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	lg, err := logging.New(logging.DefaultConfig(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	// A plaintext-only statuspage list is skipped, not fetched.
	t.Setenv("NETWEATHER_FEEDS", "statuspage")
	t.Setenv("NETWEATHER_STATUSPAGE_URLS", "http://insecure.example")
	if got := buildFeedPollers(st, lg); len(got) != 0 {
		t.Fatalf("an HTTP-only statuspage list produced %d pollers", len(got))
	}

	// A mixed list keeps the HTTPS entries and drops the rest.
	t.Setenv("NETWEATHER_STATUSPAGE_URLS", "https://ok.example,http://bad.example")
	got := buildFeedPollers(st, lg)
	if len(got) != 1 {
		t.Fatalf("pollers = %+v, want one", got)
	}
	source, ok := got[0].Source.(*feeds.Statuspage)
	if !ok {
		t.Fatalf("source = %T", got[0].Source)
	}
	if len(source.URLs) != 1 || source.URLs[0] != "https://ok.example" {
		t.Fatalf("statuspage URLs = %v, want only the HTTPS entry", source.URLs)
	}

	// A bad override keeps the compiled endpoint rather than sending the feed
	// somewhere plaintext.
	t.Setenv("NETWEATHER_FEEDS", "ioda")
	t.Setenv("NETWEATHER_IODA_URL", "http://insecure.example/down")
	got = buildFeedPollers(st, lg)
	if len(got) != 1 {
		t.Fatalf("ioda pollers = %+v", got)
	}
	ioda, ok := got[0].Source.(*feeds.IODA)
	if !ok {
		t.Fatalf("source = %T", got[0].Source)
	}
	if ioda.Endpoint == "http://insecure.example/down" {
		t.Fatal("an HTTP IODA override was applied")
	}
}

func TestTrustedProxiesFromEnv(t *testing.T) {
	// No t.Parallel: this test sets environment variables.

	if got, err := trustedProxiesFromEnv(); err != nil || got != nil {
		t.Fatalf("unset = (%v, %v), want (nil, nil)", got, err)
	}

	t.Setenv("NETWEATHER_TRUSTED_PROXIES", "10.0.0.0/8, 192.0.2.7, 2001:db8::/32")
	got, err := trustedProxiesFromEnv()
	if err != nil {
		t.Fatalf("trustedProxiesFromEnv: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("prefixes = %v, want three", got)
	}
	if got[0].String() != "10.0.0.0/8" {
		t.Errorf("first = %v, want the masked CIDR", got[0])
	}
	// A bare address becomes a single-host prefix, not a /0 or a /32 mistake.
	if got[1].String() != "192.0.2.7/32" {
		t.Errorf("bare address = %v, want a /32", got[1])
	}
	if got[2].String() != "2001:db8::/32" {
		t.Errorf("v6 = %v", got[2])
	}

	// A typo refuses startup rather than silently trusting nobody.
	t.Setenv("NETWEATHER_TRUSTED_PROXIES", "10.0.0.0/33")
	if _, err := trustedProxiesFromEnv(); err == nil {
		t.Fatal("a bad CIDR was accepted")
	}
	t.Setenv("NETWEATHER_TRUSTED_PROXIES", "not-an-address")
	if _, err := trustedProxiesFromEnv(); err == nil {
		t.Fatal("a bad address was accepted")
	}

	// Empty entries are ignored so a trailing comma is harmless.
	t.Setenv("NETWEATHER_TRUSTED_PROXIES", "10.0.0.0/8,")
	if got, err := trustedProxiesFromEnv(); err != nil || len(got) != 1 {
		t.Fatalf("trailing comma = (%v, %v)", got, err)
	}
}

func TestNextDropReport(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name                    string
		prevDropped, prevErrors uint64
		dropped, writeErrors    uint64
		report                  bool
		droppedDelta            uint64
		writeDelta              uint64
	}{
		{"quiet", 0, 0, 0, 0, false, 0, 0},
		{"unchanged after a report", 5, 2, 5, 2, false, 0, 0},
		{"new drops", 5, 2, 9, 2, true, 4, 0},
		{"new write errors", 5, 2, 5, 3, true, 0, 1},
		{"both", 0, 0, 1, 1, true, 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			report, droppedDelta, writeDelta := nextDropReport(
				tc.prevDropped, tc.prevErrors, tc.dropped, tc.writeErrors)
			if report != tc.report || droppedDelta != tc.droppedDelta || writeDelta != tc.writeDelta {
				t.Fatalf("nextDropReport = (%v, %d, %d), want (%v, %d, %d)",
					report, droppedDelta, writeDelta, tc.report, tc.droppedDelta, tc.writeDelta)
			}
		})
	}
}

func TestSeedRecordsProvenance(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NETWEATHER_CONFIG", "")
	t.Setenv("NETWEATHER_SEED", "")
	t.Setenv("NETWEATHER_DATA", filepath.Join(dir, "data"))

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "data", "netweather.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if _, err := seedTargets(ctx, st); err != nil {
		t.Fatalf("seedTargets: %v", err)
	}

	sources, err := st.TargetSources(ctx, "svc-chase")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Source != "seed:builtin" {
		t.Fatalf("sources = %+v, want one seed:builtin row", sources)
	}
	if len(sources[0].SHA256) != 64 {
		t.Fatalf("sha256 = %q, want 64 hex characters", sources[0].SHA256)
	}
	if sources[0].FetchedAt.IsZero() {
		t.Fatal("fetched_at was not recorded")
	}
}

func TestConfigLoadRecordsProvenance(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "targets.yaml")
	t.Setenv("NETWEATHER_CONFIG", cfg)
	t.Setenv("NETWEATHER_DATA", filepath.Join(dir, "data"))

	body := `version: 1
targets:
  - name: Example
    host: example.com
    port: 443
    probe: http
    class: reference
    tier: 0
`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "data", "netweather.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if _, err := loadTargets(ctx, st); err != nil {
		t.Fatalf("loadTargets: %v", err)
	}
	sources, err := st.TargetSources(ctx, "example-com-http")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Source != "config:"+cfg {
		t.Fatalf("sources = %+v, want config:%s", sources, cfg)
	}
	first := sources[0].SHA256
	if len(first) != 64 {
		t.Fatalf("sha256 = %q", first)
	}

	// An edit changes the hash in place rather than adding a second source
	// row for the same file.
	if err := os.WriteFile(cfg, []byte(body+"    # edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTargets(ctx, st); err != nil {
		t.Fatal(err)
	}
	sources, err = st.TargetSources(ctx, "example-com-http")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].SHA256 == first {
		t.Fatalf("sources after the edit = %+v, want one row with a new hash", sources)
	}
}

func TestBackupCommand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NETWEATHER_DATA", filepath.Join(dir, "data"))

	// Create a real database with one target so the copy has content to prove.
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "data", "netweather.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureVantage(ctx, "local", "direct", "primary"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertTarget(ctx, store.Target{
		ID: "t1", Host: "chase.com", Port: 443, Probe: "http",
		Name: "Chase", Class: "service", Tier: 2, IntervalS: 300,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(dir, "backup.db")
	var out bytes.Buffer
	if err := run([]string{"backup", "-o", dest}, &out); err != nil {
		t.Fatalf("run(backup): %v", err)
	}
	if !strings.Contains(out.String(), "backup written to") {
		t.Fatalf("output = %q", out.String())
	}
	if info, err := os.Stat(dest); err != nil || info.Size() == 0 {
		t.Fatalf("backup file = (%v, %v)", info, err)
	}

	// The copy is a usable database, not just bytes.
	copied, err := store.OpenReadOnly(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := copied.TargetIDs(ctx)
	_ = copied.Close()
	if err != nil || len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("backup targets = (%v, %v)", ids, err)
	}

	if err := run([]string{"backup"}, io.Discard); err == nil {
		t.Fatal("backup without -o was accepted")
	}
	if err := run([]string{"backup", "-o", dest}, io.Discard); err == nil {
		t.Fatal("backup overwrote an existing file")
	}
	if err := run([]string{"backup", "-o", dest, "extra"}, io.Discard); err == nil {
		t.Fatal("backup accepted an extra argument")
	}
}

func TestApplySeedEntriesRejectsTooMany(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "netweather.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// The cap is refused wholesale before any row is written, not truncated
	// silently (THREAT_MODEL.md T34).
	entries := make([]seed.Entry, config.MaxTargets+1)
	if _, err := applySeedEntries(ctx, st, entries, "seed:test", "x"); err == nil ||
		!strings.Contains(err.Error(), "target limit") {
		t.Fatalf("error = %v, want a target-limit refusal", err)
	}
	ids, err := st.TargetIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("a refused seed wrote %v", ids)
	}
}

func TestConfiguredCorridorEndpoints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "targets.yaml")
	body := `version: 1
corridors:
  - id: nyc-lon
    from: NYC
    to: LON
    endpoints: ["www.nytimes.com:443", "www.bbc.co.uk:443"]
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NETWEATHER_CONFIG", path)

	got := configuredCorridorEndpoints(nil).CorridorEndpoints(context.Background())
	if len(got) != 1 || len(got["nyc-lon"]) != 2 {
		t.Fatalf("endpoints = %+v, want one configured pair", got)
	}
	if got["nyc-lon"][0] != "www.nytimes.com:443" || got["nyc-lon"][1] != "www.bbc.co.uk:443" {
		t.Fatalf("endpoints = %+v, want the configured hosts", got)
	}
}
