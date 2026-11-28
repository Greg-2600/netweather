package web_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/store"
	"github.com/Greg-2600/netweather/internal/web"
)

type fakeMaintenanceReader struct {
	rows []store.MaintenanceWindow
	err  error
}

func (f *fakeMaintenanceReader) MaintenanceWindows(context.Context, int) ([]store.MaintenanceWindow, error) {
	return f.rows, f.err
}

func newServerWithMaintenance(t *testing.T, reader web.MaintenanceWindowReader) *httptest.Server {
	t.Helper()

	m := fixture()
	h, err := web.New(web.Deps{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:      m,
		Watcher:     m,
		Incidents:   incidentsFixture(),
		Maintenance: reader,
		Version:     "test",
		StartedAt:   time.Now(),
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

func TestIncidentsPageShowsMaintenanceWindows(t *testing.T) {
	t.Parallel()

	starts := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	reader := &fakeMaintenanceReader{rows: []store.MaintenanceWindow{
		{
			ID: 1, Components: "dns, cdn=akamai", Reason: "core switch upgrade",
			StartsAt: starts, EndsAt: starts.Add(2 * time.Hour),
		},
		{
			ID: 2, Components: " banking ", Reason: "provider maintenance",
			StartsAt: starts.Add(-time.Hour), EndsAt: starts, CompletedAt: starts.Add(time.Minute),
		},
	}}
	ts := newServerWithMaintenance(t, reader)
	client, _ := newClient(t)
	resp, page := getWithAccept(t, client, ts.URL+"/incidents", "") //nolint:bodyclose // body() closes it
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{
		`data-testid="maintenance-list"`, "Planned maintenance", "dns, cdn=akamai",
		"core switch upgrade", "provider maintenance", ">planned<", ">completed<",
		`data-testid="incident-list"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("incidents page is missing %q", want)
		}
	}
	if n := strings.Count(page, `data-testid="maintenance-row"`); n != 2 {
		t.Fatalf("maintenance rows = %d, want 2", n)
	}
}

func TestAtlasShowsMaintenanceWindows(t *testing.T) {
	t.Parallel()

	starts := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	reader := &fakeMaintenanceReader{rows: []store.MaintenanceWindow{{
		ID: 1, Components: "dns", Reason: "switch upgrade",
		StartsAt: starts, EndsAt: starts.Add(2 * time.Hour),
	}}}
	m := atlasFixture()
	h, err := web.New(web.Deps{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:      m,
		Watcher:     m,
		Incidents:   incidentsFixture(),
		Maintenance: reader,
		Token:       testToken,
		Version:     "test",
		StartedAt:   time.Now(),
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
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
	if !strings.Contains(page, `data-testid="atlas-maintenance-list"`) ||
		!strings.Contains(page, "switch upgrade") ||
		!strings.Contains(page, `data-testid="atlas-maintenance-row"`) {
		t.Fatal("the Atlas page did not show the declared maintenance window")
	}
}

func TestIncidentsPageWithoutMaintenanceReaderOmitsSection(t *testing.T) {
	t.Parallel()

	ts := newServerWithIncidents(t, "", fixture(), incidentsFixture())
	client, _ := newClient(t)
	resp, page := getWithAccept(t, client, ts.URL+"/incidents", "") //nolint:bodyclose // body() closes it
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if strings.Contains(page, `data-testid="maintenance-list"`) {
		t.Fatal("page claims maintenance is configured when no reader is wired")
	}
}

func TestIncidentsPageMaintenanceReadFailureIsVisible(t *testing.T) {
	t.Parallel()

	ts := newServerWithMaintenance(t, &fakeMaintenanceReader{err: errors.New("db down")})
	client, _ := newClient(t)
	resp, page := getWithAccept(t, client, ts.URL+"/incidents", "") //nolint:bodyclose // body() closes it
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(page, "incident log unavailable") {
		t.Fatalf("read failure = %d %q, want 503", resp.StatusCode, page)
	}
}
