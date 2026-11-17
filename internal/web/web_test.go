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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/ihi"
	"github.com/Greg-2600/netweather/internal/logging"
	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
	"github.com/Greg-2600/netweather/internal/streamlimit"
	"github.com/Greg-2600/netweather/internal/verdict"
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
		ASCountry: "US", ASName: "CLOUDFLARENET, US",
		IXNames: "AMS-IX, DE-CIX Frankfurt", Measurability: "ok",
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
		">Internet exchanges<", ">AMS-IX, DE-CIX Frankfurt<",
		">Prefix<", ">203.0.113.0/24<",
		">CDN<", ">cloudflare<",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("AS labels missing %q", want)
		}
	}
}

func TestTargetDetailShowsCertificateObservation(t *testing.T) {
	t.Parallel()

	expiry := time.Date(2027, 1, 16, 12, 0, 0, 0, time.UTC)
	m := board.NewMemory(board.Target{
		ID: "2", Name: "United", Host: "united.com", Class: "service",
		Sector: "airline", Country: "US", State: board.StateUp,
		Port: 443, Probe: "tls", Tier: 2, IntervalS: 300,
		CertIssuer: "CN=R11,O=Let's Encrypt,C=US", CertNotAfter: expiry,
		CertPinned: true, CertChainLen: 2, CertIntermediateSPKI: "aabbccddeeff001122",
		CertLeafFingerprint: "00112233445566778899aabbccddeeff",
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
		">Certificate issuer<", "CN=R11,O=Let&#39;s Encrypt,C=US",
		">Certificate expires<", "2027-01-16",
		">Certificate pin<", ">issuer learned<",
		">Certificate chain<", "2 certificate(s)", "intermediate aabbccddeeff0011",
		">Leaf certificate<", "sha256 0011223344556677",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("certificate row missing %q", want)
		}
	}
}

func TestTargetDetailCertificateRowsWithoutTLS(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(board.Target{
		ID: "2", Name: "United", Host: "united.com", Class: "service",
		State: board.StateUp, Port: 443, Probe: "http", Tier: 2, IntervalS: 300,
	})

	ts := newServerWithDetails(t, testToken, m, &fakeChanges{})
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/targets/2") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if got := strings.Count(page, "not a TLS probe"); got != 2 {
		t.Fatalf("non-TLS rows = %d, want both certificate rows stated as absent", got)
	}
	// The pin row is its own claim: a non-TLS target has no issuer baseline.
	if !strings.Contains(page, ">Certificate pin<") || !strings.Contains(page, ">not learned yet<") {
		t.Error("the pin row did not state that no baseline exists")
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

type fakeFeedHealth struct {
	states []store.FeedState
	err    error
}

func (f *fakeFeedHealth) FeedStates(context.Context) ([]store.FeedState, error) {
	return f.states, f.err
}

func newServerWithFeeds(t *testing.T, token string, m *board.Memory, feeds web.FeedHealth) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:    m,
		Watcher:   m,
		Feeds:     feeds,
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

func TestBoardShowsFeedHealthAndStaleness(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	feeds := &fakeFeedHealth{states: []store.FeedState{
		{Feed: "ioda", LastOKAt: now.Add(-2 * time.Minute)},
		{Feed: "radar", LastOKAt: now.Add(-3 * time.Hour), LastErr: "503 from upstream", ConsecutiveFailures: 2},
		{Feed: "statuspage"},
	}}

	ts := newServerWithFeeds(t, testToken, fixture(), feeds)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if !strings.Contains(page, `data-testid="feed-health"`) {
		t.Fatalf("feed strip missing:\n%s", page)
	}
	for _, want := range []string{
		`data-feed="ioda" data-status="ok"`,
		`data-feed="radar" data-status="stale"`,
		`data-feed="statuspage" data-status="never"`,
		`data-testid="feed-warning"`,
		"corroboration may be missing",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("feed strip missing %q", want)
		}
	}
}

func TestBoardFeedHealthStatesAreExplicit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		feeds web.FeedHealth
		want  string
	}{
		{"unconfigured", nil, "not configured"},
		{"empty", &fakeFeedHealth{}, "No external feeds are enabled"},
		{"failed", &fakeFeedHealth{err: errors.New("db down")}, "temporarily unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := newServerWithFeeds(t, testToken, fixture(), tc.feeds)
			client, _ := newClient(t)
			login(t, ts, client, testToken)

			resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
			if err != nil {
				t.Fatal(err)
			}
			page := body(t, resp)
			if !strings.Contains(page, `data-testid="feed-note"`) || !strings.Contains(page, tc.want) {
				t.Fatalf("feed note missing %q:\n%s", tc.want, page)
			}
			if strings.Contains(page, `data-testid="feed-warning"`) {
				t.Error("a note-only state rendered a stale-feed warning")
			}
		})
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

