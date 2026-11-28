package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/api"
	"github.com/Greg-2600/netweather/internal/store"
)

type fakeMaintenance struct {
	rows        []store.MaintenanceWindow
	created     []string
	completed   int64
	createErr   error
	completeErr error
	listErr     error
}

func (f *fakeMaintenance) CreateMaintenanceWindow(_ context.Context, components, reason string, startsAt, endsAt time.Time) (int64, error) {
	if f.createErr != nil {
		return 0, f.createErr
	}
	f.created = append(f.created, components+"|"+reason+"|"+startsAt.Format(time.RFC3339)+"|"+endsAt.Format(time.RFC3339))
	f.rows = append(f.rows, store.MaintenanceWindow{
		ID: int64(len(f.rows) + 1), Components: components, Reason: reason,
		StartsAt: startsAt, EndsAt: endsAt,
	})
	return int64(len(f.rows)), nil
}

func (f *fakeMaintenance) CompleteMaintenanceWindow(_ context.Context, id int64, at time.Time) error {
	if f.completeErr != nil {
		return f.completeErr
	}
	if id > int64(len(f.rows)) {
		return sql.ErrNoRows
	}
	f.completed = id
	f.rows[id-1].CompletedAt = at
	return nil
}

func (f *fakeMaintenance) MaintenanceWindows(_ context.Context, _ int) ([]store.MaintenanceWindow, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.rows, nil
}

func newMaintenanceHandler(t *testing.T, reader api.MaintenanceWindows) http.Handler {
	t.Helper()

	h, err := api.New(
		api.Config{Listen: "127.0.0.1:2600", Token: testToken},
		api.Deps{
			Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
			Maintenance: reader,
			Version:     "test",
			Commit:      "deadbeef",
			StartedAt:   time.Now(),
		},
	)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h
}

