package agent

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/probe"
)

// TestStageFor keeps the drawer's stage badge and LOGGING.md's stage list
// honest: a successful dns/doh/icmp probe must not be labelled http just
// because the instrument has no error class.
func TestStageFor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		instrument string
		class      string
		want       string
	}{
		{"dns", "", "dns"},
		{"dns", "dns_timeout", "dns"},
		{"doh", "", "doh"},
		{"doh", "doh_nxdomain", "doh"},
		{"doh", "http_timeout", "doh"},
		{"dot", "", "dot"},
		{"dot", "dot_timeout", "dot"},
		{"http", "dot_timeout", "dot"},
		{"quic", "", "quic"},
		{"quic", "quic_blocked", "quic"},
		{"http", "quic_blocked", "quic"},
		{"tls", "", "tls"},
		{"tls", "tls_handshake", "tls"},
		{"tcp", "", "tcp"},
		{"tcpping", "", "tcp"},
		{"icmp", "", "icmp"},
		{"icmp", "icmp_timeout", "icmp"},
		{"ntp", "", "ntp"},
		{"ntp", "ntp_offset", "ntp"},
		{"http", "", "http"},
		{"", "", "http"},
	}
	for _, tc := range cases {
		if got := stageFor(Target{Instrument: tc.instrument}, tc.class); got != tc.want {
			t.Errorf("stageFor(%q, %q) = %q, want %q", tc.instrument, tc.class, got, tc.want)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	header := func(v string) http.Header {
		return http.Header{"Retry-After": []string{v}}
	}

	cases := []struct {
		name   string
		header http.Header
		want   time.Duration
		ok     bool
	}{
		{"nil header", nil, 0, false},
		{"empty", http.Header{}, 0, false},
		{"blank", header("   "), 0, false},
		{"zero", header("0"), 0, false},
		{"negative", header("-5"), 0, false},
		{"invalid", header("soon"), 0, false},
		{"seconds", header("7"), 7 * time.Second, true},
		{"past date", header(now.Add(-time.Minute).Format(http.TimeFormat)), 0, false},
		{"future date", header(now.Add(45 * time.Second).Format(http.TimeFormat)), 45 * time.Second, true},
		{"capped at an hour", header("7200"), time.Hour, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := retryAfter(tc.header, now)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("retryAfter = (%v, %v), want (%v, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestDNSFailure(t *testing.T) {
	t.Parallel()

	cases := []struct {
		class string
		want  string
	}{
		{"", ""},
		{"dns_nxdomain", "nxdomain"},
		{"dns_nodata", "nodata"},
		{"dns_servfail", "servfail"},
		{"dns_refused", "refused"},
		{"dns_timeout", "timeout"},
		{"dns_weird", "error"},
		{"http_timeout", "error"},
	}
	for _, tc := range cases {
		if got := dnsFailure(probe.DNSResult{ErrorClass: tc.class}); got != tc.want {
			t.Errorf("dnsFailure(%q) = %q, want %q", tc.class, got, tc.want)
		}
	}
}

func TestDNSSuffixes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		cnames []string
		want   []string
	}{
		{"none", nil, []string{}},
		{"empty name", []string{""}, []string{}},
		{"deduplicates", []string{"a.example.com.", "b.example.com."}, []string{"example.com"}},
		{"lowercases", []string{"Edge.Foo.Cloudflare.NET."}, []string{"cloudflare.net"}},
		{"keeps distinct", []string{"a.example.com", "x.other.net"}, []string{"example.com", "other.net"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dnsSuffixes(tc.cnames)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("dnsSuffixes(%v) = %v, want %v", tc.cnames, got, tc.want)
			}
		})
	}
}

func TestPaceGap(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		ceiling float64
		start   float64
		ramp    time.Duration
		last    time.Time
		now     time.Time
		want    time.Duration
	}{
		{"no ceiling", 0, 2, 0, base, base, 0},
		{"zero rate", 10, 0, time.Minute, base, base, 0},
		{"first probe", 2, 2, 0, time.Time{}, base, 0},
		{"full gap", 2, 2, 0, base, base, 500 * time.Millisecond},
		{"partial gap", 2, 2, 0, base, base.Add(200 * time.Millisecond), 300 * time.Millisecond},
		{"elapsed past gap", 2, 2, 0, base, base.Add(600 * time.Millisecond), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Agent{
				RateCeiling: tc.ceiling,
				RateStart:   tc.start,
				RateRamp:    tc.ramp,
				RateSince:   base,
				lastProbe:   tc.last,
			}
			if got := a.paceGap(tc.now); got != tc.want {
				t.Fatalf("paceGap = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSleepCtx(t *testing.T) {
	t.Parallel()

	background := context.Background()
	cancelled, cancel := context.WithCancel(background)
	cancel()

	cases := []struct {
		name      string
		d         time.Duration
		ctx       context.Context
		injected  bool
		wantErr   error
		wantSlept int
	}{
		{"injected sleep", time.Hour, background, true, nil, 1},
		{"injected sleep honours cancellation", time.Hour, cancelled, true, context.Canceled, 1},
		{"timer", time.Millisecond, background, false, nil, 0},
		{"timer honours cancellation", time.Hour, cancelled, false, context.Canceled, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slept := 0
			var sleep func(time.Duration)
			if tc.injected {
				sleep = func(time.Duration) { slept++ }
			}
			err := sleepCtx(tc.ctx, tc.d, sleep)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("sleepCtx error = %v, want %v", err, tc.wantErr)
			}
			if slept != tc.wantSlept {
				t.Fatalf("slept = %d, want %d", slept, tc.wantSlept)
			}
		})
	}
}

func TestPrimeLockedSkipsAlreadyScheduledTargets(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	existing := now.Add(time.Minute)
	a := &Agent{
		targets: []Target{
			{ID: "a", Interval: time.Minute},
			{ID: "b", Interval: time.Minute},
		},
		next: map[string]time.Time{"a": existing},
		Rand: func() float64 { return 0.5 },
	}

	a.primeLocked(now)

	if !a.next["a"].Equal(existing) {
		t.Fatalf("existing schedule = %v, want %v", a.next["a"], existing)
	}
	if a.next["b"].IsZero() {
		t.Fatal("a new target was not scheduled")
	}
}

func TestNowFallsBackToTheRealClock(t *testing.T) {
	t.Parallel()

	if got := (&Agent{}).now(); got.IsZero() {
		t.Fatal("now returned the zero time without an injected clock")
	}
}
