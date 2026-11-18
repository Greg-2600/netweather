package web_test

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/web"
)

func coldStartServer(t *testing.T, targets ...board.Target) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:    board.NewMemory(targets...),
		Watcher:   board.NewMemory(targets...),
		Token:     testToken,
		Version:   "test",
		StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

// TestColdStartBannerReflectsTheFleetState pins roadmap 2.2: a fresh install
// whose targets have not been probed yet must not read as calm or broken. The
// banner states what is happening; it appears only while most of the fleet is
// still unknown, so it does not outstay its welcome on a healthy board.
func TestColdStartBannerReflectsTheFleetState(t *testing.T) {
	t.Parallel()

	unknown := coldStartServer(t,
		board.Target{ID: "1", Name: "A", Host: "a.test", Sector: "cloud", State: board.StateUnknown},
		board.Target{ID: "2", Name: "B", Host: "b.test", Sector: "cloud", State: board.StateUnknown},
		board.Target{ID: "3", Name: "C", Host: "c.test", Sector: "cloud", State: board.StateUnknown},
	)
	healthy := coldStartServer(t,
		board.Target{ID: "1", Name: "A", Host: "a.test", Sector: "cloud", State: board.StateUp},
		board.Target{ID: "2", Name: "B", Host: "b.test", Sector: "cloud", State: board.StateUp},
		board.Target{ID: "3", Name: "C", Host: "c.test", Sector: "cloud", State: board.StateUp},
	)

	client, _ := newClient(t)
	login(t, unknown, client, testToken)
	resp, err := client.Get(unknown.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	if !strings.Contains(page, `data-testid="cold-start"`) {
		t.Fatalf("a cold fleet drew no cold-start banner:\n%s", page)
	}
	if !strings.Contains(page, "First probes are still being scheduled") {
		t.Fatalf("the cold-start banner is empty:\n%s", page)
	}

	login(t, healthy, client, testToken)
	resp, err = client.Get(healthy.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	if page := body(t, resp); strings.Contains(page, `data-testid="cold-start"`) {
		t.Fatalf("a healthy board drew a cold-start banner:\n%s", page)
	}
}
