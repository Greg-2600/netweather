package api_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/Greg-2600/netweather/internal/api"
	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/logging"
	"github.com/Greg-2600/netweather/internal/probe"
)

// assertJSONError checks the failure contract shared by every handler: the
// status and stable code are what the client branches on, the body is JSON, and
// no credential or internal detail is echoed back.
func assertJSONError(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()

	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, wantStatus, rec.Body.String())
	}
	if wantStatus < 500 && rec.Code >= 500 {
		t.Fatalf("status = %d; invalid input must be a 4xx, not a server error (body %q)",
			rec.Code, rec.Body.String())
	}

	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("error body is not JSON: %v (body %q)", err, rec.Body.String())
	}
	if env.Error.Code != wantCode {
		t.Fatalf("error code = %q, want %q (body %q)", env.Error.Code, wantCode, rec.Body.String())
	}

	for _, secret := range []string{testToken, "wrong-token-value", "hunter2", "postgres://"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("error body leaked %q: %s", secret, rec.Body.String())
		}
	}
}

func serve(h http.Handler, method, path, token, contentType, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestProtectedAPIRoutesRequireBearer(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"board", http.MethodGet, "/api/v1/board"},
		{"targets", http.MethodGet, "/api/v1/targets"},
		{"control", http.MethodGet, "/api/v1/control"},
		{"meta", http.MethodGet, "/api/v1/meta"},
		{"logs", http.MethodGet, "/api/v1/logs"},
		{"target detail", http.MethodGet, "/api/v1/targets/t1"},
		{"target probes", http.MethodGet, "/api/v1/targets/t1/probes"},
		{"target series", http.MethodGet, "/api/v1/targets/t1/series"},
		{"scope", http.MethodGet, "/api/v1/scopes/global"},
		{"anomalies", http.MethodGet, "/api/v1/anomalies"},
		{"coverage", http.MethodGet, "/api/v1/coverage"},
		{"coverage history", http.MethodGet, "/api/v1/coverage/history"},
		{"corridors", http.MethodGet, "/api/v1/corridors"},
		{"ihi", http.MethodGet, "/api/v1/ihi"},
		{"selfcheck", http.MethodGet, "/api/v1/selfcheck"},
		{"export", http.MethodGet, "/api/v1/export"},
		{"events", http.MethodGet, "/api/v1/events"},
		{"feeds", http.MethodGet, "/api/v1/feeds"},
		{"incidents", http.MethodGet, "/api/v1/incidents"},
		{"incident detail", http.MethodGet, "/api/v1/incidents/inc-1"},
		{"debug scopes list", http.MethodGet, "/api/v1/debug/scopes"},
		{"pause", http.MethodPost, "/api/v1/pause"},
		{"resume", http.MethodPost, "/api/v1/resume"},
		{"target disable", http.MethodPost, "/api/v1/targets/t1/disable"},
		{"target trace", http.MethodPost, "/api/v1/targets/t1/trace"},
		{"incident annotation", http.MethodPost, "/api/v1/incidents/inc-1/annotation"},
		{"debug scope create", http.MethodPost, "/api/v1/debug/scopes"},
		{"vantage report", http.MethodPost, "/api/v1/vantages/remote/reports"},
		{"debug scope delete", http.MethodDelete, "/api/v1/debug/scopes/scope-1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(newHandler(t, testToken, nil), tc.method, tc.path, "")
			assertJSONError(t, rec, http.StatusUnauthorized, api.CodeUnauthorized)
			if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
				t.Fatalf("WWW-Authenticate = %q, want a Bearer challenge", got)
			}
		})
	}
}

func TestWrongBearerTokenIsRejectedAndNotEchoed(t *testing.T) {
	rec := do(newHandler(t, testToken, nil), http.MethodGet, "/api/v1/meta", "wrong-token-value")
	assertJSONError(t, rec, http.StatusUnauthorized, api.CodeUnauthorized)
}

