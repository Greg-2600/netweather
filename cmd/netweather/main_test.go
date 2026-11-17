package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestLoadTargetsRejectsUnimplementedProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dns.yaml")
	yaml := `version: 1
targets:
  - name: Root
    host: 198.41.0.4
    port: 53
    probe: dns
    class: network
    tier: 3
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

	if _, err := loadTargets(ctx, st); err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("error = %v, want an explicit unimplemented-probe error", err)
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
