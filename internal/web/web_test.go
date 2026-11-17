package web_test

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
	"github.com/Greg-2600/netweather/internal/streamlimit"
	"github.com/Greg-2600/netweather/internal/web"
)

const testToken = "test-token-0123456789"

func fixture() *board.Memory {
	return board.NewMemory(
		board.Target{ID: "1", Name: "Chase", Host: "chase.com", Sector: "financial/banking", State: board.StateUp},
		board.Target{ID: "2", Name: "United", Host: "united.com", Sector: "airline", State: board.StateDown},
		board.Target{ID: "3", Name: "Cloudflare", Host: "cloudflare.com", Sector: "cloud", State: board.StateBlocked},
	)
}

func newServer(t *testing.T, token string, m *board.Memory) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:    m,
		Watcher:   m,
		Token:     token,
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

type fakeHistory struct {
	rows []store.ProbeRow
	err  error
}

func (f *fakeHistory) RecentProbes(context.Context, string, string, int) ([]store.ProbeRow, error) {
	return f.rows, f.err
}

type fakeSeries struct {
	points []store.Rollup
	err    error
}

func (f *fakeSeries) Series(context.Context, string, string, string, time.Time, time.Time) ([]store.Rollup, error) {
	return f.points, f.err
}

type fakeChanges struct {
	changes []store.StateChange
	err     error
}

func (f *fakeChanges) StateChanges(context.Context, string, string, int) ([]store.StateChange, error) {
	return f.changes, f.err
}

func newServerWithDetails(t *testing.T, token string, m *board.Memory, changes web.StateChanges) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:    m,
		Watcher:   m,
		Changes:   changes,
		Vantage:   "local",
		Token:     token,
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

func newServerWithSeries(t *testing.T, token string, m *board.Memory, series web.Series) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:    m,
		Watcher:   m,
		Series:    series,
		Vantage:   "local",
		Token:     token,
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

func newServerWithHistory(t *testing.T, token string, m *board.Memory, history web.History) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:    m,
		Watcher:   m,
		History:   history,
		Vantage:   "local",
		Token:     token,
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

func TestTargetDetailShowsMetadata(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(board.Target{
		ID: "2", Name: "United", Host: "united.com", Class: "service",
		Sector: "airline", Country: "US", State: board.StateDown,
		Port: 443, Probe: "tls", Tier: 2, IntervalS: 300,
		ASN: 0, Prefix: "", CDN: "", Measurability: "ok",
	})

	ts := newServerWithDetails(t, testToken, m, &fakeChanges{})
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/targets/2") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if !strings.Contains(page, `data-testid="target-meta"`) {
		t.Fatalf("metadata block missing:\n%s", page)
	}
	for _, want := range []string{
		">Class<", ">service<", ">Probe<", ">tls<", ">Port<", ">443<",
		">Tier<", ">2<", ">Interval<", ">300s<",
		">ASN<", ">not learned yet<", ">Measurability<", ">ok<",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("metadata missing %q", want)
		}
	}
}

func TestTargetDetailShowsASLabels(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(board.Target{
		ID: "2", Name: "United", Host: "united.com", Class: "service",
		Sector: "airline", Country: "US", State: board.StateDown,
		Port: 443, Probe: "tls", Tier: 2, IntervalS: 300,
		ASN: 13335, Prefix: "203.0.113.0/24", CDN: "cloudflare",
		ASCountry: "US", ASName: "CLOUDFLARENET, US", Measurability: "ok",
	})

	ts := newServerWithDetails(t, testToken, m, &fakeChanges{})
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/targets/2") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	for _, want := range []string{
		">ASN<", ">AS13335<",
		">AS name<", ">CLOUDFLARENET, US<",
		">AS country<", ">US<",
		">Prefix<", ">203.0.113.0/24<",
		">CDN<", ">cloudflare<",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("AS labels missing %q", want)
		}
	}
}

