package web

import (
	"reflect"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/board"
)

func TestHealthClusterKey(t *testing.T) {
	old := 2 * longOutage
	cases := []struct {
		name  string
		state board.State
		dur   time.Duration
		want  string
	}{
		{"long down", board.StateDown, old, "outages-long"},
		{"recent down", board.StateDown, 5 * time.Minute, "outages-recent"},
		{"blocked", board.StateBlocked, old, "blocked"},
		{"degraded", board.StateDegraded, 0, "impaired"},
		{"config error", board.StateConfigError, 0, "impaired"},
		{"no data", board.StateUnknown, 0, "no-data"},
		{"healthy", board.StateUp, time.Hour, "healthy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := healthClusterKey(tc.state, tc.dur); got != tc.want {
				t.Fatalf("cluster = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTileWeightRisesWithSeverityAndDuration(t *testing.T) {
	fresh := 2 * time.Minute
	old := 3 * time.Hour
	downFresh := tileWeight(board.StateDown, fresh)
	downOld := tileWeight(board.StateDown, old)
	blockedOld := tileWeight(board.StateBlocked, old)
	healthyOld := tileWeight(board.StateUp, old)

	if downFresh <= healthyOld {
		t.Fatalf("fresh down (%d) should outweigh long healthy (%d)", downFresh, healthyOld)
	}
	if downOld <= downFresh {
		t.Fatalf("long down (%d) should outweigh fresh down (%d)", downOld, downFresh)
	}
	if downOld <= blockedOld {
		t.Fatalf("down (%d) should outweigh blocked (%d) at the same duration", downOld, blockedOld)
	}
	if downOld > 64 {
		t.Fatalf("weight must be capped, got %d", downOld)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                    "—",
		45 * time.Second:     "<1m",
		12 * time.Minute:     "12m",
		90 * time.Minute:     "1h 30m",
		26 * time.Hour:       "1d 2h",
	}
	for d, want := range cases {
		if got := humanDuration(d); got != want {
			t.Fatalf("humanDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestCompactLabel(t *testing.T) {
	cases := map[string]string{
		"AC ccTLD":        "AC",
		"NE ccTLD":        "NE",
		"x.nic.aarp DNS":  "AARP",
		"v2n1.nic.agency": "AGENCY",
		"":                "?",
	}
	for name, want := range cases {
		if got := compactLabel(name); got != want {
			t.Fatalf("compactLabel(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestLayoutMapKeepsClustersOrderedAndTilesInside(t *testing.T) {
	sectors := []*mapSector{
		{Label: "Healthy", Order: 5, Area: 60, Color: "#25b125"},
		{Label: "Long-running outages", Order: 0, Area: 40, Color: "#8f1d1d"},
	}
	for si, sec := range sectors {
		for i := 0; i < 10; i++ {
			sec.Tiles = append(sec.Tiles, &mapTile{
				Label: "target", Ticker: "T", Weight: 1 + si,
			})
		}
	}
	layoutMap(sectors)

	// The urgent cluster must be the one packed at the top-left (x≈0, y≈0).
	byLabel := map[string]*mapSector{}
	for _, sec := range sectors {
		byLabel[sec.Label] = sec
	}
	urgent := byLabel["Long-running outages"]
	if urgent.X > 0.01 || urgent.Y > 0.01 {
		t.Fatalf("urgent cluster not packed first: x=%.2f y=%.2f", urgent.X, urgent.Y)
	}
	for _, sec := range sectors {
		if !sec.ShowHead {
			t.Fatalf("large cluster %q should show a header", sec.Label)
		}
		for _, tl := range sec.Tiles {
			if !tl.ShowTicker || !tl.ShowFull {
				t.Fatalf("tile in large cluster should show a label: full=%v ticker=%v", tl.ShowFull, tl.ShowTicker)
			}
			if tl.X < 0 || tl.Y < 0 || tl.X+tl.W > 100.01 || tl.Y+tl.H > 100.01 {
				t.Fatalf("tile escaped its block: x=%.2f y=%.2f w=%.2f h=%.2f", tl.X, tl.Y, tl.W, tl.H)
			}
		}
	}
	// Blocks themselves must tile the whole map without overlapping.
	covered := 0.0
	for _, sec := range sectors {
		covered += sec.W * sec.H
	}
	if covered < 9999 || covered > 10001 {
		t.Fatalf("clusters do not tile the map exactly: covered %.2f (want ~10000)", covered)
	}
	if !reflect.DeepEqual(byLabel["Long-running outages"].Color, "#8f1d1d") {
		t.Fatalf("urgent cluster lost its colour")
	}
}
