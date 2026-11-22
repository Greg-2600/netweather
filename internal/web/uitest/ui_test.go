//go:build uitest

// Package uitest drives the real UI in a real browser. It is behind the uitest
// build tag so `go test ./...` stays fast and browser-free.
package uitest_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/logging"
	"github.com/Greg-2600/netweather/internal/store"
	"github.com/Greg-2600/netweather/internal/web"
	"github.com/mxschmitt/playwright-go"
)

const testToken = "test-token-0123456789"

var (
	pw      *playwright.Playwright
	browser playwright.Browser
)

func TestMain(m *testing.M) {
	if err := playwright.Install(); err != nil {
		fmt.Fprintln(os.Stderr, "playwright install failed:", err)
		os.Exit(1)
	}

	var err error
	pw, err = playwright.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "playwright run failed:", err)
		os.Exit(1)
	}

	// Chromium runs on every pull request; the weekly job sets this to firefox
	// and webkit as well (QUALITY.md §5.8). One browser per run keeps the gate
	// honest: a WebKit failure must name WebKit.
	name := strings.ToLower(strings.TrimSpace(os.Getenv("NETWEATHER_UI_BROWSER")))
	if name == "" {
		name = "chromium"
	}
	var browserType playwright.BrowserType
	switch name {
	case "chromium":
		browserType = pw.Chromium
	case "firefox":
		browserType = pw.Firefox
	case "webkit":
		browserType = pw.WebKit
	default:
		fmt.Fprintf(os.Stderr, "unknown NETWEATHER_UI_BROWSER %q (want chromium, firefox, or webkit)\n", name)
		_ = pw.Stop()
		os.Exit(1)
	}

	browser, err = browserType.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(true),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s launch failed: %v\n", name, err)
		_ = pw.Stop()
		os.Exit(1)
	}

	code := m.Run()

	_ = browser.Close()
	_ = pw.Stop()
	os.Exit(code)
}

func fixture() *board.Memory {
	return board.NewMemory(
		board.Target{ID: "1", Name: "Chase", Host: "chase.com", Sector: "financial/banking", State: board.StateUp},
		board.Target{ID: "2", Name: "United", Host: "united.com", Sector: "airline", State: board.StateDown},
		board.Target{ID: "3", Name: "Cloudflare", Host: "cloudflare.com", Sector: "cloud", State: board.StateBlocked},
	)
}

type harness struct {
	page    playwright.Page
	board   *board.Memory
	baseURL string
	logs    *fakeLogs
}

// fakeLogs is a controllable log ring: the UI test pushes one record and
// asserts what the browser does with it. It holds no history, because the
// board only cares about what arrives live.
type fakeLogs struct {
	ch chan logging.Record
}

func (f *fakeLogs) LogRecords(slog.Level, string, int) []logging.Record { return nil }
func (f *fakeLogs) Diagnostics() logging.Diagnostics                    { return logging.Diagnostics{} }
func (f *fakeLogs) ActiveScopes() []logging.Scope                       { return nil }
func (f *fakeLogs) Subscribe() (<-chan logging.Record, func())          { return f.ch, func() {} }

// fakeCorridors is the smallest real source the corridor page needs. It also
// supplies configured endpoints so the UI test can assert the hover text.
type fakeCorridors struct {
	statuses  []store.CorridorStatus
	series    map[string][]store.CorridorSample
	endpoints map[string][]string
}

func (f *fakeCorridors) CorridorStatus(context.Context) ([]store.CorridorStatus, error) {
	return f.statuses, nil
}

func (f *fakeCorridors) CorridorSeries(context.Context, time.Time) (map[string][]store.CorridorSample, error) {
	return f.series, nil
}

func (f *fakeCorridors) CorridorEndpoints(context.Context) map[string][]string {
	return f.endpoints
}

// newHarness starts the real handler on a random port and opens a page with a
// console-error collector attached. Screenshots land in testdata/out on failure.
func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWithBoard(t, fixture())
}

