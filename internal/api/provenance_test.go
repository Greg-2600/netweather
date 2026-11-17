package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/api"
	"github.com/Greg-2600/netweather/internal/store"
)

type fakeProvenance struct {
	rows []store.TargetSource
	err  error
}

func (f *fakeProvenance) TargetSources(context.Context, string) ([]store.TargetSource, error) {
	return f.rows, f.err
}

func provenanceHandler(t *testing.T, targets api.TargetReader, prov api.ProvenanceReader) http.Handler {
	t.Helper()

	h, err := api.New(
		api.Config{Listen: "127.0.0.1:2600", Token: testToken},
		api.Deps{
			Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
			Targets:    targets,
			Provenance: prov,
			Vantage:    "local",
			Version:    "test",
			StartedAt:  time.Now(),
		},
	)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h
}

func provenanceTargets() *fakeTargets {
	return &fakeTargets{rows: map[string]store.TargetRow{
		"t1": {
			Target: store.Target{
				ID: "t1", Host: "chase.com", Port: 443, Probe: "http",
				Name: "Chase", Class: "service", Sector: "financial/banking",
			},
		},
	}}
}

const provenanceSHA = "9f2c1a7b4e8d03569f2c1a7b4e8d03569f2c1a7b4e8d03569f2c1a7b4e8d0356"

func TestTargetDetailCarriesProvenance(t *testing.T) {
	t.Parallel()

	fetched := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	prov := &fakeProvenance{rows: []store.TargetSource{
		{TargetID: "t1", Source: "seed:builtin", FetchedAt: fetched, SHA256: provenanceSHA},
		{TargetID: "t1", Source: "config:/config/targets.yaml", FetchedAt: fetched, SHA256: "abc123"},
	}}

	rec := do(provenanceHandler(t, provenanceTargets(), prov),
		http.MethodGet, "/api/v1/targets/t1", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}

	var env struct {
		Data struct {
			Sources []struct {
				Source    string `json:"source"`
				FetchedAt string `json:"fetched_at"`
				SHA256    string `json:"sha256"`
			} `json:"sources"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Sources) != 2 {
		t.Fatalf("sources = %+v", env.Data.Sources)
	}
	if env.Data.Sources[0].Source != "seed:builtin" ||
		env.Data.Sources[0].SHA256 != provenanceSHA ||
		env.Data.Sources[0].FetchedAt != "2026-11-17T12:00:00Z" {
		t.Fatalf("first source = %+v", env.Data.Sources[0])
	}
	if env.Data.Sources[1].Source != "config:/config/targets.yaml" {
		t.Fatalf("second source = %+v", env.Data.Sources[1])
	}
}

func TestTargetProvenanceFailureIsNotATargetFailure(t *testing.T) {
	t.Parallel()

	// A provenance read that fails leaves the target readable; the block is
	// simply absent, and the handler logs the reason.
	rec := do(provenanceHandler(t, provenanceTargets(),
		&fakeProvenance{err: errors.New("db down")}),
		http.MethodGet, "/api/v1/targets/t1", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), `"sources"`) {
		t.Fatalf("a failed provenance read emitted a block: %s", rec.Body)
	}

	// An unwired reader is the same: no invented source, no error.
	rec = do(provenanceHandler(t, provenanceTargets(), nil),
		http.MethodGet, "/api/v1/targets/t1", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), `"sources"`) {
		t.Fatalf("an unwired reader emitted a block: %s", rec.Body)
	}
}