func TestBoardGroupingToggle(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(
		board.Target{
			ID: "1", Name: "Chase", Host: "chase.com", Sector: "financial/banking",
			State: board.StateUp, CDN: "akamai", ASN: 20940, Country: "US", Stage: "tls",
		},
		board.Target{
			ID: "2", Name: "United", Host: "united.com", Sector: "airline",
			State: board.StateDown, CDN: "akamai", ASN: 20940, Country: "US", Stage: "dns",
		},
		board.Target{
			ID: "3", Name: "Cloudflare", Host: "cloudflare.com", Sector: "cloud",
			State: board.StateUp, CDN: "cloudflare", ASN: 13335, Country: "DE",
		},
	)
	ts := newServer(t, testToken, m)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	get := func(path string) string {
		t.Helper()
		resp, err := client.Get(ts.URL + path) //nolint:bodyclose // body() closes it
		if err != nil {
			t.Fatal(err)
		}
		return body(t, resp)
	}

	// The toggle is always present, and sector is the default lens.
	page := get("/")
	if !strings.Contains(page, `data-testid="group-toggle"`) {
		t.Fatalf("grouping toggle missing:\n%s", page)
	}
	for _, want := range []string{
		`data-group="sector"`, `data-group="state"`, `data-group="cdn"`,
		`data-group="asn"`, `data-group="country"`, `data-group="stage"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("toggle missing %q", want)
		}
	}
	if !strings.Contains(page, `class="group-link active" href="/?group=sector"`) {
		t.Error("sector is not marked as the active default")
	}

	// Grouping by CDN merges the two Akamai targets into one bucket and keeps
	// the Cloudflare one separate.
	page = get("/?group=cdn")
	if got := strings.Count(page, `data-testid="sector"`); got != 2 {
		t.Fatalf("cdn groups = %d, want 2", got)
	}
	if !strings.Contains(page, `data-sector="akamai"`) || !strings.Contains(page, `data-sector="cloudflare"`) {
		t.Error("cdn groups missing their buckets")
	}
	if !strings.Contains(page, `class="group-link active" href="/?group=cdn"`) {
		t.Error("cdn is not marked active after switching")
	}

	// ASN buckets are labelled with their number.
	page = get("/?group=asn")
	if !strings.Contains(page, `data-sector="20940"`) || !strings.Contains(page, ">AS20940<") {
		t.Error("ASN group missing its number or label")
	}

	// Country and stage buckets are explicit about a missing value instead of
	// folding it into a real one.
	page = get("/?group=country")
	if !strings.Contains(page, `data-sector="DE"`) {
		t.Error("country group missing DE")
	}
	page = get("/?group=stage")
	if !strings.Contains(page, `data-sector="dns"`) {
		t.Error("stage group missing dns")
	}
	if !strings.Contains(page, "No stage recorded") {
		t.Error("a target with no stage was not given an explicit bucket")
	}

	// State grouping uses the state text as its label.
	page = get("/?group=state")
	if !strings.Contains(page, `data-sector="down"`) || !strings.Contains(page, ">Down<") {
		t.Error("state group missing its bucket or label")
	}
}

func TestBoardGroupingFallsBackOnATypo(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/?group=bogus") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	// A typo is not a client error: the board still renders, on the default.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(page, `data-sector="airline"`) {
		t.Fatal("an unknown grouping did not fall back to sector")
	}
	if !strings.Contains(page, `class="group-link active" href="/?group=sector"`) {
		t.Error("fallback did not mark sector active")
	}
}

func TestBoardGroupingPreservesFocusMode(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/?group=cdn&focus=problems") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	// Switching lens must not silently drop focus mode.
	if !strings.Contains(page, `href="/?group=country&amp;focus=problems"`) {
		t.Fatalf("grouping link dropped focus mode:\n%s", page)
	}
	if !strings.Contains(page, `data-testid="focus-toggle"`) {
		t.Fatal("focus toggle missing")
	}
}

func TestWorldGroupsByCountry(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(
		board.Target{ID: "1", Name: "Chase", Host: "chase.com", Country: "US", State: board.StateUp},
		board.Target{ID: "2", Name: "United", Host: "united.com", Country: "US", State: board.StateDown},
		board.Target{ID: "3", Name: "Deutsche", Host: "de.example", Country: "DE", State: board.StateUp},
		board.Target{ID: "4", Name: "NoGeo", Host: "nogeo.example", State: board.StateUp},
	)

	ts := newServer(t, testToken, m)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/world") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := strings.Count(page, `data-testid="country"`); got != 2 {
		t.Fatalf("country tiles = %d, want 2 (a missing country is not a country)", got)
	}
	for _, want := range []string{
		`data-country="US"`, `data-country="DE"`,
		`href="/?group=country&amp;country=US"`,
		"Down</span>", // the US bucket's worst state is named on the tile
		// The flag is decorative; the code is the text that always renders.
		`data-testid="country-flag"`, "🇺🇸",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("world page missing %q", want)
		}
	}
	if !strings.Contains(page, `data-testid="world-unplaced"`) {
		t.Error("a target with no country was not counted as unplaced")
	}

	// Worst state first: the US bucket holds a DOWN target, so it precedes DE.
	us := strings.Index(page, `data-country="US"`)
	de := strings.Index(page, `data-country="DE"`)
	if us < 0 || de < 0 || us > de {
		t.Fatalf("country order = US:%d DE:%d, want worst-first", us, de)
	}
}

func TestWorldEmptyAndSessionProtected(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(
		board.Target{ID: "1", Name: "NoGeo", Host: "nogeo.example", State: board.StateUp},
	)
	ts := newServer(t, testToken, m)

	_, jar := newClient(t)
	anon := noRedirectClient(jar)
	resp, err := anon.Get(ts.URL + "/world") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("anonymous status = %d, want a redirect to login", resp.StatusCode)
	}

	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err = client.Get(ts.URL + "/world") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	if !strings.Contains(page, `data-testid="empty-world"`) {
		t.Fatalf("empty world did not say why:\n%s", page)
	}
	if strings.Contains(page, `data-testid="country"`) {
		t.Error("a target with no country rendered as a country tile")
	}
}

func TestBoardCountryFilter(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(
		board.Target{ID: "1", Name: "Chase", Host: "chase.com", Country: "US", State: board.StateUp},
		board.Target{ID: "2", Name: "Deutsche", Host: "de.example", Country: "DE", State: board.StateUp},
	)
	ts := newServer(t, testToken, m)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	// Lowercase is normalised: a link that happens to be lowercase still works.
	resp, err := client.Get(ts.URL + "/?country=us") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if !strings.Contains(page, `data-testid="country-filter"`) || !strings.Contains(page, ">US<") {
		t.Fatalf("filter chip missing:\n%s", page)
	}
	if !strings.Contains(page, `data-target="chase.com"`) {
		t.Error("the filtered-in target is missing")
	}
	if strings.Contains(page, `data-target="de.example"`) {
		t.Error("a target outside the country filter rendered")
	}
	if !strings.Contains(page, `data-testid="clear-country"`) {
		t.Error("the filter cannot be cleared")
	}

	// The clear link keeps the grouping lens and focus state.
	resp, err = client.Get(ts.URL + "/?country=us&group=cdn&focus=problems") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page = body(t, resp)
	if !strings.Contains(page, `href="/?group=cdn&amp;focus=problems"`) {
		t.Fatalf("clear link dropped the other view state:\n%s", page)
	}
}

func TestBoardCountryFilterWithNoMatches(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/?country=ZZ") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(page, `data-testid="empty-board"`) {
		t.Fatalf("an empty filter result did not explain itself:\n%s", page)
	}
	if !strings.Contains(page, `data-testid="country-filter"`) {
		t.Error("an empty result hid the active filter")
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

type fakeAnnotator struct {
	mu         sync.Mutex
	history    *fakeIncidentHistory
	id         string
	annotation string
	at         time.Time
	err        error
}

func (f *fakeAnnotator) AnnotateIncident(_ context.Context, id, annotation string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.id, f.annotation, f.at = id, annotation, at
	// Mirror the store so the re-rendered page shows what was saved.
	if f.history != nil {
		for i := range f.history.items {
			if f.history.items[i].ID == id {
				f.history.items[i].Annotation = annotation
				f.history.items[i].AnnotatedAt = at
			}
		}
	}
	return nil
}

func (f *fakeAnnotator) last() (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.id, f.annotation
}

func newServerWithAnnotator(t *testing.T, token string, m *board.Memory, history web.IncidentHistory, annotator web.IncidentAnnotator) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:    m,
		Watcher:   m,
		Incidents: history,
		Annotator: annotator,
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

func TestIncidentPageAnnotatesGroundTruth(t *testing.T) {
	t.Parallel()

	history := incidentsFixture()
	annotator := &fakeAnnotator{history: history}
	ts := newServerWithAnnotator(t, testToken, fixture(), history, annotator)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/incidents/inc-open") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if !strings.Contains(page, `data-testid="save-annotation"`) {
		t.Fatalf("annotation form missing:\n%s", page)
	}
	if !strings.Contains(page, `data-testid="no-annotation"`) {
		t.Error("unannotated incident did not state that it has no ground truth")
	}

	csrf := csrfFrom(t, page)
	form := url.Values{
		"csrf":       {csrf},
		"annotation": {"AWS us-east-1, confirmed on the status page"},
	}
	resp, err = client.PostForm(ts.URL+"/incidents/inc-open/annotate", form) //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page = body(t, resp)

	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/incidents/inc-open" {
		t.Fatalf("annotation ended at %s with status %d", resp.Request.URL.Path, resp.StatusCode)
	}
	if !strings.Contains(page, `data-testid="incident-annotation"`) {
		t.Fatalf("saved annotation not rendered:\n%s", page)
	}
	if !strings.Contains(page, "AWS us-east-1, confirmed") {
		t.Fatal("annotation text missing after save")
	}

	id, text := annotator.last()
	if id != "inc-open" || text != "AWS us-east-1, confirmed on the status page" {
		t.Fatalf("annotator received (%q, %q)", id, text)
	}
}

func TestIncidentAnnotationRejectsBadRequests(t *testing.T) {
	t.Parallel()

	history := incidentsFixture()
	annotator := &fakeAnnotator{history: history}
	ts := newServerWithAnnotator(t, testToken, fixture(), history, annotator)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	// A wrong CSRF token is a 400, not a write.
	badForm := url.Values{
		"csrf":       {"wrong"},
		"annotation": {"x"},
	}
	resp, err := client.PostForm(ts.URL+"/incidents/inc-open/annotate", badForm) //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad csrf status = %d, want 400", resp.StatusCode)
	}

	// Empty annotations are refused with a valid CSRF token.
	pageResp, err := client.Get(ts.URL + "/incidents/inc-open") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	csrf := csrfFrom(t, body(t, pageResp))

	emptyForm := url.Values{
		"csrf":       {csrf},
		"annotation": {"   "},
	}
	resp, err = client.PostForm(ts.URL+"/incidents/inc-open/annotate", emptyForm) //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty annotation status = %d, want 400", resp.StatusCode)
	}

	// Without an annotator the form is not offered and the POST is 503.
	bare := newServerWithAnnotator(t, testToken, fixture(), incidentsFixture(), nil)
	client2, _ := newClient(t)
	login(t, bare, client2, testToken)

	pageResp, err = client2.Get(bare.URL + "/incidents/inc-open") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body(t, pageResp), `data-testid="save-annotation"`) {
		t.Error("annotation form rendered without an annotator")
	}
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

type fakeTargetCorr struct {
	events     []store.ExternalEvent
	err        error
	gotCountry string
	gotASN     int
}

func (f *fakeTargetCorr) ScopeCorroboration(_ context.Context, country string, asn int, _ time.Time, _ int) ([]store.ExternalEvent, error) {
	f.gotCountry, f.gotASN = country, asn
	return f.events, f.err
}

func newServerWithTargetCorr(t *testing.T, token string, m *board.Memory, corr web.TargetCorroboration) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:              m,
		Watcher:             m,
		TargetCorroboration: corr,
		Vantage:             "local",
		Token:               token,
		Version:             "test",
		StartedAt:           time.Now(),
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}

	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

func TestTargetDrawerShowsFeedCorroboration(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(board.Target{
		ID: "2", Name: "United", Host: "united.com", Country: "US", ASN: 62041,
		State: board.StateDown, Port: 443, Probe: "tls", Tier: 2, IntervalS: 300,
	})
	corr := &fakeTargetCorr{events: []store.ExternalEvent{{
		ID: 1, Feed: "ioda", ExternalID: "us-1", Kind: "outage", Severity: 0.9,
		Country: "US", ASN: 62041,
		StartedAt: time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC),
	}}}

	ts := newServerWithTargetCorr(t, testToken, m, corr)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/targets/2") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if !strings.Contains(page, `data-testid="target-corroboration"`) {
		t.Fatalf("corroboration table missing:\n%s", page)
	}
	for _, want := range []string{"ioda", "outage", ">US<", "0.90", "2026-11-17 10:00", "ongoing", "never a verdict"} {
		if !strings.Contains(page, want) {
			t.Errorf("corroboration missing %q", want)
		}
	}
	if corr.gotCountry != "US" || corr.gotASN != 62041 {
		t.Fatalf("query scope = (%q, %d), want the target's own", corr.gotCountry, corr.gotASN)
	}
	if strings.Contains(page, `data-testid="no-target-corroboration"`) {
		t.Error("empty state rendered with events present")
	}
}

func TestTargetDrawerCorroborationStates(t *testing.T) {
	t.Parallel()

	withScope := board.NewMemory(board.Target{
		ID: "2", Name: "United", Host: "united.com", Country: "US", ASN: 62041,
		State: board.StateDown, Port: 443, Probe: "tls", Tier: 2, IntervalS: 300,
	})
	noScope := board.NewMemory(board.Target{
		ID: "2", Name: "United", Host: "united.com",
		State: board.StateDown, Port: 443, Probe: "http", Tier: 2, IntervalS: 300,
	})

	cases := []struct {
		name string
		m    *board.Memory
		corr web.TargetCorroboration
		want string
	}{
		{"unconfigured", withScope, nil, "not configured"},
		{"no scope", noScope, &fakeTargetCorr{}, "no scope to match"},
		{"empty", withScope, &fakeTargetCorr{}, "No feed event in this target"},
		{"failed", withScope, &fakeTargetCorr{err: errors.New("db down")}, "temporarily unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := newServerWithTargetCorr(t, testToken, tc.m, tc.corr)
			client, _ := newClient(t)
			login(t, ts, client, testToken)

			resp, err := client.Get(ts.URL + "/targets/2") //nolint:bodyclose // body() closes it
			if err != nil {
				t.Fatal(err)
			}
			page := body(t, resp)
			if !strings.Contains(page, `data-testid="no-target-corroboration"`) || !strings.Contains(page, tc.want) {
				t.Fatalf("note missing %q:\n%s", tc.want, page)
			}
			if strings.Contains(page, `data-testid="target-corroboration-row"`) {
				t.Error("a row rendered without an event")
			}
		})
	}
}

type fakeCountryEvents struct {
	events map[string]store.ExternalEvent
	err    error
}

func (f *fakeCountryEvents) RecentCountryEvents(context.Context, time.Time) (map[string]store.ExternalEvent, error) {
	return f.events, f.err
}

func newServerWithCountryEvents(t *testing.T, token string, m *board.Memory, events web.CountryEvents) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:        m,
		Watcher:       m,
		CountryEvents: events,
		Vantage:       "local",
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

func TestWorldShowsTheFeedOverlay(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(
		board.Target{ID: "1", Name: "Chase", Host: "chase.com", Country: "US", State: board.StateUp},
		board.Target{ID: "2", Name: "Deutsche", Host: "de.example", Country: "DE", State: board.StateUp},
	)
	events := &fakeCountryEvents{events: map[string]store.ExternalEvent{
		"US": {ID: 1, Feed: "ioda", Kind: "outage", Severity: 0.87, Country: "US"},
	}}

	ts := newServerWithCountryEvents(t, testToken, m, events)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/world") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if !strings.Contains(page, `data-testid="country-feed"`) {
		t.Fatalf("feed overlay missing:\n%s", page)
	}
	for _, want := range []string{"ioda outage", "0.87", `data-country="US"`} {
		if !strings.Contains(page, want) {
			t.Errorf("overlay missing %q", want)
		}
	}
	if strings.Contains(page, `data-testid="world-overlay-note"`) {
		t.Error("a note rendered with overlay events present")
	}
	// Only US has an event; DE must not borrow it.
	if strings.Count(page, `data-testid="country-feed"`) != 1 {
		t.Error("an event was attached to the wrong country")
	}
}

func TestWorldOverlayNotes(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(
		board.Target{ID: "1", Name: "Chase", Host: "chase.com", Country: "US", State: board.StateUp},
	)
	cases := []struct {
		name string
		ce   web.CountryEvents
		want string
	}{
		{"unconfigured", nil, "not configured"},
		{"empty", &fakeCountryEvents{}, "No country-scoped feed event"},
		{"failed", &fakeCountryEvents{err: errors.New("db down")}, "temporarily unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := newServerWithCountryEvents(t, testToken, m, tc.ce)
			client, _ := newClient(t)
			login(t, ts, client, testToken)

			resp, err := client.Get(ts.URL + "/world") //nolint:bodyclose // body() closes it
			if err != nil {
				t.Fatal(err)
			}
			page := body(t, resp)
			if !strings.Contains(page, `data-testid="world-overlay-note"`) || !strings.Contains(page, tc.want) {
				t.Fatalf("note missing %q:\n%s", tc.want, page)
			}
			if strings.Contains(page, `data-testid="country-feed"`) {
				t.Error("a feed badge rendered without an event")
			}
		})
	}
}

func TestBoardDensityToggle(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(
		board.Target{ID: "1", Name: "Chase", Host: "chase.com", Country: "US", Sector: "financial/banking", State: board.StateUp},
		board.Target{ID: "2", Name: "United", Host: "united.com", Country: "US", Sector: "airline", State: board.StateDown},
	)
	ts := newServer(t, testToken, m)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	get := func(path string) string {
		t.Helper()
		resp, err := client.Get(ts.URL + path) //nolint:bodyclose // body() closes it
		if err != nil {
			t.Fatal(err)
		}
		return body(t, resp)
	}

	// Compact is the default and is stated on the page.
	page := get("/")
	if !strings.Contains(page, `data-density="compact"`) {
		t.Fatalf("default density missing:\n%s", page)
	}
	if !strings.Contains(page, `data-testid="density-compact"`) || !strings.Contains(page, `data-testid="density-comfortable"`) {
		t.Fatal("density toggle incomplete")
	}
	if !strings.Contains(page, `class="group-link active" href="/?group=sector"`) {
		t.Error("default density link is not marked active")
	}

	// Comfortable is a real lens, marked active, and stated on the board.
	page = get("/?density=comfortable")
	if !strings.Contains(page, `data-density="comfortable"`) {
		t.Fatal("comfortable density not applied")
	}
	if !strings.Contains(page, `class="group-link active" href="/?group=sector&amp;density=comfortable"`) {
		t.Error("comfortable is not marked active")
	}

	// Switching density keeps the other view state, and switching group keeps
	// density: the controls must not silently reset each other.
	page = get("/?group=cdn&focus=problems&country=US&density=comfortable")
	for _, want := range []string{
		`href="/?group=state&amp;focus=problems&amp;country=US&amp;density=comfortable"`,
		`href="/?group=cdn&amp;focus=problems&amp;country=US"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("a toggle dropped view state: missing %q", want)
		}
	}

	// A typo falls back to the default rather than breaking the page.
	page = get("/?density=huge")
	if !strings.Contains(page, `data-density="compact"`) {
		t.Fatal("an unknown density did not fall back to compact")
	}
}

