package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/api"
	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/logging"
	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
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
	existsErr error
	rowsErr   error
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

func TestTargetProbesRequiresTheProbesSuffix(t *testing.T) {
	t.Parallel()

	rec := do(newProbesHandler(t, probeFixture()), http.MethodGet, "/api/v1/targets/t1", testToken)
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