func maintenanceRequest(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMaintenanceAPIListShowsPlannedAndCompleted(t *testing.T) {
	t.Parallel()

	starts := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	fake := &fakeMaintenance{rows: []store.MaintenanceWindow{
		{ID: 1, Components: "dns", Reason: "switch upgrade", StartsAt: starts, EndsAt: starts.Add(time.Hour)},
		{
			ID: 2, Components: "cdn=akamai", Reason: "provider work", StartsAt: starts.Add(-time.Hour),
			EndsAt: starts, CompletedAt: starts.Add(time.Minute),
		},
	}}
	rec := do(newMaintenanceHandler(t, fake), http.MethodGet, "/api/v1/maintenance?limit=1", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	var env dataEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Meta.Count != 2 || env.Meta.Limit != 1 {
		t.Fatalf("meta = %+v", env.Meta)
	}
	var items []maintenanceItemTest
	if err := json.Unmarshal(env.Data, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].State != "planned" || items[1].State != "completed" ||
		items[1].CompletedAt == "" {
		t.Fatalf("items = %+v", items)
	}
}

type maintenanceItemTest struct {
	ID          int64  `json:"id"`
	Components  string `json:"components"`
	Reason      string `json:"reason"`
	StartsAt    string `json:"starts_at"`
	EndsAt      string `json:"ends_at"`
	State       string `json:"state"`
	CompletedAt string `json:"completed_at"`
}

func TestMaintenanceAPICreateAndComplete(t *testing.T) {
	t.Parallel()

	fake := &fakeMaintenance{}
	h := newMaintenanceHandler(t, fake)
	body := `{"components":"  dns, cdn=akamai  ","reason":"  planned core switch upgrade  ",` +
		`"starts_at":"2026-11-28T10:00:00Z","ends_at":"2026-11-28T12:00:00Z"}`
	rec := maintenanceRequest(t, h, http.MethodPost, "/api/v1/maintenance", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, body=%s", rec.Code, rec.Body)
	}
	if len(fake.created) != 1 || fake.created[0] != "dns, cdn=akamai|planned core switch upgrade|2026-11-28T10:00:00Z|2026-11-28T12:00:00Z" {
		t.Fatalf("stored = %v", fake.created)
	}

	rec = maintenanceRequest(t, h, http.MethodPost, "/api/v1/maintenance/1/complete", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("complete status = %d, body=%s", rec.Code, rec.Body)
	}
	if fake.completed != 1 {
		t.Fatalf("completed id = %d, want 1", fake.completed)
	}
	var env dataEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var got struct {
		State       string `json:"state"`
		CompletedAt string `json:"completed_at"`
	}
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.State != "completed" || got.CompletedAt == "" {
		t.Fatalf("completion response = %+v", got)
	}
}

func TestMaintenanceAPIRejectsBadInput(t *testing.T) {
	t.Parallel()

	good := `{"components":"dns","reason":"work","starts_at":"2026-11-28T10:00:00Z","ends_at":"2026-11-28T11:00:00Z"}`
	cases := []struct {
		name string
		body string
		want int
	}{
		{"empty components", `{"components":"  ","reason":"work","starts_at":"2026-11-28T10:00:00Z","ends_at":"2026-11-28T11:00:00Z"}`, http.StatusBadRequest},
		{"empty reason", `{"components":"dns","reason":"","starts_at":"2026-11-28T10:00:00Z","ends_at":"2026-11-28T11:00:00Z"}`, http.StatusBadRequest},
		{"long reason", `{"components":"dns","reason":"` + strings.Repeat("x", 501) + `","starts_at":"2026-11-28T10:00:00Z","ends_at":"2026-11-28T11:00:00Z"}`, http.StatusBadRequest},
		{"bad start", `{"components":"dns","reason":"work","starts_at":"yesterday","ends_at":"2026-11-28T11:00:00Z"}`, http.StatusBadRequest},
		{"bad end", `{"components":"dns","reason":"work","starts_at":"2026-11-28T10:00:00Z","ends_at":"tomorrow"}`, http.StatusBadRequest},
		{"reversed", `{"components":"dns","reason":"work","starts_at":"2026-11-28T12:00:00Z","ends_at":"2026-11-28T11:00:00Z"}`, http.StatusBadRequest},
		{"too long window", `{"components":"dns","reason":"work","starts_at":"2026-01-01T00:00:00Z","ends_at":"2026-12-01T00:00:00Z"}`, http.StatusBadRequest},
		{"unknown field", `{"components":"dns","reason":"work","starts_at":"2026-11-28T10:00:00Z","ends_at":"2026-11-28T11:00:00Z","bogus":1}`, http.StatusBadRequest},
		{"valid", good, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeMaintenance{}
			rec := maintenanceRequest(t, newMaintenanceHandler(t, fake), http.MethodPost, "/api/v1/maintenance", tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d, body=%s", rec.Code, tc.want, rec.Body)
			}
			if tc.want == http.StatusOK && len(fake.created) != 1 {
				t.Fatalf("valid window was not stored: %v", fake.created)
			}
			if tc.want != http.StatusOK && len(fake.created) != 0 {
				t.Fatalf("invalid window was stored: %v", fake.created)
			}
		})
	}
}

func TestMaintenanceAPIFailurePaths(t *testing.T) {
	t.Parallel()

	bare := do(newHandler(t, testToken, nil), http.MethodGet, "/api/v1/maintenance", testToken)
	if bare.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired list = %d, want 503", bare.Code)
	}

	fake := &fakeMaintenance{}
	h := newMaintenanceHandler(t, fake)
	rec := do(h, http.MethodPut, "/api/v1/maintenance", testToken)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT list = %d, want 405", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/maintenance/nope", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown subtree = %d, want 404", rec.Code)
	}
	rec = maintenanceRequest(t, h, http.MethodPost, "/api/v1/maintenance/99/complete", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id = %d, want 404", rec.Code)
	}
	rec = maintenanceRequest(t, h, http.MethodGet, "/api/v1/maintenance/1/complete", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET complete = %d, want 405", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/maintenance", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON create = %d, want 415", rec.Code)
	}

	fake.listErr = errors.New("db down")
	if rec := do(h, http.MethodGet, "/api/v1/maintenance", testToken); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("list failure = %d, want 503", rec.Code)
	}
	fake.createErr = errors.New("db down")
	body := `{"components":"dns","reason":"work","starts_at":"2026-11-28T10:00:00Z","ends_at":"2026-11-28T11:00:00Z"}`
	if rec := maintenanceRequest(t, h, http.MethodPost, "/api/v1/maintenance", body); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("create failure = %d, want 503", rec.Code)
	}
	fake.completeErr = errors.New("db down")
	if rec := maintenanceRequest(t, h, http.MethodPost, "/api/v1/maintenance/1/complete", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("complete failure = %d, want 503", rec.Code)
	}
}

func TestMaintenanceCapabilityIsRuntimeTruth(t *testing.T) {
	t.Parallel()

	bare := metaCapabilities(t, api.Deps{})
	if hasCapability(bare, "maintenance_windows") {
		t.Fatalf("bare instance advertises maintenance: %v", bare)
	}
	wired := metaCapabilities(t, api.Deps{Maintenance: &fakeMaintenance{}})
	if !hasCapability(wired, "maintenance_windows") {
		t.Fatalf("wired maintenance not advertised: %v", wired)
	}
}
