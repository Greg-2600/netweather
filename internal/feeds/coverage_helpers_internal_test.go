package feeds //nolint:testpackage // white-box tests cover pure helpers and injectable seams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/netx"
	"github.com/Greg-2600/netweather/internal/store"
)

func coverageNow() time.Time {
	return time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
}

func coverageLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met before the deadline")
}

// recordingSink is a small LevelSink/Sink double for the internal tests.
type recordingSink struct {
	mu         sync.Mutex
	events     []store.ExternalEvent
	levels     []store.CountryLevel
	successes  int
	failures   []string
	upsertErr  error
	successErr error
}

func (s *recordingSink) UpsertExternalEvents(_ context.Context, events []store.ExternalEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.upsertErr != nil {
		return s.upsertErr
	}
	s.events = append(s.events, events...)
	return nil
}

func (s *recordingSink) UpsertCountryLevels(_ context.Context, levels []store.CountryLevel) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.upsertErr != nil {
		return s.upsertErr
	}
	s.levels = append(s.levels, levels...)
	return nil
}

func (s *recordingSink) RecordFeedSuccess(context.Context, string, time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.successErr != nil {
		return s.successErr
	}
	s.successes++
	return nil
}

func (s *recordingSink) RecordFeedFailure(_ context.Context, _, detail string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, detail)
	return nil
}

func (s *recordingSink) SetFeedNextPoll(context.Context, string, time.Time) error { return nil }

func (s *recordingSink) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events) + len(s.levels), s.successes
}

func (s *recordingSink) failureCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.failures)
}

// scriptConn replays frames, then blocks until Close. It can fail a chosen
// write so subscribe, pong, and store-flush failures are all reachable.
type scriptConn struct {
	mu          sync.Mutex
	frames      [][]byte
	idx         int
	writes      int
	failWriteOn int
	release     chan struct{}
	once        sync.Once
}

func newScriptConn(frames ...[]byte) *scriptConn {
	return &scriptConn{frames: frames, release: make(chan struct{})}
}

func (c *scriptConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if c.idx < len(c.frames) {
		frame := c.frames[c.idx]
		c.idx++
		c.mu.Unlock()
		return copy(p, frame), nil
	}
	c.mu.Unlock()
	<-c.release
	return 0, io.EOF
}

func (c *scriptConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes++
	if c.failWriteOn == c.writes {
		return 0, errors.New("write rejected")
	}
	return len(p), nil
}

func (c *scriptConn) Close() error {
	c.once.Do(func() { close(c.release) })
	return nil
}

func risFrame(id string) []byte {
	return []byte(fmt.Sprintf(
		`{"type":"ris_message","data":{"type":"UPDATE","host":"rrc00","id":%q,"timestamp":1763000000,"peer_asn":"64496","path":[15169]}}`,
		id,
	))
}

