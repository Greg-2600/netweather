package web_test

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/store"
)

func getWithAccept(t *testing.T, client *http.Client, rawURL, accept string) (*http.Response, string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body(t, resp)
}

func TestLegacyPagesRejectUnsupportedMethods(t *testing.T) {
	t.Parallel()

	ts := newServer(t, "", fixture())
	paths := []string{
		"/board", "/coverage", "/world", "/hubs", "/corridors", "/feeds",
		"/map", "/ihi", "/diagnostics", "/incidents",
	}
	for _, path := range paths {
		t.Run(strings.TrimPrefix(path, "/"), func(t *testing.T) {
			client, _ := newClient(t)
			resp, err := client.PostForm(ts.URL+path, url.Values{}) //nolint:bodyclose // body() closes it
			if err != nil {
				t.Fatal(err)
			}
			page := body(t, resp)
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("%s status = %d, want 405; body=%q", path, resp.StatusCode, page)
			}
			if allow := resp.Header.Get("Allow"); allow != "GET, HEAD" {
				t.Errorf("%s Allow = %q, want GET, HEAD", path, allow)
			}
		})
	}
}

func TestBoardRouteRendersAndRejectsWrites(t *testing.T) {
	t.Parallel()

	ts := newServer(t, "", fixture())
	client, _ := newClient(t)

	resp, page := getWithAccept(t, client, ts.URL+"/board", "application/json") //nolint:bodyclose // body() closes it
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /board status = %d, want 200", resp.StatusCode)
	}
	if got := strings.Count(page, `data-testid="tile"`); got != 3 {
		t.Fatalf("GET /board tiles = %d, want 3", got)
	}

	req, err := http.NewRequest(http.MethodHead, ts.URL+"/board", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = client.Do(req) //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD /board status = %d, want 200", resp.StatusCode)
	}

	resp, err = client.PostForm(ts.URL+"/board", url.Values{}) //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /board status = %d, want 405", resp.StatusCode)
	}

	_, jar := newClient(t)
	client = noRedirectClient(jar)
	resp, _ = getWithAccept(t, client, ts.URL+"/board", "text/html") //nolint:bodyclose // body() closes it
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("browser /board = %d Location %q, want 303 /", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestLogoutRejectsGetAndMissingCSRF(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, page := getWithAccept(t, client, ts.URL+"/logout", "") //nolint:bodyclose // body() closes it
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /logout status = %d, want 405; body=%q", resp.StatusCode, page)
	}
	if allow := resp.Header.Get("Allow"); allow != "POST" {
		t.Errorf("GET /logout Allow = %q, want POST", allow)
	}

	resp, err := client.PostForm(ts.URL+"/logout", url.Values{}) //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("CSRF-less logout status = %d, want 403", resp.StatusCode)
	}
}

func TestPauseRejectsMethodAndMissingControl(t *testing.T) {
	t.Parallel()

	ts := newServer(t, "", fixture())
	client, _ := newClient(t)

	resp, page := getWithAccept(t, client, ts.URL+"/pause", "") //nolint:bodyclose // body() closes it
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /pause status = %d, want 405; body=%q", resp.StatusCode, page)
	}

	resp, err := client.PostForm(ts.URL+"/pause", url.Values{}) //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page = body(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(page, "probe control is not configured") {
		t.Fatalf("POST /pause without control = %d %q, want 503", resp.StatusCode, page)
	}
}

func TestPageSourceReadFailures(t *testing.T) {
	t.Parallel()

	ts := serverWithSource(t, failingSource{err: errors.New("store down")})
	client, _ := newClient(t)
	login(t, newServer(t, testToken, fixture()), client, testToken)

	cases := []struct {
		path  string
		body  string
		error bool
	}{
		{"/board", "The board could not be read", true},
		{"/map", "map unavailable", false},
		{"/hubs", "The hub view could not be read", true},
		{"/targets/2", "The target could not be read", true},
	}
	for _, tc := range cases {
		t.Run(strings.TrimPrefix(tc.path, "/"), func(t *testing.T) {
			resp, page := getWithAccept(t, client, ts.URL+tc.path, "application/json") //nolint:bodyclose // body() closes it
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("%s status = %d, want 503; body=%q", tc.path, resp.StatusCode, page)
			}
			if !strings.Contains(page, tc.body) {
				t.Errorf("%s missing %q:\n%s", tc.path, tc.body, page)
			}
			if tc.error && !strings.Contains(page, `data-testid="error-page"`) {
				t.Errorf("%s did not render the generic error page", tc.path)
			}
		})
	}
}