type fakeCorridorStatus struct {
	statuses  []store.CorridorStatus
	series    map[string][]store.CorridorSample
	err       error
	seriesErr error
}

func (f *fakeCorridorStatus) CorridorStatus(context.Context) ([]store.CorridorStatus, error) {
	return f.statuses, f.err
}

func (f *fakeCorridorStatus) CorridorSeries(context.Context, time.Time) (map[string][]store.CorridorSample, error) {
	return f.series, f.seriesErr
}

func newServerWithCorridors(t *testing.T, token string, m *board.Memory, corridors web.CorridorStatus) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:    m,
		Watcher:   m,
		Corridors: corridors,
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

func TestCorridorPage(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	latest := store.CorridorSample{
		CorridorID: "nyc-lon", TS: started.Add(time.Hour),
		RTTMS: 83, Loss: 0, OK: true,
	}
	open := store.CorridorEvent{
		ID: "ce-1", CorridorID: "nyc-lon", Kind: "rtt_step",
		StartedAt: started, Detail: "rtt 190ms against 82ms",
		Trace: " 1  192.0.2.1  4ms\ndestination 198.51.100.9 reached=true\n",
	}
	corridors := &fakeCorridorStatus{
		statuses: []store.CorridorStatus{
			{Corridor: store.Corridor{ID: "nyc-lon", From: "NYC", To: "LON"}, Latest: &latest, Open: &open},
			{Corridor: store.Corridor{ID: "lon-sin", From: "LON", To: "SIN"}},
		},
		// Three successful samples: enough for a shape, and their median is the
		// baseline the page prints.
		series: map[string][]store.CorridorSample{
			"nyc-lon": {
				{CorridorID: "nyc-lon", TS: started, RTTMS: 80, OK: true},
				{CorridorID: "nyc-lon", TS: started.Add(5 * time.Minute), RTTMS: 90, OK: true},
				{CorridorID: "nyc-lon", TS: started.Add(10 * time.Minute), RTTMS: 100, OK: true},
			},
		},
	}

	ts := newServerWithCorridors(t, testToken, fixture(), corridors)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/corridors") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(page, `data-testid="corridors"`) {
		t.Fatalf("corridor table missing:\n%s", page)
	}
	for _, want := range []string{
		"nyc-lon", "NYC → LON", "83 ms", "0%",
		"step change", "rtt_step", "rtt 190ms against 82ms",
		"waiting for a sample", "LON → SIN",
		// The baseline is the median of the window, not the latest sample.
		">90 ms<",
		// A three-sample shape and an empty five-cell loss bar.
		"▁▅█",
		"▱▱▱▱▱",
		// The trace captured with the step change.
		`data-testid="corridor-trace"`, "destination 198.51.100.9 reached=true",
		// The band under the shape: every bucket is inside the six-hour band.
		`data-testid="corridor-band"`, "···",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("corridor page missing %q", want)
		}
	}
	if strings.Contains(page, `data-testid="corridor-series-note"`) {
		t.Error("a series note rendered with samples present")
	}
	// The corridor without samples has no fabricated shape.
	if strings.Count(page, `data-testid="corridor-spark"`) != 1 {
		t.Errorf("spark shapes = %d, want only the sampled corridor", strings.Count(page, `data-testid="corridor-spark"`))
	}
}