func TestSeverityHelpersCoverEveryVocabulary(t *testing.T) {
	t.Parallel()

	statuspageCases := []struct {
		raw  map[string]any
		want float64
	}{
		{map[string]any{"impact": "critical"}, 1.0},
		{map[string]any{"impact": "major"}, 0.8},
		{map[string]any{"impact": "minor"}, 0.4},
		{map[string]any{"impact": "MAINTENANCE"}, 0.2},
		{map[string]any{"status": "resolved"}, 0.1},
		{map[string]any{"status": "investigating"}, 0.3},
	}
	for _, tc := range statuspageCases {
		if got := statuspageSeverity(tc.raw); got != tc.want {
			t.Errorf("statuspageSeverity(%v) = %v, want %v", tc.raw, got, tc.want)
		}
	}

	radarCases := []struct {
		raw  map[string]any
		want float64
	}{
		{map[string]any{"score": 0.42}, 0.42},
		{map[string]any{"score": 4.2, "confidence": 0.25}, 0.25},
		{map[string]any{"confidence": "very_high"}, 0.9},
		{map[string]any{"severity": "CRITICAL"}, 0.9},
		{map[string]any{"status": "moderate"}, 0.6},
		{map[string]any{"confidence": "low"}, 0.3},
		{map[string]any{"outage_type": "regional"}, 0.7},
		{map[string]any{"scope": "local"}, 0.4},
		{map[string]any{"note": "unknown"}, 0},
	}
	for _, tc := range radarCases {
		if got := radarSeverity(tc.raw); got != tc.want {
			t.Errorf("radarSeverity(%v) = %v, want %v", tc.raw, got, tc.want)
		}
	}

	rssCases := []struct {
		title, description string
		want               float64
	}{
		{"Service outage in EC2", "", 1.0},
		{"API unavailable", "", 1.0},
		{"Service issue", "", 0.8},
		{"Elevated error rates", "", 0.7},
		{"Degraded service", "", 0.5},
		{"Slow performance", "", 0.5},
		{"Planned advisory", "", 0.2},
		{"Routine note", "", 0.3},
	}
	for _, tc := range rssCases {
		if got := rssSeverity(tc.title, tc.description); got != tc.want {
			t.Errorf("rssSeverity(%q) = %v, want %v", tc.title, got, tc.want)
		}
	}

	gcpCases := []struct {
		raw  map[string]any
		want float64
	}{
		{map[string]any{"status_impact": "SERVICE_DISRUPTION"}, 1.0},
		{map[string]any{"status_impact": "service_information"}, 0.2},
		{map[string]any{"status_impact": "SOMETHING_ELSE"}, 0.3},
	}
	for _, tc := range gcpCases {
		if got := gcpSeverity(tc.raw); got != tc.want {
			t.Errorf("gcpSeverity(%v) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestRelativeLevelClampsBothEnds(t *testing.T) {
	t.Parallel()

	if got := relativeLevel(0, 5); got != 0 {
		t.Errorf("zero score = %v, want 0", got)
	}
	if got := relativeLevel(5, -1); got != 0 {
		t.Errorf("non-positive max = %v, want 0", got)
	}
	if got := relativeLevel(10, 1); got != 1 {
		t.Errorf("score above max = %v, want 1", got)
	}
	if got := relativeLevel(2, 10); got <= 0 || got >= 1 {
		t.Errorf("interior level = %v, want between 0 and 1", got)
	}
}

func TestASNForMessageFallsBackAndRejectsJunk(t *testing.T) {
	t.Parallel()

	path := []json.RawMessage{
		json.RawMessage(`"not an asn"`),
		json.RawMessage(`[3356,3357]`),
		json.RawMessage(`15169`),
	}
	if got := asnForMessage(path, []int{3356}, ""); got != 3356 {
		t.Errorf("configured ASN inside an AS set = %d, want 3356", got)
	}
	if got := asnForMessage(path, []int{999}, " 64496 "); got != 64496 {
		t.Errorf("peer ASN fallback = %d, want 64496", got)
	}
	if got := asnForMessage(path, nil, "not-an-asn"); got != 0 {
		t.Errorf("unparseable peer ASN = %d, want 0", got)
	}
	if got := asnForMessage(path, nil, "-5"); got != 0 {
		t.Errorf("non-positive peer ASN = %d, want 0", got)
	}
	if got := asnForMessage(nil, nil, ""); got != 0 {
		t.Errorf("empty path and peer = %d, want 0", got)
	}
}

func TestParseFrameRejectsMalformedDataAndAcceptsAnnouncements(t *testing.T) {
	t.Parallel()

	stream := &RISLiveWS{ASNs: []int{15169}, Now: coverageNow}
	for _, frame := range [][]byte{
		[]byte(`not json`),
		[]byte(`{"type":"ping","data":null}`),
		[]byte(`{"type":"ris_message","data":123}`),
		[]byte(`{"type":"ris_message","data":{"type":"UPDATE","host":"rrc00","timestamp":1763000000}}`),
	} {
		if _, ok := stream.parseFrame(frame); ok {
			t.Errorf("frame %s should have been rejected", frame)
		}
	}

	announcement := []byte(`{"type":"ris_message","data":{
		"type":"update","host":"rrc00","id":"u-ann","timestamp":0,
		"peer_asn":"64496","path":[15169],
		"announcements":[{"next_hop":"192.0.2.1","prefixes":["198.51.100.0/24"]}]
	}}`)
	event, ok := stream.parseFrame(announcement)
	if !ok {
		t.Fatal("announcement-only UPDATE was rejected")
	}
	if event.Kind != "bgp_update" || event.Severity != 0 {
		t.Errorf("event = %+v, want a plain bgp_update", event)
	}
	if event.ASN != 15169 || event.ExternalID != "risws:rrc00:u-ann" {
		t.Errorf("identity = %q, asn = %d", event.ExternalID, event.ASN)
	}
	if !event.StartedAt.Equal(coverageNow()) {
		t.Errorf("a zero upstream timestamp should fall back to now, got %v", event.StartedAt)
	}
	if !strings.Contains(event.Payload, "198.51.100.0/24") {
		t.Errorf("announced prefix missing from payload: %s", event.Payload)
	}
}

type failingResolver struct{}

func (failingResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, errors.New("resolver unavailable")
}

func TestOpenRejectsBadURLsAndDialErrors(t *testing.T) {
	t.Parallel()

	dialer := &netx.Dialer{Resolver: failingResolver{}}
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"bad escape", "ws://%zz", "url"},
		{"no host", "ws:///stream", "no host"},
		{"wrong scheme", "http://example.com/stream", "must be ws(s)"},
		{"ws dial fails", "ws://example.com/stream", "dial"},
		{"wss dial fails", "wss://example.com/stream", "dial"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stream := &RISLiveWS{URL: tc.url, Dialer: dialer}
			if _, err := stream.open(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("open(%q) = %v, want an error containing %q", tc.url, err, tc.want)
			}
		})
	}

	var dialed string
	stream := &RISLiveWS{Dial: func(_ context.Context, rawURL string) (WSConn, error) {
		dialed = rawURL
		return nil, errors.New("dial seam failed")
	}}
	if _, err := stream.open(context.Background()); err == nil {
		t.Fatal("the dial seam error was swallowed")
	}
	if dialed != DefaultRISLiveWSURL {
		t.Errorf("dialed %q, want the default URL %q", dialed, DefaultRISLiveWSURL)
	}
}

