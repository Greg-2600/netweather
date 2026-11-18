package web_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/web"
)

type fakeDisableControl struct {
	mu    sync.Mutex
	got   string
	found bool
	err   error
}

func (f *fakeDisableControl) DisableTarget(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = id
	return f.found, f.err
}

func (f *fakeDisableControl) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got
}

func serverWithDisable(t *testing.T, enabled bool, control web.TargetControl) *httptest.Server {
	t.Helper()

	m := fixture()
	h, err := web.New(web.Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:        m,
		Watcher:       m,
		TargetControl: control,
		Token:         testToken,
		Version:       "test",
		StartedAt:     time.Now(),
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	// DisableEnabled is derived from TargetControl != nil, so the enabled flag
	// is carried by whether a control is wired.
	if !enabled {
		h2, err := web.New(web.Deps{
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Source:    m,
			Watcher:   m,
			Token:     testToken,
			Version:   "test",
			StartedAt: time.Now(),
		})
		if err != nil {
			t.Fatalf("web.New: %v", err)
		}
		h = h2
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

// disablePost logs in, reads a CSRF token from the target page, and posts the
// disable action for the fixture's United target.
func disablePost(t *testing.T, ts *httptest.Server, csrf string) (int, string) {
	t.Helper()

	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/targets/2") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	if csrf == "" {
		csrf = csrfFrom(t, page)
	}

	resp, err = client.PostForm(ts.URL+"/targets/2/disable", url.Values{"csrf": {csrf}}) //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body(t, resp)
}

func TestTargetDisableButtonOnlyWhenEnabled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		enabled bool
		want    bool
	}{
		{"enabled", true, true},
		{"disabled", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			control := &fakeDisableControl{found: true}
			var ts *httptest.Server
			if tc.enabled {
				ts = serverWithDisable(t, true, control)
			} else {
				ts = serverWithDisable(t, false, nil)
			}
			client, _ := newClient(t)
			login(t, ts, client, testToken)

			resp, err := client.Get(ts.URL + "/targets/2") //nolint:bodyclose // body() closes it
			if err != nil {
				t.Fatal(err)
			}
			page := body(t, resp)
			has := strings.Contains(page, `data-testid="disable-action"`)
			if has != tc.want {
				t.Fatalf("disable button present = %v, want %v", has, tc.want)
			}
		})
	}
}

func TestTargetDisableActionDisablesTheTarget(t *testing.T) {
	t.Parallel()

	control := &fakeDisableControl{found: true}
	ts := serverWithDisable(t, true, control)

	code, _ := disablePost(t, ts, "")
	if code != http.StatusFound && code != http.StatusOK {
		t.Fatalf("status = %d, want a redirect or 200", code)
	}
	if control.last() != "2" {
		t.Fatalf("control got %q, want the target id", control.last())
	}
}

func TestTargetDisableRequiresCSRF(t *testing.T) {
	t.Parallel()

	control := &fakeDisableControl{found: true}
	ts := serverWithDisable(t, true, control)

	code, page := disablePost(t, ts, "wrong-token")
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", code)
	}
	if !strings.Contains(page, `data-testid="error-page"`) {
		t.Fatalf("a bad CSRF token did not render the generic page:\n%s", page)
	}
	if control.last() != "" {
		t.Fatal("a CSRF-less request disabled the target")
	}
}

func TestTargetDisableButtonLogsRequestID(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := fixture()
	control := &fakeDisableControl{found: true}
	h, err := web.New(web.Deps{
		Logger:        logger,
		Source:        m,
		Watcher:       m,
		TargetControl: control,
		Token:         testToken,
		Version:       "test",
		StartedAt:     time.Now(),
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/targets/2") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	csrf := csrfFrom(t, body(t, resp))

	resp, err = client.PostForm(ts.URL+"/targets/2/disable", url.Values{"csrf": {csrf}}) //nolint:bodyclose // body() closes it
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
		if rec["event"] == "target_disabled" {
			found = rec
			break
		}
	}
	if found == nil {
		t.Fatalf("no target_disabled line in:\n%s", logs.String())
	}
	if id, _ := found["request_id"].(string); id == "" {
		t.Error("the disable line carries no request id")
	}
	if id, _ := found["target_id"].(string); id != "2" {
		t.Errorf("target_id = %v, want 2", found["target_id"])
	}
}

// Keep the fixture type check compile-time.
var _ board.Source = (*board.Memory)(nil)
