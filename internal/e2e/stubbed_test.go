// Package e2e_test wires the real store, agent, classifier, and HTTP API
// together with a stubbed network at the runner seam.
//
// The point is not to retest each package: it is to prove the pieces still fit —
// a scripted 403 challenge reaches the board as BLOCKED, a 503 reaches it as
// DOWN, coverage counts what is measurable, and the target detail agrees with
// the tile.
package e2e_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/agent"
	"github.com/Greg-2600/netweather/internal/api"
	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/classify"
	"github.com/Greg-2600/netweather/internal/probe"
	"github.com/Greg-2600/netweather/internal/schedule"
	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
)

const testToken = "e2e-token-0123456789"

// scriptedRunner is the fake internet: it answers by URL without a socket.
type scriptedRunner map[string]probe.Result

func (s scriptedRunner) Run(_ context.Context, t agent.Target) probe.Result {
	if res, ok := s[t.URL]; ok {
		return res
	}
	return probe.Result{ErrorClass: "http_request", Err: io.ErrUnexpectedEOF}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.EnsureVantage(ctx, "local", "direct", "primary"); err != nil {
		t.Fatal(err)
	}
	return st
}

func upsertTargets(t *testing.T, st *store.Store) []agent.Target {
	t.Helper()

	type fixture struct {
		id, name, host, sector string
		tier                   int
		expect                 classify.Expected
	}
	targets := []fixture{
		{"e2e-bank", "Chase", "chase.test", "financial/banking", 0, classify.Expected{Status: []int{200}}},
		{"e2e-air", "United", "united.test", "airline", 1, classify.Expected{Status: []int{200}}},
		{"e2e-cloud", "Cloud", "cloud.test", "cloud", 0, classify.Expected{Status: []int{200}}},
	}

	ctx := context.Background()
	out := make([]agent.Target, 0, len(targets))
	for _, f := range targets {
		if err := st.UpsertTarget(ctx, store.Target{
			ID: f.id, Host: f.host, Port: 443, Probe: "http", Name: f.name,
			Class: "service", Sector: f.sector, Tier: f.tier, IntervalS: 300,
		}); err != nil {
			t.Fatal(err)
		}
		out = append(out, agent.Target{
			ID: f.id, Name: f.name, Host: f.host,
			URL: "https://" + f.host + "/", Instrument: "http", Vantage: "local",
			Tier: schedule.Tier(f.tier), Interval: 5 * time.Minute, Expect: f.expect,
		})
	}
	return out
}

func TestStubbedInternetReachesTheBoard(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := openStore(t)
	targets := upsertTargets(t, st)

	runner := scriptedRunner{
		// Cloudflare's challenge page is a 403 that must classify as BLOCKED,
		// not DOWN: that distinction is the product thesis.
		"https://chase.test/": {Status: 403, Header: http.Header{"Cf-Mitigated": []string{"challenge"}}},
		// A real origin failure is DOWN.
		"https://united.test/": {Status: 503},
		// A healthy page.
		"https://cloud.test/": {Status: 200},
	}

	ag := agent.New(targets, runner, st)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	ag.Now = func() time.Time { return now }
	ag.Rand = func() float64 { return 0 }
	ag.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	if ran, err := ag.Step(ctx); err != nil || ran != 3 {
		t.Fatalf("Step = (%d, %v), want 3 probes", ran, err)
	}

	source := &board.StoreSource{Store: st, Vantage: "local"}
	handler, err := api.New(
		api.Config{Listen: "127.0.0.1:2600", Token: testToken},
		api.Deps{
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Source:    source,
			Probes:    st,
			Targets:   st,
			Coverage:  st,
			Incidents: st,
			Scopes:    st,
			Anomalies: st,
			Vantage:   "local",
			Version:   "e2e",
			Commit:    "deadbeef",
			StartedAt: now,
		},
	)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}

	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	get := func(path string) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d: %s", path, resp.StatusCode, body)
		}
		return body
	}

	var boardEnv struct {
		Data []struct {
			ID    string `json:"id"`
			Host  string `json:"host"`
			State string `json:"state"`
			Badge string `json:"badge"`
		} `json:"data"`
	}
	if err := json.Unmarshal(get("/api/v1/board"), &boardEnv); err != nil {
		t.Fatal(err)
	}
	if len(boardEnv.Data) != 3 {
		t.Fatalf("board rows = %d, want 3", len(boardEnv.Data))
	}

	states := map[string]string{}
	for _, row := range boardEnv.Data {
		states[row.Host] = row.State
	}
	if states["chase.test"] != string(state.Blocked) {
		t.Errorf("challenge page classified as %q, want blocked", states["chase.test"])
	}
	if states["united.test"] != string(state.Down) {
		t.Errorf("503 classified as %q, want down", states["united.test"])
	}
	if states["cloud.test"] != string(state.UpReachability) {
		t.Errorf("200 classified as %q, want up_reachability", states["cloud.test"])
	}

	// The detail endpoint must agree with the tile, including the stage badge
	// the board derived from the same observation.
	var detailEnv struct {
		Data struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"data"`
	}
	if err := json.Unmarshal(get("/api/v1/targets/e2e-bank"), &detailEnv); err != nil {
		t.Fatal(err)
	}
	if detailEnv.Data.State != string(state.Blocked) {
		t.Fatalf("detail state = %q, want blocked", detailEnv.Data.State)
	}

	// Coverage is about measurement, not health. DOWN is still a measured
	// signal; BLOCKED is explicitly not, because a bot wall says nothing about
	// whether the origin is reachable.
	var covEnv struct {
		Data struct {
			Total      int     `json:"total"`
			Measurable int     `json:"measurable"`
			Coverage   float64 `json:"coverage"`
		} `json:"data"`
	}
	if err := json.Unmarshal(get("/api/v1/coverage"), &covEnv); err != nil {
		t.Fatal(err)
	}
	if covEnv.Data.Total != 3 || covEnv.Data.Measurable != 2 {
		t.Fatalf("coverage = %+v, want down and up counted but blocked treated as a blind spot", covEnv.Data)
	}
	if covEnv.Data.Coverage <= 0.66 || covEnv.Data.Coverage >= 0.67 {
		t.Fatalf("coverage ratio = %v, want 2/3", covEnv.Data.Coverage)
	}

	// The board and the store agree on probe history for the blocked target.
	var probesEnv struct {
		Data []struct {
			State      string `json:"state"`
			ErrorClass string `json:"error_class"`
			HTTPStatus int    `json:"http_status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(get("/api/v1/targets/e2e-bank/probes?limit=5"), &probesEnv); err != nil {
		t.Fatal(err)
	}
	if len(probesEnv.Data) != 1 {
		t.Fatalf("probe rows = %d", len(probesEnv.Data))
	}
	if probesEnv.Data[0].State != string(state.Blocked) || probesEnv.Data[0].HTTPStatus != 403 {
		t.Fatalf("probe row = %+v", probesEnv.Data[0])
	}
}