func TestMapGroupingModes(t *testing.T) {
	t.Parallel()

	since := time.Now().Add(-2 * time.Hour)
	m := board.NewMemory(
		board.Target{
			ID: "1", Name: "Bank", Host: "bank.example", Sector: "financial/banking",
			Country: "US", ASN: 13335, ASName: "CLOUDFLARENET", State: board.StateDown,
			Since: since, ErrorClass: "http_timeout",
		},
		board.Target{
			ID: "2", Name: "Airline", Host: "air.example", Sector: "airline",
			Country: "GB", ASN: 15169, State: board.StateDegraded,
		},
		board.Target{
			ID: "3", Name: "Cloud", Host: "cloud.example", Sector: "cloud",
			State: board.StateBlocked,
		},
		board.Target{
			ID: "4", Name: "Quiet", Host: "quiet.example", State: board.StateUnknown,
		},
		board.Target{
			ID: "5", Name: "Up", Host: "up.example", Sector: "cloud",
			Country: "US", ASN: 13335, ASName: "CLOUDFLARENET", State: board.StateUp,
		},
	)
	ts := newServer(t, "", m)
	client, _ := newClient(t)

	cases := []struct {
		query string
		want  []string
	}{
		{"health", []string{"current: health", "Long-running outages", "Degraded / config errors", "No data yet"}},
		{"country", []string{"current: country", `data-group="US"`, `data-group="GB"`, `data-group="unknown"`}},
		{"asn", []string{"current: asn", `data-group="CLOUDFLARENET"`, `data-group="AS15169"`, `data-group="unknown"`}},
		{"sector", []string{"current: sector", `data-group="financial/banking"`, `data-group="airline"`, `data-group="unknown"`}},
		{"not-a-mode", []string{"current: health"}},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			resp, page := getWithAccept(t, client, ts.URL+"/map?by="+url.QueryEscape(tc.query), "") //nolint:bodyclose // body() closes it
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET /map?by=%s status = %d", tc.query, resp.StatusCode)
			}
			for _, want := range tc.want {
				if !strings.Contains(page, want) {
					t.Errorf("GET /map?by=%s missing %q", tc.query, want)
				}
			}
		})
	}
}

func TestFeedsLimitAndEventTimestamps(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	feeds := &fakeFeedHealth{
		states: []store.FeedState{{
			Feed: "ioda", LastOKAt: now, NextPollAt: now.Add(time.Hour),
		}},
		events: []store.ExternalEvent{{
			Feed: "statuspage", ExternalID: "status.twilio.com:inc-1", Kind: "statuspage",
			Severity: 0.8, Country: "US", StartedAt: now, EndedAt: now.Add(30 * time.Minute),
			ObservedAt: now.Add(time.Minute),
		}},
	}
	ts := newServerWithFeeds(t, "", fixture(), feeds)
	client, _ := newClient(t)

	resp, page := getWithAccept(t, client, ts.URL+"/feeds?limit=201", "") //nolint:bodyclose // body() closes it
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clamped limit status = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{now.Add(time.Hour).Format(time.RFC3339), now.Add(30 * time.Minute).Format(time.RFC3339)} {
		if !strings.Contains(page, want) {
			t.Errorf("feeds page missing timestamp %q", want)
		}
	}

	for _, limit := range []string{"abc", "0", "-5"} {
		t.Run(limit, func(t *testing.T) {
			resp, page := getWithAccept(t, client, ts.URL+"/feeds?limit="+limit, "") //nolint:bodyclose // body() closes it
			if resp.StatusCode != http.StatusBadRequest || !strings.Contains(page, "invalid limit") {
				t.Fatalf("limit %q = %d %q, want 400", limit, resp.StatusCode, page)
			}
		})
	}
}