// newHarnessWithBoard is the same harness with an explicit board, for views that
// need a fixture the shared three-target board cannot supply (such as /hubs).
func newHarnessWithBoard(t *testing.T, m *board.Memory) *harness {
	t.Helper()
	return newHarnessWithDeps(t, m, nil)
}

// newHarnessWithDeps is the same harness with optional page dependencies, for
// views the shared three-target board cannot supply (such as /corridors).
func newHarnessWithDeps(t *testing.T, m *board.Memory, configure func(*web.Deps)) *harness {
	t.Helper()

	logs := &fakeLogs{ch: make(chan logging.Record, 8)}
	deps := web.Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:    m,
		Watcher:   m,
		Logs:      logs,
		Token:     testToken,
		Version:   "test",
		StartedAt: time.Now(),
	}
	if configure != nil {
		configure(&deps)
	}
	h, err := web.New(deps)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}

	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	ctx, err := browser.NewContext(playwright.BrowserNewContextOptions{
		Viewport: &playwright.Size{Width: 1280, Height: 800},
	})
	if err != nil {
		t.Fatalf("new context: %v", err)
	}
	t.Cleanup(func() { _ = ctx.Close() })

	page, err := ctx.NewPage()
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	page.SetDefaultTimeout(5000)

	var mu sync.Mutex
	var consoleErrors []string
	page.OnConsole(func(msg playwright.ConsoleMessage) {
		if msg.Type() == "error" {
			mu.Lock()
			consoleErrors = append(consoleErrors, msg.Text())
			mu.Unlock()
		}
	})
	page.OnPageError(func(err error) {
		mu.Lock()
		consoleErrors = append(consoleErrors, "pageerror: "+err.Error())
		mu.Unlock()
	})

	t.Cleanup(func() {
		if t.Failed() {
			dir := filepath.Join("..", "..", "..", "testdata", "out")
			if err := os.MkdirAll(dir, 0o755); err == nil {
				if shot, err := page.Screenshot(); err == nil {
					_ = os.WriteFile(filepath.Join(dir, t.Name()+".png"), shot, 0o644)
				}
			}
		}
		mu.Lock()
		defer mu.Unlock()
		for _, e := range consoleErrors {
			// A deliberate 401 on the login form is asserted by the error box;
			// the browser logs every non-2xx resource as a console error.
			if strings.Contains(e, "favicon") || strings.Contains(e, "status of 401") {
				continue
			}
			t.Errorf("browser console error: %s", e)
		}
	})

	return &harness{page: page, board: m, baseURL: ts.URL, logs: logs}
}

func (h *harness) login(t *testing.T) {
	t.Helper()

	if _, err := h.page.Goto(h.baseURL + "/login"); err != nil {
		t.Fatalf("goto login: %v", err)
	}
	if err := h.page.Locator(`[data-testid="login-token"]`).Fill(testToken); err != nil {
		t.Fatal(err)
	}
	if err := h.page.Locator(`[data-testid="login-submit"]`).Click(); err != nil {
		t.Fatal(err)
	}
	if err := h.page.WaitForURL(h.baseURL + "/"); err != nil {
		t.Fatalf("did not land on the board: %v", err)
	}
}

func TestLoginRejectsWrongTokenThenAccepts(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	if _, err := h.page.Goto(h.baseURL + "/login"); err != nil {
		t.Fatal(err)
	}
	if err := h.page.Locator(`[data-testid="login-token"]`).Fill("wrong-token-000000"); err != nil {
		t.Fatal(err)
	}
	if err := h.page.Locator(`[data-testid="login-submit"]`).Click(); err != nil {
		t.Fatal(err)
	}

	expect := playwright.NewPlaywrightAssertions(5000)
	errBox := h.page.Locator(`[data-testid="login-error"]`)
	if err := expect.Locator(errBox).ToBeVisible(); err != nil {
		t.Fatalf("no error shown for a wrong token: %v", err)
	}
	if err := expect.Locator(errBox).ToContainText("not correct"); err != nil {
		t.Fatal(err)
	}

	// The form must still work after a failure.
	if err := h.page.Locator(`[data-testid="login-token"]`).Fill(testToken); err != nil {
		t.Fatal(err)
	}
	if err := h.page.Locator(`[data-testid="login-submit"]`).Click(); err != nil {
		t.Fatal(err)
	}
	if err := h.page.WaitForURL(h.baseURL + "/"); err != nil {
		t.Fatalf("second attempt did not log in: %v", err)
	}
}

