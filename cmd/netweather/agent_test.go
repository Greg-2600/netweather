package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/store"
)

func TestRemoteSinkPostsTheBatch(t *testing.T) {
	t.Parallel()

	var (
		mu      sync.Mutex
		gotPath string
		gotAuth string
		gotBody map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"vantage":"remote","recorded":1}}`))
	}))
	t.Cleanup(srv.Close)

	sink := &remoteSink{server: srv.URL, vantage: "remote", token: "secret"}
	err := sink.Record(context.Background(), []store.Probe{
		{TargetID: "t1", VantageID: "remote", TS: time.Now()},
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/api/v1/vantages/remote/reports" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("auth = %q", gotAuth)
	}
	probes, _ := gotBody["probes"].([]any)
	if len(probes) != 1 {
		t.Fatalf("probes = %v", gotBody)
	}
}

func TestRemoteSinkSurfacesServerError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"no"}}`))
	}))
	t.Cleanup(srv.Close)

	sink := &remoteSink{server: srv.URL, vantage: "remote", token: "secret"}
	err := sink.Record(context.Background(), []store.Probe{{TargetID: "t1", VantageID: "remote", TS: time.Now()}})
	if err == nil {
		t.Fatal("expected an error on 503")
	}
}
