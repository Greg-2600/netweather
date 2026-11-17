package feeds

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// GCPStatus reads Google Cloud's incidents JSON. It is a separate adapter from
// Statuspage because the shape is a bare array with different field names and a
// distinct impact vocabulary.
type GCPStatus struct {
	// URL is the incidents endpoint, injectable for tests.
	URL      string
	Client   *http.Client
	Now      func() time.Time
	MaxBytes int64
}

// DefaultGCPStatusURL is the public incidents feed.
const DefaultGCPStatusURL = "https://status.cloud.google.com/incidents.json"

// NewGCPStatus returns a client with the production defaults.
func NewGCPStatus(client *http.Client) *GCPStatus {
	return &GCPStatus{
		URL:      DefaultGCPStatusURL,
		Client:   client,
		Now:      time.Now,
		MaxBytes: 4 << 20,
	}
}

// Fetch reads the incidents array and returns one event per incident.
func (g *GCPStatus) Fetch(ctx context.Context) ([]Event, error) {
	if g.Client == nil {
		return nil, fmt.Errorf("feeds: gcp status client is not configured")
	}

	endpoint := strings.TrimSpace(g.URL)
	if endpoint == "" {
		endpoint = DefaultGCPStatusURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("feeds: gcp status request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "netweather (+https://github.com/Greg-2600/netweather)")

	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("feeds: gcp status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feeds: gcp status returned %s", resp.Status)
	}

	maxBytes := g.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 4 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return nil, fmt.Errorf("feeds: gcp status body: %w", err)
	}

	var incidents []map[string]any
	if err := json.Unmarshal(body, &incidents); err != nil {
		return nil, fmt.Errorf("feeds: gcp status payload: %w", err)
	}

	now := g.now()
	out := make([]Event, 0, len(incidents))
	for _, raw := range incidents {
		event, ok := parseGCPIncident(raw, now)
		if !ok {
			continue
		}
		out = append(out, event)
	}
	return out, nil
}

func parseGCPIncident(raw map[string]any, observed time.Time) (Event, bool) {
	id := stringField(raw, "id")
	start, ok := timeField(raw, "begin", "created", "modified")
	if id == "" || !ok {
		return Event{}, false
	}

	event := Event{
		ExternalID: "gcp:" + id,
		Kind:       "gcp_status",
		Severity:   gcpSeverity(raw),
		StartedAt:  start,
		ObservedAt: observed.UTC(),
	}
	if end, ok := timeField(raw, "end"); ok {
		event.EndedAt = end
	}

	payload := map[string]any{
		"id":            id,
		"service_name":  stringField(raw, "service_name"),
		"external_desc": stringField(raw, "external_desc"),
		"status_impact": stringField(raw, "status_impact"),
	}
	if locations, ok := raw["currently_affected_locations"].([]any); ok {
		names := make([]string, 0, len(locations))
		for _, loc := range locations {
			if obj, ok := loc.(map[string]any); ok {
				if name := stringField(obj, "title", "id"); name != "" {
					names = append(names, name)
				}
			}
		}
		payload["locations"] = names
	}
	if encoded, err := json.Marshal(payload); err == nil {
		event.Payload = capPayload(string(encoded), 8<<10)
	}
	return event, true
}

// gcpSeverity maps GCP's status_impact vocabulary to one number.
func gcpSeverity(raw map[string]any) float64 {
	switch strings.ToUpper(stringField(raw, "status_impact")) {
	case "SERVICE_OUTAGE", "SERVICE_DISRUPTION":
		return 1.0
	case "SERVICE_DEGRADATION":
		return 0.5
	case "SERVICE_MAINTENANCE", "SERVICE_INFORMATION":
		return 0.2
	}
	return 0.3
}

func (g *GCPStatus) now() time.Time {
	if g.Now == nil {
		return time.Now()
	}
	return g.Now()
}
