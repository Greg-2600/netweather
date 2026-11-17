package web_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/web"
)

// failingSource makes a page read fail the way a store outage would. The
// detail is deliberately distinctive so the test can prove it never reaches
// the browser (THREAT_MODEL.md T15).
type failingSource struct{ err error }

func (f failingSource) Snapshot(context.Context) ([]board.Target, error) {
	return nil, f.err
}

func serverWithSource(t *testing.T, src board.Source) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
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

func TestPageFailureRendersAGenericErrorWithARequestID(t *testing.T) {
	t.Parallel()

	detail := "store: SELECT failed on netweather.db at C:\\secret\\path"
	ts := serverWithSource(t, failingSource{err: errors.New(detail)})

	// Log in against a healthy instance first: the session is an HMAC of the
	// token, so the same client can carry it to the failing one. Logging in
	// here would follow the 303 straight into the 503 under test.
	client, _ := newClient(t)
	login(t, newServer(t, testToken, fixture()), client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	headerID := resp.Header.Get("X-Request-ID")
	if headerID == "" {
		t.Fatal("no X-Request-ID header on a failed page")
	}
	if !strings.Contains(page, `data-testid="error-page"`) {
		t.Fatalf("the failure did not render the generic page:\n%s", page)
	}
	if !strings.Contains(page, headerID) {
		t.Fatalf("the page does not name the request id %q:\n%s", headerID, page)
	}
	if strings.Contains(page, detail) || strings.Contains(page, "SELECT") {
		t.Fatalf("the error page leaked the internal detail:\n%s", page)
	}

	// A different page uses the same path and the same generic statement.
	resp, err = client.Get(ts.URL + "/world") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	if page := body(t, resp); !strings.Contains(page, `data-testid="error-page"`) {
		t.Fatalf("the world failure did not render the generic page:\n%s", page)
	}
	if resp.Header.Get("X-Request-ID") == headerID {
		t.Fatal("two requests shared a request id")
	}
}
