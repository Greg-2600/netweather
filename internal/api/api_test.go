package api_test

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/api"
	"github.com/Greg-2600/netweather/internal/baseline"
	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/logging"
	"github.com/Greg-2600/netweather/internal/probe"
	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
	"github.com/Greg-2600/netweather/internal/streamlimit"
)

const testToken = "test-token-0123456789"

func newHandler(t *testing.T, token string, ring *logging.Ring) http.Handler {
	t.Helper()

	h, err := api.New(
		api.Config{Listen: "127.0.0.1:2600", Token: token},
		api.Deps{
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Ring:      ring,
			Version:   "test",
			Commit:    "deadbeef",
			StartedAt: time.Now().Add(-90 * time.Second),
		},
	)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h
}

func do(h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type errorEnvelope struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

type dataEnvelope struct {
	Data json.RawMessage `json:"data"`
	Meta struct {
		RequestID string `json:"request_id"`
		Count     int    `json:"count"`
		Limit     int    `json:"limit"`
	} `json:"meta"`
}

func TestHealthzNeedsNoAuthAndLeaksNothing(t *testing.T) {
	t.Parallel()

	rec := do(newHandler(t, testToken, nil), http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	body := rec.Body.String()
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("healthz is not JSON: %v", err)
	}
	if got["status"] != "ok" {
		t.Fatalf("status field = %v", got["status"])
	}
	for _, forbidden := range []string{"target", "vantage", "capabilities", "asn"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Errorf("healthz discloses %q", forbidden)
		}
	}
}

func TestMetaRequiresBearer(t *testing.T) {
	t.Parallel()

	rec := do(newHandler(t, testToken, nil), http.MethodGet, "/api/v1/meta", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q", got)
	}

	var e errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	if e.Error.Code != api.CodeUnauthorized {
		t.Fatalf("code = %q, want %q", e.Error.Code, api.CodeUnauthorized)
	}
}

func TestMetaWithToken(t *testing.T) {
	t.Parallel()

	ring := logging.NewRing(50, 0)
	rec := do(newHandler(t, testToken, ring), http.MethodGet, "/api/v1/meta", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}

	var env dataEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var meta struct {
		API          string   `json:"api"`
		Auth         string   `json:"auth"`
		Capabilities []string `json:"capabilities"`
		Ring         struct {
			Capacity int `json:"capacity"`
		} `json:"ring"`
	}
	if err := json.Unmarshal(env.Data, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.API != "v1" || meta.Auth != "bearer" {
		t.Fatalf("meta = %+v", meta)
	}
	if meta.Ring.Capacity != 50 {
		t.Fatalf("ring capacity = %d", meta.Ring.Capacity)
	}
	found := false
	for _, c := range meta.Capabilities {
		if c == "logs" {
			found = true
		}
	}
	if !found {
		t.Fatalf("logs missing from capabilities: %v", meta.Capabilities)
	}
	if env.Meta.RequestID == "" {
		t.Error("request_id missing from envelope")
	}
}

func TestWrongTokenIsRejected(t *testing.T) {
	t.Parallel()

	rec := do(newHandler(t, testToken, nil), http.MethodGet, "/api/v1/meta", "not-the-token-000000")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAuthNoneOnLoopback(t *testing.T) {
	t.Parallel()

	rec := do(newHandler(t, "", nil), http.MethodGet, "/api/v1/meta", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 on a loopback-only, tokenless instance", rec.Code)
	}

	var env dataEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	var meta struct {
		Auth string `json:"auth"`
	}
	_ = json.Unmarshal(env.Data, &meta)
	if meta.Auth != "none" {
		t.Fatalf("auth = %q, want none", meta.Auth)
	}
}

func seedRing(t *testing.T) *logging.Ring {
	t.Helper()

	r := logging.NewRing(100, 0)
	add := func(level slog.Level, msg string, attrs ...slog.Attr) {
		rec := slog.NewRecord(time.Now().UTC(), level, msg, 0)
		rec.AddAttrs(attrs...)
		if err := r.Handle(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
	}
	add(slog.LevelInfo, "state change",
		slog.String("target", "chase.com"), slog.String("state_to", "DOWN"), slog.String("token", "leak-me"))
	add(slog.LevelWarn, "feed stale", slog.String("feed", "radar"))
	add(slog.LevelDebug, "probe", slog.String("target", "united.com"))
	return r
}

func TestLogsFiltering(t *testing.T) {
	t.Parallel()

	h := newHandler(t, testToken, seedRing(t))

	all := do(h, http.MethodGet, "/api/v1/logs", testToken)
	if all.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", all.Code, all.Body)
	}
	var env dataEnvelope
	if err := json.Unmarshal(all.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var records []struct {
		Level   string            `json:"level"`
		Message string            `json:"message"`
		Attrs   map[string]string `json:"attrs"`
	}
	if err := json.Unmarshal(env.Data, &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("got %d records, want 3", len(records))
	}
	if strings.Contains(all.Body.String(), "leak-me") {
		t.Fatal("redaction failed: secret reached the API")
	}

	warnOnly := do(h, http.MethodGet, "/api/v1/logs?level=warn", testToken)
	_ = json.Unmarshal(warnOnly.Body.Bytes(), &env)
	_ = json.Unmarshal(env.Data, &records)
	if len(records) != 1 || records[0].Message != "feed stale" {
		t.Fatalf("level filter returned %#v", records)
	}

	byTarget := do(h, http.MethodGet, "/api/v1/logs?target=united", testToken)
	_ = json.Unmarshal(byTarget.Body.Bytes(), &env)
	_ = json.Unmarshal(env.Data, &records)
	if len(records) != 1 || records[0].Attrs["target"] != "united.com" {
		t.Fatalf("target filter returned %#v", records)
	}

	limited := do(h, http.MethodGet, "/api/v1/logs?limit=1", testToken)
	_ = json.Unmarshal(limited.Body.Bytes(), &env)
	_ = json.Unmarshal(env.Data, &records)
	if len(records) != 1 || records[0].Message != "probe" {
		t.Fatalf("limit should keep the newest record, got %#v", records)
	}

	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	none := do(h, http.MethodGet, "/api/v1/logs?since="+url.QueryEscape(future), testToken)
	_ = json.Unmarshal(none.Body.Bytes(), &env)
	_ = json.Unmarshal(env.Data, &records)
	if len(records) != 0 {
		t.Fatalf("since in the future returned %d records", len(records))
	}

	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	sincePast := do(h, http.MethodGet, "/api/v1/logs?since="+url.QueryEscape(past), testToken)
	_ = json.Unmarshal(sincePast.Body.Bytes(), &env)
	_ = json.Unmarshal(env.Data, &records)
	if len(records) != 3 {
		t.Fatalf("since an hour ago returned %d records, want 3", len(records))
	}
}

func TestLogsRejectsBadParameters(t *testing.T) {
	t.Parallel()

	h := newHandler(t, testToken, nil)
	for _, q := range []string{"limit=0", "limit=501", "limit=abc", "since=yesterday", "level=loud"} {
		rec := do(h, http.MethodGet, "/api/v1/logs?"+q, testToken)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, rec.Code)
			continue
		}
		var e errorEnvelope
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		if e.Error.Code != api.CodeInvalidArgument {
			t.Errorf("%s: code = %q", q, e.Error.Code)
		}
	}
}

func TestUnknownPathIsJSON404(t *testing.T) {
	t.Parallel()

	rec := do(newHandler(t, testToken, nil), http.MethodGet, "/api/v1/nope", testToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	var e errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("404 body is not JSON: %v", err)
	}
	if e.Error.Code != api.CodeNotFound {
		t.Fatalf("code = %q", e.Error.Code)
	}
}

func TestWrongMethodIsJSON405(t *testing.T) {
	t.Parallel()

	rec := do(newHandler(t, testToken, nil), http.MethodPost, "/api/v1/meta", testToken)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Allow"); !strings.Contains(got, "GET") {
		t.Errorf("Allow = %q", got)
	}
	var e errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Code != api.CodeMethodNotAllowed {
		t.Fatalf("code = %q", e.Error.Code)
	}
}

func TestRequestIDIsEchoed(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-Id", "abc-123_XYZ")
	rec := httptest.NewRecorder()
	newHandler(t, testToken, nil).ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Request-Id"); got != "abc-123_XYZ" {
		t.Fatalf("X-Request-Id = %q", got)
	}
}

func TestHostileRequestIDIsReplaced(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-Id", "<script>alert(1)</script>")
	rec := httptest.NewRecorder()
	newHandler(t, testToken, nil).ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Request-Id"); got == "<script>alert(1)</script>" || got == "" {
		t.Fatalf("hostile request id was not replaced: %q", got)
	}
}

func TestAPIRejectsBurstWith429(t *testing.T) {
	t.Parallel()

	h := newHandler(t, testToken, nil)

	for i := 0; i < 21; i++ {
		rec := do(h, http.MethodGet, "/api/v1/meta", testToken)
		if i < 20 {
			if rec.Code != http.StatusOK {
				t.Fatalf("request %d: status %d, want 200", i+1, rec.Code)
			}
			continue
		}
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("request 21: status %d, want 429", rec.Code)
		}
		if rec.Header().Get("Retry-After") == "" {
			t.Error("429 without Retry-After")
		}
	}
}

func newBoardHandler(t *testing.T, src board.Source) http.Handler {
	t.Helper()

	h, err := api.New(
		api.Config{Listen: "127.0.0.1:2600", Token: testToken},
		api.Deps{
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Source:    src,
			Version:   "test",
			Commit:    "deadbeef",
			StartedAt: time.Now(),
		},
	)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h
}