func TestTargetDetailShowsStateChanges(t *testing.T) {
	t.Parallel()

	changes := &fakeChanges{changes: []store.StateChange{
		{
			TS:   time.Date(2026, 11, 17, 12, 1, 0, 0, time.UTC),
			From: state.Up, To: state.Down, Reason: "certificate expired",
		},
		{
			TS:   time.Date(2026, 11, 17, 11, 0, 0, 0, time.UTC),
			From: state.Down, To: state.Up, Reason: "semantic ok",
		},
	}}

	ts := newServerWithDetails(t, testToken, fixture(), changes)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/targets/2") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if got := strings.Count(page, `data-testid="change-row"`); got != 2 {
		t.Fatalf("change rows = %d, want 2", got)
	}
	for _, want := range []string{"certificate expired", "semantic ok", "Down", "Up"} {
		if !strings.Contains(page, want) {
			t.Errorf("state changes missing %q", want)
		}
	}
	if strings.Contains(page, `data-testid="no-changes"`) {
		t.Error("empty state rendered with changes present")
	}
}

func TestTargetDetailStateChangesEmptyAndUnavailable(t *testing.T) {
	t.Parallel()

	ts := newServerWithDetails(t, testToken, fixture(), &fakeChanges{})
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/targets/2") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	if !strings.Contains(page, `data-testid="no-changes"`) {
		t.Fatal("empty state history did not say so")
	}

	ts2 := newServerWithDetails(t, testToken, fixture(), &fakeChanges{err: errors.New("db down")})
	client2, _ := newClient(t)
	login(t, ts2, client2, testToken)

	resp, err = client2.Get(ts2.URL + "/targets/2") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page = body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a history failure should not take the page down: %d", resp.StatusCode)
	}
	if !strings.Contains(page, "temporarily unavailable") {
		t.Error("state history failure was not reported")
	}
}

func TestTargetDetailShowsThe24HourStrip(t *testing.T) {
	t.Parallel()

	series := &fakeSeries{points: []store.Rollup{
		{
			Bucket: time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC),
			N:      12, Up: 12, Ewma50MS: 42,
		},
		{
			Bucket: time.Date(2026, 11, 17, 11, 0, 0, 0, time.UTC),
			N:      12, Up: 3, Degraded: 1, Down: 8, Ewma50MS: 900,
		},
	}}

	ts := newServerWithSeries(t, testToken, fixture(), series)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/targets/2") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(page, `data-testid="target-strip"`) {
		t.Fatalf("strip missing:\n%s", page)
	}
	if got := strings.Count(page, `class="cell`); got != 2 {
		t.Fatalf("cells = %d, want 2", got)
	}
	for _, want := range []string{
		"10:00 UTC: 12/12 healthy, EWMA 42 ms",
		"11:00 UTC: 4/12 healthy, EWMA 900 ms",
		"strip-down",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("strip missing %q", want)
		}
	}
	if strings.Contains(page, `data-testid="no-strip"`) {
		t.Error("empty state rendered with data present")
	}
}

func TestTargetDetailStripEmptyAndUnavailable(t *testing.T) {
	t.Parallel()

	ts := newServerWithSeries(t, testToken, fixture(), &fakeSeries{})
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/targets/2") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	if !strings.Contains(page, `data-testid="no-strip"`) {
		t.Fatal("empty series did not say so")
	}

	ts2 := newServerWithSeries(t, testToken, fixture(), &fakeSeries{err: errors.New("db down")})
	client2, _ := newClient(t)
	login(t, ts2, client2, testToken)

	resp, err = client2.Get(ts2.URL + "/targets/2") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page = body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a series failure should not take the page down: %d", resp.StatusCode)
	}
	if !strings.Contains(page, "temporarily unavailable") {
		t.Error("series failure was not reported")
	}
}

func TestTargetDetailShowsProbeHistory(t *testing.T) {
	t.Parallel()

	history := &fakeHistory{rows: []store.ProbeRow{
		{
			TS: time.Date(2026, 11, 17, 12, 1, 0, 0, time.UTC), State: state.Down,
			Stage: "tls", ErrorClass: "tls_cert_expired", LatencyMS: 41,
			Confidence: "high", Detail: "certificate expired",
		},
		{
			TS: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC), State: state.Up,
			Stage: "http", HTTPStatus: 200, LatencyMS: 80, Confidence: "high",
		},
	}}

	ts := newServerWithHistory(t, testToken, fixture(), history)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/targets/2") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if strings.Count(page, `data-testid="probe-row"`) != 2 {
		t.Fatalf("probe rows missing:\n%s", page)
	}
	for _, want := range []string{"tls_cert_expired", "41 ms", "certificate expired", "12:01:00"} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if strings.Contains(page, `data-testid="no-probes"`) {
		t.Error("empty-state shown with rows present")
	}
}

