package feeds

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Statuspage is the Statuspage family adapter. One client covers every company
// that uses statuspage.io plus the self-hosted status.<company>.com shape, and
// the result is corroboration: "you cannot reach it, the operator says it is
// operational" is the sentence that separates your path from their outage.
type Statuspage struct {
	// URLs are status page bases ("https://status.example.com") or full
	// summary.json URLs. They are operator input, not discovered.
	URLs     []string
	Client   *http.Client
	Now      func() time.Time
	MaxBytes int64
}

// NewStatuspage returns a client for the given pages.
func NewStatuspage(client *http.Client, urls []string) *Statuspage {
	return &Statuspage{
		URLs:     urls,
		Client:   client,
		Now:      time.Now,
		MaxBytes: 2 << 20,
	}
}

// Fetch polls every page and returns one event per incident. An empty incident
// list is a successful poll: "operational" is information, not absence.
func (s *Statuspage) Fetch(ctx context.Context) ([]Event, error) {
	if s.Client == nil {
		return nil, fmt.Errorf("feeds: statuspage client is not configured")
	}
	if len(s.URLs) == 0 {
		return nil, fmt.Errorf("feeds: statuspage needs at least one URL")
	}

	now := s.now()
	var out []Event
	for _, raw := range s.URLs {
		endpoint, host, err := statuspageSummaryURL(raw)
		if err != nil {
			return nil, err
		}
		events, err := s.fetchPage(ctx, endpoint, host, now)
		if err != nil {
			return nil, err
		}
		out = append(out, events...)
	}
	return out, nil
}

// statuspageSummaryURL normalises a configured value into the summary endpoint
// and a stable host label. A bare base gets the documented path appended.
func statuspageSummaryURL(raw string) (endpoint, host string, err error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", fmt.Errorf("feeds: statuspage URL is empty")
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return "", "", fmt.Errorf("feeds: statuspage URL %q: %w", trimmed, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", fmt.Errorf("feeds: statuspage URL %q must be http(s)", trimmed)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("feeds: statuspage URL %q has no host", trimmed)
	}

	if !strings.HasSuffix(u.Path, "/summary.json") {
		u.Path = strings.TrimRight(u.Path, "/") + "/api/v2/summary.json"
	}
	return u.String(), u.Host, nil
}

func (s *Statuspage) fetchPage(ctx context.Context, endpoint, host string, observed time.Time) ([]Event, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("feeds: statuspage request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "netweather (+https://github.com/Greg-2600/netweather)")

	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("feeds: statuspage %s: %w", host, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feeds: statuspage %s returned %s", host, resp.Status)
	}

	maxBytes := s.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 2 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return nil, fmt.Errorf("feeds: statuspage %s body: %w", host, err)
	}

	var summary struct {
		Page struct {
			Name string `json:"name"`
		} `json:"page"`
		Incidents []map[string]any `json:"incidents"`
	}
	if err := json.Unmarshal(body, &summary); err != nil {
		return nil, fmt.Errorf("feeds: statuspage %s payload: %w", host, err)
	}

	events := make([]Event, 0, len(summary.Incidents))
	for _, raw := range summary.Incidents {
		event, ok := parseStatuspageIncident(host, summary.Page.Name, raw, observed)
		if !ok {
			continue
		}
		events = append(events, event)
	}
	return events, nil
}

func parseStatuspageIncident(host, provider string, raw map[string]any, observed time.Time) (Event, bool) {
	id := stringField(raw, "id")
	name := stringField(raw, "name")
	start, ok := timeField(raw, "created_at", "started_at", "updated_at")
	if id == "" || !ok {
		return Event{}, false
	}

	event := Event{
		ExternalID: host + ":" + id,
		Kind:       "statuspage",
		Severity:   statuspageSeverity(raw),
		StartedAt:  start,
		ObservedAt: observed.UTC(),
	}
	if end, ok := timeField(raw, "resolved_at"); ok {
		event.EndedAt = end
	}

	// Keep the useful human fields and drop the update history, which can be
	// long and is not part of the corroboration signal.
	payload := map[string]any{
		"name":     name,
		"status":   stringField(raw, "status"),
		"impact":   stringField(raw, "impact"),
		"url":      stringField(raw, "shortlink"),
		"evidence": "provider-reported",
	}
	if provider != "" {
		payload["provider"] = provider
	}
	if encoded, err := json.Marshal(payload); err == nil {
		event.Payload = capPayload(string(encoded), 8<<10)
	}
	return event, true
}

// statuspageSeverity maps Statuspage's impact vocabulary onto one number.
func statuspageSeverity(raw map[string]any) float64 {
	switch strings.ToLower(stringField(raw, "impact")) {
	case "critical":
		return 1.0
	case "major":
		return 0.8
	case "minor":
		return 0.4
	case "maintenance":
		return 0.2
	}
	// No impact field: a resolved incident is less interesting than an active
	// one, but both are corroboration.
	if stringField(raw, "status") == "resolved" {
		return 0.1
	}
	return 0.3
}

func (s *Statuspage) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}