func TestBoardReturnsTargets(t *testing.T) {
	t.Parallel()

	since := time.Date(2026, 11, 17, 3, 14, 15, 0, time.UTC)
	m := board.NewMemory(
		board.Target{
			ID: "1", Name: "Chase", Host: "chase.com", Sector: "financial/banking",
			Country: "US", State: board.StateUp, Since: since,
		},
		board.Target{ID: "2", Name: "United", Host: "united.com", State: board.StateDown},
	)

	rec := do(newBoardHandler(t, m), http.MethodGet, "/api/v1/board", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}

	var env dataEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var items []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Host   string `json:"host"`
		Sector string `json:"sector"`
		State  string `json:"state"`
		Since  string `json:"since"`
	}
	if err := json.Unmarshal(env.Data, &items); err != nil {
		t.Fatal(err)
	}

	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if items[0].State != "up" || items[0].Name != "Chase" {
		t.Fatalf("first item = %+v", items[0])
	}
	if items[0].Since == "" {
		t.Error("since missing for a probed target")
	}
	if items[1].State != "down" || items[1].Since != "" {
		t.Fatalf("second item = %+v", items[1])
	}
	if env.Meta.Count != 2 {
		t.Errorf("meta.count = %d", env.Meta.Count)
	}
}

func TestBoardRequiresAuth(t *testing.T) {
	t.Parallel()

	rec := do(newBoardHandler(t, board.NewMemory()), http.MethodGet, "/api/v1/board", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestBoardWithoutSourceIsUnavailable(t *testing.T) {
	t.Parallel()

	rec := do(newHandler(t, testToken, nil), http.MethodGet, "/api/v1/board", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	var e errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Code != api.CodeUnavailable {
		t.Fatalf("code = %q", e.Error.Code)
	}
}

type fakeControl struct {
	mu      sync.Mutex
	paused  bool
	pauses  int
	resumes int
}

func (f *fakeControl) Pause() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = true
	f.pauses++
}

func (f *fakeControl) Resume() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = false
	f.resumes++
}

func (f *fakeControl) Paused() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused
}

func newControlHandler(t *testing.T, c api.Control) http.Handler {
	t.Helper()

	h, err := api.New(
		api.Config{Listen: "127.0.0.1:2600", Token: testToken},
		api.Deps{
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Control:   c,
			Version:   "test",
			Commit:    "deadbeef",
			StartedAt: time.Now(),
		},
	)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h
}

func TestControlRoundTrip(t *testing.T) {
	t.Parallel()

	c := &fakeControl{}
	h := newControlHandler(t, c)

	readPaused := func() bool {
		rec := do(h, http.MethodGet, "/api/v1/control", testToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("control status = %d", rec.Code)
		}
		var env dataEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		var state struct {
			Paused bool `json:"paused"`
		}
		if err := json.Unmarshal(env.Data, &state); err != nil {
			t.Fatal(err)
		}
		return state.Paused
	}

	if readPaused() {
		t.Fatal("control starts paused")
	}

	rec := do(h, http.MethodPost, "/api/v1/pause", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("pause status = %d", rec.Code)
	}
	if !readPaused() {
		t.Fatal("pause did not take effect")
	}

	rec = do(h, http.MethodPost, "/api/v1/resume", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume status = %d", rec.Code)
	}
	if readPaused() {
		t.Fatal("resume did not take effect")
	}

	if c.pauses != 1 || c.resumes != 1 {
		t.Fatalf("pauses=%d resumes=%d", c.pauses, c.resumes)
	}
}

func TestControlRequiresAuth(t *testing.T) {
	t.Parallel()

	h := newControlHandler(t, &fakeControl{})
	for _, route := range []string{"/api/v1/control", "/api/v1/pause", "/api/v1/resume"} {
		method := http.MethodGet
		if route != "/api/v1/control" {
			method = http.MethodPost
		}
		rec := do(h, method, route, "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401", method, route, rec.Code)
		}
	}
}

