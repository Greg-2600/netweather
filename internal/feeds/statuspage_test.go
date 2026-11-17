package feeds_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/feeds"
)

func statuspageFixture(t *testing.T) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/summary.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		  "status": {"indicator": "major", "description": "Partial Outage"},
		  "incidents": [
		    {"id": "inc-1", "name": "Elevated API errors", "status": "investigating",
		     "impact": "major", "created_at": "2026-11-17T10:00:00Z",
		     "shortlink": "https://stspg.io/abc",
		     "incident_updates": [{"body": "we are looking"}]},
		    {"id": "inc-0", "name": "Scheduled maintenance", "status": "resolved",
		     "impact": "maintenance", "created_at": "2026-11-16T08:00:00Z",
		     "resolved_at": "2026-11-16T09:00:00Z"}
		  ]
		}`))
	})
	return httptest.NewServer(mux)
}

func TestStatuspageFetchParsesIncidents(t *testing.T) {
	t.Parallel()

	srv := statuspageFixture(t)
	defer srv.Close()

	sp := feeds.NewStatuspage(srv.Client(), []string{srv.URL})
	sp.Now = func() time.Time { return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC) }

	events, err := sp.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}

	first := events[0]
	if first.Kind != "statuspage" || first.Severity != 0.8 {
		t.Fatalf("first = %+v", first)
	}
	if first.ExternalID == "" || first.StartedAt.IsZero() {
		t.Fatalf("identity/timeline missing: %+v", first)
	}
	if !first.EndedAt.IsZero() {
		t.Fatalf("an investigating incident carried resolved_at: %+v", first)
	}

	second := events[1]
	if second.Severity != 0.2 || second.EndedAt.IsZero() {
		t.Fatalf("resolved maintenance = %+v", second)
	}
	if second.Payload == "" || !strings.Contains(second.Payload, "Scheduled maintenance") {
		t.Fatalf("payload = %q", second.Payload)
	}
	if strings.Contains(second.Payload, "we are looking") {
		t.Error("update history leaked into the stored payload")
	}
}

func TestStatuspageURLNormalisation(t *testing.T) {
	t.Parallel()

	srv := statuspageFixture(t)
	defer srv.Close()

	for _, in := range []string{srv.URL, srv.URL + "/", srv.URL + "/api/v2/summary.json"} {
		sp := feeds.NewStatuspage(srv.Client(), []string{in})
		events, err := sp.Fetch(context.Background())
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if len(events) != 2 {
			t.Errorf("%q: events = %d", in, len(events))
		}
	}
}

func TestStatuspageEmptyIncidentsIsSuccess(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status": {"indicator": "none"}, "incidents": []}`))
	}))
	defer srv.Close()

	sp := feeds.NewStatuspage(srv.Client(), []string{srv.URL})
	events, err := sp.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %+v", events)
	}
}

func TestStatuspageConfigurationErrors(t *testing.T) {
	t.Parallel()

	if _, err := feeds.NewStatuspage(nil, []string{"https://status.example.com"}).Fetch(context.Background()); err == nil {
		t.Fatal("a nil client should be an error")
	}
	if _, err := feeds.NewStatuspage(http.DefaultClient, nil).Fetch(context.Background()); err == nil {
		t.Fatal("no URLs should be an error")
	}
	for _, bad := range []string{"", "ftp://status.example.com", "status.example.com", "https://"} {
		if _, err := feeds.NewStatuspage(http.DefaultClient, []string{bad}).Fetch(context.Background()); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestStatuspageTransportErrors(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	sp := feeds.NewStatuspage(srv.Client(), []string{srv.URL})
	if _, err := sp.Fetch(context.Background()); err == nil {
		t.Fatal("a 500 should be an error")
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer bad.Close()

	sp = feeds.NewStatuspage(bad.Client(), []string{bad.URL})
	if _, err := sp.Fetch(context.Background()); err == nil {
		t.Fatal("malformed JSON should be an error")
	}
}
