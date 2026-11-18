package feeds_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/feeds"
)

// fixture reads one recorded payload. The files are captured shapes, not live
// calls: CI parses them and never contacts a third party (QUALITY.md §5.6).
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "feeds", name)) // #nosec G304 -- repo fixture.
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return raw
}

// responder lets one test swap the served body between cases without racing
// the server's read of its handler.
type responder struct {
	mu sync.Mutex
	fn func(*http.Request) []byte
}

func bodyResponder(body []byte) *responder {
	return &responder{fn: func(*http.Request) []byte { return body }}
}

func (r *responder) set(fn func(*http.Request) []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fn = fn
}

func (r *responder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	fn := r.fn
	r.mu.Unlock()
	_, _ = w.Write(fn(req))
}

func serve(t *testing.T, r *responder) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func serveBody(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	return serve(t, bodyResponder(body))
}

func fetch(t *testing.T, f feeds.Fetcher) ([]feeds.Event, error) {
	t.Helper()
	return f.Fetch(context.Background())
}

func fixedNow() time.Time {
	return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
}

func TestFeedContractAtlas(t *testing.T) {
	t.Parallel()

	body := fixture(t, "atlas.json")
	srv := serveBody(t, body)

	a := feeds.NewAtlas(srv.Client(), "test-key", []int{98765432})
	a.Endpoint = srv.URL + "/api/v2/measurements"
	a.Now = fixedNow

	events, err := fetch(t, a)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}

	first := events[0]
	if first.Kind != "atlas_measurement" {
		t.Fatalf("kind = %q, want atlas_measurement", first.Kind)
	}
	if first.ExternalID == "" {
		t.Fatal("ExternalID missing")
	}
	if first.Severity < 0.3 || first.Severity > 0.4 {
		t.Fatalf("severity (loss) = %v, want ~0.333", first.Severity)
	}
	if first.StartedAt.IsZero() {
		t.Fatal("StartedAt missing")
	}
	if !strings.Contains(first.Payload, `"msm_id"`) {
		t.Fatalf("payload lost the measurement id: %s", first.Payload)
	}
	if !strings.Contains(first.Payload, `"rtt_ms"`) {
		t.Fatalf("payload lost the rtt: %s", first.Payload)
	}

	second := events[1]
	if second.Severity != 0 {
		t.Fatalf("severity = %v, want 0 (no loss)", second.Severity)
	}
}

func TestFeedContractIODA(t *testing.T) {
	t.Parallel()

	body := fixture(t, "ioda.json")
	srv := serveBody(t, body)

	f := feeds.NewIODA(srv.Client())
	f.Endpoint = srv.URL + "/v2/outages/events"
	f.Now = fixedNow

	events, err := fetch(t, f)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}

	first := events[0]
	if first.ExternalID != "1001" || first.Kind != "outage" {
		t.Fatalf("first = %+v", first)
	}
	if first.Country != "IR" {
		t.Fatalf("country = %q, want IR", first.Country)
	}
	if first.Severity != 0.87 {
		t.Fatalf("severity = %v, want 0.87", first.Severity)
	}
	if first.EndedAt.IsZero() {
		t.Fatal("an end time was present and was not recorded")
	}
	if !strings.Contains(first.Payload, `"entity"`) {
		t.Fatalf("payload lost the entity: %s", first.Payload)
	}
	if events[1].ASN != 64500 {
		t.Fatalf("second ASN = %d, want 64500", events[1].ASN)
	}
}

func TestFeedContractIODADegrades(t *testing.T) {
	t.Parallel()

	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(fixture(t, "ioda.json"), &envelope); err != nil {
		t.Fatal(err)
	}
	// An event with no id cannot be deduplicated, so it is skipped rather than
	// guessed. The other event must survive.
	delete(envelope.Data[0], "id")
	mutated, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}

	r := bodyResponder(mutated)
	srv := serve(t, r)
	f := feeds.NewIODA(srv.Client())
	f.Endpoint = srv.URL + "/v2/outages/events"

	events, err := fetch(t, f)
	if err != nil {
		t.Fatalf("a mutated payload errored instead of skipping: %v", err)
	}
	if len(events) != 1 || events[0].ExternalID != "1002" {
		t.Fatalf("events = %+v, want only 1002", events)
	}

	truncated := fixture(t, "ioda.json")
	r.set(func(*http.Request) []byte { return truncated[:len(truncated)/2] })
	if _, err := fetch(t, f); err == nil {
		t.Fatal("a truncated payload was accepted")
	}
}

func TestFeedContractRadar(t *testing.T) {
	t.Parallel()

	annotations := fixture(t, "radar.json")
	r := &responder{fn: func(req *http.Request) []byte {
		if strings.Contains(req.URL.Path, "annotations") {
			return annotations
		}
		return []byte(`{"result":{}}`)
	}}
	srv := serve(t, r)

	f := feeds.NewRadar(srv.Client(), "token")
	f.BaseURL = srv.URL
	f.Now = fixedNow

	events, err := fetch(t, f)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// Only the annotation endpoint carries a row in this fixture, which also
	// proves each query is routed to its own path.
	if len(events) != 1 {
		t.Fatalf("events = %+v, want only the annotation", events)
	}
	event := events[0]
	if event.Kind != "outage_annotation" || event.Country != "IR" || event.ASN != 64500 {
		t.Fatalf("event = %+v", event)
	}
	if event.Severity != 0.8 {
		t.Fatalf("severity = %v, want the 0.8 confidence", event.Severity)
	}

	// A row with no start time cannot be placed on a timeline.
	var envelope map[string]any
	if err := json.Unmarshal(annotations, &envelope); err != nil {
		t.Fatal(err)
	}
	result := envelope["result"].(map[string]any)
	delete(result["annotations"].([]any)[0].(map[string]any), "start_time")
	mutated, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	r.set(func(*http.Request) []byte { return mutated })
	if events, err := fetch(t, f); err != nil || len(events) != 0 {
		t.Fatalf("mutated = %+v, %v; want no events and no error", events, err)
	}

	r.set(func(*http.Request) []byte { return annotations[:len(annotations)/2] })
	if _, err := fetch(t, f); err == nil {
		t.Fatal("a truncated payload was accepted")
	}
}

