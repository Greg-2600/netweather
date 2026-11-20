package web_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/web"
)

func TestMapSmoke2(t *testing.T) {
	m := board.NewMemory(board.Target{ID: "t1", Name: "Example", Host: "example.com", Sector: "cloud", State: board.StateUp})
	h, err := web.New(web.Deps{
		Logger:    slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Source:    m,
		Token:     "",
		Version:   "test",
		StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/map")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("STATUS=%d LEN=%d", resp.StatusCode, len(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "map-tile") {
		t.Fatal("no map-tile in body")
	}
}

func TestMapHoverCardExposesEntityAndReason(t *testing.T) {
	m := board.NewMemory(board.Target{
		ID: "root-a", Name: "Root server A", Host: "a.root-servers.net", Port: 53,
		Class: "reference", Sector: "reference", Probe: "dns",
		State: board.StateDown, ErrorClass: "dns_nodata",
	})
	h, err := web.New(web.Deps{
		Logger:    slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Source:    m,
		Token:     "",
		Version:   "test",
		StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/map")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`data-testid="map-tip"`,
		`data-tip-name="Root server A"`,
		`data-tip-kind="DNS root server`,
		`data-tip-state="DOWN"`,
		`data-tip-why="Marked DOWN because`,
		`/static/map.js`,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("map HTML is missing %q", want)
		}
	}

	js, err := http.Get(ts.URL + "/static/map.js")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = js.Body.Close() }()
	if js.StatusCode != http.StatusOK {
		t.Fatalf("map.js status %d", js.StatusCode)
	}
}

func TestMapHealthPutsOutagesFirstInDOM(t *testing.T) {
	// The board deliberately lists the healthy target first: health clustering
	// must reorder clusters, not rely on input order, so both the picture and
	// the tab order put the outage in front.
	m := board.NewMemory(
		board.Target{ID: "a-healthy", Name: "A Healthy", Host: "healthy.example", Sector: "cloud", State: board.StateUp},
		board.Target{ID: "z-outage", Name: "Z Outage", Host: "outage.example", Sector: "cloud", State: board.StateDown, Since: time.Now()},
	)
	h, err := web.New(web.Deps{
		Logger:    slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Source:    m,
		Token:     "",
		Version:   "test",
		StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/map?by=health")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	groups := regexp.MustCompile(`data-testid="map-sector" data-group="([^"]+)"`).FindStringSubmatch(string(body))
	if len(groups) < 2 || groups[1] != "Recent outages" {
		t.Fatalf("first health cluster = %q, want Recent outages", firstMatch(groups))
	}
	idx := strings.Index(string(body), `data-testid="map-tile"`)
	if idx < 0 {
		t.Fatal("no map tile in body")
	}
	state := regexp.MustCompile(`data-tip-state="([^"]+)"`).FindStringSubmatch(string(body)[idx:])
	if len(state) < 2 || state[1] != "DOWN" {
		t.Fatalf("first tile state = %q, want DOWN", firstMatch(state))
	}
}

func firstMatch(match []string) string {
	if len(match) < 2 {
		return ""
	}
	return match[1]
}