func TestKnownAPIRoutesRejectWrongMethods(t *testing.T) {
	debugHandler := func(t *testing.T) http.Handler {
		return newDebugHandler(t, logging.NewScopes(""))
	}

	cases := []struct {
		name    string
		method  string
		path    string
		handler func(*testing.T) http.Handler
	}{
		{"meta is read only", http.MethodPost, "/api/v1/meta", nil},
		{"board is read only", http.MethodDelete, "/api/v1/board", nil},
		{"targets is read only", http.MethodPost, "/api/v1/targets", nil},
		{"incidents is read only", http.MethodPost, "/api/v1/incidents", nil},
		{"coverage is read only", http.MethodPost, "/api/v1/coverage", nil},
		{"coverage history is read only", http.MethodPost, "/api/v1/coverage/history", nil},
		{"export is read only", http.MethodPost, "/api/v1/export", nil},
		{"events is read only", http.MethodPost, "/api/v1/events", nil},
		{"feeds is read only", http.MethodPost, "/api/v1/feeds", nil},
		{"scope is read only", http.MethodPost, "/api/v1/scopes/global", nil},
		{"probes is read only", http.MethodPost, "/api/v1/targets/t1/probes", nil},
		{"pause is write only", http.MethodGet, "/api/v1/pause", nil},
		{"resume is write only", http.MethodPut, "/api/v1/resume", nil},
		{"disable is write only", http.MethodGet, "/api/v1/targets/t1/disable", nil},
		{"trace is write only", http.MethodGet, "/api/v1/targets/t1/trace", nil},
		{"annotation is write only", http.MethodGet, "/api/v1/incidents/inc-1/annotation", nil},
		{"vantage report is write only", http.MethodGet, "/api/v1/vantages/remote/reports", nil},
		{"debug scopes supports get and post", http.MethodPut, "/api/v1/debug/scopes", debugHandler},
		{"debug scope by id is delete only", http.MethodGet, "/api/v1/debug/scopes/scope-1", debugHandler},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandler(t, testToken, nil)
			if tc.handler != nil {
				h = tc.handler(t)
			}
			rec := do(h, tc.method, tc.path, testToken)
			assertJSONError(t, rec, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed)
			if got := rec.Header().Get("Allow"); got == "" {
				t.Fatal("405 response has no Allow header")
			}
		})
	}
}

func TestAnnotationBodyValidation(t *testing.T) {
	h := newAnnotationHandler(t, &fakeAnnotator{})

	cases := []struct {
		name        string
		body        string
		contentType string
		wantStatus  int
		wantCode    string
	}{
		{"empty body", "", "application/json", http.StatusBadRequest, api.CodeInvalidArgument},
		{"bad json", `{"annotation":`, "application/json", http.StatusBadRequest, api.CodeInvalidArgument},
		{"unknown field", `{"annotation":"x","bogus":1}`, "application/json", http.StatusBadRequest, api.CodeInvalidArgument},
		{"empty annotation", `{"annotation":"   "}`, "application/json", http.StatusBadRequest, api.CodeInvalidArgument},
		{"too long", `{"annotation":"` + strings.Repeat("x", 501) + `"}`, "application/json", http.StatusBadRequest, api.CodeInvalidArgument},
		{"trailing object", `{"annotation":"x"}{"annotation":"y"}`, "application/json", http.StatusBadRequest, api.CodeInvalidArgument},
		{"wrong content type", `{"annotation":"x"}`, "text/plain", http.StatusUnsupportedMediaType, api.CodeInvalidArgument},
		{"oversized body", `{"annotation":"x"}` + strings.Repeat(" ", maxActionBodyForTest), "application/json", http.StatusRequestEntityTooLarge, api.CodePayloadTooLarge},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(h, http.MethodPost, "/api/v1/incidents/inc-1/annotation",
				testToken, tc.contentType, tc.body)
			assertJSONError(t, rec, tc.wantStatus, tc.wantCode)
		})
	}
}

