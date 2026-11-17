package feeds

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// RSSStatus reads a cloud provider status RSS feed. AWS and Azure publish the
// same RSS 2.0 shape with different wording, so one adapter covers both; the
// Kind field keeps the events distinguishable in the incident panel.
type RSSStatus struct {
	// Kind is the event kind: aws_status or azure_status.
	Kind string
	// URL is the RSS endpoint, injectable for tests.
	URL      string
	Client   *http.Client
	Now      func() time.Time
	MaxBytes int64
}

// DefaultAWSStatusURL is the public all-services status feed.
const DefaultAWSStatusURL = "https://status.aws.amazon.com/rss/all.rss"

// DefaultAzureStatusURL is the public Azure status feed.
const DefaultAzureStatusURL = "https://azure.status.microsoft/en-us/status/feed/"

// NewAWSStatus returns a client with the production defaults.
func NewAWSStatus(client *http.Client) *RSSStatus {
	return &RSSStatus{
		Kind:     "aws_status",
		URL:      DefaultAWSStatusURL,
		Client:   client,
		Now:      time.Now,
		MaxBytes: 2 << 20,
	}
}

// NewAzureStatus returns a client for the Azure status feed.
func NewAzureStatus(client *http.Client) *RSSStatus {
	return &RSSStatus{
		Kind:     "azure_status",
		URL:      DefaultAzureStatusURL,
		Client:   client,
		Now:      time.Now,
		MaxBytes: 2 << 20,
	}
}

// rssEnvelope is the subset of RSS 2.0 that status feeds use.
type rssEnvelope struct {
	Channel struct {
		Items []struct {
			Title       string `xml:"title"`
			Description string `xml:"description"`
			PubDate     string `xml:"pubDate"`
			GUID        string `xml:"guid"`
			Link        string `xml:"link"`
		} `xml:"item"`
	} `xml:"channel"`
}

// Fetch reads the feed and returns one event per incident. "Service is
// operating normally" items are dropped: they describe the absence of a
// problem, which is already implied by an empty incident list.
func (a *RSSStatus) Fetch(ctx context.Context) ([]Event, error) {
	if a.Client == nil {
		return nil, fmt.Errorf("feeds: status feed client is not configured")
	}

	kind := strings.TrimSpace(a.Kind)
	if kind == "" {
		kind = "aws_status"
	}

	endpoint := strings.TrimSpace(a.URL)
	if endpoint == "" {
		endpoint = DefaultAWSStatusURL
		if kind == "azure_status" {
			endpoint = DefaultAzureStatusURL
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("feeds: status request: %w", err)
	}
	req.Header.Set("Accept", "application/rss+xml, application/xml, text/xml")
	req.Header.Set("User-Agent", "netweather (+https://github.com/Greg-2600/netweather)")

	resp, err := a.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("feeds: status feed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feeds: status feed returned %s", resp.Status)
	}

	maxBytes := a.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 2 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return nil, fmt.Errorf("feeds: status feed body: %w", err)
	}

	var feed rssEnvelope
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("feeds: status feed payload: %w", err)
	}

	now := a.now()
	out := make([]Event, 0, len(feed.Channel.Items))
	for _, item := range feed.Channel.Items {
		if isNormalStatus(item.Title) {
			continue
		}
		event, ok := parseRSSItem(kind, item.Title, item.Description, item.PubDate, item.GUID, item.Link, now)
		if !ok {
			continue
		}
		out = append(out, event)
	}
	return out, nil
}

func isNormalStatus(title string) bool {
	lower := strings.ToLower(title)
	// "Resolved" is dropped as well: RSS carries no end time, so keeping it
	// would leave a resolved incident looking ongoing forever.
	return strings.Contains(lower, "operating normally") ||
		strings.Contains(lower, "no issues") ||
		strings.Contains(lower, "all services are healthy") ||
		strings.Contains(lower, "resolved")
}

func parseRSSItem(kind, title, description, pubDate, guid, link string, observed time.Time) (Event, bool) {
	start, ok := parseRSSDate(pubDate)
	if !ok {
		// Without a timestamp the item cannot be placed on the incident
		// timeline, and guessing "now" would make every re-poll a new event.
		return Event{}, false
	}

	id := strings.TrimSpace(guid)
	if id == "" {
		id = strings.TrimSpace(link)
	}
	if id == "" {
		id = kind + ":" + start.UTC().Format(time.RFC3339) + ":" + strings.ToLower(strings.TrimSpace(title))
	}

	event := Event{
		ExternalID: id,
		Kind:       kind,
		Severity:   rssSeverity(title, description),
		StartedAt:  start,
		ObservedAt: observed.UTC(),
	}

	payload, err := json.Marshal(map[string]string{
		"title": title, "description": description, "link": link,
	})
	if err == nil {
		event.Payload = capPayload(string(payload), 8<<10)
	}
	return event, true
}

// parseRSSDate accepts the formats AWS and similar feeds emit. RFC1123 without a
// zone and RFC1123Z with one are both common.
func parseRSSDate(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC1123Z, time.RFC1123, "Mon, 2 Jan 2006 15:04:05 MST"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

// rssSeverity maps the wording AWS and Azure use to one number. It is
// deliberately coarse: the feed is corroboration, not a precise score.
func rssSeverity(title, description string) float64 {
	text := strings.ToLower(title + " " + description)
	switch {
	case strings.Contains(text, "service disruption"), strings.Contains(text, "service outage"), strings.Contains(text, "unavailable"):
		return 1.0
	case strings.Contains(text, "service issue"):
		return 0.8
	case strings.Contains(text, "increased error"), strings.Contains(text, "error rates"):
		return 0.7
	case strings.Contains(text, "degraded"), strings.Contains(text, "performance"):
		return 0.5
	case strings.Contains(text, "maintenance"), strings.Contains(text, "advisory"):
		return 0.2
	default:
		return 0.3
	}
}

func (a *RSSStatus) now() time.Time {
	if a.Now == nil {
		return time.Now()
	}
	return a.Now()
}
