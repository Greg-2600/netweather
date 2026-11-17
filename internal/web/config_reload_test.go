package web_test

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/web"
)

type fakeConfigReload struct {
	at     time.Time
	detail string
	failed bool
}

func (f *fakeConfigReload) LastReloadFailure() (time.Time, string, bool) {
	return f.at, f.detail, f.failed
}

func serverWithConfigReload(t *testing.T, reload web.ConfigReloadStatus) *httptest.Server {
	t.Helper()

	m := fixture()
	h, err := web.New(web.Deps{
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:       m,
		Watcher:      m,
		ConfigReload: reload,
		Token:        testToken,
		Version:      "test",
		StartedAt:    time.Now(),
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

func TestBoardShowsARejectedConfigReload(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	page := drawerPage(t, serverWithConfigReload(t, &fakeConfigReload{
		at:     at,
		detail: `config: targets.yaml: probe "quik" is not supported`,
		failed: true,
	}), "/")

	for _, want := range []string{
		`data-testid="config-reload"`,
		"rejected at 2026-11-17 12:00 UTC",
		"previous targets are still live",
		// html/template escapes the quotes in the config error; that is the
		// point, so assert the readable words rather than the raw bytes.
		"quik",
		"is not supported",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("board missing %q", want)
		}
	}
}

func TestBoardOmitsTheConfigBannerAfterASuccessfulReload(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		reader web.ConfigReloadStatus
	}{
		{"no failure", &fakeConfigReload{}},
		{"no reader", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			page := drawerPage(t, serverWithConfigReload(t, tc.reader), "/")
			if strings.Contains(page, `data-testid="config-reload"`) {
				t.Fatalf("the board drew a config banner:\n%s", page)
			}
		})
	}
}
