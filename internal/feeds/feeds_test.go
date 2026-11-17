package feeds_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/feeds"
	"github.com/Greg-2600/netweather/internal/store"
)

type fakeFetcher struct {
	events []feeds.Event
	err    error
	calls  int
}

func (f *fakeFetcher) Fetch(context.Context) ([]feeds.Event, error) {
	f.calls++
	return f.events, f.err
}

type fakeSink struct {
	mu         sync.Mutex
	events     []store.ExternalEvent
	successes  []time.Time
	failures   []string
	nextPoll   time.Time
	upsertErr  error
	successErr error
	failureErr error
}

func (f *fakeSink) UpsertExternalEvents(_ context.Context, events []store.ExternalEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.events = append(f.events, events...)
	return nil
}

func (f *fakeSink) RecordFeedSuccess(_ context.Context, _ string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.successErr != nil {
		return f.successErr
	}
	f.successes = append(f.successes, at)
	return nil
}

func (f *fakeSink) RecordFeedFailure(_ context.Context, _, detail string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failureErr != nil {
		return f.failureErr
	}
	f.failures = append(f.failures, detail)
	return nil
}

func (f *fakeSink) SetFeedNextPoll(_ context.Context, _ string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextPoll = at
	return nil
}

func TestPollerRecordsEventsAndSuccess(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	src := &fakeFetcher{events: []feeds.Event{
		{
			ExternalID: "evt-1", Kind: "outage", Severity: 0.9, Country: "IR",
			StartedAt: now.Add(-time.Hour), ObservedAt: now, Payload: `{"id":"evt-1"}`,
		},
	}}
	sink := &fakeSink{}

	p := &feeds.Poller{
		Feed: "ioda", Source: src, Sink: sink, Every: 10 * time.Minute,
		Now: func() time.Time { return now },
	}

	n, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if n != 1 || len(sink.events) != 1 {
		t.Fatalf("polled %d, recorded %d", n, len(sink.events))
	}
	if sink.events[0].Feed != "ioda" || sink.events[0].ExternalID != "evt-1" {
		t.Fatalf("event = %+v", sink.events[0])
	}
	if sink.events[0].Country != "IR" || sink.events[0].Severity != 0.9 {
		t.Fatalf("event fields = %+v", sink.events[0])
	}
	if len(sink.successes) != 1 || !sink.successes[0].Equal(now) {
		t.Fatalf("successes = %v", sink.successes)
	}
	if !sink.nextPoll.Equal(now.Add(10 * time.Minute)) {
		t.Fatalf("next poll = %v", sink.nextPoll)
	}
}

func TestPollerRecordsFetchFailure(t *testing.T) {
	t.Parallel()

	sink := &fakeSink{}
	p := &feeds.Poller{Feed: "ioda", Source: &fakeFetcher{err: errors.New("503 from upstream")}, Sink: sink}

	if _, err := p.Poll(context.Background()); err == nil {
		t.Fatal("a fetch failure must surface")
	}
	if len(sink.failures) != 1 || !strings.Contains(sink.failures[0], "503") {
		t.Fatalf("failures = %v", sink.failures)
	}
	if len(sink.successes) != 0 {
		t.Fatal("a failed poll recorded success")
	}
}

func TestPollerPropagatesSinkFailure(t *testing.T) {
	t.Parallel()

	sink := &fakeSink{upsertErr: errors.New("disk full")}
	p := &feeds.Poller{Feed: "ioda", Source: &fakeFetcher{}, Sink: sink}
	if _, err := p.Poll(context.Background()); err == nil {
		t.Fatal("a sink failure must surface")
	}
}

func TestPollerFailureWhileRecordingFailure(t *testing.T) {
	t.Parallel()

	sink := &fakeSink{failureErr: errors.New("disk full")}
	p := &feeds.Poller{Feed: "ioda", Source: &fakeFetcher{err: errors.New("boom")}, Sink: sink}

	_, err := p.Poll(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not recorded") {
		t.Fatalf("error = %v, want the recording failure", err)
	}
}

func TestLoopPollsImmediatelyAndStops(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	src := &fakeFetcher{}
	sink := &fakeSink{}

	go func() {
		feeds.Loop(ctx, &feeds.Poller{
			Feed: "ioda", Source: src, Sink: sink, Every: time.Millisecond,
		}, nil)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for src.calls == 0 {
		select {
		case <-deadline:
			t.Fatal("loop did not poll immediately")
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop did not stop after cancel")
	}
}

func TestIODAFetchParsesTheEnvelope(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	payload := `{
	  "data": [
	    {"id": 1001, "start": 1794900000, "end": 1794903600, "score": 0.87,
	     "entity": {"type": "country", "code": "IR", "name": "Iran"},
	     "datasource": "bgp"},
	    {"id": "evt-asn", "start": 1794910000000, "score": 0.4,
	     "entity": {"type": "asn", "code": "3320"}},
	    {"id": "no-start"},
	    {"start": 1794900000}
	  ]
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("from") == "" || r.URL.Query().Get("until") == "" {
			t.Errorf("window parameters missing: %s", r.URL.RawQuery)
		}
		if got := r.Header.Get("User-Agent"); !strings.HasPrefix(got, "netweather") {
			t.Errorf("User-Agent = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	client := feeds.NewIODA(srv.Client())
	client.Endpoint = srv.URL
	client.Now = func() time.Time { return now }

	events, err := client.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want the two well-formed ones: %+v", len(events), events)
	}

	first := events[0]
	if first.ExternalID != "1001" {
		t.Fatalf("first id = %q", first.ExternalID)
	}
	if first.Country != "IR" || first.Kind != "outage" || first.Severity != 0.87 {
		t.Fatalf("first = %+v", first)
	}
	if !first.StartedAt.Equal(time.Unix(1794900000, 0).UTC()) {
		t.Fatalf("start = %v", first.StartedAt)
	}
	if first.EndedAt.IsZero() {
		t.Fatal("end was not parsed")
	}
	if first.Payload == "" {
		t.Fatal("raw payload was dropped")
	}

	second := events[1]
	if second.ASN != 3320 {
		t.Fatalf("asn = %d", second.ASN)
	}
	// Millisecond timestamps are normalised, not reinterpreted as year 58000.
	if second.StartedAt.Year() != now.Year() {
		t.Fatalf("millisecond start = %v", second.StartedAt)
	}
}

func TestIODARejectsBadResponses(t *testing.T) {
	t.Parallel()

	tests := map[string]http.HandlerFunc{
		"status": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
		"body": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not json"))
		},
	}

	for name, handler := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(handler)
			defer srv.Close()

			client := feeds.NewIODA(srv.Client())
			client.Endpoint = srv.URL

			if _, err := client.Fetch(context.Background()); err == nil {
				t.Fatal("expected an error")
			}
		})
	}

	if _, err := (&feeds.IODA{}).Fetch(context.Background()); err == nil {
		t.Fatal("a client without an HTTP client should fail explicitly")
	}
}

func TestIODACapsTheRawPayload(t *testing.T) {
	t.Parallel()

	big := strings.Repeat("x", 20000)
	payload := `{"data":[{"id":"big","start":1794900000,"note":"` + big + `"}]}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	client := feeds.NewIODA(srv.Client())
	client.Endpoint = srv.URL

	events, err := client.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	if len(events[0].Payload) > 8<<10 {
		t.Fatalf("payload length = %d, want it capped at 8 KiB", len(events[0].Payload))
	}
}