func TestDebugScopeBodyValidation(t *testing.T) {
	h := newDebugHandler(t, logging.NewScopes(""))

	cases := []struct {
		name        string
		body        string
		contentType string
		wantStatus  int
		wantCode    string
	}{
		{"empty body", "", "application/json", http.StatusBadRequest, api.CodeInvalidArgument},
		{"bad json", `{"target":`, "application/json", http.StatusBadRequest, api.CodeInvalidArgument},
		{"unknown field", `{"target":"chase.com","ttl":"1m","bogus":1}`, "application/json", http.StatusBadRequest, api.CodeInvalidArgument},
		{"no selectors", `{"level":"debug","ttl":"1m"}`, "application/json", http.StatusBadRequest, api.CodeInvalidArgument},
		{"missing ttl", `{"target":"chase.com"}`, "application/json", http.StatusBadRequest, api.CodeInvalidArgument},
		{"ttl over the ceiling", `{"target":"chase.com","ttl":"2h"}`, "application/json", http.StatusBadRequest, api.CodeInvalidArgument},
		{"trailing object", `{"target":"chase.com","ttl":"1m"}{}`, "application/json", http.StatusBadRequest, api.CodeInvalidArgument},
		{"wrong content type", `{}`, "text/plain", http.StatusUnsupportedMediaType, api.CodeInvalidArgument},
		{"oversized body", `{"target":"chase.com","ttl":"1m"}` + strings.Repeat(" ", maxActionBodyForTest), "application/json", http.StatusRequestEntityTooLarge, api.CodePayloadTooLarge},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(h, http.MethodPost, "/api/v1/debug/scopes",
				testToken, tc.contentType, tc.body)
			assertJSONError(t, rec, tc.wantStatus, tc.wantCode)
		})
	}
}

func TestVantageReportBodyValidation(t *testing.T) {
	t.Setenv("NETWEATHER_VANTAGE_SIGNING_SECRET", "")
	t.Setenv("NETWEATHER_VANTAGE_CLIENT_CA", "")

	h := newVantageHandler(t, &fakeProbeRecorder{})

	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"empty body", "", http.StatusBadRequest, api.CodeInvalidArgument},
		{"bad json", `{"probes":`, http.StatusBadRequest, api.CodeInvalidArgument},
		{"empty probes", `{"probes":[]}`, http.StatusBadRequest, api.CodeInvalidArgument},
		{"missing target", `{"probes":[{"VantageID":"remote"}]}`, http.StatusBadRequest, api.CodeInvalidArgument},
		{"mismatched vantage", `{"probes":[{"TargetID":"t1","VantageID":"other"}]}`, http.StatusBadRequest, api.CodeInvalidArgument},
		{"oversized body", strings.Repeat("x", (4<<20)+1), http.StatusRequestEntityTooLarge, api.CodePayloadTooLarge},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(h, http.MethodPost, "/api/v1/vantages/remote/reports",
				testToken, "application/json", tc.body)
			assertJSONError(t, rec, tc.wantStatus, tc.wantCode)
		})
	}
}

