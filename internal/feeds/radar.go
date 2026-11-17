package feeds

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Radar is Cloudflare's BGP and traffic feed. The BGP events are the cyberwar
// signal: named hijacks, route leaks, and traffic anomalies. It is the second
// corroboration source because a summary answers "is something large happening"
// before a target list can.
//
// The token is the operator's: no shared key ships in the binary, and a missing
// token disables the feed rather than silently polling as nobody.
type Radar struct {
	// BaseURL is the Radar API root, injectable for tests.
	BaseURL string
	// Token is a Cloudflare API token with Radar read access.
	Token string
	// Client performs the requests. Nil is a configuration error at call time.
	Client *http.Client
	// Lookback bounds each query. Radar defaults to its own window; an explicit
	// one keeps responses small and the poll interval honest.
	Lookback time.Duration
	Now      func() time.Time
	// MaxBytes caps each response before parsing.
	MaxBytes int64
}

// DefaultRadarBase is the public v4 Radar root.
const DefaultRadarBase = "https://api.cloudflare.com/client/v4/radar"

// NewRadar returns a client with the production defaults and the given token.
func NewRadar(client *http.Client, token string) *Radar {
	return &Radar{
		BaseURL:  DefaultRadarBase,
		Token:    token,
		Client:   client,
		Lookback: 24 * time.Hour,
		Now:      time.Now,
		MaxBytes: 4 << 20,
	}
}

// radarQuery is one endpoint and the kind its rows become.
type radarQuery struct {
	path string
	kind string
}

var radarQueries = []radarQuery{
	{"/bgp/hijacks/events", "hijack"},
	{"/bgp/leaks/events", "leak"},
	{"/traffic_anomalies", "traffic_anomaly"},
}

// Fetch polls every Radar summary endpoint. A feed that cannot answer one of
// them is stale rather than partially fresh: the poller's health signal must
// not be silently degraded to "some data arrived".
func (r *Radar) Fetch(ctx context.Context) ([]Event, error) {
	if r.Client == nil {
		return nil, fmt.Errorf("feeds: radar client is not configured")
	}
	if strings.TrimSpace(r.Token) == "" {
		return nil, fmt.Errorf("feeds: radar token is not configured")
	}

	base := strings.TrimRight(r.BaseURL, "/")
	if base == "" {
		base = DefaultRadarBase
	}

	now := r.now()
	lookback := r.Lookback
	if lookback <= 0 {
		lookback = 24 * time.Hour
	}
	days := int(lookback.Hours() / 24)
	if days < 1 {
		days = 1
	}

	var out []Event
	for _, q := range radarQueries {
		events, err := r.fetchQuery(ctx, base+q.path, q.kind, days, now)
		if err != nil {
			return nil, err
		}
		out = append(out, events...)
	}
	return out, nil
}

func (r *Radar) fetchQuery(ctx context.Context, endpoint, kind string, days int, observed time.Time) ([]Event, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("feeds: radar endpoint: %w", err)
	}
	q := u.Query()
	q.Set("dateRange", fmt.Sprintf("%dd", days))
	q.Set("format", "json")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("feeds: radar request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.Token)
	req.Header.Set("User-Agent", "netweather (+https://github.com/Greg-2600/netweather)")

	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("feeds: radar %s: %w", kind, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("feeds: radar %s: token rejected (%s)", kind, resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feeds: radar %s returned %s", kind, resp.Status)
	}

	maxBytes := r.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 4 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return nil, fmt.Errorf("feeds: radar %s body: %w", kind, err)
	}

	return parseRadar(body, kind, observed)
}

// parseRadar is tolerant by design: Radar has moved fields between releases,
// and a monitor that breaks when an upstream adds a key is not useful. Rows
// without an id or a start time are skipped, because they cannot be
// deduplicated or placed on a timeline.
func parseRadar(body []byte, kind string, observed time.Time) ([]Event, error) {
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("feeds: radar %s payload: %w", kind, err)
	}

	result, _ := envelope["result"].(map[string]any)
	if result == nil {
		// Some endpoints answer with the array at the top level.
		result = envelope
	}

	rows := firstArray(result, "events", "trafficAnomalies", "annotations", "data")
	out := make([]Event, 0, len(rows))
	for _, raw := range rows {
		event, ok := parseRadarEvent(raw, kind, observed)
		if !ok {
			continue
		}
		out = append(out, event)
	}
	return out, nil
}

func firstArray(m map[string]any, keys ...string) []map[string]any {
	for _, key := range keys {
		rows, ok := m[key].([]any)
		if !ok {
			continue
		}
		out := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			if obj, ok := row.(map[string]any); ok {
				out = append(out, obj)
			}
		}
		return out
	}
	return nil
}

func parseRadarEvent(raw map[string]any, kind string, observed time.Time) (Event, bool) {
	start, ok := timeField(raw, "start_time", "startDate", "startTime", "start")
	if !ok {
		return Event{}, false
	}

	event := Event{
		Kind:       kind,
		StartedAt:  start,
		ObservedAt: observed.UTC(),
	}
	event.ExternalID = stringField(raw, "id", "hijack_id", "leak_id", "event_id")
	if event.ExternalID == "" {
		// A stable synthetic key keeps re-polls idempotent even when the
		// upstream omits an id.
		subject := stringField(raw, "prefix", "prefixes", "origin_asn", "asn")
		event.ExternalID = kind + ":" + start.UTC().Format(time.RFC3339) + ":" + subject
	}

	if end, ok := timeField(raw, "end_time", "endDate", "endTime", "end"); ok {
		event.EndedAt = end
	}

	event.Country = strings.ToUpper(stringField(raw, "location", "country", "country_code"))
	if country := strings.ToUpper(stringField(raw, "location_code")); country != "" {
		event.Country = country
	}
	if event.Country != "" && len(event.Country) > 2 {
		// Radar sometimes returns a country name; keep ISO codes only, because
		// the scope layer compares codes.
		event.Country = ""
	}

	if n, err := strconv.Atoi(stringField(raw, "origin_asn", "asn", "hijacker_asn", "target_asn")); err == nil {
		event.ASN = n
	}

	event.Severity = radarSeverity(raw)
	if payload, err := json.Marshal(raw); err == nil {
		event.Payload = capPayload(string(payload), 8<<10)
	}
	return event, true
}

// radarSeverity normalises Radar's three different severity vocabularies:
// hijack confidence, traffic-anomaly status, and a numeric score where present.
func radarSeverity(raw map[string]any) float64 {
	if v, ok := raw["score"].(float64); ok && v >= 0 && v <= 1 {
		return v
	}
	if v, ok := raw["confidence"].(float64); ok && v >= 0 && v <= 1 {
		return v
	}

	label := strings.ToLower(stringField(raw, "confidence", "status", "severity"))
	switch label {
	case "high", "critical", "very_high":
		return 0.9
	case "medium", "moderate":
		return 0.6
	case "low":
		return 0.3
	}
	return 0
}

func (r *Radar) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}