func TestTargetDetailWithEmptyHistory(t *testing.T) {
	t.Parallel()

	ts := newServerWithHistory(t, testToken, fixture(), &fakeHistory{})
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/targets/2") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if !strings.Contains(page, `data-testid="no-probes"`) {
		t.Fatalf("empty history should say so explicitly:\n%s", page)
	}
	if strings.Contains(page, `data-testid="probe-row"`) {
		t.Error("rows rendered for an empty history")
	}
}

func TestTargetDetailWithoutHistoryConfiguration(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/targets/2") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if !strings.Contains(page, `data-testid="no-history"`) {
		t.Fatal("a missing history source must be stated, not implied")
	}
}

func newClient(t *testing.T) (*http.Client, *cookiejar.Jar) {
	t.Helper()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, Timeout: 10 * time.Second}, jar
}

func noRedirectClient(jar *cookiejar.Jar) *http.Client {
	return &http.Client{
		Jar:     jar,
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

var csrfRE = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func csrfFrom(t *testing.T, body string) string {
	t.Helper()
	m := csrfRE.FindStringSubmatch(body)
	if len(m) != 2 {
		t.Fatalf("no csrf token in page")
	}
	return m[1]
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func login(t *testing.T, ts *httptest.Server, client *http.Client, token string) {
	t.Helper()

	resp, err := client.Get(ts.URL + "/login") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	csrf := csrfFrom(t, page)

	resp, err = client.PostForm(ts.URL+"/login", url.Values{"token": {token}, "csrf": {csrf}}) //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)
	// The client follows the 303 to "/", so success looks like a 200 board.
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/" {
		t.Fatalf("login ended at %s with status %d, want the board (200)", resp.Request.URL.Path, resp.StatusCode)
	}
}

func TestRobotsDisallowsEverything(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())

	resp, err := http.Get(ts.URL + "/robots.txt") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(page, "Disallow: /") {
		t.Fatalf("robots.txt does not disallow crawling: %q", page)
	}
}

func TestBoardRequiresLogin(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	_, jar := newClient(t)
	client := noRedirectClient(jar)

	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Fatalf("Location = %q", loc)
	}
}

func TestLoginRejectsMissingCSRF(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	client, _ := newClient(t)

	resp, err := client.PostForm(ts.URL+"/login", url.Values{"token": {testToken}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestLoginRejectsWrongToken(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	client, jar := newClient(t)

	resp, err := client.Get(ts.URL + "/login") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	csrf := csrfFrom(t, body(t, resp))

	resp, err = client.PostForm(ts.URL+"/login", url.Values{"token": {"wrong-token-000000"}, "csrf": {csrf}}) //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if !strings.Contains(page, "not correct") {
		t.Fatalf("no error message in page")
	}

	u, _ := url.Parse(ts.URL)
	for _, c := range jar.Cookies(u) {
		if c.Name == "nw_session" {
			t.Fatal("a session cookie was issued for a wrong token")
		}
	}
}

func TestBoardTileBadgeShowsTheStage(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(
		board.Target{
			ID: "1", Name: "Chase", Host: "chase.com", State: board.StateDown,
			Stage: "tls", ErrorClass: "tls_cert_expired",
		},
		board.Target{
			ID: "2", Name: "United", Host: "united.com", State: board.StateBlocked,
			Stage: "http", HTTPStatus: 403,
		},
	)
	ts := newServer(t, testToken, m)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if got := strings.Count(page, `data-testid="tile-badge"`); got != 2 {
		t.Fatalf("badges = %d, want 2", got)
	}
	for _, want := range []string{">TLS<", `title="tls_cert_expired"`, ">403<", `title="HTTP 403"`} {
		if !strings.Contains(page, want) {
			t.Errorf("board missing badge %q", want)
		}
	}
}

func TestBoardFocusHidesHealthyTargets(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/?focus=problems") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if strings.Contains(page, `data-target="chase.com"`) {
		t.Error("healthy target survived focus mode")
	}
	if !strings.Contains(page, `data-target="united.com"`) {
		t.Error("down target was hidden by focus mode")
	}
	// The summary still describes the whole fleet.
	if !strings.Contains(page, `data-testid="count-up"`) {
		t.Error("counts stopped describing the whole fleet")
	}
	if !strings.Contains(page, "Show all") {
		t.Error("focus toggle did not offer the way back")
	}
}

func TestBoardFocusWithNothingToShow(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(
		board.Target{ID: "1", Name: "Chase", Host: "chase.com", State: board.StateUp},
	)
	ts := newServer(t, testToken, m)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/?focus=problems") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if !strings.Contains(page, "No target is down or degraded.") {
		t.Fatal("focus mode with nothing to show did not say so")
	}
}

