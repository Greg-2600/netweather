package web_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Greg-2600/netweather/internal/board"
)

func TestHubsGroupsByCityAndState(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(
		board.Target{
			ID: "svc-a", Name: "Atlanta, GA hub — ATL airport", Host: "atl.example",
			Sector: "us/hub", State: board.StateUp,
		},
		board.Target{
			ID: "svc-b", Name: "Boston, MA hub — city portal", Host: "bos.example",
			Sector: "us/hub", State: board.StateDown, ErrorClass: "http_timeout",
		},
		board.Target{
			ID: "svc-c", Name: "Austin, TX hub — city portal", Host: "aus.example",
			Sector: "us/hub", State: board.StateUpReachability,
		},
		board.Target{
			ID: "svc-d", Name: "Malformed hub name", Host: "bad.example",
			Sector: "us/hub", State: board.StateUp,
		},
		board.Target{
			ID: "svc-e", Name: "Chase", Host: "chase.example",
			Sector: "financial/banking", State: board.StateUp,
		},
	)

	ts := newServer(t, testToken, m)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/hubs") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	if got := strings.Count(page, `data-testid="hub"`); got != 3 {
		t.Fatalf("hub city blocks = %d, want 3", got)
	}
	if got := strings.Count(page, `data-testid="map-tile"`); got != 3 {
		t.Fatalf("hub endpoint boxes = %d, want 3", got)
	}
	for _, want := range []string{
		`data-testid="treemap"`, `data-testid="map-tile"`,
		`data-city="Atlanta"`, `data-city="Boston"`, `data-city="Austin"`,
		`href="/targets/svc-b"`, `title="city portal — Down — http_timeout"`,
		`aria-label="Boston, MA: 1 target(s), worst Down"`,
		`data-tip-name="city portal"`, `data-tip-group="Boston, MA"`,
		`data-tip-state="DOWN"`, `data-tip-why="Marked DOWN because the HTTPS request timed out`,
		`data-testid="map-tip"`, `data-testid="hubs-note"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("hubs page missing %q", want)
		}
	}
	if strings.Contains(page, "Malformed hub name") {
		t.Error("a malformed hub name rendered as a placed site")
	}

	// The Down city must precede the healthy cities in the DOM.
	down := strings.Index(page, `data-city="Boston"`)
	up := strings.Index(page, `data-city="Atlanta"`)
	if down < 0 || up < 0 || down > up {
		t.Fatalf("hub order = Boston:%d Atlanta:%d, want worst-first", down, up)
	}

	// The geographic projection is the same three aggregates drawn on the
	// CONUS base map: it sits before the treemap, uses the published anchors,
	// and links each marker to its city block.
	projection := strings.Index(page, `data-testid="hub-projection"`)
	treemap := strings.Index(page, `data-testid="treemap"`)
	if projection < 0 || treemap < 0 || projection >= treemap {
		t.Fatalf("projection=%d treemap=%d, want projection first", projection, treemap)
	}
	// Boston and Austin have published anchors; Atlanta is intentionally not
	// in the anchor table, so it stays in the treemap but is not plotted.
	if got := strings.Count(page, `class="projection-marker hub-marker us-hub-marker`); got != 2 {
		t.Errorf("hub map markers = %d, want 2 (Atlanta has no anchor)", got)
	}
	if !strings.Contains(page, `data-testid="hub-projection-unmapped"`) {
		t.Error("unanchored Atlanta was not reported on the projection")
	}
	for _, want := range []string{
		`src="/static/us-projection.svg"`,
		`left: 91.55%; top: 28.00%;`, // Boston, MA Albers anchor
		`data-hub="Boston, MA"`, `href="#hub-boston-ma"`,
		`id="hub-boston-ma"`,
		`approximate city centre`, `endpoint hosting location is not measured`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("hub projection missing %q", want)
		}
	}
	// The page keeps its same-origin defer script, but adds no inline script
	// and no third-party map asset on top of the shared base image.
	if strings.Contains(page, "<script>") || strings.Contains(page, `<script src="http`) {
		t.Error("hub projection added an inline or third-party script")
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "style-src 'self' 'unsafe-inline'") {
		t.Errorf("hubs CSP = %q, want bounded inline styles", csp)
	}
}

func TestHubsProjectionUnmappedLabel(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(
		board.Target{
			ID: "svc-a", Name: "Boston, MA hub — city portal", Host: "bos.example",
			Sector: "us/hub", State: board.StateUp,
		},
		board.Target{
			ID: "svc-b", Name: "Nowhere, ZZ hub — city portal", Host: "zz.example",
			Sector: "us/hub", State: board.StateUp,
		},
	)
	ts := newServer(t, testToken, m)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/hubs") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	// One city is placed and one is reported, never guessed at a fake position.
	if got := strings.Count(page, `data-testid="hub"`); got != 2 {
		t.Fatalf("hub city blocks = %d, want 2 (map is independent of the treemap)", got)
	}
	if got := strings.Count(page, `us-hub-marker`); got != 1 {
		t.Errorf("hub map markers = %d, want 1 placed anchor", got)
	}
	if !strings.Contains(page, `data-testid="hub-projection-unmapped"`) ||
		!strings.Contains(page, "1 hub city label(s) have no published city-centre anchor") {
		t.Error("unmapped hub city was not reported")
	}
	if strings.Contains(page, `data-hub="Nowhere, ZZ"`) {
		t.Error("an unanchored city was given a map marker")
	}
}

func TestHubsEmptyAndSessionProtected(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(
		board.Target{
			ID: "svc-a", Name: "Chase", Host: "chase.example",
			Sector: "financial/banking", State: board.StateUp,
		},
	)
	ts := newServer(t, testToken, m)

	_, jar := newClient(t)
	anon := noRedirectClient(jar)
	resp, err := anon.Get(ts.URL + "/hubs") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("anonymous status = %d, want a redirect to login", resp.StatusCode)
	}

	client, _ := newClient(t)
	login(t, ts, client, testToken)
	resp, err = client.Get(ts.URL + "/hubs") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	if !strings.Contains(page, `data-testid="empty-hubs"`) {
		t.Fatalf("empty hubs did not say why:\n%s", page)
	}
	if strings.Contains(page, `data-testid="hub"`) {
		t.Error("a non-hub target rendered as a hub tile")
	}
}
