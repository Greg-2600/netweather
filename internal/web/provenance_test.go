package web_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/store"
	"github.com/Greg-2600/netweather/internal/web"
)

type fakeProvenance struct {
	rows []store.TargetSource
	err  error
}

func (f *fakeProvenance) TargetSources(context.Context, string) ([]store.TargetSource, error) {
	return f.rows, f.err
}

func serverWithProvenance(t *testing.T, prov web.ProvenanceReader) *httptest.Server {
	t.Helper()

	m := fixture()
	h, err := web.New(web.Deps{
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:     m,
		Watcher:    m,
		Provenance: prov,
		Token:      testToken,
		Version:    "test",
		StartedAt:  time.Now(),
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

func drawerPage(t *testing.T, ts *httptest.Server, path string) string {
	t.Helper()

	client, _ := newClient(t)
	login(t, ts, client, testToken)
	resp, err := client.Get(ts.URL + path) //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	return body(t, resp)
}

func TestTargetDrawerShowsProvenance(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	prov := &fakeProvenance{rows: []store.TargetSource{
		{
			TargetID: "2", Source: "seed:builtin", FetchedAt: at,
			SHA256: "9f2c1a7b4e8d0356aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}}

	page := drawerPage(t, serverWithProvenance(t, prov), "/targets/2")
	for _, want := range []string{
		`data-testid="target-sources"`, "seed:builtin",
		"9f2c1a7b4e8d", "2026-11-17 12:00",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("drawer missing %q", want)
		}
	}
}

func TestTargetDrawerStatesEmptyAndFailedProvenance(t *testing.T) {
	t.Parallel()

	// An empty read is a stated gap, not a blank panel.
	empty := drawerPage(t, serverWithProvenance(t, &fakeProvenance{}), "/targets/2")
	if !strings.Contains(empty, `data-testid="target-sources-note"`) {
		t.Fatal("an empty provenance read drew no note")
	}
	if strings.Contains(empty, `data-testid="target-sources"`) {
		t.Fatal("an empty provenance read drew a table")
	}

	// A failed read says so and keeps the drawer readable.
	failed := drawerPage(t, serverWithProvenance(t,
		&fakeProvenance{err: errors.New("db down")}), "/targets/2")
	if !strings.Contains(failed, "temporarily unavailable") {
		t.Fatal("a failed provenance read did not say so")
	}

	// An unwired reader omits the panel rather than implying the target has no
	// source.
	unwired := drawerPage(t, serverWithProvenance(t, nil), "/targets/2")
	if strings.Contains(unwired, "Source</h2>") {
		t.Fatal("an unwired provenance reader drew the panel")
	}
}