func TestBoardRendersStatesAccessibly(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.login(t)

	expect := playwright.NewPlaywrightAssertions(5000)
	tiles := h.page.Locator(`[data-testid="tile"]`)

	if err := expect.Locator(tiles).ToHaveCount(3); err != nil {
		t.Fatalf("tile count: %v", err)
	}

	down := h.page.Locator(`[data-target="united.com"]`)
	if err := expect.Locator(down).ToHaveAttribute("data-state", "down"); err != nil {
		t.Fatal(err)
	}
	// Colour is not the only carrier: the accessible name includes the state.
	if err := expect.Locator(down).ToHaveAccessibleName("United (united.com): Down"); err != nil {
		t.Fatalf("accessible name: %v", err)
	}

	// Every state must be distinguishable without colour.
	for _, sel := range []string{
		`[data-target="united.com"] .glyph`,
		`[data-target="cloudflare.com"] .glyph`,
		`[data-target="chase.com"] .glyph`,
	} {
		text, err := h.page.Locator(sel).TextContent()
		if err != nil || strings.TrimSpace(text) == "" {
			t.Errorf("%s has no glyph", sel)
		}
	}
}

func TestLiveUpdateArrivesOverSSE(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.login(t)

	expect := playwright.NewPlaywrightAssertions(5000)
	tile := h.page.Locator(`[data-target="united.com"]`)

	if err := expect.Locator(tile).ToHaveAttribute("data-state", "down"); err != nil {
		t.Fatal(err)
	}
	if err := expect.Locator(h.page.Locator(`[data-testid="connection"]`)).ToHaveAttribute("data-state", "live"); err != nil {
		t.Fatalf("SSE did not connect: %v", err)
	}

	if _, changed := h.board.Set("2", board.StateUp); !changed {
		t.Fatal("fixture did not change")
	}

	if err := expect.Locator(tile).ToHaveAttribute("data-state", "up"); err != nil {
		t.Fatalf("tile did not update from SSE: %v", err)
	}
	if err := expect.Locator(tile.Locator(".state-text")).ToHaveText("Up"); err != nil {
		t.Fatalf("state label did not update: %v", err)
	}
	// The live update must keep the non-colour marker, not just the label:
	// rewriting the whole .state element used to delete it (DESIGN.md 10.7).
	if err := expect.Locator(tile.Locator(".glyph")).ToHaveText("●"); err != nil {
		t.Fatalf("state glyph did not survive the update: %v", err)
	}

	// The summary chip follows the tile, not the initial render.
	upChip := h.page.Locator(`[data-count-state="up"] b`)
	if err := expect.Locator(upChip).ToHaveText("2"); err != nil {
		t.Fatalf("up count did not update: %v", err)
	}
}

