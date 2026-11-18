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

func serverWithAnnotationLogs(t *testing.T, history web.IncidentHistory, annotator web.IncidentAnnotator) (*httptest.Server, *bytes.Buffer) {
	t.Helper()

	var logs bytes.Buffer
	m := fixture()
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h, err := web.New(web.Deps{
		Logger:    logger,
		Source:    m,
		Watcher:   m,
		Incidents: history,
		Annotator: annotator,
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

// TestAnnotationFromTheDashboardIsAudited pins the audit shape on the browser
// surface: the annotation line names the incident and the request, so ground
// truth can be traced back to the page that wrote it.
func TestAnnotationFromTheDashboardIsAudited(t *testing.T) {
	t.Parallel()

	history := incidentsFixture()
	ts, logs := serverWithAnnotationLogs(t, history, &fakeAnnotator{history: history})
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/incidents/inc-open") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	csrf := csrfFrom(t, body(t, resp))

	resp, err = client.PostForm(ts.URL+"/incidents/inc-open/annotate", url.Values{ //nolint:bodyclose // body() closes it
		"csrf":       {csrf},
		"annotation": {"AWS us-east-1, confirmed on the status page"},
	})
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
		if rec["event"] == "incident_annotated" {
			found = rec
			break
		}
	}
	if found == nil {
		t.Fatalf("no incident_annotated line in:\n%s", logs.String())
	}
	if id, _ := found["incident_id"].(string); id != "inc-open" {
		t.Errorf("incident_id = %v, want inc-open", found["incident_id"])
	}
	if id, _ := found["request_id"].(string); id == "" {
		t.Error("the annotation line carries no request id")
	}
}
