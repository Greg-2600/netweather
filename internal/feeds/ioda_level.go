package feeds

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Greg-2600/netweather/internal/store"
)

// DefaultIODASummaryEndpoint is IODA's ranked summary of current outages. One
// call returns the top entities with IODA's own overall score and event count,
// which is what makes a bounded server-side level possible: the world page
// reads a stored snapshot instead of calling IODA once per country.
const DefaultIODASummaryEndpoint = "https://api.ioda.inetintel.cc.gatech.edu/v2/outages/summary"

// IODALevelSource names where a stored country level came from. The world view
// shows it beside the level so an operator never mistakes the number for a
// verdict or for one of netweather's own probes.
const IODALevelSource = "ioda-summary"

// LevelLimitBounds are the hard limits on how many countries one poll stores.
// The upstream accepts a larger limit, but an early field trial reads a small,
// stable snapshot; a bounded poll is the whole point of this feed.
const (
	MinIODALevelLimit     = 1
	MaxIODALevelLimit     = 100
	DefaultIODALevelLimit = 25
)

// FetchLevels reads IODA's ranked country summary and maps it into bounded,
// normalised per-country levels. The response is a single ranked call: `data`
// is ordered by IODA's own overall score, so the array index is the rank.
func (i *IODA) FetchLevels(ctx context.Context) ([]store.CountryLevel, error) {
	if i.Client == nil {
		return nil, fmt.Errorf("feeds: ioda client is not configured")
	}
	endpoint := i.SummaryEndpoint
	if endpoint == "" {
		endpoint = DefaultIODASummaryEndpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("feeds: ioda summary endpoint: %w", err)
	}

	now := i.now()
	lookback := i.Lookback
	if lookback <= 0 {
		lookback = 24 * time.Hour
	}
	limit := i.LevelLimit
	if limit < MinIODALevelLimit || limit > MaxIODALevelLimit {
		limit = DefaultIODALevelLimit
	}

	q := u.Query()
	q.Set("from", strconv.FormatInt(now.Add(-lookback).Unix(), 10))
	q.Set("until", strconv.FormatInt(now.Unix(), 10))
	q.Set("entityType", "country")
	q.Set("limit", strconv.Itoa(limit))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("feeds: ioda summary request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "netweather (+https://github.com/Greg-2600/netweather)")

	resp, err := i.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("feeds: ioda summary: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feeds: ioda summary returned %s", resp.Status)
	}

	maxBytes := i.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 4 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return nil, fmt.Errorf("feeds: ioda summary body: %w", err)
	}

	var envelope struct {
		Data []struct {
			Scores struct {
				Overall float64 `json:"overall"`
			} `json:"scores"`
			EventCount int `json:"event_cnt"`
			Entity     struct {
				Code string `json:"code"`
				Type string `json:"type"`
			} `json:"entity"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("feeds: ioda summary payload: %w", err)
	}

	// Keep only country entities with a code. The level is normalised against
	// the largest score in this same window, so the snapshot stays comparable
	// even though IODA's raw score is an unbounded magnitude.
	type row struct {
		country string
		score   float64
		events  int
		rank    int
	}
	rows := make([]row, 0, len(envelope.Data))
	maxScore := 0.0
	for idx, item := range envelope.Data {
		code := strings.ToUpper(strings.TrimSpace(item.Entity.Code))
		if code == "" || !strings.EqualFold(item.Entity.Type, "country") {
			continue
		}
		score := item.Scores.Overall
		if score < 0 {
			score = 0
		}
		rows = append(rows, row{country: code, score: score, events: item.EventCount, rank: idx + 1})
		if score > maxScore {
			maxScore = score
		}
	}

	out := make([]store.CountryLevel, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.CountryLevel{
			Country:    r.country,
			Level:      relativeLevel(r.score, maxScore),
			Score:      r.score,
			EventCount: r.events,
			Rank:       r.rank,
			ObservedAt: now.UTC(),
			Source:     IODALevelSource,
		})
	}
	return out, nil
}

// relativeLevel maps IODA's unbounded overall score onto 0-1 using a log
// scale against the top-ranked country in the same poll. It is a relative
// level, not an IODA probability: the UI names the source and keeps the raw
// score so the operator can see the magnitude behind the number.
func relativeLevel(score, max float64) float64 {
	if score <= 0 || max <= 0 {
		return 0
	}
	level := math.Log10(1+score) / math.Log10(1+max)
	if level < 0 {
		return 0
	}
	if level > 1 {
		return 1
	}
	return level
}
