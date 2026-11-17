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
	"time"
)

// fakeDebugAPI mimics the debug-scope endpoints closely enough to test the CLI
// contract: status codes, envelope shape, bearer auth, and method handling.
type fakeDebugAPI struct {
	mu      sync.Mutex
	scopes  map[string]debugScope
	token   string
	created int
}

func newFakeDebugAPI(t *testing.T) (*fakeDebugAPI, *httptest.Server) {
	t.Helper()

	api := &fakeDebugAPI{scopes: make(map[string]debugScope), token: "cli-token"}
	mux := http.NewServeMux()

	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+api.token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"missing bearer token"}}`))
			return false
		}
		return true
	}

	mux.HandleFunc("/api/v1/debug/scopes", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			api.mu.Lock()
			list := make([]debugScope, 0, len(api.scopes))
			for _, sc := range api.scopes {
				list = append(list, sc)
			}
			api.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": list, "meta": map[string]any{"count": len(list)}})

		case http.MethodPost:
			var req map[string]string
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			ttl, err := time.ParseDuration(req["ttl"])
			if err != nil || ttl <= 0 || ttl > time.Hour {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"code":"invalid_argument","message":"ttl must be a duration such as 10m"}}`))
				return
			}
			api.mu.Lock()
			api.created++
			sc := debugScope{
				ID:        "dbg-" + strings.Repeat("x", api.created),
				Target:    req["target"],
				Component: req["component"],
				Level:     req["level"],
				CreatedAt: time.Now().UTC(),
				ExpiresAt: time.Now().UTC().Add(ttl),
			}
			api.scopes[sc.ID] = sc
			api.mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": sc, "meta": map[string]any{}})

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/v1/debug/scopes/", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/debug/scopes/")
		api.mu.Lock()
		_, ok := api.scopes[id]
		delete(api.scopes, id)
		api.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	return api, httptest.NewServer(mux)
}

func setupDebugCLI(t *testing.T, srv *httptest.Server, token string) {
	t.Helper()
	t.Setenv("NETWEATHER_API_URL", srv.URL)
	t.Setenv("NETWEATHER_UI_TOKEN", token)
	t.Setenv("NETWEATHER_UI_TOKEN_FILE", "")
}

func TestDebugCLIEnableStatusDisable(t *testing.T) {
	api, srv := newFakeDebugAPI(t)
	defer srv.Close()
	setupDebugCLI(t, srv, api.token)

	var out bytes.Buffer
	if err := run([]string{
		"debug", "enable", "--target", "chase.com", "--level", "trace", "--for", "10m",
	}, &out); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !strings.Contains(out.String(), "enabled until") {
		t.Fatalf("enable output = %q", out.String())
	}

	api.mu.Lock()
	var id string
	for got := range api.scopes {
		id = got
	}
	api.mu.Unlock()
	if id == "" {
		t.Fatal("the API never received a scope")
	}

	out.Reset()
	if err := run([]string{"debug", "status"}, &out); err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, want := range []string{id, "trace", "target=chase.com", "expires"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status output missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := run([]string{"debug", "disable", "--id", id}, &out); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if !strings.Contains(out.String(), "disabled") {
		t.Fatalf("disable output = %q", out.String())
	}

	api.mu.Lock()
	remaining := len(api.scopes)
	api.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("scopes left = %d", remaining)
	}
}

func TestDebugCLIStatusJSON(t *testing.T) {
	api, srv := newFakeDebugAPI(t)
	defer srv.Close()
	setupDebugCLI(t, srv, api.token)

	// Empty is a valid answer in both forms: an empty array, not null.
	out := &bytes.Buffer{}
	if err := run([]string{"debug", "status", "--json"}, out); err != nil {
		t.Fatalf("status --json: %v", err)
	}
	var empty struct {
		Data []debugScope `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &empty); err != nil {
		t.Fatalf("output is not JSON: %v: %s", err, out.String())
	}
	if empty.Data == nil || len(empty.Data) != 0 {
		t.Fatalf("empty status data = %#v, want an empty array", empty.Data)
	}

	if err := run([]string{
		"debug", "enable", "--target", "chase.com", "--component", "agent", "--for", "5m",
	}, io.Discard); err != nil {
		t.Fatalf("enable: %v", err)
	}

	out.Reset()
	if err := run([]string{"debug", "status", "--json"}, out); err != nil {
		t.Fatalf("status --json: %v", err)
	}
	var env struct {
		Data []debugScope `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("output is not JSON: %v: %s", err, out.String())
	}
	if len(env.Data) != 1 {
		t.Fatalf("scopes = %+v", env.Data)
	}
	if env.Data[0].Target != "chase.com" || env.Data[0].Component != "agent" {
		t.Fatalf("scope = %+v", env.Data[0])
	}
}

func TestDebugCLIDisableAll(t *testing.T) {
	api, srv := newFakeDebugAPI(t)
	defer srv.Close()
	setupDebugCLI(t, srv, api.token)

	for i := 0; i < 2; i++ {
		if err := run([]string{
			"debug", "enable", "--component", "feeds", "--for", "5m",
		}, io.Discard); err != nil {
			t.Fatalf("enable: %v", err)
		}
	}

	out := &bytes.Buffer{}
	if err := run([]string{"debug", "disable", "--all"}, out); err != nil {
		t.Fatalf("disable --all: %v", err)
	}
	if !strings.Contains(out.String(), "disabled 2 scope") {
		t.Fatalf("output = %q", out.String())
	}

	api.mu.Lock()
	remaining := len(api.scopes)
	api.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("scopes left = %d", remaining)
	}
}

func TestDebugCLIStatusEmpty(t *testing.T) {
	api, srv := newFakeDebugAPI(t)
	defer srv.Close()
	setupDebugCLI(t, srv, api.token)

	out := &bytes.Buffer{}
	if err := run([]string{"debug", "status"}, out); err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out.String(), "no active debug scopes") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestDebugCLIValidationAndFailures(t *testing.T) {
	api, srv := newFakeDebugAPI(t)
	defer srv.Close()

	// No token: the CLI must refuse before making a request.
	t.Setenv("NETWEATHER_API_URL", srv.URL)
	t.Setenv("NETWEATHER_UI_TOKEN", "")
	t.Setenv("NETWEATHER_UI_TOKEN_FILE", "")
	if err := run([]string{"debug", "status"}, io.Discard); err == nil {
		t.Fatal("status without a token should fail")
	}

	setupDebugCLI(t, srv, api.token)

	for _, args := range [][]string{
		{"debug"},
		{"debug", "nonsense"},
		{"debug", "enable", "--target", "chase.com"}, // no --for
		{"debug", "enable", "--for", "2h"},           // no selector
		{"debug", "disable"},                         // no --all/--id
		{"debug", "disable", "--all", "--id", "dbg-x"},
		{"debug", "status", "extra"},
	} {
		if err := run(args, io.Discard); err == nil {
			t.Errorf("%v: expected an error", args)
		}
	}

	// A wrong token surfaces the API's message rather than a bare status.
	setupDebugCLI(t, srv, "wrong-token")
	err := run([]string{"debug", "status"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "missing bearer token") {
		t.Fatalf("wrong token error = %v", err)
	}
}
