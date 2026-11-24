package web_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/web"
)

func accessServer(t *testing.T, src board.Source, logs *bytes.Buffer) *httptest.Server {
	t.Helper()

	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h, err := web.New(web.Deps{
		Logger:    logger,
		Source:    src,
		Token:     testToken,
		Version:   "test",
		StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

// findAccessLine returns the http_request record for one route.
func findAccessLine(t *testing.T, logs *bytes.Buffer, route string) map[string]any {
	t.Helper()

	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["event"] == "http_request" && rec["route"] == route {
			return rec
		}
	}
	t.Fatalf("no access line for %s in:\n%s", route, logs.String())
	return nil
}

func TestBoardRequestsAreLogged(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	ts := accessServer(t, fixture(), &logs)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)

	rec := findAccessLine(t, &logs, "/")
	if rec["level"] != "DEBUG" {
		t.Errorf("level = %v, want DEBUG for a 200", rec["level"])
	}
	if status, _ := rec["status"].(float64); status != http.StatusOK {
		t.Errorf("status = %v, want 200", rec["status"])
	}
	if id, _ := rec["request_id"].(string); id == "" {
		t.Error("the access line carries no request id")
	}
	if method, _ := rec["method"].(string); method != http.MethodGet {
		t.Errorf("method = %v", rec["method"])
	}
}

func TestAppStylesUseReadableNativeFont(t *testing.T) {
	t.Parallel()

	css, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	text := string(css)
	for _, want := range []string{
		"font: 16px/1.5",
		"text-rendering: optimizeLegibility",
		"-webkit-font-smoothing: antialiased",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("app.css missing %q", want)
		}
	}
}

func TestServerErrorsAreLoggedAtWarn(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	ts := accessServer(t, failingSource{err: errors.New("db down")}, &logs)

	// Log in against a healthy instance: the session is an HMAC of the token,
	// so the same client can carry it to the failing one.
	client, _ := newClient(t)
	login(t, newServer(t, testToken, fixture()), client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)

	rec := findAccessLine(t, &logs, "/")
	if rec["level"] != "WARN" {
		t.Errorf("level = %v, want WARN for a 503", rec["level"])
	}
	if status, _ := rec["status"].(float64); status != http.StatusServiceUnavailable {
		t.Errorf("status = %v, want 503", rec["status"])
	}
}
