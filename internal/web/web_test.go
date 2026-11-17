package web_test

import (
	"bufio"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"log/slog"

	"github.com/Greg-2600/netweather/internal/board"
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
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func login(t *testing.T, ts *httptest.Server, client *http.Client, token string) {
	t.Helper()

	resp, err := client.Get(ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	csrf := csrfFrom(t, page)

	resp, err = client.PostForm(ts.URL+"/login", url.Values{"token": {token}, "csrf": {csrf}})
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

	resp, err := http.Get(ts.URL + "/robots.txt")
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
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestLoginRejectsWrongToken(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	client, jar := newClient(t)

	resp, err := client.Get(ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	csrf := csrfFrom(t, body(t, resp))

	resp, err = client.PostForm(ts.URL+"/login", url.Values{"token": {"wrong-token-000000"}, "csrf": {csrf}})
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

func TestLoginAndBoardRender(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/")
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
}

func TestBoardEscapesTargetNames(t *testing.T) {
	t.Parallel()

	m := board.NewMemory(board.Target{
		ID: "x", Name: `<script>alert(1)</script>`, Host: "evil.example", State: board.StateUp,
	})
	ts := newServer(t, testToken, m)
	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/")
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

	resp, err := client.Get(ts.URL + "/targets/2")
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

	resp, err = client.Get(ts.URL + "/targets/nope")
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
		resp, err := http.Get(ts.URL + path)
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
	defer resp.Body.Close()

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

	waitForLine(t, lines, "retry:", 2*time.Second)

	m.Set("2", board.StateUp)

	waitForLine(t, lines, `"state":"up"`, 3*time.Second)
}

func TestSSERequiresSession(t *testing.T) {
	t.Parallel()

	ts := newServer(t, testToken, fixture())
	_, jar := newClient(t)
	client := noRedirectClient(jar)

	resp, err := client.Get(ts.URL + "/events")
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

	resp, err := http.Get(ts.URL + "/")
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

	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	csrf := csrfFrom(t, body(t, resp))

	resp, err = client.PostForm(ts.URL+"/logout", url.Values{"csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)
	// The 303 to /login is followed, so success is ending on the login page.
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/login" {
		t.Fatalf("logout ended at %s with status %d, want /login", resp.Request.URL.Path, resp.StatusCode)
	}

	noRedirect := noRedirectClient(jar)
	resp, err = noRedirect.Get(ts.URL + "/")
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
