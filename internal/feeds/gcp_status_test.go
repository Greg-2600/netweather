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

func gcpStatusFixture(t *testing.T) *httptest.Server {
	t.Helper()

	const payload = `[
	  {
	    "id": "gcp-1",
	    "service_name": "Google Cloud Pub/Sub",
	    "external_desc": "Some messages are delayed",
	    "begin": "2026-11-17T10:00:00-08:00",
	    "end": "2026-11-17T11:00:00-08:00",
	    "status_impact": "SERVICE_DEGRADATION",
	    "currently_affected_locations": [{"title": "us-east1", "id": "us-east1"}]
	  },
	  {
	    "id": "gcp-2",
	    "service_name": "Compute Engine",
	    "external_desc": "VM creation failures",
	    "begin": "2026-11-17T12:00:00-08:00",
	    "status_impact": "SERVICE_OUTAGE"
	  },
	  {
	    "id": "gcp-3",
	    "service_name": "Maintenance",
	    "begin": "2026-11-17T09:00:00-08:00",
	    "status_impact": "SERVICE_MAINTENANCE"
	  },
	  {
	    "service_name": "no id",
	    "begin": "2026-11-17T09:00:00-08:00"
	  },
	  {
	    "id": "gcp-4",
	    "service_name": "no begin"
	  }
	]`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); !strings.Contains(got, "json") {
			t.Errorf("Accept = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	return srv
}

func TestGCPStatusFetchParsesIncidents(t *testing.T) {
	t.Parallel()

	srv := gcpStatusFixture(t)
	defer srv.Close()

	gcp := feeds.NewGCPStatus(srv.Client())
	gcp.URL = srv.URL
	gcp.Now = func() time.Time { return time.Date(2026, 11, 17, 20, 0, 0, 0, time.UTC) }

	events, err := gcp.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %d, want the three well-formed incidents: %+v", len(events), events)
	}

	first := events[0]
	if first.ExternalID != "gcp:gcp-1" || first.Kind != "gcp_status" {
		t.Fatalf("first = %+v", first)
	}
	if first.Severity != 0.5 {
		t.Fatalf("degradation severity = %v", first.Severity)
	}
	if first.EndedAt.IsZero() {
		t.Fatalf("first end was not parsed: %+v", first)
	}
	if first.Payload == "" || !strings.Contains(first.Payload, "us-east1") || !strings.Contains(first.Payload, "Pub/Sub") {
		t.Fatalf("payload = %q", first.Payload)
	}
	// begin carries an offset; the stored time must be UTC.
	if first.StartedAt.Location() != time.UTC {
		t.Fatalf("start location = %v", first.StartedAt.Location())
	}

	if events[1].Severity != 1.0 {
		t.Fatalf("outage severity = %v", events[1].Severity)
	}
	if events[2].Severity != 0.2 {
		t.Fatalf("maintenance severity = %v", events[2].Severity)
	}
}

func TestGCPStatusRejectsBadResponses(t *testing.T) {
	t.Parallel()

	if _, err := feeds.NewGCPStatus(nil).Fetch(context.Background()); err == nil {
		t.Fatal("a nil client should be an error")
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	gcp := feeds.NewGCPStatus(bad.Client())
	gcp.URL = bad.URL
	if _, err := gcp.Fetch(context.Background()); err == nil {
		t.Fatal("a 503 should be an error")
	}

	badJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer badJSON.Close()
	gcp = feeds.NewGCPStatus(badJSON.Client())
	gcp.URL = badJSON.URL
	if _, err := gcp.Fetch(context.Background()); err == nil {
		t.Fatal("malformed JSON should be an error")
	}
}