func TestCorridorPageSeriesFailureIsStated(t *testing.T) {
	t.Parallel()

	latest := store.CorridorSample{CorridorID: "nyc-lon", TS: time.Now().UTC(), RTTMS: 83, OK: true}
	corridors := &fakeCorridorStatus{
		statuses: []store.CorridorStatus{
			{Corridor: store.Corridor{ID: "nyc-lon", From: "NYC", To: "LON"}, Latest: &latest},
		},
		seriesErr: errors.New("db down"),
	}

	ts := newServerWithCorridors(t, testToken, fixture(), corridors)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/corridors") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	// A failed history query must not take the current sample away, and must
	// say why the shapes are missing.
	if !strings.Contains(page, `data-testid="corridor-series-note"`) {
		t.Fatalf("series failure was silent:\n%s", page)
	}
	if !strings.Contains(page, "temporarily unavailable") {
		t.Error("series note does not say the history is unavailable")
	}
	if !strings.Contains(page, `data-testid="corridor-row"`) || !strings.Contains(page, "83 ms") {
		t.Error("a series failure removed the current sample")
	}
	if strings.Contains(page, `data-testid="corridor-spark"`) {
		t.Error("a spark rendered without a series")
	}
}

func TestCorridorPageNotes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		corridors web.CorridorStatus
		want      string
	}{
		{"unconfigured", nil, "not configured"},
		{"empty", &fakeCorridorStatus{}, "No corridors are configured"},
		{"failed", &fakeCorridorStatus{err: errors.New("db down")}, "temporarily unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := newServerWithCorridors(t, testToken, fixture(), tc.corridors)
			client, _ := newClient(t)
			login(t, ts, client, testToken)

			resp, err := client.Get(ts.URL + "/corridors") //nolint:bodyclose // body() closes it
			if err != nil {
				t.Fatal(err)
			}
			page := body(t, resp)
			if !strings.Contains(page, `data-testid="no-corridors"`) || !strings.Contains(page, tc.want) {
				t.Fatalf("note missing %q:\n%s", tc.want, page)
			}
			if strings.Contains(page, `data-testid="corridor-row"`) {
				t.Error("a row rendered without a corridor")
			}
		})
	}
}

