package web_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/web"
)

func serverWithControlLogs(t *testing.T) (*httptest.Server, *bytes.Buffer) {
	t.Helper()

	var logs bytes.Buffer
	m := fixture()
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h, err := web.New(web.Deps{
		Logger:    logger,
		Source:    m,
		Watcher:   m,
		Control:   &fakeControl{},
		Token:     testToken,
		Version:   "test",
		StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts, &logs
}

// TestProbeControlFromTheDashboardIsAudited pins the LOGGING.md §3 audit shape
// on the browser surface: a pause names the request, so "who stopped probing"
// is answerable from the log.
func TestProbeControlFromTheDashboardIsAudited(t *testing.T) {
	t.Parallel()

	ts, logs := serverWithControlLogs(t)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	csrf := csrfFrom(t, body(t, resp))

	resp, err = client.PostForm(ts.URL+"/pause", url.Values{"csrf": {csrf}}) //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)

	var found map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["event"] == "probe_control" {
			found = rec
			break
		}
	}
	if found == nil {
		t.Fatalf("no probe_control line in:\n%s", logs.String())
	}
	if paused, _ := found["paused"].(bool); !paused {
		t.Errorf("paused = %v, want true", found["paused"])
	}
	if id, _ := found["request_id"].(string); id == "" {
		t.Error("the pause line carries no request id")
	}
	if comp, _ := found["component"].(string); comp != "http" {
		t.Errorf("component = %v, want http", found["component"])
	}
}
