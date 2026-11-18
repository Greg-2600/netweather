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

// Atlas reads results from RIPE Atlas measurements the operator owns (or public
// anchor measurements). It is corroboration only: a result is an observation
// from RIPE's probes, not an outage verdict about one of this monitor's
// targets, and it carries no target id to map onto.
//
// RIPE Atlas spends credits to create measurements. This adapter never does:
// it only reads the results endpoint, which is free, and only for the
// measurement ids the operator names. Credits are spent only when the operator
// creates a measurement, outside this binary.
type Atlas struct {
	// Endpoint is the measurements base URL. It is injectable so tests do not
	// need RIPE Atlas.
	Endpoint string
	// Key is the operator's RIPE Atlas API key. It is optional: public
	// measurements are readable without one, and a private measurement needs it.
	Key string
	// Measurements are the measurement ids to read. An empty list disables the
	// feed, because reading every measurement an account owns would be a
	// guessing game rather than a configured source.
	Measurements []int
	Client       *http.Client
	Now          func() time.Time
	// Lookback bounds the query window; the API returns recent results anyway,
	// but an explicit window keeps the response small and the poll cheap.
	Lookback time.Duration
	MaxBytes int64
}

// DefaultAtlasEndpoint is the public v2 measurements base. Results live under
// /{id}/results/.
const DefaultAtlasEndpoint = "https://atlas.ripe.net/api/v2/measurements"

// NewAtlas returns a client with the production defaults. A nil client is a
// configuration error at call time, not a panic here.
func NewAtlas(client *http.Client, key string, measurements []int) *Atlas {
	cp := make([]int, len(measurements))
	copy(cp, measurements)
	return &Atlas{
		Endpoint:     DefaultAtlasEndpoint,
		Key:          strings.TrimSpace(key),
		Measurements: cp,
		Client:       client,
		Now:          time.Now,
		Lookback:     24 * time.Hour,
		MaxBytes:     4 << 20,
	}
}

// Fetch polls every configured measurement and returns one event per result
// record. An empty result set is a successful poll: "no measurement in the
// window" is information, not absence.
func (a *Atlas) Fetch(ctx context.Context) ([]Event, error) {
	if a.Client == nil {
		return nil, fmt.Errorf("feeds: atlas client is not configured")
	}
	if len(a.Measurements) == 0 {
		return nil, fmt.Errorf("feeds: atlas needs at least one measurement id")
	}

	endpoint := a.Endpoint
	if endpoint == "" {
		endpoint = DefaultAtlasEndpoint
	}
	base, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("feeds: atlas endpoint: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("feeds: atlas endpoint %q must be http(s)", endpoint)
	}
	if base.Host == "" {
		return nil, fmt.Errorf("feeds: atlas endpoint %q has no host", endpoint)
	}

	now := a.now()
	lookback := a.Lookback
	if lookback <= 0 {
		lookback = 24 * time.Hour
	}
	start := now.Add(-lookback)

	maxBytes := a.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 4 << 20
	}

	out := make([]Event, 0)
	ids := make([]int, len(a.Measurements))
	copy(ids, a.Measurements)
	sort.Ints(ids)

	for _, id := range ids {
		events, err := a.fetchMeasurement(ctx, base, id, start, now, maxBytes)
		if err != nil {
			return nil, err
		}
		out = append(out, events...)
	}
	return out, nil
}

func (a *Atlas) fetchMeasurement(ctx context.Context, base *url.URL, id int, start, stop time.Time, maxBytes int64) ([]Event, error) {
	u := *base
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strconv.Itoa(id) + "/results/"
	q := u.Query()
	q.Set("start", strconv.FormatInt(start.Unix(), 10))
	q.Set("stop", strconv.FormatInt(stop.Unix(), 10))
	if a.Key != "" {
		q.Set("key", a.Key)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("feeds: atlas request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "netweather (+https://github.com/Greg-2600/netweather)")

	resp, err := a.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("feeds: atlas: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feeds: atlas returned %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return nil, fmt.Errorf("feeds: atlas body: %w", err)
	}

	var results []struct {
		From      int64        `json:"from"`
		ProbeID   int64        `json:"prb_id"`
		Timestamp int64        `json:"timestamp"`
		MSMID     int64        `json:"msm_id"`
		Result    []atlasProbe `json:"result"`
		DstAddr   string       `json:"dst_addr"`
	}
	if err := json.Unmarshal(body, &results); err != nil {
		return nil, fmt.Errorf("feeds: atlas payload: %w", err)
	}

	out := make([]Event, 0, len(results))
	now := a.now()
	for _, r := range results {
		if len(r.Result) == 0 {
			continue
		}
		failures := 0
		var rttSum float64
		var rttN int
		for _, p := range r.Result {
			if p.Err != "" {
				failures++
				continue
			}
			if p.RTT > 0 {
				rttSum += p.RTT
				rttN++
			}
		}
		total := len(r.Result)
		loss := float64(failures) / float64(total)
		// Severity is the loss ratio: 1 means every probe failed, 0 means
		// none did. RTT is evidence, not a verdict, so it rides in the payload.
		severity := loss

		ts := r.Timestamp
		if ts <= 0 {
			ts = now.Unix()
		}
		started := time.Unix(ts, 0).UTC()

		msm := r.MSMID
		if msm == 0 {
			msm = int64(id)
		}
		probeID := r.ProbeID
		if probeID == 0 {
			probeID = r.From
		}

		payload := atlasPayload(r, rttSum, rttN, failures, total)
		out = append(out, Event{
			ExternalID: fmt.Sprintf("atlas:%d:%d:%d", msm, ts, probeID),
			Kind:       "atlas_measurement",
			Severity:   severity,
			StartedAt:  started,
			EndedAt:    time.Time{},
			ObservedAt: now,
			Payload:    payload,
		})
	}
	return out, nil
}

// atlasProbe is one entry in a ping result array. RTT is in milliseconds;
// Err is non-empty when that probe failed.
type atlasProbe struct {
	RTT float64 `json:"rtt"`
	Err string  `json:"err"`
}

func atlasPayload(r struct {
	From      int64        `json:"from"`
	ProbeID   int64        `json:"prb_id"`
	Timestamp int64        `json:"timestamp"`
	MSMID     int64        `json:"msm_id"`
	Result    []atlasProbe `json:"result"`
	DstAddr   string       `json:"dst_addr"`
}, rttSum float64, rttN, failures, total int,
) string {
	mean := 0.0
	if rttN > 0 {
		mean = rttSum / float64(rttN)
	}
	b, _ := json.Marshal(map[string]any{
		"msm_id":  r.MSMID,
		"from":    r.From,
		"dst":     r.DstAddr,
		"rtt_ms":  mean,
		"loss":    float64(failures) / float64(total),
		"samples": total,
	})
	return string(b)
}

func (a *Atlas) now() time.Time {
	if a.Now == nil {
		return time.Now()
	}
	return a.Now()
}