func TestBoardCollectsUnmeasurableIntoItsOwnQuietSection(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(
		board.Target{ID: "1", Name: "Chase", Host: "chase.com", Sector: "banking", State: board.StateUp},
		board.Target{
			ID: "2", Name: "Country anchor", Host: "anchor.example", Sector: "banking",
			State: board.StateUnmeasurable,
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

	if !strings.Contains(page, `data-sector="unmeasurable"`) {
		t.Fatalf("unmeasurable section missing:\n%s", page)
	}
	if !strings.Contains(page, `muted-group`) {
		t.Error("the unmeasurable section is not marked quiet")
	}

	// The unmeasurable target is not folded into its sector group.
	bankingStart := strings.Index(page, `data-sector="banking"`)
	unmeasurableStart := strings.Index(page, `data-sector="unmeasurable"`)
	if bankingStart < 0 || unmeasurableStart < 0 {
		t.Fatalf("groups missing: banking=%d unmeasurable=%d", bankingStart, unmeasurableStart)
	}
	if unmeasurableStart < bankingStart {
		t.Error("the unmeasurable section is not last")
	}
	bankingBlock := page[bankingStart:unmeasurableStart]
	if strings.Contains(bankingBlock, `data-target="anchor.example"`) {
		t.Error("the unmeasurable target is still in its sector group")
	}

	// It is collapsed: a target the monitor cannot see is not a problem to expand.
	sectionStart := strings.LastIndex(page[:unmeasurableStart], "<details")
	sectionTag := page[sectionStart:unmeasurableStart]
	if strings.Contains(sectionTag, " open") {
		t.Error("the unmeasurable section starts expanded")
	}
}

type fakeSeriesBatch struct {
	series map[string][]store.Rollup
	err    error
}

func (f *fakeSeriesBatch) SeriesForTargets(context.Context, []string, string, string, time.Time, time.Time) (map[string][]store.Rollup, error) {
	return f.series, f.err
}

func newServerWithSeriesBatch(t *testing.T, token string, m *board.Memory, batch web.SeriesBatch) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:      m,
		Watcher:     m,
		SeriesBatch: batch,
		Vantage:     "local",
		Token:       token,
		Version:     "test",
		StartedAt:   time.Now(),
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}

	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

func TestBoardSparklineInComfortableDensity(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(
		board.Target{ID: "1", Name: "Chase", Host: "chase.com", Sector: "banking", State: board.StateUp},
		board.Target{ID: "2", Name: "United", Host: "united.com", Sector: "airline", State: board.StateDown},
	)
	batch := &fakeSeriesBatch{series: map[string][]store.Rollup{
		"1": {
			{N: 4, Up: 4},
			{N: 4, Up: 4},
		},
	}}

	ts := newServerWithSeriesBatch(t, testToken, m, batch)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	get := func(path string) string {
		t.Helper()
		resp, err := client.Get(ts.URL + path) //nolint:bodyclose // body() closes it
		if err != nil {
			t.Fatal(err)
		}
		return body(t, resp)
	}

	// Comfortable renders the shape for the target that has history.
	page := get("/?density=comfortable")
	if !strings.Contains(page, `data-testid="tile-spark"`) {
		t.Fatalf("comfortable board has no sparkline:\n%s", page)
	}
	if got := strings.Count(page, `data-testid="tile-spark"`); got != 1 {
		t.Fatalf("sparklines = %d, want only the target with history", got)
	}
	if !strings.Contains(page, "██") {
		t.Error("two fully healthy buckets did not render as full blocks")
	}

	// Compact stays compact: the shape is a comfortable-density feature.
	page = get("/")
	if strings.Contains(page, `data-testid="tile-spark"`) {
		t.Error("compact density rendered a sparkline")
	}
}

func TestBoardSparklineFailureIsNotFatal(t *testing.T) {
	t.Parallel()

	ts := newServerWithSeriesBatch(t, testToken, fixture(), &fakeSeriesBatch{err: errors.New("db down")})
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/?density=comfortable") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the board to survive a history failure", resp.StatusCode)
	}
	if strings.Contains(page, `data-testid="tile-spark"`) {
		t.Error("a sparkline rendered from a failed query")
	}
	if !strings.Contains(page, `data-testid="tile"`) {
		t.Error("a history failure removed the tiles")
	}
}

func TestBoardPaginatesGroupsInComfortableDensity(t *testing.T) {
	t.Parallel()

	targets := make([]board.Target, 0, 65)
	for i := 0; i < 65; i++ {
		id := strconv.Itoa(i)
		targets = append(targets, board.Target{
			ID: "t" + id, Name: "Target " + id, Host: "t" + id + ".example",
			Sector: "banking", State: board.StateUp,
		})
	}

	ts := newServer(t, testToken, board.NewMemory(targets...))
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	get := func(path string) (int, string) {
		t.Helper()
		resp, err := client.Get(ts.URL + path) //nolint:bodyclose // body() closes it
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, body(t, resp)
	}

	// The first comfortable page renders one page and says what is behind it.
	status, page := get("/?density=comfortable")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if got := strings.Count(page, `data-testid="tile"`); got != 60 {
		t.Fatalf("first page tiles = %d, want 60", got)
	}
	if !strings.Contains(page, `data-testid="group-more"`) || !strings.Contains(page, "5 more not shown") {
		t.Fatalf("pager missing:\n%s", page)
	}
	if !strings.Contains(page, `data-testid="group-more-link"`) || !strings.Contains(page, `data-testid="group-all-link"`) {
		t.Fatal("pager links missing")
	}

	// The links carry the density with them; following "show all" renders
	// every tile and drops the pager.
	_, page = get("/?group=sector&density=comfortable&show=banking:65")
	if got := strings.Count(page, `data-testid="tile"`); got != 65 {
		t.Fatalf("show-all tiles = %d, want 65", got)
	}
	if strings.Contains(page, `data-testid="group-more"`) {
		t.Error("the pager survived a show-all request")
	}

	// Compact is the lens that fits, so it never pages.
	_, page = get("/")
	if got := strings.Count(page, `data-testid="tile"`); got != 65 {
		t.Fatalf("compact tiles = %d, want all 65", got)
	}
	if strings.Contains(page, `data-testid="group-more"`) {
		t.Error("compact density paged")
	}

	// A malformed show value is a typo, not a client error: the first page
	// still renders.
	status, page = get("/?density=comfortable&show=nonsense")
	if status != http.StatusOK {
		t.Fatalf("malformed show status = %d, want 200", status)
	}
	if got := strings.Count(page, `data-testid="tile"`); got != 60 {
		t.Fatalf("malformed show tiles = %d, want the default page", got)
	}
}

func TestTargetDetailShowsDNSDivergence(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	m := board.NewMemory(
		board.Target{
			ID: "2", Name: "United", Host: "united.com", State: board.StateUp,
			Port: 53, Probe: "dns", Tier: 2, IntervalS: 300,
			DNSVerdict: "DNS_ANOMALY", DNSConfidence: "medium",
			DNSDetail: "one resolver disagrees with its own baseline",
			DNSAt:     at,
		},
		board.Target{
			ID: "3", Name: "Quiet", Host: "quiet.example", State: board.StateUp,
			Port: 53, Probe: "dns", Tier: 2, IntervalS: 300,
		},
	)

	ts := newServerWithDetails(t, testToken, m, &fakeChanges{})
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	get := func(id string) string {
		t.Helper()
		resp, err := client.Get(ts.URL + "/targets/" + id) //nolint:bodyclose // body() closes it
		if err != nil {
			t.Fatal(err)
		}
		return body(t, resp)
	}

	page := get("2")
	for _, want := range []string{
		">DNS divergence<", "DNS_ANOMALY (medium)",
		"2026-11-17 12:00", "disagrees with its own baseline",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("divergence row missing %q", want)
		}
	}

	// A target with no divergence says so instead of leaving the row blank.
	if !strings.Contains(get("3"), "none observed") {
		t.Error("a target without divergence did not say so")
	}
}

type fakeDNSObservations struct {
	records []store.DNSDivergenceRecord
	err     error
}

func (f *fakeDNSObservations) RecentDNSDivergences(context.Context, int) ([]store.DNSDivergenceRecord, error) {
	return f.records, f.err
}