func TestBoardHealthySectorsStartCollapsed(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	// The airline sector holds a DOWN tile, so it is expanded; the all-up
	// financial sector is collapsed.
	if !strings.Contains(page, `data-sector="airline" open`) {
		t.Fatalf("problem sector is collapsed:\n%s", page)
	}
	if strings.Contains(page, `data-sector="financial/banking" open`) {
		t.Fatal("healthy sector started expanded")
	}
}

func TestGeneratedTargetIcons(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/icon/1.svg") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "private") {
		t.Fatalf("Cache-Control = %q", cc)
	}
	for _, want := range []string{"<svg", `aria-label="Chase"`, ">C<", "hsl("} {
		if !strings.Contains(page, want) {
			t.Errorf("icon missing %q:\n%s", want, page)
		}
	}

	// Two targets must not get the same colour by accident of sharing a host
	// prefix; the hue is derived from the host, so distinct hosts differ.
	resp2, err := client.Get(ts.URL + "/icon/2.svg") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	second := body(t, resp2)
	if page == second {
		t.Error("two targets rendered identical icons")
	}
}

func TestIconRejectsMissingTargetsAndBadPaths(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	for _, path := range []string{"/icon/nope.svg", "/icon/1.png", "/icon/.svg"} {
		resp, err := client.Get(ts.URL + path) //nolint:bodyclose // body() closes it
		if err != nil {
			t.Fatal(err)
		}
		_ = body(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestIconRequiresSession(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	_, jar := newClient(t)
	client := noRedirectClient(jar)

	resp, err := client.Get(ts.URL + "/icon/1.svg") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want a redirect to login", resp.StatusCode)
	}
}

func TestBoardTilesUseGeneratedIcons(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if !strings.Contains(page, `class="favicon"`) {
		t.Fatalf("tiles have no icon element:\n%s", page)
	}
	if !strings.Contains(page, `src="/icon/1.svg"`) {
		t.Error("tile icon does not point at the generated endpoint")
	}
}

func TestBoardGroupsBySector(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if got := strings.Count(page, `data-testid="sector"`); got != 3 {
		t.Fatalf("sectors = %d, want 3", got)
	}
	for _, want := range []string{
		`data-sector="airline"`, `data-sector="cloud"`, `data-sector="financial/banking"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("board missing sector %q", want)
		}
	}

	// The sector with the worst state comes first: airline holds a DOWN tile.
	airline := strings.Index(page, `data-sector="airline"`)
	cloud := strings.Index(page, `data-sector="cloud"`)
	banking := strings.Index(page, `data-sector="financial/banking"`)
	if airline < 0 || cloud < 0 || banking < 0 || airline > cloud || cloud > banking {
		t.Fatalf("sector order = airline:%d cloud:%d banking:%d, want worst-first", airline, cloud, banking)
	}
}

func TestBoardPutsUnsectoredTargetsInOther(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(
		board.Target{ID: "1", Name: "Loose", Host: "loose.example", State: board.StateUp},
	)
	ts := newServer(t, testToken, m)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if !strings.Contains(page, `data-sector="other"`) {
		t.Fatalf("unsectored target was not grouped under other:\n%s", page)
	}
	if !strings.Contains(page, ">Other<") {
		t.Error("group label is not human-readable")
	}
}

func TestLoginAndBoardRender(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	if got := strings.Count(page, `data-testid="tile"`); got != 3 {
		t.Fatalf("tiles = %d, want 3", got)
	}
	for _, want := range []string{
		`data-target="chase.com"`, `data-state="up"`,
		`data-target="united.com"`, `data-state="down"`,
		`data-target="cloudflare.com"`, `data-state="blocked"`,
		`aria-label="United (united.com): Down`, // state is in the accessible name
		`data-testid="count-down"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("board missing %q", want)
		}
	}
	if strings.Contains(page, `data-testid="empty-board"`) {
		t.Error("empty state rendered with targets present")
	}

	// Failures cluster first: the board answers "what is down?", not "what is
	// alphabetical".
	down := strings.Index(page, `data-target="united.com"`)
	up := strings.Index(page, `data-target="chase.com"`)
	if down < 0 || up < 0 || down > up {
		t.Fatalf("failures are not clustered first (down=%d, up=%d)", down, up)
	}
}

func TestBoardEscapesTargetNames(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(board.Target{
		ID: "x", Name: `<script>alert(1)</script>`, Host: "evil.example", State: board.StateUp,
	})
	ts := newServer(t, testToken, m)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if strings.Contains(page, "<script>alert(1)</script>") {
		t.Fatal("target name was not HTML-escaped")
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Fatal("escaped name not present at all; is the tile rendered?")
	}
}

func TestTargetDetail(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/targets/2") //nolint:bodyclose // body() closes it //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(page, "United") || !strings.Contains(page, `data-testid="target-state"`) {
		t.Fatal("detail page missing content")
	}

	resp, err = client.Get(ts.URL + "/targets/nope") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown target status = %d", resp.StatusCode)
	}
}

func TestStaticAssets(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())

	for path, wantType := range map[string]string{
		"/static/app.css": "text/css",
		"/static/app.js":  "javascript",
	} {
		resp, err := http.Get(ts.URL + path) //nolint:bodyclose // body() closes it
		if err != nil {
			t.Fatal(err)
		}
		_ = body(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, wantType) {
			t.Errorf("%s: Content-Type %q, want %s", path, ct, wantType)
		}
	}
}

func TestSSEStreamCap(t *testing.T) {
	t.Parallel()

	m := fixture()
	h, err := web.New(web.Deps{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:      m,
		Watcher:     m,
		StreamLimit: streamlimit.New(1, 1),
		Token:       testToken,
		Version:     "test",
		StartedAt:   time.Now(),
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	client, _ := newClient(t)
	login(t, ts, client, testToken)

	streamClient := &http.Client{Jar: client.Jar} // no timeout: the stream stays open

	first, err := streamClient.Get(ts.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Body.Close() }()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first stream status = %d", first.StatusCode)
	}

	second, err := streamClient.Get(ts.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Body.Close() }()
	if second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second stream status = %d, want 429 at the cap", second.StatusCode)
	}
}

func TestSSEDeliversStateDelta(t *testing.T) {
	t.Parallel()

	m := fixture()
	ts := newServer(t, testToken, m)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	streamClient := &http.Client{Jar: client.Jar} // no timeout: the stream stays open
	resp, err := streamClient.Get(ts.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}

	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()

	waitForLine(t, lines, "retry:", 5*time.Second)

	m.Set("2", board.StateUp)

	waitForLine(t, lines, `"state":"up"`, 5*time.Second)
}

func newServerWithIncidentWatcher(t *testing.T, token string, m *board.Memory, incidents board.Watcher) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:          m,
		Watcher:         m,
		IncidentWatcher: incidents,
		Token:           token,
		Version:         "test",
		StartedAt:       time.Now(),
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}

	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

func TestSSEDeliversIncidentEvents(t *testing.T) {
	t.Parallel()

	m := fixture()
	incidentChanges := board.NewBroadcast()
	ts := newServerWithIncidentWatcher(t, testToken, m, incidentChanges)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	streamClient := &http.Client{Jar: client.Jar} // no timeout: the stream stays open
	resp, err := streamClient.Get(ts.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()

	waitForLine(t, lines, "retry:", 5*time.Second)

	incidentChanges.Notify()

	waitForLine(t, lines, "event: incident", 5*time.Second)
	waitForLine(t, lines, `"updated_at"`, 5*time.Second)
}

func TestSSERequiresSession(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	_, jar := newClient(t)
	client := noRedirectClient(jar)

	resp, err := client.Get(ts.URL + "/events") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 to login", resp.StatusCode)
	}
}

func TestAuthDisabledOnLoopbackStyleConfig(t *testing.T) {
	t.Parallel()

	ts := newServer(t, "", fixture())

	resp, err := http.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(page, `data-testid="tile"`) {
		t.Fatal("board did not render with auth disabled")
	}
	if strings.Contains(page, `data-testid="logout"`) {
		t.Fatal("logout shown when auth is disabled")
	}
}

func TestLogoutClearsSession(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	client, jar := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	csrf := csrfFrom(t, body(t, resp))

	resp, err = client.PostForm(ts.URL+"/logout", url.Values{"csrf": {csrf}}) //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)
	// The 303 to /login is followed, so success is ending on the login page.
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/login" {
		t.Fatalf("logout ended at %s with status %d, want /login", resp.Request.URL.Path, resp.StatusCode)
	}

	noRedirect := noRedirectClient(jar)
	resp, err = noRedirect.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("board after logout: status = %d, want redirect", resp.StatusCode)
	}
}

func waitForLine(t *testing.T, lines <-chan string, needle string, timeout time.Duration) {
	t.Helper()

	deadline := time.After(timeout)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("stream closed before %q", needle)
			}
			if strings.Contains(line, needle) {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q", needle)
		}
	}
}

type fakeIncidentHistory struct {
	items   []store.Incident
	attrs   []store.Attribution
	targets []string
	err     error
}

func (f *fakeIncidentHistory) Incidents(context.Context, bool, int) ([]store.Incident, error) {
	return f.items, f.err
}

func (f *fakeIncidentHistory) IncidentByID(_ context.Context, id string) (store.Incident, []store.Attribution, []string, error) {
	if f.err != nil {
		return store.Incident{}, nil, nil, f.err
	}
	for _, inc := range f.items {
		if inc.ID == id {
			return inc, f.attrs, f.targets, nil
		}
	}
	return store.Incident{}, nil, nil, sql.ErrNoRows
}

func newServerWithIncidents(t *testing.T, token string, m *board.Memory, history web.IncidentHistory) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:    m,
		Watcher:   m,
		Incidents: history,
		Token:     token,
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

func incidentsFixture() *fakeIncidentHistory {
	opened := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	return &fakeIncidentHistory{
		items: []store.Incident{
			{
				ID: "inc-open", OpenedAt: opened, Verdict: "INCIDENT",
				Scope: "sector", ScopeValue: "airline",
				Headline: "5 of 6 targets failing on sector=airline", Confidence: "high",
			},
			{
				ID: "inc-closed", OpenedAt: opened.Add(-time.Hour), ClosedAt: opened,
				Verdict: "INCIDENT", Scope: "cdn", ScopeValue: "akamai",
				Headline: "17 of 21 targets failing on cdn=akamai", Confidence: "medium",
				ClosedReason: "cluster cleared",
			},
		},
		attrs: []store.Attribution{
			{Rank: 1, Kind: "sector", Value: "airline", Failing: 5, Total: 6, Share: 5.0 / 6.0, Score: 1.5},
		},
		targets: []string{"svc-united", "svc-delta"},
	}
}

type fakeControl struct {
	mu     sync.Mutex
	paused bool
}

func (f *fakeControl) Pause() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = true
}

func (f *fakeControl) Resume() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = false
}

func (f *fakeControl) Paused() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused
}

func newServerWithControl(t *testing.T, token string, m *board.Memory, control web.Control) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:    m,
		Watcher:   m,
		Control:   control,
		Token:     token,
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

type fakeCoverage struct {
	cov store.Coverage
	err error
}

func (f *fakeCoverage) Coverage(context.Context, string, time.Time) (store.Coverage, error) {
	return f.cov, f.err
}

func newServerWithCoverage(t *testing.T, token string, m *board.Memory, cov web.Coverage) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:    m,
		Watcher:   m,
		Coverage:  cov,
		Token:     token,
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

type fakeCoverageHistory struct {
	points []store.CoveragePoint
	err    error
}

func (f *fakeCoverageHistory) CoverageHistory(context.Context, string, string, int) ([]store.CoveragePoint, error) {
	return f.points, f.err
}

func newServerWithCoverageTrend(t *testing.T, token string, m *board.Memory, cov web.Coverage, trend web.CoverageHistory) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:        m,
		Watcher:       m,
		Coverage:      cov,
		CoverageTrend: trend,
		Token:         token,
		Version:       "test",
		StartedAt:     time.Now(),
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}

	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

func TestCoverageTrend(t *testing.T) {
	t.Parallel()

	cov := &fakeCoverage{cov: store.Coverage{
		Totals: store.CoverageTotals{Total: 100, Measurable: 80},
	}}
	trend := &fakeCoverageHistory{points: []store.CoveragePoint{
		{Bucket: time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), Scope: "global", Total: 100, Measurable: 100},
		{Bucket: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC), Scope: "global", Total: 100, Measurable: 80},
	}}

	ts := newServerWithCoverageTrend(t, testToken, fixture(), cov, trend)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/coverage") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if got := strings.Count(page, `data-testid="trend-row"`); got != 2 {
		t.Fatalf("trend rows = %d, want 2", got)
	}
	for _, want := range []string{"2026-11-15", "2026-11-16", "100%", "80%", "80/100"} {
		if !strings.Contains(page, want) {
			t.Errorf("trend missing %q", want)
		}
	}
}

func TestCoverageTrendEmptyAndUnavailable(t *testing.T) {
	t.Parallel()

	cov := &fakeCoverage{cov: store.Coverage{Totals: store.CoverageTotals{Total: 10, Measurable: 9}}}

	ts := newServerWithCoverageTrend(t, testToken, fixture(), cov, &fakeCoverageHistory{})
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/coverage") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	if !strings.Contains(page, `data-testid="no-trend"`) {
		t.Error("empty history did not say so")
	}

	ts2 := newServerWithCoverageTrend(t, testToken, fixture(), cov,
		&fakeCoverageHistory{err: errors.New("db down")})
	client2, _ := newClient(t)
	login(t, ts2, client2, testToken)

	resp, err = client2.Get(ts2.URL + "/coverage") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page = body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a history failure should not take the whole panel down: %d", resp.StatusCode)
	}
	if !strings.Contains(page, "temporarily unavailable") {
		t.Error("history failure was not reported")
	}
}

func TestCoveragePage(t *testing.T) {
	t.Parallel()

	cov := &fakeCoverage{cov: store.Coverage{
		Totals: store.CoverageTotals{Total: 100, Measurable: 80, Unmeasurable: 10},
		Sectors: []store.CoverageBucket{
			{Kind: "sector", Value: "airline", Total: 10, Measurable: 9},
		},
		BlindSpots: []store.CoverageBucket{
			{Kind: "country", Value: "NL", Total: 10, Measurable: 2},
		},
		Stale: []store.StaleTarget{
			{ID: "t1", Host: "chase.com", State: string(state.Unknown), IntervalS: 300},
		},
	}}

	ts := newServerWithCoverage(t, testToken, fixture(), cov)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/coverage") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for _, want := range []string{
		`data-testid="blind-spot"`,
		`data-value="NL"`,
		"80 of 100 targets measurable",
		"airline",
		"chase.com",
		`data-testid="stale-row"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("coverage page missing %q", want)
		}
	}
}

func TestCoveragePageEmptyStates(t *testing.T) {
	t.Parallel()

	ts := newServerWithCoverage(t, testToken, fixture(), &fakeCoverage{})
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/coverage") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if !strings.Contains(page, `data-testid="no-blind-spots"`) {
		t.Error("empty blind spots should be stated explicitly")
	}
	if !strings.Contains(page, `data-testid="no-stale"`) {
		t.Error("empty stale list should be stated explicitly")
	}
}