func TestInvalidResourceIDsRejected(t *testing.T) {
	cases := []struct {
		name        string
		method      string
		path        string
		contentType string
		body        string
		handler     func(*testing.T) http.Handler
	}{
		{"empty target id", http.MethodGet, "/api/v1/targets/", "", "", nil},
		{"unknown target suffix", http.MethodGet, "/api/v1/targets/t1/nope", "", "", nil},
		{"nested target suffix", http.MethodGet, "/api/v1/targets/t1/extra/probes", "", "", func(t *testing.T) http.Handler {
			return newProbesHandler(t, probeFixture())
		}},
		{"nested target disable", http.MethodPost, "/api/v1/targets/a/b/disable", "", "", func(t *testing.T) http.Handler {
			return newTargetControlHandler(t, &fakeTargetControl{found: true})
		}},
		{"nested target trace", http.MethodPost, "/api/v1/targets/a/b/trace", "", "", func(t *testing.T) http.Handler {
			return newTraceHandler(t, traceTargets(),
				&fakeTracer{result: probe.TraceResult{Destination: netip.MustParseAddr("203.0.113.10")}},
				&fakeResolver{addrs: []netip.Addr{netip.MustParseAddr("203.0.113.10")}})
		}},
		{"empty incident id", http.MethodGet, "/api/v1/incidents/", "", "", func(t *testing.T) http.Handler {
			return newIncidentsHandler(t, incidentFixture())
		}},
		{"nested incident id", http.MethodGet, "/api/v1/incidents/a/b", "", "", func(t *testing.T) http.Handler {
			return newIncidentsHandler(t, incidentFixture())
		}},
		{"nested annotation id", http.MethodPost, "/api/v1/incidents/a/b/annotation", "application/json", `{"annotation":"x"}`, func(t *testing.T) http.Handler {
			return newAnnotationHandler(t, &fakeAnnotator{})
		}},
		{"empty vantage id", http.MethodGet, "/api/v1/vantages/", "", "", nil},
		{"missing vantage suffix", http.MethodGet, "/api/v1/vantages/remote", "", "", nil},
		{"unknown vantage suffix", http.MethodPost, "/api/v1/vantages/remote/nope", "", "", nil},
		{"empty scope", http.MethodGet, "/api/v1/scopes/", "", "", func(t *testing.T) http.Handler {
			return newScopesHandler(t, &fakeScopes{})
		}},
		{"nested scope", http.MethodGet, "/api/v1/scopes/global/extra", "", "", func(t *testing.T) http.Handler {
			return newScopesHandler(t, &fakeScopes{})
		}},
		{"empty debug scope id", http.MethodDelete, "/api/v1/debug/scopes/", "", "", func(t *testing.T) http.Handler {
			return newDebugHandler(t, logging.NewScopes(""))
		}},
		{"nested debug scope id", http.MethodDelete, "/api/v1/debug/scopes/a/b", "", "", func(t *testing.T) http.Handler {
			return newDebugHandler(t, logging.NewScopes(""))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandler(t, testToken, nil)
			if tc.handler != nil {
				h = tc.handler(t)
			}
			rec := serve(h, tc.method, tc.path, testToken, tc.contentType, tc.body)
			assertJSONError(t, rec, http.StatusNotFound, api.CodeNotFound)
		})
	}
}