func newServerWithDNSObservations(t *testing.T, token string, m *board.Memory, obs web.DNSObservations) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:          m,
		Watcher:         m,
		DNSObservations: obs,
		Vantage:         "local",
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

func TestBoardShowsCollapsedDNSObservations(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(board.Target{ID: "1", Name: "Chase", Host: "chase.com", State: board.StateUp})
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	obs := &fakeDNSObservations{records: []store.DNSDivergenceRecord{
		{
			TargetID: "1", Host: "chase.com", TS: at,
			Verdict: "DNS_ANOMALY", Confidence: "medium",
			Detail: "one resolver disagrees with its own baseline",
		},
	}}

	ts := newServerWithDNSObservations(t, testToken, m, obs)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if !strings.Contains(page, `data-testid="dns-strip"`) {
		t.Fatalf("DNS strip missing:\n%s", page)
	}
	for _, want := range []string{
		"chase.com", "DNS_ANOMALY (medium)", "2026-11-17 12:00",
		"disagrees with its own baseline", "never changes a tile's state",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("DNS strip missing %q", want)
		}
	}
	// It is a collapsed strip, not an open block.
	if !strings.Contains(page, `<details class="dns-strip"`) {
		t.Error("the DNS strip is not a details block")
	}
	if strings.Contains(page, `<details class="dns-strip" open`) {
		t.Error("the DNS strip starts expanded")
	}
}

func TestBoardDNSObservationFailureIsStated(t *testing.T) {
	t.Parallel()

	obs := &fakeDNSObservations{err: errors.New("db down")}
	ts := newServerWithDNSObservations(t, testToken, fixture(), obs)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the board to survive a failed strip", resp.StatusCode)
	}
	if !strings.Contains(page, `data-testid="dns-note"`) || !strings.Contains(page, "temporarily unavailable") {
		t.Fatal("a failed DNS observation query was silent")
	}
	if !strings.Contains(page, `data-testid="tile"`) {
		t.Error("a failed strip removed the tiles")
	}
}

type fakeProvider struct {
	events []store.ExternalEvent
	err    error
	gotURL string
}

func (f *fakeProvider) ProviderCorroboration(_ context.Context, statusURL string, _ time.Time, _ int) ([]store.ExternalEvent, error) {
	f.gotURL = statusURL
	return f.events, f.err
}

func newServerWithProvider(t *testing.T, token string, m *board.Memory, provider web.ProviderStatus) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:    m,
		Watcher:   m,
		Provider:  provider,
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

func TestTargetDrawerShowsProviderStatus(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	m := board.NewMemory(
		board.Target{
			ID: "2", Name: "Chase", Host: "chase.com", State: board.StateDown,
			StatusURL: "https://status.chase.com",
		},
		board.Target{ID: "3", Name: "Loose", Host: "loose.example", State: board.StateUp},
	)
	provider := &fakeProvider{events: []store.ExternalEvent{{
		ID: 1, Feed: "statuspage", ExternalID: "status.chase.com:inc-1",
		Kind: "statuspage", Severity: 0.8, StartedAt: at, ObservedAt: at,
	}}}

	ts := newServerWithProvider(t, testToken, m, provider)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	get := func(id string) string {
		t.Helper()
		resp, err := client.Get(ts.URL + "/targets/" + id) //nolint:bodyclose // body() closes it
		if err != nil {
			t.Fatal(err)
		}
		return body(t, resp)
	}

	page := get("2")
	if !strings.Contains(page, `data-testid="provider-corroboration"`) {
		t.Fatalf("provider panel missing:\n%s", page)
	}
	for _, want := range []string{
		">Provider status<", "https://status.chase.com",
		"statuspage", "0.80", "ongoing", "own report",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("provider panel missing %q", want)
		}
	}
	if provider.gotURL != "https://status.chase.com" {
		t.Fatalf("queried URL = %q", provider.gotURL)
	}

	// A target with no declared page has no provider claim at all.
	page = get("3")
	if strings.Contains(page, ">Provider status<") {
		t.Error("a target without a status URL rendered a provider panel")
	}
}

func TestTargetDrawerProviderNotes(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		provider web.ProviderStatus
		want     string
	}{
		{"unconfigured", nil, "not configured"},
		{"empty", &fakeProvider{}, "reports no incident"},
		{"failed", &fakeProvider{err: errors.New("db down")}, "temporarily unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := board.NewMemory(board.Target{
				ID: "2", Name: "Chase", Host: "chase.com", State: board.StateDown,
				StatusURL: "https://status.chase.com", DNSAt: at,
			})
			ts := newServerWithProvider(t, testToken, m, tc.provider)
			client, _ := newClient(t)
			login(t, ts, client, testToken)

			resp, err := client.Get(ts.URL + "/targets/2") //nolint:bodyclose // body() closes it
			if err != nil {
				t.Fatal(err)
			}
			page := body(t, resp)
			if !strings.Contains(page, `data-testid="no-provider-corroboration"`) || !strings.Contains(page, tc.want) {
				t.Fatalf("note missing %q:\n%s", tc.want, page)
			}
			if strings.Contains(page, `data-testid="provider-corroboration-row"`) {
				t.Error("a row rendered without an event")
			}
		})
	}
}

func TestCorridorPageMarksOutOfBandBuckets(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	samples := make([]store.CorridorSample, 0, 31)
	for i := 0; i < 30; i++ {
		samples = append(samples, store.CorridorSample{
			CorridorID: "nyc-lon", TS: started.Add(time.Duration(i) * 5 * time.Minute),
			RTTMS: 80, OK: true,
		})
	}
	// A single late spike: outside the six-hour band the detector compares
	// against, and outside the newest bucket's own band because the bucket is
	// excluded from it.
	samples = append(samples, store.CorridorSample{
		CorridorID: "nyc-lon", TS: started.Add(30 * 5 * time.Minute),
		RTTMS: 200, OK: true,
	})
	latest := samples[len(samples)-1]

	corridors := &fakeCorridorStatus{
		statuses: []store.CorridorStatus{{
			Corridor: store.Corridor{ID: "nyc-lon", From: "NYC", To: "LON"},
			Latest:   &latest,
		}},
		series: map[string][]store.CorridorSample{"nyc-lon": samples},
	}

	ts := newServerWithCorridors(t, testToken, fixture(), corridors)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/corridors") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if !strings.Contains(page, `data-testid="corridor-band"`) {
		t.Fatalf("band markers missing:\n%s", page)
	}
	if !strings.Contains(page, "▲") {
		t.Error("the out-of-band bucket was not marked above the band")
	}
	if !strings.Contains(page, "·") {
		t.Error("an in-band bucket was not marked inside the band")
	}
}

type fakeLogs struct {
	records []logging.Record
	diag    logging.Diagnostics
	scopes  []logging.Scope
	stream  chan logging.Record
}

func (f *fakeLogs) LogRecords(slog.Level, string, int) []logging.Record { return f.records }
func (f *fakeLogs) Diagnostics() logging.Diagnostics                    { return f.diag }
func (f *fakeLogs) ActiveScopes() []logging.Scope                       { return f.scopes }

func (f *fakeLogs) Subscribe() (<-chan logging.Record, func()) {
	if f.stream == nil {
		f.stream = make(chan logging.Record, 8)
	}
	return f.stream, func() {}
}

func newServerWithLogs(t *testing.T, token string, m *board.Memory, logs web.LogDiagnostics) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:    m,
		Watcher:   m,
		Logs:      logs,
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

