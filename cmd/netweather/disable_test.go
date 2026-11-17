package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fakeDisableAPI struct {
	mu       sync.Mutex
	disabled []string
}

func newFakeDisableAPI(t *testing.T) (*fakeDisableAPI, *httptest.Server) {
	t.Helper()

	api := &fakeDisableAPI{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer disable-token-0123456789" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/targets":
			_, _ = io.WriteString(w, `{"data":[{"id":"svc-chase","host":"chase.com"}],"meta":{}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/targets/svc-chase/disable":
			api.mu.Lock()
			api.disabled = append(api.disabled, r.URL.Path)
			api.mu.Unlock()
			_, _ = io.WriteString(w, `{"data":{"target":"svc-chase","state":"disabled"},"meta":{}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return api, srv
}

func setupDisableCLI(t *testing.T, srv *httptest.Server) {
	t.Helper()
	t.Setenv("NETWEATHER_API_URL", srv.URL)
	t.Setenv("NETWEATHER_UI_TOKEN", "disable-token-0123456789")
	t.Setenv("NETWEATHER_UI_TOKEN_FILE", "")
}

func TestDisableCLIStopsAHostByName(t *testing.T) {
	api, srv := newFakeDisableAPI(t)
	setupDisableCLI(t, srv)

	var out bytes.Buffer
	if err := run([]string{"disable", "chase.com"}, &out); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if !strings.Contains(out.String(), "disabled chase.com (svc-chase)") {
		t.Fatalf("output = %q", out.String())
	}

	api.mu.Lock()
	got := append([]string(nil), api.disabled...)
	api.mu.Unlock()
	if len(got) != 1 || got[0] != "/api/v1/targets/svc-chase/disable" {
		t.Fatalf("disabled = %v", got)
	}
}

func TestDisableCLIAcceptsTheIDAndJSON(t *testing.T) {
	api, srv := newFakeDisableAPI(t)
	setupDisableCLI(t, srv)

	var out bytes.Buffer
	if err := run([]string{"disable", "-json", "svc-chase"}, &out); err != nil {
		t.Fatalf("disable: %v", err)
	}
	var env struct {
		Data struct {
			Target string `json:"target"`
			Host   string `json:"host"`
			State  string `json:"state"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("output is not an envelope: %v (%s)", err, out.String())
	}
	if env.Data.Target != "svc-chase" || env.Data.Host != "chase.com" || env.Data.State != "disabled" {
		t.Fatalf("data = %+v", env.Data)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.disabled) != 1 {
		t.Fatalf("disabled = %v", api.disabled)
	}
}

func TestDisableCLIRejectsAnUnknownTarget(t *testing.T) {
	_, srv := newFakeDisableAPI(t)
	setupDisableCLI(t, srv)

	var out bytes.Buffer
	err := run([]string{"disable", "nope.example"}, &out)
	if err == nil || !strings.Contains(err.Error(), "no target matches") {
		t.Fatalf("error = %v, want a no-match refusal", err)
	}

	if err := run([]string{"disable"}, &out); err == nil {
		t.Fatal("a missing target argument was accepted")
	}
}