func TestInvalidQueryParametersRejected(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		handler func(*testing.T) http.Handler
	}{
		{"logs limit zero", "/api/v1/logs?limit=0", nil},
		{"logs limit over max", "/api/v1/logs?limit=501", nil},
		{"logs limit not a number", "/api/v1/logs?limit=abc", nil},
		{"logs since not a timestamp", "/api/v1/logs?since=yesterday", nil},
		{"logs unknown level", "/api/v1/logs?level=loud", nil},
		{"targets unknown state", "/api/v1/targets?state=not-a-state", func(t *testing.T) http.Handler {
			return newBoardHandler(t, board.NewMemory())
		}},
		{"probes limit zero", "/api/v1/targets/t1/probes?limit=0", func(t *testing.T) http.Handler {
			return newProbesHandler(t, probeFixture())
		}},
		{"series unknown resolution", "/api/v1/targets/t1/series?resolution=2m", func(t *testing.T) http.Handler {
			return newProbesHandler(t, probeFixture())
		}},
		{"series bad from", "/api/v1/targets/t1/series?from=not-a-time", func(t *testing.T) http.Handler {
			return newProbesHandler(t, probeFixture())
		}},
		{"series inverted window", "/api/v1/targets/t1/series?from=2026-11-18T00:00:00Z&to=2026-11-17T00:00:00Z", func(t *testing.T) http.Handler {
			return newProbesHandler(t, probeFixture())
		}},
		{"incidents limit zero", "/api/v1/incidents?limit=0", func(t *testing.T) http.Handler {
			return newIncidentsHandler(t, incidentFixture())
		}},
		{"incidents bad cursor", "/api/v1/incidents?cursor=not-a-cursor", func(t *testing.T) http.Handler {
			return newIncidentsHandler(t, incidentFixture())
		}},
		{"anomalies limit zero", "/api/v1/anomalies?limit=0", func(t *testing.T) http.Handler {
			return newAnomaliesHandler(t, &fakeAnomalies{})
		}},
		{"feeds limit zero", "/api/v1/feeds?limit=0", func(t *testing.T) http.Handler {
			return newFeedsHandler(t, &fakeFeeds{})
		}},
		{"unknown scope", "/api/v1/scopes/bogus", func(t *testing.T) http.Handler {
			return newScopesHandler(t, &fakeScopes{})
		}},
		{"scope missing value", "/api/v1/scopes/sector", func(t *testing.T) http.Handler {
			return newScopesHandler(t, &fakeScopes{})
		}},
		{"scope invalid asn", "/api/v1/scopes/asn?value=-1", func(t *testing.T) http.Handler {
			return newScopesHandler(t, &fakeScopes{})
		}},
		{"history unknown scope", "/api/v1/coverage/history?scope=bogus", func(t *testing.T) http.Handler {
			return newCoverageHistoryHandler(t, &fakeCoverageHistory{})
		}},
		{"history missing value", "/api/v1/coverage/history?scope=sector", func(t *testing.T) http.Handler {
			return newCoverageHistoryHandler(t, &fakeCoverageHistory{})
		}},
		{"history invalid asn", "/api/v1/coverage/history?scope=asn&value=abc", func(t *testing.T) http.Handler {
			return newCoverageHistoryHandler(t, &fakeCoverageHistory{})
		}},
		{"history days zero", "/api/v1/coverage/history?days=0", func(t *testing.T) http.Handler {
			return newCoverageHistoryHandler(t, &fakeCoverageHistory{})
		}},
		{"history days over max", "/api/v1/coverage/history?days=366", func(t *testing.T) http.Handler {
			return newCoverageHistoryHandler(t, &fakeCoverageHistory{})
		}},
		{"history days not a number", "/api/v1/coverage/history?days=abc", func(t *testing.T) http.Handler {
			return newCoverageHistoryHandler(t, &fakeCoverageHistory{})
		}},
		{"export unknown format", "/api/v1/export?format=xml", func(t *testing.T) http.Handler {
			return newExportHandler(t, &fakeExporter{})
		}},
		{"export bad from", "/api/v1/export?from=not-a-time", func(t *testing.T) http.Handler {
			return newExportHandler(t, &fakeExporter{})
		}},
		{"export inverted window", "/api/v1/export?from=2026-11-18T00:00:00Z&to=2026-11-17T00:00:00Z", func(t *testing.T) http.Handler {
			return newExportHandler(t, &fakeExporter{})
		}},
		{"export window over cap", "/api/v1/export?from=2026-01-01T00:00:00Z&to=2026-03-01T00:00:00Z", func(t *testing.T) http.Handler {
			return newExportHandler(t, &fakeExporter{})
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandler(t, testToken, nil)
			if tc.handler != nil {
				h = tc.handler(t)
			}
			rec := do(h, http.MethodGet, tc.path, testToken)
			assertJSONError(t, rec, http.StatusBadRequest, api.CodeInvalidArgument)
		})
	}
}

func TestStorageFailuresDoNotLeakDetails(t *testing.T) {
	const sentinel = "postgres://user:hunter2@db.internal"

	t.Run("annotation", func(t *testing.T) {
		h := newAnnotationHandler(t, &fakeAnnotator{err: errors.New(sentinel)})
		rec := serve(h, http.MethodPost, "/api/v1/incidents/inc-1/annotation",
			testToken, "application/json", `{"annotation":"x"}`)
		assertJSONError(t, rec, http.StatusServiceUnavailable, api.CodeUnavailable)
	})

	t.Run("vantage report", func(t *testing.T) {
		t.Setenv("NETWEATHER_VANTAGE_SIGNING_SECRET", "")
		t.Setenv("NETWEATHER_VANTAGE_CLIENT_CA", "")
		h := newVantageHandler(t, &fakeProbeRecorder{err: errors.New(sentinel)})
		rec := serve(h, http.MethodPost, "/api/v1/vantages/remote/reports",
			testToken, "application/json", `{"probes":[{"TargetID":"t1","VantageID":"remote"}]}`)
		assertJSONError(t, rec, http.StatusServiceUnavailable, api.CodeUnavailable)
	})
}

// maxActionBodyForTest is one byte over the documented 8 KiB action-body cap.
const maxActionBodyForTest = 8<<10 + 1
