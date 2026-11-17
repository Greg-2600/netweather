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

func radarFixture(t *testing.T) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("/bgp/hijacks/events", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer radar-token" {
			t.Errorf("Authorization = %q", got)
		}
		if r.URL.Query().Get("dateRange") != "1d" {
			t.Errorf("dateRange = %q", r.URL.Query().Get("dateRange"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		  "result": {"events": [
		    {"id": "hijack-1", "start_time": "2026-11-17T10:00:00Z",
		     "end_time": "2026-11-17T10:30:00Z", "prefix": "203.0.113.0/24",
		     "origin_asn": 64500, "hijacker_asn": 64501, "confidence": "high",
		     "location": "us"},
		    {"id": "no-start", "confidence": "low"}
		  ]}
		}`))
	})

	mux.HandleFunc("/bgp/leaks/events", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
		  "result": {"events": [
		    {"leak_id": "leak-7", "startTime": "2026-11-17T09:00:00Z",
		     "origin_asn": "3320", "confidence": 0.7, "location_code": "DE"}
		  ]}
		}`))
	})

	mux.HandleFunc("/traffic_anomalies", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
		  "result": {"trafficAnomalies": [
		    {"startDate": "2026-11-17T08:00:00Z", "asn": 15169,
		     "location": "US", "status": "MEDIUM", "type": "traffic"}
		  ]}
		}`))
	})

	mux.HandleFunc("/annotations/outages", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
		  "result": {"annotations": [
		    {"id": "ann-1", "start_date": "2026-11-17T07:00:00Z",
		     "end_date": "2026-11-17T09:00:00Z", "asn": 64500,
		     "locations": ["IR"], "outage_type": "nationwide",
		     "description": "country-wide disruption"}
		  ]}
		}`))
	})

	return httptest.NewServer(mux)
}

func TestRadarFetchParsesAllFourSources(t *testing.T) {
	t.Parallel()

	srv := radarFixture(t)
	defer srv.Close()

	radar := feeds.NewRadar(srv.Client(), "radar-token")
	radar.BaseURL = srv.URL
	radar.Lookback = 24 * time.Hour
	radar.Now = func() time.Time { return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC) }

	events, err := radar.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("events = %d, want the four well-formed rows: %+v", len(events), events)
	}

	byKind := map[string]feeds.Event{}
	for _, e := range events {
		byKind[e.Kind] = e
	}

	hijack := byKind["hijack"]
	if hijack.ExternalID != "hijack-1" || hijack.ASN != 64500 || hijack.Country != "US" {
		t.Fatalf("hijack = %+v", hijack)
	}
	if hijack.Severity != 0.9 {
		t.Fatalf("hijack severity = %v, want high confidence", hijack.Severity)
	}
	if hijack.EndedAt.IsZero() || hijack.Payload == "" {
		t.Fatalf("hijack end/payload missing: %+v", hijack)
	}

	leak := byKind["leak"]
	if leak.ExternalID != "leak-7" || leak.ASN != 3320 || leak.Country != "DE" {
		t.Fatalf("leak = %+v", leak)
	}
	if leak.Severity != 0.7 {
		t.Fatalf("leak severity = %v, want the numeric confidence", leak.Severity)
	}

	anomaly := byKind["traffic_anomaly"]
	if anomaly.ASN != 15169 || anomaly.Country != "US" {
		t.Fatalf("anomaly = %+v", anomaly)
	}
	if anomaly.ExternalID == "" || !strings.HasPrefix(anomaly.ExternalID, "traffic_anomaly:") {
		t.Fatalf("anomaly id = %q, want a synthetic stable key", anomaly.ExternalID)
	}
	if anomaly.Severity != 0.6 {
		t.Fatalf("anomaly severity = %v, want MEDIUM", anomaly.Severity)
	}

	annotation := byKind["outage_annotation"]
	if annotation.ExternalID != "ann-1" || annotation.Country != "IR" || annotation.ASN != 64500 {
		t.Fatalf("annotation = %+v", annotation)
	}
	if annotation.Severity != 1.0 {
		t.Fatalf("annotation severity = %v, want nationwide", annotation.Severity)
	}
	if annotation.EndedAt.IsZero() {
		t.Fatalf("annotation end missing: %+v", annotation)
	}
}

func TestRadarRequiresConfiguration(t *testing.T) {
	t.Parallel()

	if _, err := feeds.NewRadar(nil, "token").Fetch(context.Background()); err == nil {
		t.Fatal("a nil client should be an error")
	}
	if _, err := feeds.NewRadar(http.DefaultClient, "").Fetch(context.Background()); err == nil {
		t.Fatal("a missing token should be an error")
	}
}

func TestRadarRejectsABadToken(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	radar := feeds.NewRadar(srv.Client(), "bad")
	radar.BaseURL = srv.URL

	_, err := radar.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "token rejected") {
		t.Fatalf("error = %v, want a token rejection", err)
	}
}

func TestRadarRejectsMalformedResponses(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	radar := feeds.NewRadar(srv.Client(), "token")
	radar.BaseURL = srv.URL

	if _, err := radar.Fetch(context.Background()); err == nil {
		t.Fatal("malformed JSON should be an error")
	}
}
