package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/agent"
	"github.com/Greg-2600/netweather/internal/logging"
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

func TestRunUnknownCommandIsAnError(t *testing.T) {
	t.Parallel()

	err := run([]string{"definitely-not-a-command"}, io.Discard)
	if err == nil {
		t.Fatal("run(unknown) = nil, want an error")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("error %q does not name the problem", err)
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

	got, err := seedTargets(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Fatalf("agent targets = %d, want 10", len(got))
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
	if len(rows) != 10 {
		t.Fatalf("stored targets = %d, want 10", len(rows))
	}
	for _, r := range rows {
		if r.State != state.Unknown {
			t.Errorf("%s state = %s before any probe, want unknown", r.ID, r.State)
		}
	}
}

func TestTargetURL(t *testing.T) {
	t.Parallel()

	tests := map[int]string{
		443: "https://example.com:443/",
		80:  "http://example.com:80/",
	}
	for port, want := range tests {
		if got := targetURL("example.com", port); got != want {
			t.Errorf("targetURL(%d) = %q, want %q", port, got, want)
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

func TestLoadTargetsRejectsUnimplementedProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "icmp.yaml")
	yaml := `version: 1
targets:
  - name: Gateway
    host: 192.0.2.1
    port: 0
    probe: icmp
    class: network
    tier: 3
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

	if _, err := loadTargets(ctx, st); err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("error = %v, want an explicit unimplemented-probe error", err)
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
	ag, err := newTorAgent(ctx, st, nil, lg)
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
	if _, err := newTorAgent(ctx, st, nil, lg); err == nil {
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
	ag, err := newTorAgent(ctx, st, direct, lg)
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
	_, err = newTorAgent(ctx, st, []agent.Target{{ID: "dns", Instrument: "dns"}}, lg)
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