func TestCoveragePageUnavailableWithoutSource(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/coverage") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestPauseButtonControlsProbing(t *testing.T) {
	t.Parallel()

	control := &fakeControl{}
	ts := newServerWithControl(t, testToken, fixture(), control)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	if !strings.Contains(page, `data-testid="probe-toggle"`) || !strings.Contains(page, "Pause probing") {
		t.Fatalf("pause control missing:\n%s", page)
	}
	if strings.Contains(page, `data-testid="paused-banner"`) {
		t.Fatal("paused banner shown while running")
	}

	csrf := csrfFrom(t, page)
	resp, err = client.PostForm(ts.URL+"/pause", url.Values{"csrf": {csrf}}) //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page = body(t, resp)
	if !control.Paused() {
		t.Fatal("control was not paused")
	}
	if !strings.Contains(page, `data-testid="paused-banner"`) || !strings.Contains(page, "Resume probing") {
		t.Fatalf("paused state not rendered:\n%s", page)
	}

	resp, err = client.PostForm(ts.URL+"/resume", url.Values{"csrf": {csrf}}) //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page = body(t, resp)
	if control.Paused() {
		t.Fatal("control was not resumed")
	}
	if strings.Contains(page, `data-testid="paused-banner"`) {
		t.Fatal("banner survived resume")
	}
}

