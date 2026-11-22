package web_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/store"
	"github.com/Greg-2600/netweather/internal/web"
)

func atlasFixture() *board.Memory {
	return board.NewMemory(
		board.Target{
			ID: "1", Name: "Chase", Host: "chase.com", Country: "US",
			Sector: "financial/banking", State: board.StateUp,
		},
		board.Target{
			ID: "2", Name: "United", Host: "united.com", Country: "DE",
			Sector: "airline", State: board.StateDown,
		},
		board.Target{
			ID: "3", Name: "Dallas, TX hub — DFW airport", Host: "dfw.example",
			Country: "US", Sector: "us/hub", State: board.StateDown,
		},
	)
}

func newAtlasServer(t *testing.T, m *board.Memory) *httptest.Server {
	t.Helper()
	started := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	latest := store.CorridorSample{
		CorridorID: "nyc-lon", TS: started.Add(time.Hour),
		RTTMS: 83, Loss: 0, OK: true,
	}
	corridors := &fakeCorridorStatus{
		statuses: []store.CorridorStatus{
			{Corridor: store.Corridor{ID: "nyc-lon", From: "NYC", To: "LON"}, Latest: &latest},
			{Corridor: store.Corridor{ID: "lon-sin", From: "LON", To: "SIN"}},
		},
		endpoints: map[string][]string{
			"nyc-lon": {"www.nytimes.com:443", "www.bbc.co.uk:443"},
			"lon-sin": {"www.bbc.co.uk:443", "www.straitstimes.com:443"},
		},
	}
	feeds := &fakeFeedHealth{
		states: []store.FeedState{{Feed: "ioda-level", LastOKAt: time.Now().UTC()}},
		events: []store.ExternalEvent{{
			Feed: "ioda-level", ExternalID: "ev-1", Kind: "outage", Country: "US",
			StartedAt: started, ObservedAt: started.Add(time.Minute),
		}},
	}
	incidents := &fakeIncidentHistory{items: []store.Incident{{
		ID: "inc-1", OpenedAt: started, Scope: "country", ScopeValue: "US",
		Headline: "Path trouble", Confidence: "high",
	}}}

	h, err := web.New(web.Deps{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:            m,
		Watcher:           m,
		Corridors:         corridors,
		CorridorEndpoints: corridors,
		Coverage:          &fakeCoverage{},
		Feeds:             feeds,
		Incidents:         incidents,
		Vantage:           "local",
		Token:             testToken,
		Version:           "test",
		StartedAt:         time.Now(),
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

func TestAtlasCombinesEveryViewMapFirst(t *testing.T) {
	t.Parallel()

	ts := newAtlasServer(t, atlasFixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/atlas") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	page := body(t, resp)

	if !strings.Contains(page, "Everything, all tabs") {
		t.Error("atlas subtitle missing")
	}
	hero := strings.Index(page, `data-testid="atlas-projection"`)
	groups := strings.Index(page, `data-testid="atlas-groups"`)
	if hero < 0 || groups < 0 || hero >= groups {
		t.Errorf("map must be the focal point before the detail sections: hero=%d groups=%d", hero, groups)
	}

	for _, want := range []string{
		`data-testid="atlas"`,
		`data-testid="atlas-pin"`,
		`/static/world-projection.svg`,
		`data-testid="atlas-map-country" data-atlas-kind="Country" data-atlas-title="US"`,
		`data-testid="atlas-map-hub" data-atlas-kind="US hub label"`,
		`data-testid="atlas-map-corridor" data-atlas-kind="Corridor label"`,
		`data-testid="atlas-map-line"`,
		`data-testid="atlas-group"`,
		`data-testid="treemap"`,
		`data-testid="atlas-hub-treemap"`,
		`data-testid="corridor-row"`,
		`data-testid="atlas-coverage"`,
		`data-testid="atlas-incident"`,
		`data-testid="atlas-feed"`,
		`data-testid="atlas-ihi"`,
		`data-testid="atlas-diagnostics"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("atlas page missing %q", want)
		}
	}
	if got := strings.Count(page, `data-testid="map-tile"`); got != 3 {
		t.Errorf("atlas treemap tiles = %d, want 3", got)
	}
	if got := strings.Count(page, `data-testid="atlas-map-country"`); got != 2 {
		t.Errorf("atlas country markers = %d, want 2", got)
	}
	if got := strings.Count(page, `data-testid="atlas-map-line"`); got != 2 {
		t.Errorf("atlas corridor lines = %d, want 2", got)
	}
	if got := strings.Count(page, `data-testid="corridor-row"`); got != 2 {
		t.Errorf("atlas corridor rows = %d, want 2", got)
	}
	// With no health builder, the board omits the IHI chip. The atlas must not
	// invent one from the /ihi "not configured" note.
	if strings.Contains(page, `data-testid="atlas-ihi-chip"`) {
		t.Error("atlas added an IHI chip without a health builder; board omits it")
	}

	// Same-origin assets only: two external scripts and no inline script,
	// external references, or stylesheet import.
	if got := strings.Count(page, "<script"); got != 2 {
		t.Errorf("script tags = %d, want 2 same-origin files", got)
	}
	for _, unwanted := range []string{
		`<script>`, `src="http`, `href="http`, "@import", `onclick=`, `onload=`,
	} {
		if strings.Contains(strings.ToLower(page), unwanted) {
			t.Errorf("atlas page contains %q; assets must stay same-origin and script-free of inline code", unwanted)
		}
	}
	for _, asset := range []string{`<script src="/static/map.js" defer></script>`, `<script src="/static/atlas.js" defer></script>`} {
		if !strings.Contains(page, asset) {
			t.Errorf("atlas page missing %q", asset)
		}
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") ||
		!strings.Contains(csp, "style-src 'self' 'unsafe-inline'") || strings.Contains(csp, "script-src") {
		t.Errorf("atlas CSP = %q, want self-only defaults and the bounded inline-style exception", csp)
	}
}

func TestAtlasIHINoScoreChipMatchesBoard(t *testing.T) {
	t.Parallel()

	// A health builder with no scoreable components is not "not configured":
	// the board and /ihi both state that explicitly, so the atlas chip must
	// appear with the same note rather than silently disappearing.
	ts := newServerWithHealth(t, testToken, atlasFixture(), &fakeHealth{})
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/atlas") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(page, `data-testid="atlas-ihi-chip"`) {
		t.Fatalf("atlas omitted the IHI chip for a builder with no scoreable components:\n%s", page)
	}
	if !strings.Contains(page, `title="No component could be scored yet."`) ||
		!strings.Contains(page, `>IHI -</a>`) {
		t.Fatalf("atlas IHI chip does not state the no-score note:\n%s", page)
	}
}

func TestAtlasSessionProtectedAndMethodChecked(t *testing.T) {
	t.Parallel()

	ts := newAtlasServer(t, atlasFixture())

	_, jar := newClient(t)
	anon := noRedirectClient(jar)
	resp, err := anon.Get(ts.URL + "/atlas") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode == http.StatusOK {
		t.Fatal("anonymous atlas read returned 200")
	}
	_ = body(t, resp)

	client, _ := newClient(t)
	login(t, ts, client, testToken)
	post, err := client.Post(ts.URL+"/atlas", "application/x-www-form-urlencoded", strings.NewReader("")) //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	if post.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /atlas status = %d, want 405", post.StatusCode)
	}
	_ = body(t, post)
}

func TestAtlasStatesMissingOptionalProviders(t *testing.T) {
	t.Parallel()

	// The smallest server has no coverage, incident, feed, or corridor reader.
	// The atlas must still render and say so instead of showing empty health.
	ts := newServer(t, testToken, fixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/atlas") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	page := body(t, resp)
	for _, want := range []string{
		`data-testid="atlas-coverage-note"`,
		`data-testid="atlas-incidents-note"`,
		`data-testid="atlas-feeds-note"`,
		`data-testid="atlas-no-corridors"`,
		`data-testid="atlas-empty-map"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("atlas missing note %q", want)
		}
	}
}
