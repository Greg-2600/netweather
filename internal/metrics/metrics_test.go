package metrics_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Greg-2600/netweather/internal/metrics"
	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
)

type fakeSource struct {
	counts    map[state.State]int
	open      int
	fleet     store.FleetStats
	countsErr error
	openErr   error
	fleetErr  error
}

func (f *fakeSource) StateCounts(context.Context, string) (map[state.State]int, error) {
	return f.counts, f.countsErr
}

func (f *fakeSource) OpenIncidentCount(context.Context) (int, error) {
	return f.open, f.openErr
}

func (f *fakeSource) FleetStats(context.Context, string) (store.FleetStats, error) {
	return f.fleet, f.fleetErr
}

func scrape(t *testing.T, src metrics.Source) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metrics.Handler(src, "local").ServeHTTP(rec, req)
	return rec
}

func TestExposition(t *testing.T) {
	t.Parallel()

	src := &fakeSource{
		counts: map[state.State]int{
			state.Down: 3, state.Up: 7, state.Blocked: 2, state.Degraded: 1,
		},
		open: 2,
		fleet: store.FleetStats{
			ByClass:            map[string]int{"service": 6, "country": 2},
			Total:              8,
			EffectivePerSecond: 0.25,
			Blocked:            2,
			Unmeasurable:       1,
			ASNCount:           3,
			CountryCount:       4,
			TopASN:             13335,
			TopASNName:         "Cloudflare, Inc.",
			TopASNCount:        4,
			TopASNShare:        0.5,
		},
	}
	rec := scrape(t, src)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q", ct)
	}

	body := rec.Body.String()
	for _, want := range []string{
		`netweather_targets{state="down"} 3`,
		`netweather_targets{state="up"} 7`,
		`netweather_targets{state="blocked"} 2`,
		`netweather_targets{state="degraded"} 1`,
		`netweather_targets{state="unknown"} 0`,
		"netweather_incidents_open 2",
		"# TYPE netweather_targets gauge",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q:\n%s", want, body)
		}
	}

	// No per-target labels, ever.
	if strings.Contains(body, "host=") || strings.Contains(body, "target=") {
		t.Fatalf("per-target labels leaked into metrics:\n%s", body)
	}
}

func TestStoreFailureIsUnavailable(t *testing.T) {
	t.Parallel()

	rec := scrape(t, &fakeSource{countsErr: errors.New("db down")})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	rec = scrape(t, &fakeSource{openErr: errors.New("db down")})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("open count failure status = %d, want 503", rec.Code)
	}
}

func TestZeroCountsStillExposeEveryState(t *testing.T) {
	t.Parallel()

	body := scrape(t, &fakeSource{}).Body.String()
	for _, s := range []string{"up", "up_reachability", "degraded", "down", "blocked", "unknown", "config_error", "unmeasurable"} {
		if !strings.Contains(body, `state="`+s+`"`) {
			t.Errorf("state %q missing from exposition", s)
		}
	}
}
