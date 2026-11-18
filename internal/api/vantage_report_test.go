package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/api"
	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
)

type fakeProbeRecorder struct {
	got []store.Probe
	err error
}

func (f *fakeProbeRecorder) Record(_ context.Context, probes []store.Probe) error {
	f.got = append([]store.Probe(nil), probes...)
	return f.err
}

func newVantageHandler(t *testing.T, rec *fakeProbeRecorder) http.Handler {
	t.Helper()
	h, err := api.New(
		api.Config{Listen: "127.0.0.1:2600", Token: testToken},
		api.Deps{
			Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
			Version:       "test",
			StartedAt:     time.Now(),
			ProbeRecorder: rec,
		},
	)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h
}

func TestVantageReportRequiresBearer(t *testing.T) {
	h := newVantageHandler(t, &fakeProbeRecorder{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/vantages/remote/reports", bytes.NewReader([]byte(`{"probes":[]}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestVantageReportRejectsEmptyBatch(t *testing.T) {
	h := newVantageHandler(t, &fakeProbeRecorder{})
	body, _ := json.Marshal(map[string]any{"probes": []any{}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/vantages/remote/reports", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestVantageReportRecordsTheBatch(t *testing.T) {
	rec := &fakeProbeRecorder{}
	h := newVantageHandler(t, rec)

	probe := store.Probe{
		TargetID:  "t1",
		VantageID: "remote",
		TS:        time.Now(),
		State:     state.Up,
	}
	body, _ := json.Marshal(map[string]any{"probes": []store.Probe{probe}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/vantages/remote/reports", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var env struct {
		Data struct {
			Vantage  string `json:"vantage"`
			Recorded int    `json:"recorded"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.Vantage != "remote" || env.Data.Recorded != 1 {
		t.Fatalf("response = %+v", env.Data)
	}
	if len(rec.got) != 1 || rec.got[0].TargetID != "t1" {
		t.Fatalf("recorder got %+v", rec.got)
	}
}

func TestVantageReportRejectsMismatchedVantage(t *testing.T) {
	rec := &fakeProbeRecorder{}
	h := newVantageHandler(t, rec)

	probe := store.Probe{TargetID: "t1", VantageID: "other", TS: time.Now(), State: state.Up}
	body, _ := json.Marshal(map[string]any{"probes": []store.Probe{probe}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/vantages/remote/reports", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if len(rec.got) != 0 {
		t.Fatalf("recorder was called despite mismatch: %+v", rec.got)
	}
}

func TestVantageReportDisabledWithoutRecorder(t *testing.T) {
	h, err := api.New(api.Config{Listen: "127.0.0.1:2600", Token: testToken}, api.Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version:   "test",
		StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"probes": []store.Probe{{TargetID: "t1", VantageID: "remote", TS: time.Now(), State: state.Up}}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/vantages/remote/reports", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rr.Code, rr.Body.String())
	}
}

func TestVantageReportMethodNotAllowed(t *testing.T) {
	h := newVantageHandler(t, &fakeProbeRecorder{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/vantages/remote/reports", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}

func TestVantageReportUnknownSuffix(t *testing.T) {
	h := newVantageHandler(t, &fakeProbeRecorder{})
	body, _ := json.Marshal(map[string]any{"probes": []store.Probe{{TargetID: "t1", VantageID: "remote", TS: time.Now(), State: state.Up}}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/vantages/remote/nope", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestVantageReportMalformedBody(t *testing.T) {
	h := newVantageHandler(t, &fakeProbeRecorder{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/vantages/remote/reports", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestVantageReportMissingTargetID(t *testing.T) {
	h := newVantageHandler(t, &fakeProbeRecorder{})
	probe := store.Probe{VantageID: "remote", TS: time.Now(), State: state.Up}
	body, _ := json.Marshal(map[string]any{"probes": []store.Probe{probe}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/vantages/remote/reports", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestVantageReportRecorderError(t *testing.T) {
	rec := &fakeProbeRecorder{err: io.ErrUnexpectedEOF}
	h := newVantageHandler(t, rec)
	probe := store.Probe{TargetID: "t1", VantageID: "remote", TS: time.Now(), State: state.Up}
	body, _ := json.Marshal(map[string]any{"probes": []store.Probe{probe}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/vantages/remote/reports", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}