func TestCoverageProviderFailureAndConcentration(t *testing.T) {
	t.Parallel()

	broken := newServerWithCoverage(t, fixture(), &fakeCoverage{err: errors.New("db down")})
	client, _ := newClient(t)
	resp, page := getWithAccept(t, client, broken.URL+"/coverage", "") //nolint:bodyclose // body() closes it
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(page, "coverage unavailable") {
		t.Fatalf("provider failure = %d %q, want 503", resp.StatusCode, page)
	}

	lastProbe := time.Now().Add(-5 * time.Minute)
	cov := &fakeCoverage{cov: store.Coverage{
		Totals: store.CoverageTotals{Total: 10, Measurable: 8, Unmeasurable: 1},
		Concentration: store.CoverageConcentration{
			ASNCount: 2, CountryCount: 2, TopASN: 13335, TopASNName: "CLOUDFLARENET",
			TopASNShare: 0.5, Top10Share: 0.9, SingleAnchor: 1, ThinSectors: 1,
		},
		Stale: []store.StaleTarget{{ID: "t1", Host: "chase.com", State: "unknown", LastProbe: lastProbe}},
	}}
	ts := newServerWithCoverage(t, fixture(), cov)
	resp, page = getWithAccept(t, client, ts.URL+"/coverage", "") //nolint:bodyclose // body() closes it
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("coverage status = %d", resp.StatusCode)
	}
	for _, want := range []string{"Top-ASN share", "AS13335 CLOUDFLARENET", "chase.com", "5m"} {
		if !strings.Contains(page, want) {
			t.Errorf("coverage page missing %q", want)
		}
	}
	if strings.Contains(page, ">never<") {
		t.Error("a probed stale target was shown as never probed")
	}
}

func TestIncidentsListFailureAndMethod(t *testing.T) {
	t.Parallel()

	ts := newServerWithIncidents(t, "", fixture(), &fakeIncidentHistory{err: errors.New("db down")})
	client, _ := newClient(t)

	resp, page := getWithAccept(t, client, ts.URL+"/incidents", "") //nolint:bodyclose // body() closes it
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(page, "incident log unavailable") {
		t.Fatalf("incident list failure = %d %q, want 503", resp.StatusCode, page)
	}

	resp, err := client.PostForm(ts.URL+"/incidents", url.Values{}) //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /incidents status = %d, want 405", resp.StatusCode)
	}
}

func TestIncidentDetailErrorPaths(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		server func(t *testing.T) *httptest.Server
		path   string
		status int
		body   string
	}{
		{
			name: "generic history error",
			server: func(t *testing.T) *httptest.Server {
				return newServerWithIncidents(t, "", fixture(), &fakeIncidentHistory{err: errors.New("db down")})
			},
			path: "/incidents/inc-open", status: http.StatusServiceUnavailable, body: "incident unavailable",
		},
		{
			name: "no incidents configured",
			server: func(t *testing.T) *httptest.Server {
				return newServer(t, "", fixture())
			},
			path: "/incidents/inc-open", status: http.StatusServiceUnavailable, body: "incident log is not configured",
		},
		{
			name: "empty id",
			server: func(t *testing.T) *httptest.Server {
				return newServerWithIncidents(t, "", fixture(), incidentsFixture())
			},
			path: "/incidents/", status: http.StatusNotFound, body: "no such incident",
		},
		{
			name: "id with slash",
			server: func(t *testing.T) *httptest.Server {
				return newServerWithIncidents(t, "", fixture(), incidentsFixture())
			},
			path: "/incidents/a/b", status: http.StatusNotFound, body: "no such incident",
		},
		{
			name: "archive read failure",
			server: func(t *testing.T) *httptest.Server {
				return newServerWithIncidentProbes(t, "", fixture(), incidentsFixture(),
					&fakeIncidentProbes{err: errors.New("db down")})
			},
			path: "/incidents/inc-open", status: http.StatusOK, body: "Archived probes are temporarily unavailable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := tc.server(t)
			client, _ := newClient(t)
			resp, page := getWithAccept(t, client, ts.URL+tc.path, "") //nolint:bodyclose // body() closes it
			if resp.StatusCode != tc.status || !strings.Contains(page, tc.body) {
				t.Fatalf("%s = %d %q, want %d containing %q", tc.path, resp.StatusCode, page, tc.status, tc.body)
			}
		})
	}
}

