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
		t.Fatalf("hub tiles = %d, want 3", got)
	}
	for _, want := range []string{
		`data-city="Atlanta"`, `data-city="Boston"`, `data-city="Austin"`,
		`href="/targets/svc-b"`, `title="city portal — Down — http_timeout"`,
		`aria-label="Boston, MA: 1 target(s), worst Down"`,
		`data-testid="hubs-note"`,
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
