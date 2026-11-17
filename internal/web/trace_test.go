package web_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	neturl "net/url"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/probe"
	"github.com/Greg-2600/netweather/internal/tracebudget"
	"github.com/Greg-2600/netweather/internal/web"
)

type fakeWebTracer struct {
	res probe.TraceResult
	got netip.Addr
}

func (f *fakeWebTracer) Run(_ context.Context, dst netip.Addr) probe.TraceResult {
	f.got = dst
	return f.res
}

type fakeWebResolver struct {
	addrs []netip.Addr
	err   error
}

func (f *fakeWebResolver) CheckHost(context.Context, string) ([]netip.Addr, error) {
	return f.addrs, f.err
}

func serverWithTrace(t *testing.T, enabled bool, tracer web.Tracer, resolver web.HostResolver, budget *tracebudget.Governor) *httptest.Server {
	t.Helper()

	m := fixture()
	h, err := web.New(web.Deps{
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Source:       m,
		Watcher:      m,
		Token:        testToken,
		TraceEnabled: enabled,
		Tracer:       tracer,
		Resolver:     resolver,
		TraceBudget:  budget,
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

// tracePost logs in, reads a CSRF token from the board, and posts the trace
// action for the fixture's United target.
func tracePost(t *testing.T, ts *httptest.Server, csrf string) (int, string) {
	t.Helper()

	client, _ := newClient(t)
	login(t, ts, client, testToken)

	resp, err := client.Get(ts.URL + "/") //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	if csrf == "" {
		csrf = csrfFrom(t, page)
	}

	resp, err = client.PostForm(ts.URL+"/targets/2/trace", neturl.Values{"csrf": {csrf}}) //nolint:bodyclose // body() closes it
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body(t, resp)
}

func TestTargetTraceActionRunsAWalk(t *testing.T) {
	t.Parallel()

	tracer := &fakeWebTracer{res: probe.TraceResult{
		Destination: netip.MustParseAddr("203.0.113.10"),
		Reached:     true,
		Hops: []probe.TraceHop{
			{TTL: 1, Addr: netip.MustParseAddr("10.0.0.1"), RTT: 4 * time.Millisecond},
			{TTL: 2, Addr: netip.MustParseAddr("203.0.113.10"), RTT: 11 * time.Millisecond},
		},
	}}
	resolver := &fakeWebResolver{addrs: []netip.Addr{netip.MustParseAddr("203.0.113.10")}}

	ts := serverWithTrace(t, true, tracer, resolver, tracebudget.New(1, time.Hour))
	code, page := tracePost(t, ts, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", code, page)
	}
	if tracer.got != netip.MustParseAddr("203.0.113.10") {
		t.Fatalf("the trace ran against %s, want the resolved address", tracer.got)
	}
	for _, want := range []string{
		`data-testid="trace-hops"`, "10.0.0.1", "203.0.113.10",
		`data-testid="trace-reached"`, "local gateway",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("trace page missing %q", want)
		}
	}
}

func TestTargetTraceActionStatesItsRefusals(t *testing.T) {
	t.Parallel()

	// A two-run budget with both runs already spent: the page should say the
	// configured limit, not the remaining count.
	exhausted := tracebudget.New(2, time.Hour)
	for i := 0; i < 2; i++ {
		if ok, _ := exhausted.Allow(); !ok {
			t.Fatal("the test budget was exhausted early")
		}
	}

	cases := []struct {
		name     string
		enabled  bool
		tracer   web.Tracer
		resolver web.HostResolver
		budget   *tracebudget.Governor
		want     string
	}{
		{
			name:     "disabled capability",
			enabled:  false,
			tracer:   &fakeWebTracer{},
			resolver: &fakeWebResolver{addrs: []netip.Addr{netip.MustParseAddr("203.0.113.10")}},
			budget:   tracebudget.New(1, time.Hour),
			want:     "Traceroute is disabled",
		},
		{
			name:     "exhausted budget",
			enabled:  true,
			tracer:   &fakeWebTracer{},
			resolver: &fakeWebResolver{addrs: []netip.Addr{netip.MustParseAddr("203.0.113.10")}},
			budget:   exhausted,
			want:     "limited to 2 runs per hour",
		},
		{
			name:     "unresolvable host",
			enabled:  true,
			tracer:   &fakeWebTracer{},
			resolver: &fakeWebResolver{err: errors.New("no address")},
			budget:   tracebudget.New(1, time.Hour),
			want:     "could not be resolved",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := serverWithTrace(t, tc.enabled, tc.tracer, tc.resolver, tc.budget)
			code, page := tracePost(t, ts, "")
			if code != http.StatusOK {
				t.Fatalf("status = %d:\n%s", code, page)
			}
			if !strings.Contains(page, tc.want) {
				t.Fatalf("trace page does not say %q:\n%s", tc.want, page)
			}
		})
	}
}

func TestTargetTraceActionRequiresCSRF(t *testing.T) {
	t.Parallel()

	ts := serverWithTrace(t, true, &fakeWebTracer{}, &fakeWebResolver{}, tracebudget.New(1, time.Hour))
	code, page := tracePost(t, ts, "wrong-token")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if !strings.Contains(page, `data-testid="error-page"`) {
		t.Fatalf("a bad CSRF token did not render the generic page:\n%s", page)
	}
}

func TestTargetTraceButtonOnlyWhenEnabled(t *testing.T) {
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

			ts := serverWithTrace(t, tc.enabled, &fakeWebTracer{}, &fakeWebResolver{}, tracebudget.New(1, time.Hour))
			client, _ := newClient(t)
			login(t, ts, client, testToken)

			resp, err := client.Get(ts.URL + "/targets/2") //nolint:bodyclose // body() closes it
			if err != nil {
				t.Fatal(err)
			}
			page := body(t, resp)
			has := strings.Contains(page, `data-testid="trace-action"`)
			if has != tc.want {
				t.Fatalf("trace button present = %v, want %v", has, tc.want)
			}
		})
	}
}

// Ensure the fixture type stays what the helper expects; a compile-time check
// is clearer than a runtime one when the fixture changes.
var _ board.Source = (*board.Memory)(nil)