func TestIncidentDetailCorroborationPlaces(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 11, 17, 11, 30, 0, 0, time.UTC)
	corr := &fakeCorroboration{events: []store.ExternalEvent{
		{Feed: "ris-live", Kind: "bgp_withdrawal", Severity: 1, ASN: 7018, StartedAt: at, EndedAt: at.Add(time.Hour)},
		{Feed: "ioda", Kind: "outage", Severity: 0.4, StartedAt: at},
	}}
	ts := newServerWithCorroboration(t, "", fixture(), incidentsFixture(), corr)
	client, _ := newClient(t)

	resp, page := getWithAccept(t, client, ts.URL+"/incidents/inc-open", "") //nolint:bodyclose // body() closes it
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for _, want := range []string{"AS7018", `<td class="mono">-</td>`, at.Add(time.Hour).Format("2006-01-02 15:04")} {
		if !strings.Contains(page, want) {
			t.Errorf("corroboration page missing %q", want)
		}
	}
}

func TestIncidentAnnotationErrorPaths(t *testing.T) {
	t.Parallel()

	history := incidentsFixture()
	ts := newServerWithAnnotator(t, testToken, fixture(), history, &fakeAnnotator{history: history})
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, page := getWithAccept(t, client, ts.URL+"/incidents/inc-open/annotate", "") //nolint:bodyclose // body() closes it
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET annotate = %d %q, want 405", resp.StatusCode, page)
	}

	_, page = getWithAccept(t, client, ts.URL+"/incidents/inc-open", "") //nolint:bodyclose // body() closes it
	csrf := csrfFrom(t, page)
	post := func(path, text string) (*http.Response, string) {
		resp, err := client.PostForm(ts.URL+path, url.Values{ //nolint:bodyclose // body() closes it
			"csrf": {csrf}, "annotation": {text},
		})
		if err != nil {
			t.Fatal(err)
		}
		return resp, body(t, resp)
	}

	for _, path := range []string{"/incidents/a/b/annotate"} {
		resp, page = post(path, "ground truth") //nolint:bodyclose // post closes it
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s = %d %q, want 404", path, resp.StatusCode, page)
		}
	}

	resp, page = post("/incidents/inc-open/annotate", strings.Repeat("x", 501)) //nolint:bodyclose // post closes it
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("long annotation = %d %q, want 400", resp.StatusCode, page)
	}

	bare := newServerWithAnnotator(t, testToken, fixture(), incidentsFixture(), nil)
	client2, _ := newClient(t)
	login(t, bare, client2, testToken)
	resp, err := client2.PostForm(bare.URL+"/incidents/inc-open/annotate", url.Values{"annotation": {"x"}}) //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page = body(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(page, "annotation is not configured") {
		t.Fatalf("unconfigured annotate = %d %q, want 503", resp.StatusCode, page)
	}

	for _, tc := range []struct {
		name   string
		err    error
		status int
		body   string
	}{
		{"missing incident", sql.ErrNoRows, http.StatusNotFound, "no such incident"},
		{"store failure", errors.New("db down"), http.StatusServiceUnavailable, "annotation unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := incidentsFixture()
			annotator := &fakeAnnotator{history: h, err: tc.err}
			ts := newServerWithAnnotator(t, testToken, fixture(), h, annotator)
			client, _ := newClient(t)
			login(t, ts, client, testToken)
			_, page := getWithAccept(t, client, ts.URL+"/incidents/inc-open", "") //nolint:bodyclose // body() closes it
			csrf := csrfFrom(t, page)
			resp, err := client.PostForm(ts.URL+"/incidents/inc-open/annotate", url.Values{ //nolint:bodyclose // body() closes it
				"csrf": {csrf}, "annotation": {"ground truth"},
			})
			if err != nil {
				t.Fatal(err)
			}
			page = body(t, resp)
			if resp.StatusCode != tc.status || !strings.Contains(page, tc.body) {
				t.Fatalf("annotate = %d %q, want %d containing %q", resp.StatusCode, page, tc.status, tc.body)
			}
		})
	}
}
