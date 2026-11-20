package web //nolint:testpackage // white-box tests pin the map hover-card explanations

import (
	"strings"
	"testing"

	"github.com/Greg-2600/netweather/internal/board"
)

func TestTileEntityDescribesTheTarget(t *testing.T) {
	cases := []struct {
		name   string
		target board.Target
		want   []string
	}{
		{
			name: "root server",
			target: board.Target{
				Name: "Root server A", Host: "a.root-servers.net",
				Class: "reference", Sector: "reference", Probe: "dns",
			},
			want: []string{"DNS root server", "com zone"},
		},
		{
			name: "ccTLD",
			target: board.Target{
				Name: "Nepal ccTLD", Country: "NP",
				Class: "country", Sector: "country", Probe: "dns",
			},
			want: []string{"Country-code top-level domain .np", "Nepal"},
		},
		{
			name: "IDN ccTLD",
			target: board.Target{
				Name: "Egypt (مصر) ccTLD", Country: "EG",
				Class: "country", Sector: "country", Probe: "dns",
			},
			want: []string{"Country-code top-level domain .eg", "Egypt"},
		},
		{
			name: "DoH reference",
			target: board.Target{
				Name: "Cloudflare DoH", Host: "cloudflare-dns.com",
				Class: "reference", Sector: "reference", Probe: "doh",
			},
			want: []string{"DNS-over-HTTPS resolver", "control endpoint"},
		},
		{
			name: "web service",
			target: board.Target{
				Name: "Example Air", Host: "example.com",
				Class: "service", Sector: "airline", Probe: "http",
			},
			want: []string{"Public airline service", "example.com"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tileEntity(tc.target)
			for _, part := range tc.want {
				if !strings.Contains(got, part) {
					t.Fatalf("tileEntity() = %q, want it to contain %q", got, part)
				}
			}
		})
	}
}

func TestTileWhyExplainsStateAndSignal(t *testing.T) {
	cases := []struct {
		name   string
		target board.Target
		want   []string
	}{
		{
			name:   "down with dns timeout",
			target: board.Target{State: board.StateDown, ErrorClass: "dns_timeout"},
			want:   []string{"Marked DOWN", "timed out", "counts as an outage"},
		},
		{
			name:   "down with nodata",
			target: board.Target{State: board.StateDown, ErrorClass: "dns_nodata"},
			want:   []string{"Marked DOWN", "no address or delegation records"},
		},
		{
			name:   "blocked by status",
			target: board.Target{State: board.StateBlocked, HTTPStatus: 403, ErrorClass: "forbidden (403)"},
			want:   []string{"Marked BLOCKED", "forbidden (403)", "not an outage"},
		},
		{
			name:   "config error",
			target: board.Target{State: board.StateConfigError, ErrorClass: "404 on probe path"},
			want:   []string{"Marked CONFIG ERROR", "needs fixing"},
		},
		{
			name:   "unknown",
			target: board.Target{State: board.StateUnknown},
			want:   []string{"Marked NO DATA", "no probe result"},
		},
		{
			name:   "up",
			target: board.Target{State: board.StateUp, ErrorClass: "semantic_ok"},
			want:   []string{"Marked UP", "expected content"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tileWhy(tc.target)
			for _, part := range tc.want {
				if !strings.Contains(got, part) {
					t.Fatalf("tileWhy() = %q, want it to contain %q", got, part)
				}
			}
		})
	}
}

func TestTileSignalFallsBackToHTTPStatus(t *testing.T) {
	if got := tileSignal(board.Target{HTTPStatus: 451}); got != "the server returned HTTP 451" {
		t.Fatalf("tileSignal() = %q", got)
	}
}

func TestTileStateLabel(t *testing.T) {
	cases := map[board.State]string{
		board.StateUp:             "UP",
		board.StateUpReachability: "UP",
		board.StateDown:           "DOWN",
		board.StateBlocked:        "BLOCKED",
		board.StateDegraded:       "DEGRADED",
		board.StateConfigError:    "CONFIG ERROR",
		board.StateUnmeasurable:   "UNMEASURABLE",
		board.StateUnknown:        "NO DATA",
	}
	for state, want := range cases {
		if got := tileStateLabel(state); got != want {
			t.Fatalf("tileStateLabel(%q) = %q, want %q", state, got, want)
		}
	}
}

func TestMapTipIsPlainTextWithTheSameFacts(t *testing.T) {
	got := mapTip("Nepal ccTLD", "Country-code top-level domain .np", "DOWN", "dns_timeout", "19h 37m", "Because DNS timed out.")
	if !strings.Contains(got, "Nepal ccTLD") || !strings.Contains(got, "dns_timeout") || strings.Count(got, "\n") < 3 {
		t.Fatalf("mapTip() = %q", got)
	}
}
