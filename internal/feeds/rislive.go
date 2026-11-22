package feeds

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RISLive reads BGP update history from RIPEstat. It is corroboration, never a
// verdict: a withdrawal or a path change is evidence that something moved in
// the routing table, and it carries the ASN it concerns so the drawer can show
// it beside a target's own ASN.
//
// RIS Live itself is a real-time WebSocket firehose. This adapter uses the
// RIPEstat history endpoint instead, which is a bounded REST read: it fits the
// feed model, needs no persistent connection, and costs nothing per poll. The
// real-time stream is a separate, post-MVP concern (`docs/DESIGN.md` §8.3).
type RISLive struct {
	// Endpoint is the RIPEstat bgp-updates base URL. It is injectable so tests
	// do not need RIPE.
	Endpoint string
	// ASNs are the autonomous systems to read. An empty list disables the feed,
	// because reading every ASN on the internet is not a configured source.
	ASNs   []int
	Client *http.Client
	Now    func() time.Time
	// Lookback bounds the query window; the endpoint returns everything in it.
	Lookback time.Duration
	MaxBytes int64
}

// DefaultRISLiveEndpoint is the RIPEstat bgp-updates data endpoint.
const DefaultRISLiveEndpoint = "https://stat.ripe.net/data/bgp-updates/data.json"

// NewRISLive returns a client with the production defaults.
func NewRISLive(client *http.Client, asns []int) *RISLive {
	cp := make([]int, len(asns))
	copy(cp, asns)
	return &RISLive{
		Endpoint: DefaultRISLiveEndpoint,
		ASNs:     cp,
		Client:   client,
		Now:      time.Now,
		Lookback: 1 * time.Hour,
		MaxBytes: 8 << 20,
	}
}

// Fetch polls every configured ASN and returns one event per BGP update. An
// empty window is a successful poll: "no routing change" is information.
func (r *RISLive) Fetch(ctx context.Context) ([]Event, error) {
	if r.Client == nil {
		return nil, fmt.Errorf("feeds: ris-live client is not configured")
	}
	if len(r.ASNs) == 0 {
		return nil, fmt.Errorf("feeds: ris-live needs at least one ASN")
	}

	endpoint := r.Endpoint
	if endpoint == "" {
		endpoint = DefaultRISLiveEndpoint
	}
	base, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("feeds: ris-live endpoint: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("feeds: ris-live endpoint %q must be http(s)", endpoint)
	}
	if base.Host == "" {
		return nil, fmt.Errorf("feeds: ris-live endpoint %q has no host", endpoint)
	}

	now := r.now()
	lookback := r.Lookback
	if lookback <= 0 {
		lookback = 1 * time.Hour
	}
	start := now.Add(-lookback)

	maxBytes := r.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 8 << 20
	}

	asns := make([]int, len(r.ASNs))
	copy(asns, r.ASNs)
	sort.Ints(asns)

	out := make([]Event, 0)
	for _, asn := range asns {
		events, err := r.fetchASN(ctx, base, asn, start, now, maxBytes)
		if err != nil {
			return nil, err
		}
		out = append(out, events...)
	}
	return out, nil
}

func (r *RISLive) fetchASN(ctx context.Context, base *url.URL, asn int, start, stop time.Time, maxBytes int64) ([]Event, error) {
	u := *base
	q := u.Query()
	q.Set("resource", "AS"+strconv.Itoa(asn))
	q.Set("starttime", start.UTC().Format(time.RFC3339))
	q.Set("endtime", stop.UTC().Format(time.RFC3339))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("feeds: ris-live request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "netweather (+https://github.com/Greg-2600/netweather)")

	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("feeds: ris-live: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feeds: ris-live returned %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return nil, fmt.Errorf("feeds: ris-live body: %w", err)
	}

	var envelope struct {
		Data struct {
			Updates []struct {
				Timestamp float64 `json:"timestamp"`
				Type      string  `json:"type"`
				Attrs     struct {
					ASPath []int  `json:"as_path"`
					Prefix string `json:"target_prefix"`
				} `json:"attrs"`
			} `json:"updates"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("feeds: ris-live payload: %w", err)
	}

	now := r.now()
	out := make([]Event, 0, len(envelope.Data.Updates))
	for i, u := range envelope.Data.Updates {
		ts := time.Unix(int64(u.Timestamp), 0).UTC()
		if ts.IsZero() {
			ts = now
		}
		severity := 0.0
		kind := "bgp_update"
		if strings.EqualFold(u.Type, "W") {
			kind = "bgp_withdrawal"
			severity = 1.0
		}
		payload, _ := json.Marshal(map[string]any{
			"asn":      asn,
			"type":     u.Type,
			"prefix":   u.Attrs.Prefix,
			"as_path":  u.Attrs.ASPath,
			"evidence": "ip-routing-corroboration",
		})
		out = append(out, Event{
			ExternalID: fmt.Sprintf("ris:%d:%d:%s:%d", asn, int64(u.Timestamp), u.Type, i),
			Kind:       kind,
			Severity:   severity,
			ASN:        asn,
			StartedAt:  ts,
			ObservedAt: now,
			Payload:    string(payload),
		})
	}
	return out, nil
}

func (r *RISLive) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}