func TestControlUnavailableWithoutImplementation(t *testing.T) {
	t.Parallel()

	h := newHandler(t, testToken, nil)
	rec := do(h, http.MethodPost, "/api/v1/pause", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestPauseRejectsGET(t *testing.T) {
	t.Parallel()

	rec := do(newControlHandler(t, &fakeControl{}), http.MethodGet, "/api/v1/pause", testToken)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "POST" {
		t.Fatalf("Allow = %q", got)
	}
}

type fakeProbes struct {
	rows      map[string][]store.ProbeRow
	series    []store.Rollup
	existsErr error
	rowsErr   error
	seriesErr error
}

func (f *fakeProbes) Series(context.Context, string, string, string, time.Time, time.Time) ([]store.Rollup, error) {
	return f.series, f.seriesErr
}

func (f *fakeProbes) RecentProbes(_ context.Context, id, _ string, limit int) ([]store.ProbeRow, error) {
	if f.rowsErr != nil {
		return nil, f.rowsErr
	}
	rows := f.rows[id]
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func (f *fakeProbes) TargetExists(_ context.Context, id string) (bool, error) {
	if f.existsErr != nil {
		return false, f.existsErr
	}
	_, ok := f.rows[id]
	return ok, nil
}

func newProbesHandler(t *testing.T, p api.ProbeReader) http.Handler {
	t.Helper()

	h, err := api.New(
		api.Config{Listen: "127.0.0.1:2600", Token: testToken},
		api.Deps{
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Probes:    p,
			Vantage:   "local",
			Version:   "test",
			Commit:    "deadbeef",
			StartedAt: time.Now(),
		},
	)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h
}

func probeFixture() *fakeProbes {
	return &fakeProbes{rows: map[string][]store.ProbeRow{
		"t1": {
			{
				TS:    time.Date(2026, 11, 17, 12, 1, 0, 0, time.UTC),
				State: state.Down, Stage: "tls", ErrorClass: "tls_cert_expired",
				LatencyMS: 41, Confidence: "high", Detail: "certificate expired",
			},
			{
				TS:    time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC),
				State: state.Up, Stage: "http", HTTPStatus: 200,
				LatencyMS: 80, Confidence: "high",
			},
		},
	}}
}

func TestTargetProbesReturnsHistory(t *testing.T) {
	t.Parallel()

	h := newProbesHandler(t, probeFixture())
	rec := do(h, http.MethodGet, "/api/v1/targets/t1/probes", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}

	var env dataEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var items []struct {
		TS         string `json:"ts"`
		State      string `json:"state"`
		Stage      string `json:"stage"`
		ErrorClass string `json:"error_class"`
		Confidence string `json:"confidence"`
	}
	if err := json.Unmarshal(env.Data, &items); err != nil {
		t.Fatal(err)
	}

	if len(items) != 2 {
		t.Fatalf("items = %d", len(items))
	}
	if items[0].State != "down" || items[0].Stage != "tls" || items[0].ErrorClass != "tls_cert_expired" {
		t.Fatalf("newest item = %+v", items[0])
	}
	if items[1].State != "up" {
		t.Fatalf("second item = %+v", items[1])
	}
	if env.Meta.Limit != 20 {
		t.Errorf("meta.limit = %d, want the default 20", env.Meta.Limit)
	}
}

func TestTargetProbesLimit(t *testing.T) {
	t.Parallel()

	h := newProbesHandler(t, probeFixture())
	rec := do(h, http.MethodGet, "/api/v1/targets/t1/probes?limit=1", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	var env dataEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	var items []map[string]any
	_ = json.Unmarshal(env.Data, &items)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if env.Meta.Limit != 1 {
		t.Fatalf("meta.limit = %d", env.Meta.Limit)
	}
}

func TestTargetProbesUnknownTarget(t *testing.T) {
	t.Parallel()

	rec := do(newProbesHandler(t, probeFixture()), http.MethodGet, "/api/v1/targets/nope/probes", testToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestTargetSubtreeRejectsUnknownSuffix(t *testing.T) {
	t.Parallel()

	rec := do(newProbesHandler(t, probeFixture()), http.MethodGet, "/api/v1/targets/t1/bogus", testToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestTargetProbesRejectsBadLimit(t *testing.T) {
	t.Parallel()

	rec := do(newProbesHandler(t, probeFixture()), http.MethodGet, "/api/v1/targets/t1/probes?limit=0", testToken)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestTargetProbesUnavailableWithoutReader(t *testing.T) {
	t.Parallel()

	rec := do(newHandler(t, testToken, nil), http.MethodGet, "/api/v1/targets/t1/probes", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestTargetProbesRequiresAuth(t *testing.T) {
	t.Parallel()

	rec := do(newProbesHandler(t, probeFixture()), http.MethodGet, "/api/v1/targets/t1/probes", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

type fakeIncidents struct {
	items    []store.Incident
	attrs    []store.Attribution
	targets  []string
	lastOpen bool
	err      error
}

func (f *fakeIncidents) Incidents(_ context.Context, openOnly bool, _ int) ([]store.Incident, error) {
	f.lastOpen = openOnly
	return f.items, f.err
}

func (f *fakeIncidents) IncidentByID(_ context.Context, id string) (store.Incident, []store.Attribution, []string, error) {
	if f.err != nil {
		return store.Incident{}, nil, nil, f.err
	}
	for _, inc := range f.items {
		if inc.ID == id {
			return inc, f.attrs, f.targets, nil
		}
	}
	return store.Incident{}, nil, nil, sql.ErrNoRows
}

func newIncidentsHandler(t *testing.T, reader api.IncidentReader) http.Handler {
	t.Helper()

	h, err := api.New(
		api.Config{Listen: "127.0.0.1:2600", Token: testToken},
		api.Deps{
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Incidents: reader,
			Version:   "test",
			Commit:    "deadbeef",
			StartedAt: time.Now(),
		},
	)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h
}

func incidentFixture() *fakeIncidents {
	opened := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	return &fakeIncidents{
		items: []store.Incident{
			{
				ID: "inc-1", OpenedAt: opened, Verdict: "INCIDENT",
				Scope: "sector", ScopeValue: "airline",
				Headline: "5 of 6 targets failing on sector=airline", Confidence: "high",
			},
		},
		attrs: []store.Attribution{
			{Rank: 1, Kind: "sector", Value: "airline", Failing: 5, Total: 6, Share: 5.0 / 6.0, Score: 1.5},
		},
		targets: []string{"t1", "t2"},
	}
}

func TestIncidentsList(t *testing.T) {
	t.Parallel()

	reader := incidentFixture()
	rec := do(newIncidentsHandler(t, reader), http.MethodGet, "/api/v1/incidents?open=true", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if !reader.lastOpen {
		t.Error("open=true was not passed through")
	}

	var env dataEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var items []struct {
		ID         string `json:"id"`
		Scope      string `json:"scope"`
		ScopeValue string `json:"scope_value"`
		Headline   string `json:"headline"`
		OpenedAt   string `json:"opened_at"`
	}
	if err := json.Unmarshal(env.Data, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d", len(items))
	}
	if items[0].ID != "inc-1" || items[0].Scope != "sector" || items[0].ScopeValue != "airline" {
		t.Fatalf("item = %+v", items[0])
	}
	if items[0].OpenedAt == "" {
		t.Error("opened_at missing")
	}
	if env.Meta.Count != 1 {
		t.Errorf("meta.count = %d", env.Meta.Count)
	}
}

func TestIncidentByID(t *testing.T) {
	t.Parallel()

	rec := do(newIncidentsHandler(t, incidentFixture()), http.MethodGet, "/api/v1/incidents/inc-1", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}

	var env dataEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var detail struct {
		Incident struct {
			ID string `json:"id"`
		} `json:"incident"`
		Attribution []struct {
			Kind  string  `json:"kind"`
			Value string  `json:"value"`
			Share float64 `json:"share"`
		} `json:"attribution"`
		Targets []string `json:"targets"`
	}
	if err := json.Unmarshal(env.Data, &detail); err != nil {
		t.Fatal(err)
	}

	if detail.Incident.ID != "inc-1" {
		t.Fatalf("incident = %+v", detail.Incident)
	}
	if len(detail.Attribution) != 1 || detail.Attribution[0].Value != "airline" {
		t.Fatalf("attribution = %+v", detail.Attribution)
	}
	if len(detail.Targets) != 2 {
		t.Fatalf("targets = %v", detail.Targets)
	}
}

func TestIncidentByIDMissing(t *testing.T) {
	t.Parallel()

	rec := do(newIncidentsHandler(t, incidentFixture()), http.MethodGet, "/api/v1/incidents/nope", testToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestIncidentsRejectBadLimit(t *testing.T) {
	t.Parallel()

	rec := do(newIncidentsHandler(t, incidentFixture()), http.MethodGet, "/api/v1/incidents?limit=0", testToken)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestIncidentsUnavailableWithoutReader(t *testing.T) {
	t.Parallel()

	rec := do(newHandler(t, testToken, nil), http.MethodGet, "/api/v1/incidents", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestIncidentsRequireAuth(t *testing.T) {
	t.Parallel()

	rec := do(newIncidentsHandler(t, incidentFixture()), http.MethodGet, "/api/v1/incidents", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

type failingSource struct{}

func (failingSource) Snapshot(context.Context) ([]board.Target, error) {
	return nil, errors.New("boom")
}

func TestBoardSourceFailureIsUnavailable(t *testing.T) {
	t.Parallel()

	rec := do(newBoardHandler(t, failingSource{}), http.MethodGet, "/api/v1/board", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestIncidentsReaderFailureIsUnavailable(t *testing.T) {
	t.Parallel()

	reader := incidentFixture()
	reader.err = errors.New("db down")

	rec := do(newIncidentsHandler(t, reader), http.MethodGet, "/api/v1/incidents", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("list status = %d, want 503", rec.Code)
	}

	rec = do(newIncidentsHandler(t, reader), http.MethodGet, "/api/v1/incidents/inc-1", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("detail status = %d, want 503", rec.Code)
	}
}

func TestIncidentPathRejectsNestedAndEmptyIDs(t *testing.T) {
	t.Parallel()

	reader := incidentFixture()
	for _, path := range []string{"/api/v1/incidents/", "/api/v1/incidents/a/b"} {
		rec := do(newIncidentsHandler(t, reader), http.MethodGet, path, testToken)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
	}
}

func TestTargetProbesReaderErrorsAreUnavailable(t *testing.T) {
	t.Parallel()

	reader := &fakeProbes{rows: map[string][]store.ProbeRow{"t1": nil}, existsErr: errors.New("boom")}
	rec := do(newProbesHandler(t, reader), http.MethodGet, "/api/v1/targets/t1/probes", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("exists status = %d, want 503", rec.Code)
	}

	reader = &fakeProbes{rows: map[string][]store.ProbeRow{"t1": nil}, rowsErr: errors.New("boom")}
	rec = do(newProbesHandler(t, reader), http.MethodGet, "/api/v1/targets/t1/probes", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("rows status = %d, want 503", rec.Code)
	}
}

func TestMetricsMountsOnlyWhenConfigured(t *testing.T) {
	t.Parallel()

	// Without a handler the route does not exist.
	rec := do(newHandler(t, testToken, nil), http.MethodGet, "/metrics", testToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when metrics are not mounted", rec.Code)
	}

	body := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("netweather_targets{state=\"down\"} 1\n"))
	})
	h, err := api.New(
		api.Config{Listen: "127.0.0.1:2600", Token: testToken},
		api.Deps{
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Metrics:   body,
			Version:   "test",
			Commit:    "deadbeef",
			StartedAt: time.Now(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	if rec := do(h, http.MethodGet, "/metrics", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated metrics status = %d, want 401", rec.Code)
	}
	rec = do(h, http.MethodGet, "/metrics", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "netweather_targets") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func targetsFixture(t *testing.T) http.Handler {
	m := board.NewMemory(
		board.Target{ID: "1", Name: "Chase", Host: "chase.com", Class: "service", Sector: "financial/banking", Country: "US", State: board.StateUp},
		board.Target{ID: "2", Name: "Delta", Host: "delta.com", Class: "service", Sector: "airline", Country: "US", State: board.StateDown},
		board.Target{ID: "3", Name: "Root", Host: "198.41.0.4", Class: "network", Sector: "dns", Country: "NL", State: board.StateUnknown},
	)
	return newBoardHandler(t, m)
}

func decodeTargets(t *testing.T, body []byte) []struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Class   string `json:"class"`
	Sector  string `json:"sector"`
	Country string `json:"country"`
	State   string `json:"state"`
} {
	t.Helper()

	var env dataEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	var items []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Class   string `json:"class"`
		Sector  string `json:"sector"`
		Country string `json:"country"`
		State   string `json:"state"`
	}
	if err := json.Unmarshal(env.Data, &items); err != nil {
		t.Fatal(err)
	}
	return items
}

func TestTargetsFilters(t *testing.T) {
	t.Parallel()

	h := targetsFixture(t)

	all := decodeTargets(t, do(h, http.MethodGet, "/api/v1/targets", testToken).Body.Bytes())
	if len(all) != 3 {
		t.Fatalf("all = %d, want 3", len(all))
	}

	services := decodeTargets(t, do(h, http.MethodGet, "/api/v1/targets?class=service", testToken).Body.Bytes())
	if len(services) != 2 {
		t.Fatalf("class filter = %d, want 2", len(services))
	}

	airline := decodeTargets(t, do(h, http.MethodGet, "/api/v1/targets?sector=airline", testToken).Body.Bytes())
	if len(airline) != 1 || airline[0].ID != "2" {
		t.Fatalf("sector filter = %+v", airline)
	}

	down := decodeTargets(t, do(h, http.MethodGet, "/api/v1/targets?state=down", testToken).Body.Bytes())
	if len(down) != 1 || down[0].ID != "2" {
		t.Fatalf("state filter = %+v", down)
	}

	nl := decodeTargets(t, do(h, http.MethodGet, "/api/v1/targets?country=NL", testToken).Body.Bytes())
	if len(nl) != 1 || nl[0].ID != "3" {
		t.Fatalf("country filter = %+v", nl)
	}

	none := decodeTargets(t, do(h, http.MethodGet, "/api/v1/targets?class=service&state=unknown", testToken).Body.Bytes())
	if len(none) != 0 {
		t.Fatalf("combined filter = %+v", none)
	}
}

func TestTargetsRejectsUnknownState(t *testing.T) {
	t.Parallel()

	rec := do(targetsFixture(t), http.MethodGet, "/api/v1/targets?state=melted", testToken)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var e errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Code != api.CodeInvalidArgument {
		t.Fatalf("code = %q", e.Error.Code)
	}
}

func TestTargetsUnavailableWithoutSource(t *testing.T) {
	t.Parallel()

	rec := do(newHandler(t, testToken, nil), http.MethodGet, "/api/v1/targets", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestTargetsRequireAuth(t *testing.T) {
	t.Parallel()

	rec := do(targetsFixture(t), http.MethodGet, "/api/v1/targets", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestTargetSeries(t *testing.T) {
	t.Parallel()

	reader := &fakeProbes{
		rows: map[string][]store.ProbeRow{"t1": nil},
		series: []store.Rollup{
			{Bucket: time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC), N: 12, Up: 10, Down: 2},
			{Bucket: time.Date(2026, 11, 17, 11, 0, 0, 0, time.UTC), N: 12, Up: 12},
		},
	}

	rec := do(newProbesHandler(t, reader), http.MethodGet,
		"/api/v1/targets/t1/series?resolution=1h&from=2026-11-17T09:00:00Z&to=2026-11-17T12:00:00Z", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}

	var env dataEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var items []struct {
		Bucket string `json:"bucket"`
		N      int    `json:"n"`
		Up     int    `json:"up"`
		Down   int    `json:"down"`
	}
	if err := json.Unmarshal(env.Data, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d", len(items))
	}
	if items[0].Bucket != "2026-11-17T10:00:00Z" || items[0].N != 12 || items[0].Down != 2 {
		t.Fatalf("first bucket = %+v", items[0])
	}
}

func TestTargetSeriesRejectsBadWindowsAndResolutions(t *testing.T) {
	t.Parallel()

	h := newProbesHandler(t, &fakeProbes{rows: map[string][]store.ProbeRow{"t1": nil}})
	for _, query := range []string{
		"resolution=2w",
		"from=yesterday",
		"to=soon",
		"from=2026-11-17T12:00:00Z&to=2026-11-17T11:00:00Z",
	} {
		rec := do(h, http.MethodGet, "/api/v1/targets/t1/series?"+query, testToken)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", query, rec.Code)
		}
	}
}

func TestTargetSeriesUnknownTarget(t *testing.T) {
	t.Parallel()

	rec := do(newProbesHandler(t, &fakeProbes{rows: map[string][]store.ProbeRow{}}),
		http.MethodGet, "/api/v1/targets/nope/series", testToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestTargetSeriesUnavailableWithoutReader(t *testing.T) {
	t.Parallel()

	rec := do(newHandler(t, testToken, nil), http.MethodGet, "/api/v1/targets/t1/series", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestTargetSeriesRequiresAuth(t *testing.T) {
	t.Parallel()

	rec := do(newProbesHandler(t, &fakeProbes{rows: map[string][]store.ProbeRow{"t1": nil}}),
		http.MethodGet, "/api/v1/targets/t1/series", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

type fakeScopes struct {
	counts     map[state.State]int
	judgeable  int
	history    []float64
	err        error
	historyErr error
}

func (f *fakeScopes) ScopeStates(context.Context, string, string, string) (map[state.State]int, int, error) {
	return f.counts, f.judgeable, f.err
}

func (f *fakeScopes) ScopeHistory(context.Context, string, string, time.Time) ([]float64, error) {
	return f.history, f.historyErr
}

func newScopesHandler(t *testing.T, reader api.ScopeReader) http.Handler {
	t.Helper()

	h, err := api.New(
		api.Config{Listen: "127.0.0.1:2600", Token: testToken},
		api.Deps{
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Scopes:    reader,
			Version:   "test",
			Commit:    "deadbeef",
			StartedAt: time.Now(),
		},
	)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h
}

func decodeScope(t *testing.T, body []byte) struct {
	Scope      string         `json:"scope"`
	Status     string         `json:"status"`
	Mode       string         `json:"mode"`
	Health     *float64       `json:"health"`
	Coverage   float64        `json:"coverage"`
	Measurable int            `json:"measurable"`
	Judgeable  int            `json:"judgeable"`
	States     map[string]int `json:"states"`
	Baseline   struct {
		N      int     `json:"n"`
		Median float64 `json:"median"`
		Sigma  float64 `json:"sigma"`
		Usable bool    `json:"usable"`
	} `json:"baseline"`
} {
	t.Helper()

	var env dataEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	var out struct {
		Scope      string         `json:"scope"`
		Status     string         `json:"status"`
		Mode       string         `json:"mode"`
		Health     *float64       `json:"health"`
		Coverage   float64        `json:"coverage"`
		Measurable int            `json:"measurable"`
		Judgeable  int            `json:"judgeable"`
		States     map[string]int `json:"states"`
		Baseline   struct {
			N      int     `json:"n"`
			Median float64 `json:"median"`
			Sigma  float64 `json:"sigma"`
			Usable bool    `json:"usable"`
		} `json:"baseline"`
	}
	if err := json.Unmarshal(env.Data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestScopeHealthAndCoverage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		counts     map[state.State]int
		judgeable  int
		wantStatus string
		wantHealth bool
	}{
		{
			name:       "healthy",
			counts:     map[state.State]int{state.Up: 90, state.Down: 10},
			judgeable:  100,
			wantStatus: "ok",
			wantHealth: true,
		},
		{
			name:       "low coverage",
			counts:     map[state.State]int{state.Up: 60, state.Unknown: 40},
			judgeable:  100,
			wantStatus: "low_coverage",
			wantHealth: true,
		},
		{
			name:       "too few measurable",
			counts:     map[state.State]int{state.Up: 5, state.Unknown: 95},
			judgeable:  100,
			wantStatus: "unknown",
			wantHealth: false,
		},
		{
			name:       "empty scope",
			counts:     map[state.State]int{},
			judgeable:  0,
			wantStatus: "unknown",
			wantHealth: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := do(newScopesHandler(t, &fakeScopes{counts: tc.counts, judgeable: tc.judgeable}),
				http.MethodGet, "/api/v1/scopes/global", testToken)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}

			got := decodeScope(t, rec.Body.Bytes())
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if (got.Health != nil) != tc.wantHealth {
				t.Fatalf("health = %v, want present=%v", got.Health, tc.wantHealth)
			}
			if got.Mode != "learning" {
				t.Fatalf("mode = %q", got.Mode)
			}
			if len(got.States) != 8 {
				t.Fatalf("states = %v", got.States)
			}
		})
	}
}

func TestScopeBaselineBecomesUsable(t *testing.T) {
	t.Parallel()

	history := make([]float64, baseline.Window)
	for i := range history {
		history[i] = 0.9
	}

	rec := do(newScopesHandler(t, &fakeScopes{
		counts:    map[state.State]int{state.Up: 45, state.Down: 5},
		judgeable: 50,
		history:   history,
	}), http.MethodGet, "/api/v1/scopes/global", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	got := decodeScope(t, rec.Body.Bytes())
	if got.Mode != "normal" {
		t.Fatalf("mode = %q, want normal", got.Mode)
	}
	if !got.Baseline.Usable || got.Baseline.N != baseline.Window {
		t.Fatalf("baseline = %+v", got.Baseline)
	}
	if got.Baseline.Median != 0.9 {
		t.Fatalf("median = %v", got.Baseline.Median)
	}
}

func TestScopeBaselineLearning(t *testing.T) {
	t.Parallel()

	rec := do(newScopesHandler(t, &fakeScopes{
		counts:    map[state.State]int{state.Up: 9},
		judgeable: 10,
		history:   []float64{0.9, 0.9},
	}), http.MethodGet, "/api/v1/scopes/global", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	got := decodeScope(t, rec.Body.Bytes())
	if got.Mode != "learning" || got.Baseline.Usable {
		t.Fatalf("mode = %q, baseline = %+v", got.Mode, got.Baseline)
	}
}

func TestScopeValidation(t *testing.T) {
	t.Parallel()

	h := newScopesHandler(t, &fakeScopes{judgeable: 1, counts: map[state.State]int{state.Up: 1}})
	for _, path := range []string{
		"/api/v1/scopes/planet",
		"/api/v1/scopes/sector",
		"/api/v1/scopes/asn?value=not-a-number",
	} {
		rec := do(h, http.MethodGet, path, testToken)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", path, rec.Code)
		}
	}
}

func TestScopeUnavailableAndAuth(t *testing.T) {
	t.Parallel()

	rec := do(newHandler(t, testToken, nil), http.MethodGet, "/api/v1/scopes/global", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	rec = do(newScopesHandler(t, &fakeScopes{}), http.MethodGet, "/api/v1/scopes/global", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

type fakeAnomalies struct {
	rows []store.Anomaly
	err  error
}

func (f *fakeAnomalies) Anomalies(context.Context, int) ([]store.Anomaly, error) {
	return f.rows, f.err
}

func newAnomaliesHandler(t *testing.T, reader api.AnomalyReader) http.Handler {
	t.Helper()

	h, err := api.New(
		api.Config{Listen: "127.0.0.1:2600", Token: testToken},
		api.Deps{
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Anomalies: reader,
			Version:   "test",
			Commit:    "deadbeef",
			StartedAt: time.Now(),
		},
	)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h
}

func TestAnomaliesList(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	reader := &fakeAnomalies{rows: []store.Anomaly{
		{
			ID: 2, TS: ts, Scope: "global", Kind: "health_drop",
			Observed: 0.62, Baseline: 0.94, MAD: 0.01, Sigma: 0.0148,
			Buckets: 2, IncidentID: "inc-1",
		},
		{
			ID: 1, TS: ts.Add(-5 * time.Minute), Scope: "sector", ScopeValue: "airline",
			Kind: "health_drop", Observed: 0.7, Baseline: 0.95, MAD: 0.01,
			Sigma: 0.0148, Buckets: 2,
		},
	}}

	rec := do(newAnomaliesHandler(t, reader), http.MethodGet, "/api/v1/anomalies?limit=5", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}

	var env dataEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var items []struct {
		ID         int64   `json:"id"`
		TS         string  `json:"ts"`
		Scope      string  `json:"scope"`
		ScopeValue string  `json:"scope_value"`
		Kind       string  `json:"kind"`
		Observed   float64 `json:"observed"`
		IncidentID string  `json:"incident_id"`
	}
	if err := json.Unmarshal(env.Data, &items); err != nil {
		t.Fatal(err)
	}

	if len(items) != 2 {
		t.Fatalf("items = %d", len(items))
	}
	if items[0].Scope != "global" || items[0].IncidentID != "inc-1" || items[0].TS != "2026-11-17T12:00:00Z" {
		t.Fatalf("first = %+v", items[0])
	}
	if items[1].ScopeValue != "airline" || items[1].Observed != 0.7 {
		t.Fatalf("second = %+v", items[1])
	}
	if env.Meta.Count != 2 || env.Meta.Limit != 5 {
		t.Fatalf("meta = %+v", env.Meta)
	}
}

func TestAnomaliesUnavailableAndAuth(t *testing.T) {
	t.Parallel()

	rec := do(newHandler(t, testToken, nil), http.MethodGet, "/api/v1/anomalies", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	rec = do(newAnomaliesHandler(t, &fakeAnomalies{}), http.MethodGet, "/api/v1/anomalies", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAnomaliesRejectBadLimitAndSurfaceErrors(t *testing.T) {
	t.Parallel()

	rec := do(newAnomaliesHandler(t, &fakeAnomalies{}), http.MethodGet, "/api/v1/anomalies?limit=0", testToken)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}

	rec = do(newAnomaliesHandler(t, &fakeAnomalies{err: errors.New("db down")}),
		http.MethodGet, "/api/v1/anomalies", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

type fakeTargets struct {
	rows map[string]store.TargetRow
	err  error
}

func (f *fakeTargets) TargetByID(_ context.Context, _, id string) (store.TargetRow, error) {
	if f.err != nil {
		return store.TargetRow{}, f.err
	}
	row, ok := f.rows[id]
	if !ok {
		return store.TargetRow{}, sql.ErrNoRows
	}
	return row, nil
}

type fakeCoverage struct {
	cov store.Coverage
	err error
}

func (f *fakeCoverage) Coverage(context.Context, string, time.Time) (store.Coverage, error) {
	return f.cov, f.err
}

func newTargetDetailHandler(t *testing.T, targets api.TargetReader, cov api.CoverageReader) http.Handler {
	t.Helper()

	h, err := api.New(
		api.Config{Listen: "127.0.0.1:2600", Token: testToken},
		api.Deps{
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Targets:   targets,
			Coverage:  cov,
			Vantage:   "local",
			Version:   "test",
			Commit:    "deadbeef",
			StartedAt: time.Now(),
		},
	)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h
}

func TestTargetDetail(t *testing.T) {
	t.Parallel()

	since := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	targets := &fakeTargets{rows: map[string]store.TargetRow{
		"t1": {
			Target: store.Target{
				ID: "t1", Host: "chase.com", Port: 443, Probe: "http",
				Name: "Chase", Class: "service", Sector: "financial/banking",
				Country: "US", Tier: 2, IntervalS: 300,
			},
			State: state.Down,
			Since: since,
		},
	}}

	rec := do(newTargetDetailHandler(t, targets, nil), http.MethodGet, "/api/v1/targets/t1", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}

	var env dataEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var item struct {
		ID     string `json:"id"`
		Host   string `json:"host"`
		Class  string `json:"class"`
		Sector string `json:"sector"`
		State  string `json:"state"`
		Since  string `json:"since"`
	}
	if err := json.Unmarshal(env.Data, &item); err != nil {
		t.Fatal(err)
	}
	if item.ID != "t1" || item.Host != "chase.com" || item.Class != "service" {
		t.Fatalf("item = %+v", item)
	}
	if item.State != "down" || item.Since != "2026-11-17T12:00:00Z" {
		t.Fatalf("state/since = %+v", item)
	}
}

func TestTargetDetailMissingAndUnavailable(t *testing.T) {
	t.Parallel()

	targets := &fakeTargets{rows: map[string]store.TargetRow{}}
	rec := do(newTargetDetailHandler(t, targets, nil), http.MethodGet, "/api/v1/targets/nope", testToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}

	rec = do(newProbesHandler(t, probeFixture()), http.MethodGet, "/api/v1/targets/t1", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 without a target reader", rec.Code)
	}

	targets.err = errors.New("db down")
	rec = do(newTargetDetailHandler(t, targets, nil), http.MethodGet, "/api/v1/targets/t1", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 on a read failure", rec.Code)
	}
}

func TestCoveragePanel(t *testing.T) {
	t.Parallel()

	cov := &fakeCoverage{cov: store.Coverage{
		Totals: store.CoverageTotals{Total: 100, Measurable: 80, Unmeasurable: 10},
		Classes: []store.CoverageBucket{
			{Kind: "class", Value: "service", Total: 90, Measurable: 80},
		},
		BlindSpots: []store.CoverageBucket{
			{Kind: "country", Value: "NL", Total: 10, Measurable: 2, Unmeasurable: 8},
		},
		Stale: []store.StaleTarget{
			{ID: "t1", Host: "chase.com", State: string(state.Unknown), IntervalS: 300},
			{
				ID: "t2", Host: "united.com", State: string(state.Up), IntervalS: 300,
				LastProbe: time.Now().UTC().Add(-2 * time.Hour),
			},
		},
	}}

	rec := do(newTargetDetailHandler(t, nil, cov), http.MethodGet, "/api/v1/coverage", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}

	var env dataEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Total      int              `json:"total"`
		Measurable int              `json:"measurable"`
		Coverage   float64          `json:"coverage"`
		BlindSpots []map[string]any `json:"blind_spots"`
		Stale      []map[string]any `json:"stale"`
	}
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatal(err)
	}

	if got.Total != 100 || got.Measurable != 80 || got.Coverage != 0.8 {
		t.Fatalf("totals = %+v", got)
	}
	if len(got.BlindSpots) != 1 || got.BlindSpots[0]["value"] != "NL" {
		t.Fatalf("blind spots = %+v", got.BlindSpots)
	}
	if len(got.Stale) != 2 {
		t.Fatalf("stale = %+v", got.Stale)
	}
	// A never-probed target has no last_probe; a stale one carries an age.
	if _, ok := got.Stale[0]["last_probe"]; ok {
		t.Fatalf("never-probed target carried last_probe: %+v", got.Stale[0])
	}
	if age, ok := got.Stale[1]["age_seconds"].(float64); !ok || age <= 0 {
		t.Fatalf("stale target age = %+v", got.Stale[1])
	}
}

func TestCoverageUnavailableAndAuth(t *testing.T) {
	t.Parallel()

	rec := do(newHandler(t, testToken, nil), http.MethodGet, "/api/v1/coverage", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	rec = do(newTargetDetailHandler(t, nil, &fakeCoverage{err: errors.New("db down")}),
		http.MethodGet, "/api/v1/coverage", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 on a read failure", rec.Code)
	}

	rec = do(newTargetDetailHandler(t, nil, &fakeCoverage{}), http.MethodGet, "/api/v1/coverage", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

type fakeSelfChecks struct {
	rows []store.SelfCheck
	err  error
}

func (f *fakeSelfChecks) LatestSelfChecks(context.Context) ([]store.SelfCheck, error) {
	return f.rows, f.err
}

func newSelfCheckHandler(t *testing.T, reader api.SelfCheckReader) http.Handler {
	t.Helper()

	h, err := api.New(
		api.Config{Listen: "127.0.0.1:2600", Token: testToken},
		api.Deps{
			Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
			SelfChecks: reader,
			Version:    "test",
			Commit:     "deadbeef",
			StartedAt:  time.Now(),
		},
	)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h
}

func TestSelfCheckList(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	reader := &fakeSelfChecks{rows: []store.SelfCheck{
		{ID: 1, TS: ts, Check: "clock", OK: true, Value: "2026-11-17T12:00:00Z", Detail: "1s skew"},
		{ID: 2, TS: ts, Check: "disk", OK: false, Detail: "read-only file system"},
	}}

	rec := do(newSelfCheckHandler(t, reader), http.MethodGet, "/api/v1/selfcheck", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}

	var env dataEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var items []struct {
		Check  string `json:"check"`
		OK     bool   `json:"ok"`
		Value  string `json:"value"`
		Detail string `json:"detail"`
		TS     string `json:"ts"`
	}
	if err := json.Unmarshal(env.Data, &items); err != nil {
		t.Fatal(err)
	}

	if len(items) != 2 {
		t.Fatalf("items = %d", len(items))
	}
	if items[0].Check != "clock" || !items[0].OK || items[0].TS != "2026-11-17T12:00:00Z" {
		t.Fatalf("first = %+v", items[0])
	}
	if items[1].Check != "disk" || items[1].OK || items[1].Detail == "" {
		t.Fatalf("second = %+v", items[1])
	}
	if env.Meta.Count != 2 {
		t.Errorf("meta.count = %d", env.Meta.Count)
	}
}

func TestSelfCheckUnavailableAndAuth(t *testing.T) {
	t.Parallel()

	rec := do(newHandler(t, testToken, nil), http.MethodGet, "/api/v1/selfcheck", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	rec = do(newSelfCheckHandler(t, &fakeSelfChecks{err: errors.New("db down")}),
		http.MethodGet, "/api/v1/selfcheck", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 on a read failure", rec.Code)
	}

	rec = do(newSelfCheckHandler(t, &fakeSelfChecks{}), http.MethodGet, "/api/v1/selfcheck", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

type fakeExporter struct {
	rows []store.ExportRow
	err  error
	from time.Time
	to   time.Time
}

func (f *fakeExporter) ExportProbes(_ context.Context, from, to time.Time, fn func(store.ExportRow) error) error {
	f.from, f.to = from, to
	if f.err != nil {
		return f.err
	}
	for _, row := range f.rows {
		if err := fn(row); err != nil {
			return err
		}
	}
	return nil
}

func newExportHandler(t *testing.T, exporter api.Exporter) http.Handler {
	t.Helper()

	h, err := api.New(
		api.Config{Listen: "127.0.0.1:2600", Token: testToken},
		api.Deps{
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Exporter:  exporter,
			Version:   "test",
			Commit:    "deadbeef",
			StartedAt: time.Now(),
		},
	)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h
}

func exportFixture() *fakeExporter {
	return &fakeExporter{rows: []store.ExportRow{
		{
			TargetID: "t1", Host: "chase.com", VantageID: "local",
			TS:    time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC),
			State: state.Up, Stage: "http", HTTPStatus: 200,
			LatencyMS: 42, Confidence: "high",
		},
		{
			TargetID: "t2", Host: "united.com", VantageID: "local",
			TS:    time.Date(2026, 11, 17, 12, 1, 0, 0, time.UTC),
			State: state.Down, Stage: "tls", ErrorClass: "tls_cert_expired",
			Confidence: "high", Detail: "certificate expired",
		},
	}}
}

func TestExportCSV(t *testing.T) {
	t.Parallel()

	rec := do(newExportHandler(t, exportFixture()), http.MethodGet,
		"/api/v1/export?format=csv&from=2026-11-17T00:00:00Z&to=2026-11-18T00:00:00Z", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("Content-Disposition = %q", cd)
	}

	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want header + 2 rows:\n%s", len(lines), rec.Body)
	}
	if !strings.HasPrefix(lines[0], "target_id,host,vantage,ts,state") {
		t.Fatalf("header = %q", lines[0])
	}
	if !strings.Contains(lines[1], "chase.com") || !strings.Contains(lines[1], "up") {
		t.Fatalf("row 1 = %q", lines[1])
	}
	if !strings.Contains(lines[2], "tls_cert_expired") {
		t.Fatalf("row 2 = %q", lines[2])
	}
}

func TestExportJSONL(t *testing.T) {
	t.Parallel()

	rec := do(newExportHandler(t, exportFixture()), http.MethodGet,
		"/api/v1/export?format=jsonl&from=2026-11-17T00:00:00Z&to=2026-11-18T00:00:00Z", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/x-ndjson") {
		t.Fatalf("Content-Type = %q", ct)
	}

	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2 objects", len(lines))
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &row); err != nil {
		t.Fatalf("jsonl row: %v", err)
	}
	if row["host"] != "united.com" || row["error_class"] != "tls_cert_expired" {
		t.Fatalf("row = %v", row)
	}
}

func TestExportRejectsBadInput(t *testing.T) {
	t.Parallel()

	h := newExportHandler(t, exportFixture())
	for _, query := range []string{
		"format=xml",
		"from=yesterday",
		"to=soon",
		"from=2026-11-18T00:00:00Z&to=2026-11-17T00:00:00Z",
		"from=2026-01-01T00:00:00Z&to=2026-11-17T00:00:00Z",
	} {
		rec := do(h, http.MethodGet, "/api/v1/export?"+query, testToken)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", query, rec.Code)
		}
	}
}

func TestExportUnavailableAndAuth(t *testing.T) {
	t.Parallel()

	rec := do(newHandler(t, testToken, nil), http.MethodGet, "/api/v1/export", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	rec = do(newExportHandler(t, exportFixture()), http.MethodGet, "/api/v1/export", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func newEventsHandler(t *testing.T, src board.Source, watcher board.Watcher) http.Handler {
	t.Helper()

	h, err := api.New(
		api.Config{Listen: "127.0.0.1:2600", Token: testToken},
		api.Deps{
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Source:    src,
			Watcher:   watcher,
			Vantage:   "local",
			Version:   "test",
			Commit:    "deadbeef",
			StartedAt: time.Now(),
		},
	)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h
}

// eventStream reads an SSE response once and shares the lines across calls.
// A one-shot goroutine per readEvent call would keep reading ahead and swallow
// the next event, which is exactly the flake this helper exists to avoid.
type eventStream struct {
	lines chan string
	errs  chan error
}

func newEventStream(r *bufio.Reader) *eventStream {
	s := &eventStream{lines: make(chan string, 64), errs: make(chan error, 1)}
	go func() {
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				s.errs <- err
				return
			}
			s.lines <- strings.TrimRight(line, "\r\n")
		}
	}()
	return s
}

// readEvent consumes lines until it finds the named event. The timeout makes a
// lost wake-up fail the test instead of hanging the package.
func (s *eventStream) readEvent(t *testing.T, want string, timeout time.Duration) string {
	t.Helper()

	deadline := time.After(timeout)
	event := ""
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out after %s waiting for %q", timeout, want)
		case err := <-s.errs:
			t.Fatalf("stream ended before %q: %v", want, err)
		case line := <-s.lines:
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: ") && event == want:
				return strings.TrimPrefix(line, "data: ")
			}
		}
	}
}

func TestEventsStreamCap(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(board.Target{ID: "1", Host: "chase.com", State: board.StateUp})

	h, err := api.New(
		api.Config{Listen: "127.0.0.1:2600", Token: testToken},
		api.Deps{
			Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
			Source:      m,
			Watcher:     m,
			StreamLimit: streamlimit.New(1, 1),
			Vantage:     "local",
			Version:     "test",
			Commit:      "deadbeef",
			StartedAt:   time.Now(),
		},
	)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}

	ts := httptest.NewServer(h)
	defer ts.Close()

	open := func() *http.Response {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/api/v1/events", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	first := open()
	defer func() { _ = first.Body.Close() }()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first stream status = %d", first.StatusCode)
	}

	second := open()
	defer func() { _ = second.Body.Close() }()
	if second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second stream status = %d, want 429 at the cap", second.StatusCode)
	}
	if second.Header.Get("Retry-After") == "" {
		t.Error("429 without a Retry-After hint")
	}

	// Closing the first stream releases the slot. The handler notices through
	// the request context, so allow a moment rather than assuming it is
	// synchronous.
	_ = first.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		third := open()
		status := third.StatusCode
		_ = third.Body.Close()
		if status == http.StatusOK {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("closing the first stream did not free its slot")
}

func TestEventsSendsSnapshotThenDeltas(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(
		board.Target{ID: "1", Name: "Chase", Host: "chase.com", State: board.StateUp},
		board.Target{ID: "2", Name: "United", Host: "united.com", State: board.StateUp},
	)

	ts := httptest.NewServer(newEventsHandler(t, m, m))
	defer ts.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Content-Type = %q status = %d body = %s", ct, resp.StatusCode, body)
	}

	stream := newEventStream(bufio.NewReader(resp.Body))
	snapshot := stream.readEvent(t, "snapshot", 5*time.Second)
	var snap struct {
		Targets []struct {
			Target  string `json:"target"`
			State   string `json:"state"`
			Vantage string `json:"vantage"`
		} `json:"targets"`
	}
	if err := json.Unmarshal([]byte(snapshot), &snap); err != nil {
		t.Fatalf("snapshot: %v (%s)", err, snapshot)
	}
	if len(snap.Targets) != 2 {
		t.Fatalf("snapshot targets = %d", len(snap.Targets))
	}
	if snap.Targets[0].Vantage != "local" {
		t.Fatalf("vantage = %q", snap.Targets[0].Vantage)
	}

	if _, changed := m.Set("1", board.StateDown); !changed {
		t.Fatal("fixture did not change")
	}

	delta := stream.readEvent(t, "state", 5*time.Second)
	var state struct {
		Target string `json:"target"`
		State  string `json:"state"`
		Text   string `json:"state_text"`
	}
	if err := json.Unmarshal([]byte(delta), &state); err != nil {
		t.Fatalf("delta: %v (%s)", err, delta)
	}
	if state.Target != "chase.com" || state.State != "down" {
		t.Fatalf("delta = %+v", state)
	}
	if state.Text != "Down" {
		t.Fatalf("state_text = %q", state.Text)
	}
}

func TestEventsRequiresAuthAndSource(t *testing.T) {
	t.Parallel()

	h := newEventsHandler(t, board.NewMemory(), nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	rec = do(newHandler(t, testToken, nil), http.MethodGet, "/api/v1/events", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 with no source", rec.Code)
	}
}

func TestEventsHeartbeatOnlyWithoutWatcher(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(board.Target{ID: "1", Host: "chase.com", State: board.StateUp})
	ts := httptest.NewServer(newEventsHandler(t, m, nil))
	defer ts.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	stream := newEventStream(bufio.NewReader(resp.Body))
	if got := stream.readEvent(t, "snapshot", 5*time.Second); !strings.Contains(got, "chase.com") {
		t.Fatalf("snapshot = %s", got)
	}
}

func TestEventsEmitsIncidentChanges(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(board.Target{ID: "1", Host: "chase.com", State: board.StateUp})
	incidentChanges := board.NewBroadcast()
	reader := incidentFixture()

	h, err := api.New(
		api.Config{Listen: "127.0.0.1:2600", Token: testToken},
		api.Deps{
			Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
			Source:          m,
			Watcher:         m,
			IncidentWatcher: incidentChanges,
			Incidents:       reader,
			Version:         "test",
			Commit:          "deadbeef",
			StartedAt:       time.Now(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(h)
	defer ts.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	events := newEventStream(bufio.NewReader(resp.Body))
	events.readEvent(t, "snapshot", 5*time.Second)

	incidentChanges.Notify()

	data := events.readEvent(t, "incident", 5*time.Second)
	var got struct {
		ID       string `json:"id"`
		Headline string `json:"headline"`
		Scope    string `json:"scope"`
	}
	if err := json.Unmarshal([]byte(data), &got); err != nil {
		t.Fatalf("incident event: %v (%s)", err, data)
	}
	if got.ID != "inc-1" || got.Scope != "sector" || got.Headline == "" {
		t.Fatalf("incident event = %+v", got)
	}
}

type fakeFeeds struct {
	states []store.FeedState
	events []store.ExternalEvent
	err    error
}

func (f *fakeFeeds) FeedStates(context.Context) ([]store.FeedState, error) {
	return f.states, f.err
}

func (f *fakeFeeds) ExternalEvents(context.Context, int) ([]store.ExternalEvent, error) {
	return f.events, f.err
}

func newFeedsHandler(t *testing.T, reader api.FeedReader) http.Handler {
	t.Helper()

	h, err := api.New(
		api.Config{Listen: "127.0.0.1:2600", Token: testToken},
		api.Deps{
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Feeds:     reader,
			Version:   "test",
			Commit:    "deadbeef",
			StartedAt: time.Now(),
		},
	)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h
}

func feedsFixture() *fakeFeeds {
	now := time.Now().UTC()
	return &fakeFeeds{
		states: []store.FeedState{
			{
				Feed: "ioda", LastOKAt: now.Add(-2 * time.Minute),
				NextPollAt: now.Add(8 * time.Minute),
			},
			{
				Feed: "radar", LastOKAt: now.Add(-2 * time.Hour),
				LastErr: "503 from upstream", ConsecutiveFailures: 3,
			},
		},
		events: []store.ExternalEvent{
			{
				ID: 1, Feed: "ioda", ExternalID: "evt-1", Kind: "outage",
				Severity: 0.87, Country: "IR",
				StartedAt: now.Add(-time.Hour), ObservedAt: now,
			},
		},
	}
}

func TestFeedsList(t *testing.T) {
	t.Parallel()

	rec := do(newFeedsHandler(t, feedsFixture()), http.MethodGet, "/api/v1/feeds", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}

	var env dataEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Feeds []struct {
			Feed                string `json:"feed"`
			LastOKAt            string `json:"last_ok_at"`
			LastErr             string `json:"last_err"`
			ConsecutiveFailures int    `json:"consecutive_failures"`
			Stale               bool   `json:"stale"`
		} `json:"feeds"`
		Events []struct {
			Feed       string  `json:"feed"`
			ExternalID string  `json:"external_id"`
			Kind       string  `json:"kind"`
			Severity   float64 `json:"severity"`
			Country    string  `json:"country"`
			StartedAt  string  `json:"started_at"`
		} `json:"events"`
	}
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatal(err)
	}

	if len(got.Feeds) != 2 {
		t.Fatalf("feeds = %+v", got.Feeds)
	}
	if got.Feeds[0].Feed != "ioda" || got.Feeds[0].Stale {
		t.Fatalf("ioda = %+v", got.Feeds[0])
	}
	if !got.Feeds[1].Stale || got.Feeds[1].ConsecutiveFailures != 3 {
		t.Fatalf("radar = %+v", got.Feeds[1])
	}
	if len(got.Events) != 1 || got.Events[0].Country != "IR" || got.Events[0].Kind != "outage" {
		t.Fatalf("events = %+v", got.Events)
	}
	if got.Events[0].StartedAt == "" {
		t.Error("started_at missing")
	}
	if env.Meta.Count != 1 {
		t.Errorf("meta.count = %d", env.Meta.Count)
	}
}

func TestFeedsUnavailableBadLimitAndAuth(t *testing.T) {
	t.Parallel()

	rec := do(newHandler(t, testToken, nil), http.MethodGet, "/api/v1/feeds", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	rec = do(newFeedsHandler(t, feedsFixture()), http.MethodGet, "/api/v1/feeds?limit=0", testToken)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}

	rec = do(newFeedsHandler(t, &fakeFeeds{err: errors.New("db down")}), http.MethodGet, "/api/v1/feeds", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 on a read failure", rec.Code)
	}

	rec = do(newFeedsHandler(t, feedsFixture()), http.MethodGet, "/api/v1/feeds", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func newDebugHandler(t *testing.T, scopes api.DebugScopes) http.Handler {
	t.Helper()

	h, err := api.New(
		api.Config{Listen: "127.0.0.1:2600", Token: testToken},
		api.Deps{
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Debug:     scopes,
			Version:   "test",
			Commit:    "deadbeef",
			StartedAt: time.Now(),
		},
	)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h
}

func TestDebugScopeCreateListDelete(t *testing.T) {
	t.Parallel()

	scopes := logging.NewScopes("")
	h := newDebugHandler(t, scopes)

	body := strings.NewReader(`{"target":"chase.com","level":"trace","ttl":"10m"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/debug/scopes", body)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", rec.Code, rec.Body)
	}

	var created struct {
		Data logging.Scope `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Data.ID == "" || created.Data.Level != "trace" || created.Data.Target != "chase.com" {
		t.Fatalf("created = %+v", created.Data)
	}
	scopeID := created.Data.ID

	rec = do(h, http.MethodGet, "/api/v1/debug/scopes", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	var env dataEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var list []logging.Scope
	if err := json.Unmarshal(env.Data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != scopeID {
		t.Fatalf("list = %+v", list)
	}

	rec = do(h, http.MethodDelete, "/api/v1/debug/scopes/"+scopeID, testToken)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", rec.Code)
	}
	if len(scopes.List()) != 0 {
		t.Fatal("scope survived delete")
	}
}

func TestDebugScopeValidation(t *testing.T) {
	t.Parallel()

	h := newDebugHandler(t, logging.NewScopes(""))

	tests := []struct {
		name        string
		body        string
		contentType string
		want        int
	}{
		{"no selectors", `{"level":"debug","ttl":"1m"}`, "application/json", http.StatusBadRequest},
		{"bad ttl", `{"target":"chase.com","ttl":"soon"}`, "application/json", http.StatusBadRequest},
		{"missing ttl", `{"target":"chase.com"}`, "application/json", http.StatusBadRequest},
		{"unknown field", `{"target":"chase.com","ttl":"1m","bogus":1}`, "application/json", http.StatusBadRequest},
		{"wrong content type", `{}`, "text/plain", http.StatusUnsupportedMediaType},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodPost, "/api/v1/debug/scopes", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+testToken)
			req.Header.Set("Content-Type", tc.contentType)

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

func TestDebugScopeUnavailableAuthAndMethods(t *testing.T) {
	t.Parallel()

	rec := do(newHandler(t, testToken, nil), http.MethodGet, "/api/v1/debug/scopes", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	h := newDebugHandler(t, logging.NewScopes(""))
	rec = do(h, http.MethodGet, "/api/v1/debug/scopes", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	rec = do(h, http.MethodPut, "/api/v1/debug/scopes", testToken)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}

	rec = do(h, http.MethodDelete, "/api/v1/debug/scopes/nope", testToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}

	rec = do(h, http.MethodGet, "/api/v1/debug/scopes/nope", testToken)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

type fakeCoverageHistory struct {
	points []store.CoveragePoint
	err    error
}

func (f *fakeCoverageHistory) CoverageHistory(context.Context, string, string, int) ([]store.CoveragePoint, error) {
	return f.points, f.err
}

func newCoverageHistoryHandler(t *testing.T, reader api.CoverageHistoryReader) http.Handler {
	t.Helper()

	h, err := api.New(
		api.Config{Listen: "127.0.0.1:2600", Token: testToken},
		api.Deps{
			Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
			CoverageHistory: reader,
			Version:         "test",
			Commit:          "deadbeef",
			StartedAt:       time.Now(),
		},
	)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h
}

func TestCoverageHistory(t *testing.T) {
	t.Parallel()

	reader := &fakeCoverageHistory{points: []store.CoveragePoint{
		{
			Bucket: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC),
			Scope:  "global", Total: 100, Measurable: 90, Blocked: 5, Unmeasurable: 5,
		},
		{
			Bucket: time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC),
			Scope:  "global", Total: 100, Measurable: 80, Blocked: 10, Unmeasurable: 10,
		},
	}}

	rec := do(newCoverageHistoryHandler(t, reader), http.MethodGet,
		"/api/v1/coverage/history?scope=global&days=7", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}

	var env dataEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var items []struct {
		Bucket     string  `json:"bucket"`
		Scope      string  `json:"scope"`
		Total      int     `json:"total"`
		Measurable int     `json:"measurable"`
		Coverage   float64 `json:"coverage"`
	}
	if err := json.Unmarshal(env.Data, &items); err != nil {
		t.Fatal(err)
	}

	if len(items) != 2 {
		t.Fatalf("items = %d", len(items))
	}
	if items[0].Bucket != "2026-11-16" || items[1].Bucket != "2026-11-17" {
		t.Fatalf("buckets = %+v", items)
	}
	if items[1].Coverage != 0.8 {
		t.Fatalf("coverage = %v", items[1].Coverage)
	}
	if env.Meta.Limit != 7 {
		t.Fatalf("meta.limit = %d", env.Meta.Limit)
	}
}

func TestCoverageHistoryValidation(t *testing.T) {
	t.Parallel()

	h := newCoverageHistoryHandler(t, &fakeCoverageHistory{})
	for _, query := range []string{
		"scope=planet",
		"scope=sector",
		"scope=asn&value=not-a-number",
		"days=0",
		"days=366",
		"days=soon",
	} {
		rec := do(h, http.MethodGet, "/api/v1/coverage/history?"+query, testToken)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", query, rec.Code)
		}
	}
}

func TestCoverageHistoryUnavailableAndAuth(t *testing.T) {
	t.Parallel()

	rec := do(newHandler(t, testToken, nil), http.MethodGet, "/api/v1/coverage/history", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	rec = do(newCoverageHistoryHandler(t, &fakeCoverageHistory{err: errors.New("db down")}),
		http.MethodGet, "/api/v1/coverage/history", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 on a read failure", rec.Code)
	}

	rec = do(newCoverageHistoryHandler(t, &fakeCoverageHistory{}),
		http.MethodGet, "/api/v1/coverage/history", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

type fakeTracer struct {
	result probe.TraceResult
	calls  int
	got    netip.Addr
}

func (f *fakeTracer) Run(_ context.Context, dst netip.Addr) probe.TraceResult {
	f.calls++
	f.got = dst
	return f.result
}

type fakeResolver struct {
	addrs []netip.Addr
	err   error
	hosts []string
}

func (f *fakeResolver) CheckHost(_ context.Context, host string) ([]netip.Addr, error) {
	f.hosts = append(f.hosts, host)
	return f.addrs, f.err
}

func newTraceHandler(t *testing.T, targets api.TargetReader, tracer api.Tracer, resolver api.HostResolver) http.Handler {
	t.Helper()

	h, err := api.New(
		api.Config{Listen: "127.0.0.1:2600", Token: testToken},
		api.Deps{
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Targets:   targets,
			Tracer:    tracer,
			Resolver:  resolver,
			Vantage:   "local",
			Version:   "test",
			Commit:    "deadbeef",
			StartedAt: time.Now(),
		},
	)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h
}

func traceTargets() *fakeTargets {
	return &fakeTargets{rows: map[string]store.TargetRow{
		"t1": {
			Target: store.Target{
				ID: "t1", Host: "chase.com", Port: 443, Probe: "http",
				Name: "Chase", Class: "service", Sector: "financial/banking",
				Country: "US", Tier: 2, IntervalS: 300,
			},
			State: state.Down,
		},
	}}
}

func TestTraceAction(t *testing.T) {
	t.Parallel()

	tracer := &fakeTracer{result: probe.TraceResult{
		Destination: netip.MustParseAddr("203.0.113.10"),
		Reached:     true,
		Hops: []probe.TraceHop{
			{TTL: 1, Addr: netip.MustParseAddr("10.0.0.1"), RTT: 4 * time.Millisecond},
			{TTL: 2, Addr: netip.MustParseAddr("203.0.113.10"), RTT: 11 * time.Millisecond},
		},
	}}
	resolver := &fakeResolver{addrs: []netip.Addr{netip.MustParseAddr("203.0.113.10")}}

	rec := do(newTraceHandler(t, traceTargets(), tracer, resolver),
		http.MethodPost, "/api/v1/targets/t1/trace", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}

	var env dataEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Target      string `json:"target"`
		Host        string `json:"host"`
		Destination string `json:"destination"`
		Reached     bool   `json:"reached"`
		Hops        []struct {
			TTL     int    `json:"ttl"`
			Addr    string `json:"addr"`
			RTTMS   int64  `json:"rtt_ms"`
			Timeout bool   `json:"timeout"`
		} `json:"hops"`
	}
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatal(err)
	}

	if got.Target != "Chase" || got.Host != "chase.com" || !got.Reached {
		t.Fatalf("response = %+v", got)
	}
	if len(got.Hops) != 2 || got.Hops[0].Addr != "10.0.0.1" || got.Hops[0].RTTMS != 4 {
		t.Fatalf("hops = %+v", got.Hops)
	}
	if tracer.got != netip.MustParseAddr("203.0.113.10") {
		t.Fatalf("traced %v, want the resolved address", tracer.got)
	}
	if len(resolver.hosts) != 1 || resolver.hosts[0] != "chase.com" {
		t.Fatalf("resolved hosts = %v", resolver.hosts)
	}
}

func TestTraceRecordsTimeouts(t *testing.T) {
	t.Parallel()

	tracer := &fakeTracer{result: probe.TraceResult{
		Destination: netip.MustParseAddr("203.0.113.10"),
		Hops: []probe.TraceHop{
			{TTL: 1, Addr: netip.MustParseAddr("10.0.0.1"), RTT: time.Millisecond},
			{TTL: 2},
		},
	}}
	resolver := &fakeResolver{addrs: []netip.Addr{netip.MustParseAddr("203.0.113.10")}}

	rec := do(newTraceHandler(t, traceTargets(), tracer, resolver),
		http.MethodPost, "/api/v1/targets/t1/trace", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}

	var env dataEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Reached bool `json:"reached"`
		Hops    []struct {
			TTL     int    `json:"ttl"`
			Addr    string `json:"addr"`
			Timeout bool   `json:"timeout"`
		} `json:"hops"`
	}
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Reached {
		t.Fatal("a silent path reported reached")
	}
	if len(got.Hops) != 2 || !got.Hops[1].Timeout || got.Hops[1].Addr != "" {
		t.Fatalf("hops = %+v", got.Hops)
	}
}

func TestTraceRateLimit(t *testing.T) {
	t.Parallel()

	tracer := &fakeTracer{result: probe.TraceResult{
		Destination: netip.MustParseAddr("203.0.113.10"),
		Reached:     true,
	}}
	resolver := &fakeResolver{addrs: []netip.Addr{netip.MustParseAddr("203.0.113.10")}}
	h := newTraceHandler(t, traceTargets(), tracer, resolver)

	for i := 0; i < 10; i++ {
		rec := do(h, http.MethodPost, "/api/v1/targets/t1/trace", testToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("run %d: status = %d", i+1, rec.Code)
		}
	}

	rec := do(h, http.MethodPost, "/api/v1/targets/t1/trace", testToken)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 after the hourly budget", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 without a Retry-After hint")
	}
	if tracer.calls != 10 {
		t.Fatalf("tracer ran %d times, want 10", tracer.calls)
	}
}

func TestTraceRejectsBadRequests(t *testing.T) {
	t.Parallel()

	okTracer := &fakeTracer{result: probe.TraceResult{Destination: netip.MustParseAddr("203.0.113.10")}}
	okResolver := &fakeResolver{addrs: []netip.Addr{netip.MustParseAddr("203.0.113.10")}}

	// GET is not an action.
	rec := do(newTraceHandler(t, traceTargets(), okTracer, okResolver),
		http.MethodGet, "/api/v1/targets/t1/trace", testToken)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}

	// Unconfigured.
	rec = do(newHandler(t, testToken, nil), http.MethodPost, "/api/v1/targets/t1/trace", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured status = %d, want 503", rec.Code)
	}

	// Unknown target.
	rec = do(newTraceHandler(t, &fakeTargets{rows: map[string]store.TargetRow{}}, okTracer, okResolver),
		http.MethodPost, "/api/v1/targets/nope/trace", testToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown target status = %d, want 404", rec.Code)
	}

	// A target that does not resolve.
	rec = do(newTraceHandler(t, traceTargets(), okTracer, &fakeResolver{err: errors.New("nxdomain")}),
		http.MethodPost, "/api/v1/targets/t1/trace", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("resolve failure status = %d, want 503", rec.Code)
	}

	// No trace capability: a visible reason, not a 500.
	unavailable := &fakeTracer{result: probe.TraceResult{
		ErrorClass: "trace_unavailable", Err: errors.New("CAP_NET_RAW is required"),
	}}
	rec = do(newTraceHandler(t, traceTargets(), unavailable, okResolver),
		http.MethodPost, "/api/v1/targets/t1/trace", testToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "CAP_NET_RAW") {
		t.Fatalf("reason was not surfaced: %s", rec.Body)
	}

	// IPv6 is unsupported in this release.
	unsupported := &fakeTracer{result: probe.TraceResult{
		ErrorClass: "trace_unsupported", Err: errors.New("IPv4 only"),
	}}
	rec = do(newTraceHandler(t, traceTargets(), unsupported, okResolver),
		http.MethodPost, "/api/v1/targets/t1/trace", testToken)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unsupported status = %d, want 400", rec.Code)
	}

	// Auth is required.
	rec = do(newTraceHandler(t, traceTargets(), okTracer, okResolver),
		http.MethodPost, "/api/v1/targets/t1/trace", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func postIdempotent(h http.Handler, path, token, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestIdempotencyReplaysActions(t *testing.T) {
	t.Parallel()

	scopes := logging.NewScopes("")
	h := newDebugHandler(t, scopes)

	body := `{"target":"chase.com","level":"debug","ttl":"5m"}`
	first := postIdempotent(h, "/api/v1/debug/scopes", testToken, "retry-1", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, body=%s", first.Code, first.Body)
	}
	second := postIdempotent(h, "/api/v1/debug/scopes", testToken, "retry-1", body)
	if second.Code != http.StatusCreated {
		t.Fatalf("replay status = %d", second.Code)
	}
	if second.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("replay was not marked")
	}
	if second.Body.String() != first.Body.String() {
		t.Fatalf("replay body differs:\n%s\n%s", first.Body, second.Body)
	}
	if got := len(scopes.List()); got != 1 {
		t.Fatalf("scopes = %d, want 1: the retry acted twice", got)
	}
}

func TestIdempotencyScopesKeysByAction(t *testing.T) {
	t.Parallel()

	scopes := logging.NewScopes("")
	h := newDebugHandler(t, scopes)

	first := postIdempotent(h, "/api/v1/debug/scopes", testToken, "shared-key",
		`{"target":"chase.com","ttl":"5m"}`)
	second := postIdempotent(h, "/api/v1/debug/scopes", testToken, "shared-key",
		`{"target":"united.com","ttl":"5m"}`)

	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("statuses = %d/%d", first.Code, second.Code)
	}
	// Same key, different target: the key is not a cache of "the last thing I
	// asked for", it is a retry identity. Replaying would silently drop the
	// second scope, which is why the body is not part of the key but the caller
	// is expected to reuse a key only for the same request.
	if second.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("a repeated key was not replayed")
	}
	if got := len(scopes.List()); got != 1 {
		t.Fatalf("scopes = %d, want 1", got)
	}
}

func TestIdempotencyWithoutAKeyActsEveryTime(t *testing.T) {
	t.Parallel()

	scopes := logging.NewScopes("")
	h := newDebugHandler(t, scopes)

	body := `{"target":"chase.com","ttl":"5m"}`
	for i := 0; i < 2; i++ {
		rec := postIdempotent(h, "/api/v1/debug/scopes", testToken, "", body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d", rec.Code)
		}
	}
	if got := len(scopes.List()); got != 2 {
		t.Fatalf("scopes = %d, want 2 without a key", got)
	}
}

func TestIdempotencyRejectsAnOversizedKey(t *testing.T) {
	t.Parallel()

	h := newDebugHandler(t, logging.NewScopes(""))
	rec := postIdempotent(h, "/api/v1/debug/scopes", testToken,
		strings.Repeat("k", 129), `{"target":"chase.com","ttl":"5m"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	t.Parallel()

	rec := do(newHandler(t, testToken, nil), http.MethodGet, "/api/v1/meta", testToken)
	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
		"Cache-Control":          "no-store",
		"X-Robots-Tag":           "noindex, nofollow",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}