func TestPauseRequiresCSRF(t *testing.T) {
	t.Parallel()

	control := &fakeControl{}
	ts := newServerWithControl(t, testToken, fixture(), control)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.PostForm(ts.URL+"/pause", url.Values{}) //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if control.Paused() {
		t.Fatal("a CSRF-less request paused probing")
	}
}

func TestPauseRequiresSession(t *testing.T) {
	t.Parallel()

	ts := newServerWithControl(t, testToken, fixture(), &fakeControl{})
	_, jar := newClient(t)
	client := noRedirectClient(jar)

	resp, err := client.PostForm(ts.URL+"/pause", url.Values{}) //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want a redirect to login", resp.StatusCode)
	}
}

func TestIncidentsPage(t *testing.T) {
	t.Parallel()

	ts := newServerWithIncidents(t, testToken, fixture(), incidentsFixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/incidents") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if strings.Count(page, `data-testid="incident-row"`) != 2 {
		t.Fatalf("rows missing:\n%s", page)
	}
	for _, want := range []string{
		`href="/incidents/inc-open"`,
		"5 of 6 targets failing on sector=airline",
		"open",
		"closed",
		"cluster cleared",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

func TestIncidentsPageEmptyState(t *testing.T) {
	t.Parallel()

	ts := newServerWithIncidents(t, testToken, fixture(), &fakeIncidentHistory{})
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/incidents") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if !strings.Contains(page, `data-testid="no-incidents"`) {
		t.Fatal("empty incident log should say so")
	}
}

func TestIncidentDetailPage(t *testing.T) {
	t.Parallel()

	ts := newServerWithIncidents(t, testToken, fixture(), incidentsFixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/incidents/inc-open") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for _, want := range []string{
		`data-testid="attribution-row"`,
		"airline",
		"83%",
		"svc-united",
		"svc-delta",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("detail missing %q", want)
		}
	}
}