func TestRunOnceCoversWriteAndFlushFailures(t *testing.T) {
	t.Parallel()

	t.Run("open fails", func(t *testing.T) {
		t.Parallel()
		stream := &RISLiveWS{
			ASNs: []int{15169}, Sink: &recordingSink{},
			Dial: func(context.Context, string) (WSConn, error) {
				return nil, errors.New("dial refused")
			},
		}
		if _, err := stream.runOnce(context.Background(), coverageLogger()); err == nil || !strings.Contains(err.Error(), "dial refused") {
			t.Errorf("error = %v, want the dial failure", err)
		}
	})

	t.Run("subscribe write fails", func(t *testing.T) {
		t.Parallel()
		conn := newScriptConn()
		conn.failWriteOn = 1
		stream := &RISLiveWS{
			ASNs: []int{15169}, Sink: &recordingSink{},
			Dial: func(context.Context, string) (WSConn, error) { return conn, nil },
		}
		if _, err := stream.runOnce(context.Background(), coverageLogger()); err == nil || !strings.Contains(err.Error(), "subscribe write") {
			t.Errorf("error = %v, want the subscribe write failure", err)
		}
	})

	t.Run("pong write fails", func(t *testing.T) {
		t.Parallel()
		conn := newScriptConn([]byte(`{"type":"ping","data":null}`))
		conn.failWriteOn = 2
		stream := &RISLiveWS{
			ASNs: []int{15169}, Sink: &recordingSink{},
			Dial: func(context.Context, string) (WSConn, error) { return conn, nil },
		}
		if _, err := stream.runOnce(context.Background(), coverageLogger()); err == nil || !strings.Contains(err.Error(), "pong") {
			t.Errorf("error = %v, want the pong write failure", err)
		}
	})

	t.Run("max batch flushes", func(t *testing.T) {
		t.Parallel()
		sink := &recordingSink{}
		conn := newScriptConn(risFrame("u1"), risFrame("u2"))
		stream := &RISLiveWS{
			ASNs: []int{15169}, Sink: sink, MaxBatch: 1, FlushEvery: time.Hour,
			Dial: func(context.Context, string) (WSConn, error) { return conn, nil },
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := stream.runOnce(ctx, coverageLogger())
			done <- err
		}()

		waitForCondition(t, 2*time.Second, func() bool {
			events, successes := sink.counts()
			return events >= 2 && successes >= 2
		})
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("runOnce: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("runOnce did not stop after cancel")
		}
		events, successes := sink.counts()
		if events != 2 || successes != 2 {
			t.Errorf("events=%d successes=%d, want two of each", events, successes)
		}
	})

	t.Run("flush errors surface", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name string
			sink *recordingSink
			want string
		}{
			{"upsert", &recordingSink{upsertErr: errors.New("disk full")}, "disk full"},
			{"success", &recordingSink{successErr: errors.New("health not recorded")}, "health not recorded"},
		}
		for _, tc := range cases {
			conn := newScriptConn(risFrame("u3"))
			stream := &RISLiveWS{
				ASNs: []int{15169}, Sink: tc.sink, MaxBatch: 1, FlushEvery: time.Hour,
				Dial: func(context.Context, string) (WSConn, error) { return conn, nil },
			}
			if _, err := stream.runOnce(context.Background(), coverageLogger()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s: error = %v, want %q", tc.name, err, tc.want)
			}
		}
	})
}