func TestDiagnosticsPageShowsSetupAndRecords(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	logs := &fakeLogs{
		diag: logging.Diagnostics{
			Level: "INFO", Format: "json", File: "/data/netweather.log",
			RingRecords: 120, RingCapacity: 2000, Dropped: 3, WriteErrors: 1, Scopes: 1,
		},
		scopes: []logging.Scope{{
			ID: "sc-1", Target: "chase.com", Level: "debug",
			CreatedAt: at, ExpiresAt: at.Add(time.Hour),
		}},
		records: []logging.Record{
			{Time: at, Level: slog.LevelInfo, LevelName: "INFO", Message: "older line", Attrs: map[string]string{"target": "chase.com"}},
			{Time: at.Add(time.Minute), Level: slog.LevelWarn, LevelName: "WARN", Message: "newest line", Attrs: map[string]string{"target": "chase.com", "error": "timeout"}},
		},
	}

	ts := newServerWithLogs(t, testToken, fixture(), logs)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/diagnostics") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	for _, want := range []string{
		`data-testid="diagnostics-stats"`,
		"INFO", "json", "/data/netweather.log",
		"120 of 2000 records", "Dropped lines", ">3<", ">1<",
		"sc-1", "target=chase.com", "debug",
		"newest line", "older line", "error=timeout",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("diagnostics page missing %q", want)
		}
	}

	// Newest first: the line that explains a failure is the last one written.
	newest := strings.Index(page, "newest line")
	older := strings.Index(page, "older line")
	if newest < 0 || older < 0 || newest > older {
		t.Errorf("records are not newest-first (newest=%d older=%d)", newest, older)
	}
	// The severity is in the row's data attribute, so a reader that cannot see
	// the colour still gets it.
	if !strings.Contains(page, `data-level="WARN"`) {
		t.Error("the record row does not carry its level")
	}
}

func TestDiagnosticsPageWithoutLogs(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/diagnostics") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the page to render without a ring", resp.StatusCode)
	}
	if !strings.Contains(page, `data-testid="diagnostics-note"`) || !strings.Contains(page, "not wired") {
		t.Fatal("an unwired ring was silent")
	}
	if strings.Contains(page, `data-testid="diagnostics-stats"`) {
		t.Error("stats rendered without a ring")
	}
}

func TestTargetDrawerShowsWhyLogLines(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	m := board.NewMemory(board.Target{
		ID: "2", Name: "Chase", Host: "chase.com", State: board.StateDown,
	})
	logs := &fakeLogs{records: []logging.Record{{
		Time: at, Level: slog.LevelWarn, LevelName: "WARN",
		Message: "probe failed", Attrs: map[string]string{"target": "chase.com", "error_class": "tcp_refused"},
	}}}

	ts := newServerWithLogs(t, testToken, m, logs)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/targets/2") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if !strings.Contains(page, `data-testid="target-why"`) {
		t.Fatalf("Why panel missing:\n%s", page)
	}
	for _, want := range []string{"probe failed", "error_class=tcp_refused", ">Why?<"} {
		if !strings.Contains(page, want) {
			t.Errorf("Why panel missing %q", want)
		}
	}
}

func TestTargetDrawerWhyWithoutLogs(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(board.Target{ID: "2", Name: "Chase", Host: "chase.com", State: board.StateDown})
	ts := newServerWithDetails(t, testToken, m, &fakeChanges{})
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/targets/2") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if !strings.Contains(page, `data-testid="no-target-why"`) || !strings.Contains(page, "not wired") {
		t.Fatal("an unwired ring was not stated in the drawer")
	}
}

type fakeBudget struct {
	used      int64
	allowance int64
	exhausted bool
}

func (f fakeBudget) Used() int64      { return f.used }
func (f fakeBudget) Allowance() int64 { return f.allowance }
func (f fakeBudget) Exhausted() bool  { return f.exhausted }

func newServerWithBudget(t *testing.T, token string, m *board.Memory, budget web.BudgetState) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:    m,
		Watcher:   m,
		Budget:    budget,
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

func TestBoardShowsSpentEgressBudget(t *testing.T) {
	t.Parallel()

	ts := newServerWithBudget(t, testToken, fixture(), fakeBudget{
		used: 2048 << 20, allowance: 2048 << 20, exhausted: true,
	})
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if !strings.Contains(page, `data-testid="budget-banner"`) {
		t.Fatalf("a spent budget was silent:\n%s", page)
	}
	for _, want := range []string{"Daily egress budget spent", "2.0 GB", "00:00 UTC"} {
		if !strings.Contains(page, want) {
			t.Errorf("budget banner missing %q", want)
		}
	}
}

func TestBoardWithoutSpentBudgetHasNoBanner(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		budget web.BudgetState
	}{
		{"none configured", nil},
		{"room left", fakeBudget{used: 100 << 20, allowance: 2048 << 20}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := newServerWithBudget(t, testToken, fixture(), tc.budget)
			client, _ := newClient(t)
			login(t, ts, client, testToken)

			resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
			if err != nil {
				t.Fatal(err)
			}
			page := body(t, resp)
			if strings.Contains(page, `data-testid="budget-banner"`) {
				t.Error("a banner rendered without a spent budget")
			}
		})
	}
}

type fakeIncidentProbes struct {
	rows []store.ProbeRow
	err  error
}

func (f *fakeIncidentProbes) IncidentProbes(context.Context, string, int) ([]store.ProbeRow, error) {
	return f.rows, f.err
}

func newServerWithIncidentProbes(t *testing.T, token string, m *board.Memory, history web.IncidentHistory, probes web.IncidentProbeReader) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:         m,
		Watcher:        m,
		Incidents:      history,
		IncidentProbes: probes,
		Token:          token,
		Version:        "test",
		StartedAt:      time.Now(),
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}

	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

func TestIncidentDetailShowsArchivedProbes(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 11, 17, 11, 30, 0, 0, time.UTC)
	probes := &fakeIncidentProbes{rows: []store.ProbeRow{
		{TS: at, State: state.Down, Stage: "http", ErrorClass: "tcp_refused", LatencyMS: 0, Confidence: "high", Detail: "refused"},
		{TS: at.Add(5 * time.Minute), State: state.Up, Stage: "http", LatencyMS: 42, Confidence: "high", Detail: "200"},
	}}

	ts := newServerWithIncidentProbes(t, testToken, fixture(), incidentsFixture(), probes)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/incidents/inc-open") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if !strings.Contains(page, `data-testid="incident-probes"`) {
		t.Fatalf("archive panel missing:\n%s", page)
	}
	for _, want := range []string{"incident-probe-row", "tcp_refused", "42 ms", "Pinned from the raw probe stream"} {
		if !strings.Contains(page, want) {
			t.Errorf("archive panel missing %q", want)
		}
	}
	// Oldest first: the failure is explained by what came after it.
	if strings.Index(page, "11:30:00") > strings.Index(page, "11:35:00") {
		t.Error("archived probes are not oldest-first")
	}
}