type fakeCorroboration struct {
	events []store.ExternalEvent
	err    error
}

func (f *fakeCorroboration) Corroboration(context.Context, store.Incident, time.Time, int) ([]store.ExternalEvent, error) {
	return f.events, f.err
}

func newServerWithCorroboration(t *testing.T, token string, m *board.Memory, history web.IncidentHistory, corr web.Corroboration) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:        m,
		Watcher:       m,
		Incidents:     history,
		Corroboration: corr,
		Token:         token,
		Version:       "test",
		StartedAt:     time.Now(),
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}

	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

func TestIncidentDetailShowsCorroboration(t *testing.T) {
	t.Parallel()

	corr := &fakeCorroboration{events: []store.ExternalEvent{
		{
			ID: 1, Feed: "ioda", ExternalID: "ir-1", Kind: "outage",
			Severity: 0.9, Country: "IR",
			StartedAt: time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC),
		},
	}}

	ts := newServerWithCorroboration(t, testToken, fixture(), incidentsFixture(), corr)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/incidents/inc-open") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(page, `data-testid="incident-corroboration"`) {
		t.Fatalf("corroboration table missing:\n%s", page)
	}
	for _, want := range []string{"ioda", "outage", ">IR<", "0.90", "ongoing"} {
		if !strings.Contains(page, want) {
			t.Errorf("corroboration missing %q", want)
		}
	}
	if strings.Contains(page, `data-testid="no-corroboration"`) {
		t.Error("empty state rendered with events present")
	}
}

func TestIncidentCorroborationEmptyAndUnconfigured(t *testing.T) {
	t.Parallel()

	ts := newServerWithCorroboration(t, testToken, fixture(), incidentsFixture(), &fakeCorroboration{})
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/incidents/inc-open") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	if !strings.Contains(page, `data-testid="no-corroboration"`) {
		t.Fatal("empty corroboration did not say so")
	}
	if !strings.Contains(page, "No feed event overlapped") {
		t.Error("empty state lacks an explanation")
	}

	// A failed query still renders the incident, with a note.
	ts2 := newServerWithCorroboration(t, testToken, fixture(), incidentsFixture(),
		&fakeCorroboration{err: errors.New("db down")})
	client2, _ := newClient(t)
	login(t, ts2, client2, testToken)

	resp, err = client2.Get(ts2.URL + "/incidents/inc-open") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page = body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a corroboration failure should not hide the incident: %d", resp.StatusCode)
	}
	if !strings.Contains(page, "temporarily unavailable") {
		t.Error("corroboration failure was not reported")
	}
}

func TestIncidentDetailMissing(t *testing.T) {
	t.Parallel()

	ts := newServerWithIncidents(t, testToken, fixture(), incidentsFixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/incidents/nope") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
