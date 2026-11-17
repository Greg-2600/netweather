//go:build uitest

// Package uitest drives the real UI in a real browser. It is behind the uitest
// build tag so `go test ./...` stays fast and browser-free.
package uitest_test

import (
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

	browser, err = pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(true),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "chromium launch failed:", err)
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
}

// newHarness starts the real handler on a random port and opens a page with a
// console-error collector attached. Screenshots land in testdata/out on failure.
func newHarness(t *testing.T) *harness {
	t.Helper()

	m := fixture()
	h, err := web.New(web.Deps{
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

	return &harness{page: page, board: m, baseURL: ts.URL}
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
	if err := expect.Locator(tile.Locator(".state")).ToHaveText("Up"); err != nil {
		t.Fatalf("state label did not update: %v", err)
	}

	// The summary chip follows the tile, not the initial render.
	upChip := h.page.Locator(`[data-count-state="up"] b`)
	if err := expect.Locator(upChip).ToHaveText("2"); err != nil {
		t.Fatalf("up count did not update: %v", err)
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

	if err := h.page.Locator("text=Back to the board").Click(); err != nil {
		t.Fatal(err)
	}
	if err := h.page.WaitForURL(h.baseURL + "/"); err != nil {
		t.Fatal(err)
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