// TestWarnLogLineAppearsOnTheBoard proves the SSE log stream has a consumer:
// a WARN record pushed by the server shows up as a quiet, accessible notice,
// and the page does not need a reload to show it.
func TestWarnLogLineAppearsOnTheBoard(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.login(t)

	expect := playwright.NewPlaywrightAssertions(5000)
	if err := expect.Locator(h.page.Locator(`[data-testid="connection"]`)).ToHaveAttribute("data-state", "live"); err != nil {
		t.Fatalf("SSE did not connect: %v", err)
	}

	h.logs.ch <- logging.Record{
		Time: time.Now(), Level: slog.LevelWarn, LevelName: "WARN",
		Message: "self-check failed",
	}

	line := h.page.Locator(`[data-testid="log-line"]`)
	text, err := line.TextContent()
	if err != nil {
		t.Fatalf("log notice did not appear: %v", err)
	}
	if !strings.Contains(text, "WARN") || !strings.Contains(text, "self-check failed") {
		t.Fatalf("log notice text = %q", text)
	}
	if err := expect.Locator(h.page.Locator(`[data-testid="log-notice"]`)).ToHaveAttribute("role", "status"); err != nil {
		t.Fatalf("the notice is not announced: %v", err)
	}
}

func TestKeyboardReachesTiles(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.login(t)

	if err := h.page.Locator("body").Click(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 8; i++ {
		if err := h.page.Keyboard().Press("Tab"); err != nil {
			t.Fatal(err)
		}
		id, err := h.page.Evaluate(`() => document.activeElement?.dataset?.testid || ""`)
		if err != nil {
			t.Fatal(err)
		}
		if s, _ := id.(string); s == "tile" {
			return
		}
	}
	t.Fatal("tabbing never reached a tile")
}

func TestTargetDetailNavigation(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.login(t)

	expect := playwright.NewPlaywrightAssertions(5000)

	if err := h.page.Locator(`[data-target="united.com"]`).Click(); err != nil {
		t.Fatal(err)
	}
	if err := h.page.WaitForURL(h.baseURL + "/targets/2"); err != nil {
		t.Fatalf("tile did not navigate: %v", err)
	}
	if err := expect.Locator(h.page.Locator(`[data-testid="target-state"]`)).ToHaveText("Down"); err != nil {
		t.Fatal(err)
	}

	// The drawer is a working page, not the narrow login card. At the harness
	// width it should use most of the viewport, so a regression to the auth
	// card's 26rem cap is visible here.
	widthValue, err := h.page.Locator(`[data-testid="target-detail"]`).Evaluate("el => el.getBoundingClientRect().width", nil)
	if err != nil {
		t.Fatal(err)
	}
	width, ok := toFloat(widthValue)
	if !ok || width < 700 {
		t.Fatalf("target detail width = %v, want at least 700px", widthValue)
	}

	if err := h.page.Locator("text=Back to the board").Click(); err != nil {
		t.Fatal(err)
	}
	if err := h.page.WaitForURL(h.baseURL + "/"); err != nil {
		t.Fatal(err)
	}
}

func TestCorridorNameAndHover(t *testing.T) {
	t.Parallel()

	corridors := &fakeCorridors{
		statuses: []store.CorridorStatus{{
			Corridor: store.Corridor{ID: "nyc-lon", From: "NYC", To: "LON"},
		}},
		endpoints: map[string][]string{"nyc-lon": {"www.nytimes.com:443", "www.bbc.co.uk:443"}},
	}
	h := newHarnessWithDeps(t, fixture(), func(deps *web.Deps) {
		deps.Corridors = corridors
		deps.CorridorEndpoints = corridors
	})
	h.login(t)

	if _, err := h.page.Goto(h.baseURL + "/corridors"); err != nil {
		t.Fatal(err)
	}
	ordered, err := h.page.Evaluate(`() => {
		const p = document.querySelector('[data-testid="corridor-projection"]');
		const t = document.querySelector('[data-testid="corridors"]');
		return !!p && !!t && (p.compareDocumentPosition(t) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := ordered.(bool); !ok {
		t.Fatal("corridor projection must render above the table")
	}
	expect := playwright.NewPlaywrightAssertions(5000)
	row := h.page.Locator(`[data-testid="corridor-row"][data-corridor="nyc-lon"]`)
	if err := expect.Locator(row.Locator(".corridor-name")).ToHaveText("NYC → LON"); err != nil {
		t.Fatal(err)
	}
	if err := expect.Locator(row.Locator(".corridor-id")).ToHaveText("nyc-lon"); err != nil {
		t.Fatal(err)
	}
	if err := expect.Locator(row.Locator(".corridor-path")).ToHaveText("www.nytimes.com → www.bbc.co.uk"); err != nil {
		t.Fatal(err)
	}

	for _, cell := range []struct{ selector, want string }{
		{":nth-child(1)", "www.nytimes.com:443 and www.bbc.co.uk:443"},
		{":nth-child(2)", "www.nytimes.com:443 → www.bbc.co.uk:443"},
	} {
		value, err := row.Locator("td"+cell.selector).Evaluate("el => el.getAttribute('title')", nil)
		if err != nil {
			t.Fatal(err)
		}
		title, ok := value.(string)
		if !ok || !strings.Contains(title, cell.want) {
			t.Fatalf("hover title = %v, want it to contain %q", value, cell.want)
		}
	}

	// The projection places only the operator labels and says plainly that the
	// straight line is not a measured or physical path.
	marker := h.page.Locator(`[data-testid="corridor-map-marker"][data-code="NYC"]`)
	if err := expect.Locator(marker).ToBeVisible(); err != nil {
		t.Fatal(err)
	}
	value, err := marker.Evaluate("el => el.getAttribute('title')", nil)
	if err != nil {
		t.Fatal(err)
	}
	title, ok := value.(string)
	if !ok || !strings.Contains(title, "Approximate city-centre coordinate; endpoint hosting location is not measured.") {
		t.Fatalf("projection marker title = %v", value)
	}
	value, err = h.page.Locator(`[data-testid="corridor-map-line"][data-corridor="nyc-lon"]`).Evaluate("el => el.getAttribute('aria-label')", nil)
	if err != nil {
		t.Fatal(err)
	}
	lineTitle, ok := value.(string)
	if !ok || !strings.Contains(lineTitle, "this straight line is not the physical or undersea-cable path") {
		t.Fatalf("projection line title = %v", value)
	}
}

func TestDiagnosticsLinkAndPage(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.login(t)

	expect := playwright.NewPlaywrightAssertions(5000)

	if err := h.page.Locator(`[data-testid="diagnostics-link"] a`).Click(); err != nil {
		t.Fatalf("the footer link did not click: %v", err)
	}
	if err := h.page.WaitForURL(h.baseURL + "/diagnostics"); err != nil {
		t.Fatalf("the link did not reach /diagnostics: %v", err)
	}
	if err := expect.Locator(h.page.Locator(`[data-testid="diagnostics-stats"]`)).ToBeVisible(); err != nil {
		t.Fatalf("the setup table did not render: %v", err)
	}
	if err := expect.Locator(h.page.Locator("h1")).ToHaveText("Diagnostics"); err != nil {
		t.Fatalf("heading = %v", err)
	}
	// The harness wires an empty ring, so the page must state that instead of
	// rendering an empty records table.
	if err := expect.Locator(h.page.Locator(`[data-testid="no-diagnostics-records"]`)).ToBeVisible(); err != nil {
		t.Fatalf("an empty ring was not stated: %v", err)
	}
}

func TestMobileViewportDoesNotOverflow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	if err := h.page.SetViewportSize(390, 844); err != nil {
		t.Fatal(err)
	}
	h.login(t)

	overflow, err := h.page.Evaluate(`() => document.documentElement.scrollWidth - window.innerWidth`)
	if err != nil {
		t.Fatal(err)
	}
	if px, ok := toFloat(overflow); !ok || px > 1 {
		t.Fatalf("horizontal overflow at 390px: %v", overflow)
	}
}

// toFloat normalises the several numeric types a JS evaluation can decode to.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

// TestStateShapesDifferByMoreThanColour is the browser-side check for
// DESIGN.md §10.7: every state changes the ring style and, in compact density,
// the fill pattern, so a reader who cannot tell red from green still sees a
// different tile.
func TestStateShapesDifferByMoreThanColour(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.login(t)

	raw, err := h.page.Evaluate(`() => {
		const pick = (host, prop) => {
			const el = document.querySelector('[data-target="' + host + '"]');
			return el ? getComputedStyle(el).getPropertyValue(prop) : '';
		};
		return {
			upStyle: pick('chase.com', 'border-top-style'),
			upWidth: pick('chase.com', 'border-top-width'),
			downStyle: pick('united.com', 'border-top-style'),
			downWidth: pick('united.com', 'border-top-width'),
			blockedStyle: pick('cloudflare.com', 'border-top-style'),
			blockedWidth: pick('cloudflare.com', 'border-top-width'),
			downPattern: pick('united.com', 'background-image'),
		};
	}`)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("evaluation returned %T", raw)
	}
	str := func(key string) string {
		value, _ := got[key].(string)
		return value
	}

	if str("upStyle") != "solid" || str("upWidth") != "1px" {
		t.Errorf("up ring = %q/%q, want solid/1px", str("upStyle"), str("upWidth"))
	}
	if str("downStyle") != "solid" || str("downWidth") != "2px" {
		t.Errorf("down ring = %q/%q, want solid/2px", str("downStyle"), str("downWidth"))
	}
	if str("blockedStyle") != "dotted" {
		t.Errorf("blocked ring = %q, want dotted", str("blockedStyle"))
	}
	if str("upStyle") == str("blockedStyle") || str("upWidth") == str("downWidth") {
		t.Error("states differ only by colour")
	}
	if !strings.Contains(str("downPattern"), "linear-gradient") {
		t.Errorf("down tile has no compact pattern: %q", str("downPattern"))
	}
}

func TestFeedsPageRenders(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.login(t)

	expect := playwright.NewPlaywrightAssertions(5000)

	if _, err := h.page.Goto(h.baseURL + "/feeds"); err != nil {
		t.Fatalf("goto /feeds: %v", err)
	}
	if err := expect.Locator(h.page.Locator("h1")).ToHaveText("Feeds"); err != nil {
		t.Fatalf("heading = %v", err)
	}
	if err := expect.Locator(h.page.Locator(`[data-testid="feeds-summary"]`)).ToBeVisible(); err != nil {
		t.Fatalf("summary not visible: %v", err)
	}
	if err := expect.Locator(h.page.Locator(`[data-testid="no-feeds"]`)).ToBeVisible(); err != nil {
		t.Fatalf("no-feeds note not visible: %v", err)
	}
	if err := expect.Locator(h.page.Locator(`[data-testid="no-events"]`)).ToBeVisible(); err != nil {
		t.Fatalf("no-events note not visible: %v", err)
	}
}

func TestMapPageRenders(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.login(t)

	expect := playwright.NewPlaywrightAssertions(5000)

	if _, err := h.page.Goto(h.baseURL + "/map"); err != nil {
		t.Fatalf("goto /map: %v", err)
	}
	if err := expect.Locator(h.page.Locator("h1")).ToHaveText("Map"); err != nil {
		t.Fatalf("heading = %v", err)
	}
	if err := expect.Locator(h.page.Locator(`[data-testid="map-summary"]`)).ToBeVisible(); err != nil {
		t.Fatalf("summary not visible: %v", err)
	}
	if err := expect.Locator(h.page.Locator(`[data-testid="treemap"]`)).ToBeVisible(); err != nil {
		t.Fatalf("treemap not visible: %v", err)
	}
}

func TestHubsPageRenders(t *testing.T) {
	t.Parallel()
	h := newHarnessWithBoard(t, board.NewMemory(
		board.Target{
			ID: "hub-1", Name: "Dallas, TX hub — DFW airport", Host: "dfw.example",
			Sector: "us/hub", State: board.StateDown, ErrorClass: "http_timeout",
		},
		board.Target{
			ID: "hub-2", Name: "Boston, MA hub — city portal", Host: "bos.example",
			Sector: "us/hub", State: board.StateUp,
		},
	))
	h.login(t)

	expect := playwright.NewPlaywrightAssertions(5000)

	if _, err := h.page.Goto(h.baseURL + "/hubs"); err != nil {
		t.Fatalf("goto /hubs: %v", err)
	}
	if err := expect.Locator(h.page.Locator("h1")).ToHaveText("US hubs"); err != nil {
		t.Fatalf("heading = %v", err)
	}
	if err := expect.Locator(h.page.Locator(`[data-testid="hubs-summary"]`)).ToBeVisible(); err != nil {
		t.Fatalf("summary not visible: %v", err)
	}
	if err := expect.Locator(h.page.Locator(`[data-testid="treemap"]`)).ToBeVisible(); err != nil {
		t.Fatalf("hub treemap not visible: %v", err)
	}
	if err := expect.Locator(h.page.Locator(`[data-testid="hub"]`)).ToHaveCount(2); err != nil {
		t.Fatalf("hub city count: %v", err)
	}
	if err := expect.Locator(h.page.Locator(`[data-testid="map-tile"]`)).ToHaveCount(2); err != nil {
		t.Fatalf("hub endpoint count: %v", err)
	}
	if err := expect.Locator(h.page.Locator(`[data-testid="hub"]`).First()).
		ToHaveAttribute("data-city", "Dallas"); err != nil {
		t.Fatalf("worst hub not first: %v", err)
	}
}

func TestWorldProjectionRenders(t *testing.T) {
	t.Parallel()
	h := newHarnessWithBoard(t, board.NewMemory(
		board.Target{ID: "1", Name: "Chase", Host: "chase.com", Country: "US", State: board.StateDown},
		board.Target{ID: "2", Name: "Deutsche", Host: "de.example", Country: "DE", State: board.StateUp},
		board.Target{
			ID: "3", Name: "Dallas, TX hub — DFW airport", Host: "dfw.example",
			Country: "US", Sector: "us/hub", State: board.StateDown,
		},
	))
	h.login(t)

	expect := playwright.NewPlaywrightAssertions(5000)

	if _, err := h.page.Goto(h.baseURL + "/world"); err != nil {
		t.Fatalf("goto /world: %v", err)
	}
	if err := expect.Locator(h.page.Locator(`[data-testid="world-projection"]`)).ToBeVisible(); err != nil {
		t.Fatalf("projection section not visible: %v", err)
	}
	ordered, err := h.page.Evaluate(`() => {
		const p = document.querySelector('[data-testid="world-projection"]');
		const g = document.querySelector('[data-testid="world"]');
		return !!p && !!g && (p.compareDocumentPosition(g) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := ordered.(bool); !ok {
		t.Fatal("world projection must render above the country grid")
	}
	if err := expect.Locator(h.page.Locator(`[data-testid="world-map-country"]`)).ToHaveCount(2); err != nil {
		t.Fatalf("country marker count: %v", err)
	}
	if err := expect.Locator(h.page.Locator(`[data-testid="world-map-hub"]`)).ToHaveCount(1); err != nil {
		t.Fatalf("hub marker count: %v", err)
	}
	if err := expect.Locator(h.page.Locator(`[data-testid="world-map-hub"]`)).
		ToHaveAttribute("data-hub", "Dallas, TX"); err != nil {
		t.Fatalf("hub marker label: %v", err)
	}
	if err := expect.Locator(h.page.Locator(`[data-testid="world-map-hub"]`)).
		ToHaveAttribute("title", "Dallas, TX hub label anchor — 1 endpoint(s), worst Down. Approximate city-centre coordinate; endpoint hosting location is not measured."); err != nil {
		t.Fatalf("hub marker title should state the caveat: %v", err)
	}
}