func TestFeedContractStatuspage(t *testing.T) {
	t.Parallel()

	body := fixture(t, "statuspage.json")
	r := bodyResponder(body)
	srv := serve(t, r)
	f := feeds.NewStatuspage(srv.Client(), []string{srv.URL + "/api/v2/summary.json"})
	f.Now = fixedNow

	events, err := fetch(t, f)
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
	if !strings.HasSuffix(first.ExternalID, ":inc-1") {
		t.Fatalf("external id = %q, want the page host and incident", first.ExternalID)
	}
	if first.EndedAt.IsZero() {
		t.Fatal("resolved_at was present and was not recorded")
	}
	if events[1].Severity != 0.2 {
		t.Fatalf("maintenance severity = %v, want 0.2", events[1].Severity)
	}

	var summary struct {
		Incidents []map[string]any `json:"incidents"`
	}
	if err := json.Unmarshal(body, &summary); err != nil {
		t.Fatal(err)
	}
	delete(summary.Incidents[0], "id")
	mutated, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	r.set(func(*http.Request) []byte { return mutated })
	if events, err := fetch(t, f); err != nil || len(events) != 1 {
		t.Fatalf("mutated = %+v, %v; want only inc-2", events, err)
	}

	r.set(func(*http.Request) []byte { return body[:len(body)/2] })
	if _, err := fetch(t, f); err == nil {
		t.Fatal("a truncated payload was accepted")
	}
}

func TestFeedContractGCPStatus(t *testing.T) {
	t.Parallel()

	body := fixture(t, "gcp-status.json")
	r := bodyResponder(body)
	srv := serve(t, r)
	f := feeds.NewGCPStatus(srv.Client())
	f.URL = srv.URL
	f.Now = fixedNow

	events, err := fetch(t, f)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	first := events[0]
	if first.ExternalID != "gcp:gcp-inc-1" || first.Kind != "gcp_status" {
		t.Fatalf("first = %+v", first)
	}
	if first.Severity != 1.0 {
		t.Fatalf("outage severity = %v, want 1.0", first.Severity)
	}
	if !strings.Contains(first.Payload, "us-central1") {
		t.Fatalf("payload lost the affected locations: %s", first.Payload)
	}
	if events[1].Severity != 0.2 {
		t.Fatalf("maintenance severity = %v, want 0.2", events[1].Severity)
	}

	var incidents []map[string]any
	if err := json.Unmarshal(body, &incidents); err != nil {
		t.Fatal(err)
	}
	delete(incidents[0], "id")
	mutated, err := json.Marshal(incidents)
	if err != nil {
		t.Fatal(err)
	}
	r.set(func(*http.Request) []byte { return mutated })
	if events, err := fetch(t, f); err != nil || len(events) != 1 {
		t.Fatalf("mutated = %+v, %v; want only the second incident", events, err)
	}

	r.set(func(*http.Request) []byte { return body[:len(body)/2] })
	if _, err := fetch(t, f); err == nil {
		t.Fatal("a truncated payload was accepted")
	}
}

func TestFeedContractRSSStatusFeeds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		file     string
		build    func(client *http.Client, url string) *feeds.RSSStatus
		kind     string
		severity float64
	}{
		{
			name: "aws", file: "aws-status.xml", kind: "aws_status", severity: 1.0,
			build: func(c *http.Client, url string) *feeds.RSSStatus {
				f := feeds.NewAWSStatus(c)
				f.URL = url
				return f
			},
		},
		{
			name: "azure", file: "azure-status.xml", kind: "azure_status", severity: 0.8,
			build: func(c *http.Client, url string) *feeds.RSSStatus {
				f := feeds.NewAzureStatus(c)
				f.URL = url
				return f
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			body := fixture(t, tc.file)
			r := bodyResponder(body)
			srv := serve(t, r)
			f := tc.build(srv.Client(), srv.URL+"/all.rss")
			f.Now = fixedNow

			events, err := fetch(t, f)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if len(events) != 1 {
				t.Fatalf("events = %d, want 1 (the normal-status item must be dropped)", len(events))
			}
			if events[0].Kind != tc.kind || events[0].Severity != tc.severity {
				t.Fatalf("event = %+v", events[0])
			}
			if !strings.Contains(strings.ToLower(events[0].Payload), "service") {
				t.Fatalf("payload = %s", events[0].Payload)
			}

			// Without a parseable date an item cannot join the timeline.
			mutated := strings.Replace(string(body), "Mon, 17 Nov 2026 1", "not a date 1", 1)
			r.set(func(*http.Request) []byte { return []byte(mutated) })
			if events, err := fetch(t, f); err != nil || len(events) != 0 {
				t.Fatalf("mutated = %+v, %v; want no events and no error", events, err)
			}

			r.set(func(*http.Request) []byte { return body[:len(body)/2] })
			if _, err := fetch(t, f); err == nil {
				t.Fatal("a truncated feed was accepted")
			}
		})
	}
}
