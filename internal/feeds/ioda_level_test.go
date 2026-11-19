package feeds_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/feeds"
	"github.com/Greg-2600/netweather/internal/store"
)

// recordedSummary is a trimmed, recorded IODA v2 outages.summary response. The
// overall scores are the large magnitudes IODA actually returns; they are not
// probabilities, which is why the adapter normalises them before storing.
const recordedSummary = `{
  "type": "outages.summary",
  "data": [
    {"scores": {"bgp.median": 72369.2, "overall": 56448000}, "event_cnt": 2,
     "entity": {"code": "NE", "name": "Niger", "type": "country"}},
    {"scores": {"bgp.median": 464607.4, "overall": 464607.4}, "event_cnt": 1,
     "entity": {"code": "bh", "name": "Bahrain", "type": "country"}},
    {"scores": {"overall": 12.5}, "event_cnt": 4,
     "entity": {"code": "AS15169", "name": "Google", "type": "asn"}},
    {"scores": {"overall": 0}, "event_cnt": 0,
     "entity": {"code": "ER", "name": "Eritrea", "type": "country"}}
  ]
}`

func TestIODAFetchLevelsNormalisesARankedSnapshot(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(recordedSummary))
	}))
	defer srv.Close()

	ioda := feeds.NewIODA(srv.Client())
	ioda.SummaryEndpoint = srv.URL
	ioda.Now = func() time.Time { return now }
	ioda.LevelLimit = 10

	levels, err := ioda.FetchLevels(context.Background())
	if err != nil {
		t.Fatalf("FetchLevels: %v", err)
	}
	if len(levels) != 3 {
		t.Fatalf("levels = %d, want 3 country rows (the ASN row must be skipped)", len(levels))
	}

	wantCodes := []string{"NE", "BH", "ER"}
	for i, want := range wantCodes {
		if levels[i].Country != want {
			t.Errorf("level[%d].Country = %q, want %q", i, levels[i].Country, want)
		}
	}
	if levels[0].Rank != 1 || levels[1].Rank != 2 || levels[2].Rank != 4 {
		t.Errorf("ranks = %d, %d, %d; want 1, 2, 4 (original array order)",
			levels[0].Rank, levels[1].Rank, levels[2].Rank)
	}
	if levels[0].EventCount != 2 || levels[1].EventCount != 1 {
		t.Errorf("event counts = %d, %d; want 2, 1", levels[0].EventCount, levels[1].EventCount)
	}
	if levels[0].Score != 56448000 || levels[1].Score != 464607.4 {
		t.Errorf("scores = %v, %v", levels[0].Score, levels[1].Score)
	}

	// The top-ranked country is 1.0; the second keeps a meaningful relative
	// level because the scale is logarithmic, and a zero score is 0.
	if levels[0].Level < 0.999 || levels[0].Level > 1 {
		t.Errorf("top level = %v, want ~1", levels[0].Level)
	}
	if levels[1].Level <= 0.5 || levels[1].Level >= 1 {
		t.Errorf("second level = %v, want between 0.5 and 1", levels[1].Level)
	}
	if levels[2].Level != 0 {
		t.Errorf("zero-score level = %v, want 0", levels[2].Level)
	}
	for _, l := range levels {
		if l.Source != feeds.IODALevelSource {
			t.Errorf("source = %q, want %q", l.Source, feeds.IODALevelSource)
		}
		if !l.ObservedAt.Equal(now) {
			t.Errorf("observed = %v, want %v", l.ObservedAt, now)
		}
	}

	for _, want := range []string{"entityType=country", "limit=10", "from=", "until="} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}
}

func TestIODAFetchLevelsRejectsUpstreamFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream down", http.StatusBadGateway)
	}))
	defer srv.Close()

	ioda := feeds.NewIODA(srv.Client())
	ioda.SummaryEndpoint = srv.URL
	if _, err := ioda.FetchLevels(context.Background()); err == nil {
		t.Fatal("FetchLevels succeeded against a 502")
	}
}

func TestIODAFetchLevelsRequiresAClient(t *testing.T) {
	t.Parallel()

	ioda := &feeds.IODA{}
	if _, err := ioda.FetchLevels(context.Background()); err == nil {
		t.Fatal("FetchLevels succeeded without an HTTP client")
	}
}

type fakeLevelFetcher struct {
	levels []store.CountryLevel
	err    error
}

func (f *fakeLevelFetcher) FetchLevels(context.Context) ([]store.CountryLevel, error) {
	return f.levels, f.err
}

type fakeLevelSink struct {
	levels     []store.CountryLevel
	failures   []string
	successes  int
	nextPoll   time.Time
	upsertErr  error
	successErr error
}

func (f *fakeLevelSink) UpsertCountryLevels(_ context.Context, levels []store.CountryLevel) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.levels = append(f.levels, levels...)
	return nil
}

func (f *fakeLevelSink) RecordFeedSuccess(context.Context, string, time.Time) error {
	if f.successErr != nil {
		return f.successErr
	}
	f.successes++
	return nil
}

func (f *fakeLevelSink) RecordFeedFailure(_ context.Context, _, detail string, _ time.Time) error {
	f.failures = append(f.failures, detail)
	return nil
}

func (f *fakeLevelSink) SetFeedNextPoll(_ context.Context, _ string, at time.Time) error {
	f.nextPoll = at
	return nil
}

func TestLevelPollerStoresSnapshotAndHealth(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	src := &fakeLevelFetcher{levels: []store.CountryLevel{{
		Country: "NE", Level: 1, Score: 56448000, Rank: 1,
		ObservedAt: now, Source: feeds.IODALevelSource,
	}}}
	sink := &fakeLevelSink{}
	p := &feeds.LevelPoller{
		Feed: "ioda-level", Source: src, Sink: sink,
		Every: 5 * time.Minute, Now: func() time.Time { return now },
	}

	n, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if n != 1 || len(sink.levels) != 1 || sink.successes != 1 {
		t.Fatalf("polled=%d stored=%d successes=%d", n, len(sink.levels), sink.successes)
	}
	if !sink.nextPoll.Equal(now.Add(5 * time.Minute)) {
		t.Errorf("next poll = %v", sink.nextPoll)
	}
}

func TestLevelPollerRecordsFailure(t *testing.T) {
	t.Parallel()

	sink := &fakeLevelSink{}
	p := &feeds.LevelPoller{
		Feed:   "ioda-level",
		Source: &fakeLevelFetcher{err: errors.New("summary unavailable")},
		Sink:   sink,
	}
	if _, err := p.Poll(context.Background()); err == nil {
		t.Fatal("Poll succeeded after a fetch failure")
	}
	if len(sink.failures) != 1 || sink.successes != 0 {
		t.Fatalf("failures=%v successes=%d", sink.failures, sink.successes)
	}
}
