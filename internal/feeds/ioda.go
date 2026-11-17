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

// IODA is the Georgia Tech Internet Outage Detection and Analysis feed. It is
// the first corroboration source because it already answers "is something large
// happening in this country?"; reimplementing it would be the wrong project.
type IODA struct {
	// Endpoint is the events URL. It is injectable so tests do not need IODA.
	Endpoint string
	Client   *http.Client
	// Lookback bounds the query window; the API returns recent events anyway,
	// but an explicit window keeps the response small.
	Lookback time.Duration
	Now      func() time.Time
	// MaxBytes caps the response before parsing.
	MaxBytes int64
}

// DefaultIODAEndpoint is the public v2 outages endpoint.
const DefaultIODAEndpoint = "https://api.ioda.inetintel.cc.gatech.edu/v2/outages/events"

// NewIODA returns a client with the production defaults. A nil client is a
// configuration error at call time, not a panic here.
func NewIODA(client *http.Client) *IODA {
	return &IODA{
		Endpoint: DefaultIODAEndpoint,
		Client:   client,
		Lookback: 24 * time.Hour,
		Now:      time.Now,
		MaxBytes: 4 << 20,
	}
}

// Fetch polls the endpoint and maps its events into the neutral shape.
//
// The parser is deliberately tolerant: IODA's payload has added fields over
// time, and a monitor that breaks when an upstream adds a key is not useful.
// Unknown fields are kept in the capped raw payload instead of being dropped.
func (i *IODA) Fetch(ctx context.Context) ([]Event, error) {
	if i.Client == nil {
		return nil, fmt.Errorf("feeds: ioda client is not configured")
	}
	endpoint := i.Endpoint
	if endpoint == "" {
		endpoint = DefaultIODAEndpoint
	}

	now := i.now()
	lookback := i.Lookback
	if lookback <= 0 {
		lookback = 24 * time.Hour
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("feeds: ioda endpoint: %w", err)
	}
	q := u.Query()
	q.Set("from", strconv.FormatInt(now.Add(-lookback).Unix(), 10))
	q.Set("until", strconv.FormatInt(now.Unix(), 10))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("feeds: ioda request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "netweather (+https://github.com/Greg-2600/netweather)")

	resp, err := i.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("feeds: ioda: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feeds: ioda returned %s", resp.Status)
	}

	maxBytes := i.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 4 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return nil, fmt.Errorf("feeds: ioda body: %w", err)
	}

	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("feeds: ioda payload: %w", err)
	}

	out := make([]Event, 0, len(envelope.Data))
	for _, raw := range envelope.Data {
		event, ok := parseIODAEvent(raw, now)
		if !ok {
			continue
		}
		out = append(out, event)
	}
	return out, nil
}

func (i *IODA) now() time.Time {
	if i.Now == nil {
		return time.Now()
	}
	return i.Now()
}

// parseIODAEvent extracts the stable fields and keeps everything else as raw
// JSON. An event with no id or no start is skipped: it cannot be deduplicated
// or placed on a timeline, and guessing would be worse than dropping it.
func parseIODAEvent(raw map[string]any, observed time.Time) (Event, bool) {
	id := stringField(raw, "id", "event_id")
	if id == "" {
		return Event{}, false
	}

	start, ok := timeField(raw, "start", "started_at", "time")
	if !ok {
		return Event{}, false
	}

	event := Event{
		ExternalID: id,
		Kind:       "outage",
		Severity:   floatField(raw, "score", "severity"),
		StartedAt:  start,
		ObservedAt: observed.UTC(),
	}
	if end, ok := timeField(raw, "end", "ended_at"); ok {
		event.EndedAt = end
	}

	// IODA nests the affected entity: {type: "country", code: "IR"}.
	if entity, ok := raw["entity"].(map[string]any); ok {
		code := stringField(entity, "code", "name")
		switch strings.ToLower(stringField(entity, "type")) {
		case "country":
			event.Country = strings.ToUpper(code)
		case "asn":
			if n, err := strconv.Atoi(code); err == nil {
				event.ASN = n
			}
		}
	}
	// Some payloads flatten it instead.
	if event.Country == "" {
		event.Country = strings.ToUpper(stringField(raw, "country", "country_code"))
	}
	if event.ASN == 0 {
		if n, err := strconv.Atoi(stringField(raw, "asn")); err == nil {
			event.ASN = n
		}
	}

	if payload, err := json.Marshal(raw); err == nil {
		event.Payload = capPayload(string(payload), 8<<10)
	}

	return event, true
}

func stringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case float64:
			return strconv.FormatInt(int64(v), 10)
		}
	}
	return ""
}

func floatField(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := m[k].(float64); ok {
			return v
		}
	}
	return 0
}

// timeField accepts unix seconds or milliseconds; IODA has used both across
// versions and a monitor should not care.
func timeField(m map[string]any, keys ...string) (time.Time, bool) {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			// Radar returns RFC 3339 timestamps; IODA has used numbers.
			if parsed, err := time.Parse(time.RFC3339Nano, s); err == nil {
				return parsed.UTC(), true
			}
		}

		v, ok := m[k].(float64)
		if !ok || v <= 0 {
			continue
		}
		n := int64(v)
		if n > 1e12 {
			return time.UnixMilli(n).UTC(), true
		}
		return time.Unix(n, 0).UTC(), true
	}
	return time.Time{}, false
}

func capPayload(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