func TestIncidentDetailWithoutArchiveStatesIt(t *testing.T) {
	t.Parallel()

	// No reader: the panel must say so rather than render an empty table.
	ts := newServerWithIncidents(t, testToken, fixture(), incidentsFixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/incidents/inc-open") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if !strings.Contains(page, `data-testid="no-incident-probes"`) ||
		!strings.Contains(page, "archive is not configured") {
		t.Fatalf("an unwired archive was silent:\n%s", page)
	}

	// A wired reader with no rows is a different, honest message.
	empty := newServerWithIncidentProbes(t, testToken, fixture(), incidentsFixture(), &fakeIncidentProbes{})
	client2, _ := newClient(t)
	login(t, empty, client2, testToken)
	resp, err = client2.Get(empty.URL + "/incidents/inc-open") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page = body(t, resp)
	if !strings.Contains(page, "No probes were pinned around this incident") {
		t.Fatalf("an empty archive was not stated:\n%s", page)
	}
}

type fakeHealth struct {
	index ihi.Index
}

func (f *fakeHealth) Build(context.Context) ihi.Index { return f.index }

func newServerWithHealth(t *testing.T, token string, m *board.Memory, health web.HealthIndex) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:    m,
		Watcher:   m,
		Health:    health,
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

func TestBoardShowsIHIChip(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	health := &fakeHealth{index: ihi.Index{
		Score: 82.4,
		Parts: []ihi.Part{
			{Name: "Own health", Score: 90, Weight: 0.7, At: at},
			{Name: "External outage severity", Score: 60, Weight: 0.3, At: at},
		},
		Dropped: []ihi.Dropped{{Name: "BGP stability", Reason: "stale"}},
	}}

	ts := newServerWithHealth(t, testToken, fixture(), health)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if !strings.Contains(page, `data-testid="ihi-chip"`) || !strings.Contains(page, "IHI 82") {
		t.Fatalf("board chip missing:\n%s", page)
	}
	if !strings.Contains(page, `href="/ihi"`) {
		t.Error("the chip does not link to the components")
	}

	// The page carries the number, the weights, the evidence time, and what
	// was left out.
	resp, err = client.Get(ts.URL + "/ihi") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page = body(t, resp)
	for _, want := range []string{
		`data-testid="ihi-score"`, "IHI <b>82</b>",
		`data-testid="ihi-parts"`, "Own health", "90", "70%", "12:00 UTC",
		`data-testid="ihi-dropped"`, "BGP stability", "stale",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("ihi page missing %q", want)
		}
	}
}

func TestBoardAndIHIPageWithoutAnIndex(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body(t, resp), `data-testid="ihi-chip"`) {
		t.Error("a chip rendered without an index")
	}

	// A configured index with nothing scoreable says so rather than showing 0.
	empty := newServerWithHealth(t, testToken, fixture(), &fakeHealth{})
	client2, _ := newClient(t)
	login(t, empty, client2, testToken)
	resp, err = client2.Get(empty.URL + "/ihi") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	if !strings.Contains(page, `data-testid="ihi-empty"`) || !strings.Contains(page, "No component could be scored") {
		t.Fatalf("an empty index was not stated:\n%s", page)
	}
}

type fakeVerdict struct {
	res verdict.Result
}

func (f *fakeVerdict) Build(context.Context) verdict.Result { return f.res }

func newServerWithVerdict(t *testing.T, token string, m *board.Memory, v web.VerdictSource) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:    m,
		Watcher:   m,
		Verdict:   v,
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

func TestBoardShowsTheGlobalVerdict(t *testing.T) {
	t.Parallel()

	ts := newServerWithVerdict(t, testToken, fixture(), &fakeVerdict{res: verdict.Result{
		Verdict: verdict.Local, Detail: "2 of 3 reference targets failing; this is likely your path",
	}})
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if !strings.Contains(page, `data-testid="verdict"`) || !strings.Contains(page, `data-verdict="LOCAL"`) {
		t.Fatalf("verdict badge missing:\n%s", page)
	}
	if !strings.Contains(page, "2 of 3 reference targets failing") {
		t.Error("the verdict detail is not carried in the title")
	}
	// A LOCAL verdict is not a stale monitor, so no staleness banner.
	if strings.Contains(page, `data-testid="stale-banner"`) {
		t.Error("a LOCAL verdict rendered the staleness banner")
	}
}

func TestStaleVerdictRaisesAProminentBanner(t *testing.T) {
	t.Parallel()

	ts := newServerWithVerdict(t, testToken, fixture(), &fakeVerdict{res: verdict.Result{
		Verdict: verdict.Stale, Detail: "no probe in 4m0s (reference cadence 30s)",
	}})
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if !strings.Contains(page, `data-testid="stale-banner"`) || !strings.Contains(page, "not probing") {
		t.Fatalf("STALE did not raise a banner:\n%s", page)
	}
	if !strings.Contains(page, "no probe in 4m0s") {
		t.Error("the banner does not explain the lateness")
	}
}

func TestBoardWithoutAVerdictShowsNoBadge(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	if strings.Contains(page, `data-testid="verdict"`) {
		t.Fatal("a verdict badge rendered without a source")
	}
}

type fakeAnomalies struct {
	rows []store.Anomaly
	err  error
}

func (f *fakeAnomalies) Anomalies(context.Context, int) ([]store.Anomaly, error) {
	return f.rows, f.err
}

func newServerWithAnomalies(t *testing.T, token string, m *board.Memory, anomalies web.AnomalyReader) *httptest.Server {
	t.Helper()

	h, err := web.New(web.Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:    m,
		Watcher:   m,
		Anomalies: anomalies,
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

func TestBoardShowsTheCoverageAdvisory(t *testing.T) {
	t.Parallel()

	at := time.Now().UTC().Add(-30 * time.Minute)
	ts := newServerWithAnomalies(t, testToken, fixture(), &fakeAnomalies{rows: []store.Anomaly{
		{
			TS: at, Scope: "sector", ScopeValue: "airline", Kind: "coverage_drop",
			Observed: 0.42, Baseline: 0.95, Buckets: 2,
		},
	}})
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	if !strings.Contains(page, `data-testid="coverage-advisory"`) {
		t.Fatalf("coverage advisory missing:\n%s", page)
	}
	for _, want := range []string{"sector airline", "42%", "95%", "visibility loss, not an outage"} {
		if !strings.Contains(page, want) {
			t.Errorf("coverage advisory missing %q", want)
		}
	}
}

func TestBoardOmitsStaleAndNonCoverageAdvisories(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	cases := []struct {
		name string
		rows []store.Anomaly
	}{
		{"a health drop is not a coverage advisory", []store.Anomaly{
			{TS: now.Add(-10 * time.Minute), Scope: "global", Kind: "health_drop", Observed: 0.5, Baseline: 0.9},
		}},
		{"an old coverage drop is history", []store.Anomaly{
			{TS: now.Add(-12 * time.Hour), Scope: "global", Kind: "coverage_drop", Observed: 0.1, Baseline: 0.9},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := newServerWithAnomalies(t, testToken, fixture(), &fakeAnomalies{rows: tc.rows})
			client, _ := newClient(t)
			login(t, ts, client, testToken)

			resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(body(t, resp), `data-testid="coverage-advisory"`) {
				t.Error("an advisory rendered where it should not")
			}
		})
	}
}

func TestLocalVerdictMutesDownTiles(t *testing.T) {
	t.Parallel()

	ts := newServerWithVerdict(t, testToken, fixture(), &fakeVerdict{res: verdict.Result{
		Verdict: verdict.Local, Detail: "2 of 3 reference targets failing; this is likely your path",
	}})
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	// united.com is DOWN in the fixture; the tile keeps its state and gains the
	// local-path presentation.
	if !strings.Contains(page, `data-testid="local-path"`) || !strings.Contains(page, "your connection") {
		t.Fatalf("a LOCAL verdict did not mute the down tile:\n%s", page)
	}
	if !strings.Contains(page, "local-path") {
		t.Error("the tile does not carry the local-path class")
	}
	// The confirmed state is unchanged.
	if !strings.Contains(page, `data-target="united.com"`) {
		t.Error("the down target disappeared from the board")
	}

	// Without LOCAL the tile is not muted.
	ok := newServerWithVerdict(t, testToken, fixture(), &fakeVerdict{res: verdict.Result{
		Verdict: verdict.OK, Detail: "within the baseline band",
	}})
	client2, _ := newClient(t)
	login(t, ok, client2, testToken)
	resp, err = client2.Get(ok.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body(t, resp), `data-testid="local-path"`) {
		t.Error("an OK verdict muted a down tile")
	}
}