type loopFetcher struct {
	calls  atomic.Int64
	levels []store.CountryLevel
	err    error
}

func (f *loopFetcher) FetchLevels(context.Context) ([]store.CountryLevel, error) {
	f.calls.Add(1)
	return f.levels, f.err
}

func TestLevelLoopPollsImmediatelyOnScheduleAndStops(t *testing.T) {
	t.Parallel()

	t.Run("success with the default logger", func(t *testing.T) {
		t.Parallel()
		fetcher := &loopFetcher{levels: []store.CountryLevel{{Country: "IR", Level: 1}}}
		sink := &recordingSink{}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			LevelLoop(ctx, &LevelPoller{
				Feed: "ioda-level", Source: fetcher, Sink: sink,
				Every: time.Millisecond, Now: coverageNow,
			}, nil)
			close(done)
		}()

		waitForCondition(t, 2*time.Second, func() bool { return fetcher.calls.Load() >= 2 })
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("LevelLoop did not stop after cancel")
		}
		events, successes := sink.counts()
		if events == 0 || successes == 0 {
			t.Errorf("events=%d successes=%d, want stored levels", events, successes)
		}
	})

	t.Run("failure with an injected logger", func(t *testing.T) {
		t.Parallel()
		fetcher := &loopFetcher{err: errors.New("summary unavailable")}
		sink := &recordingSink{}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			LevelLoop(ctx, &LevelPoller{
				Feed: "ioda-level", Source: fetcher, Sink: sink,
				Every: time.Hour, Now: coverageNow,
			}, coverageLogger())
			close(done)
		}()

		waitForCondition(t, 2*time.Second, func() bool { return sink.failureCount() > 0 })
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("LevelLoop did not stop after cancel")
		}
		if fetcher.calls.Load() == 0 {
			t.Fatal("LevelLoop never polled")
		}
	})
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("transport unavailable")
}

var _ http.RoundTripper = failingTransport{}

func failingClient() *http.Client {
	return &http.Client{Transport: failingTransport{}}
}
