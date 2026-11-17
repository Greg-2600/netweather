package anomaly_test

import (
	"context"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/anomaly"
	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
)

// learningSource is one scope with an unusable baseline, so the absolute-floor
// path is the only detector that can speak.
func learningSource(scope, value string, counts map[state.State]int) *fakeSource {
	key := scope + "|" + value
	return &fakeSource{
		scopes:    []store.ScopeRef{{Scope: scope, Value: value}},
		counts:    map[string]map[state.State]int{key: counts},
		judgeable: map[string]int{key: 40},
		history:   map[string][]float64{key: normalHistory()[:10]},
		coverage:  map[string][]float64{key: normalCoverage()},
	}
}

func TestEvaluateAppliesTheLearningFloor(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	// 34 of 40 healthy is 0.85, below the 0.90 global floor.
	src := learningSource("global", "", map[state.State]int{state.Up: 34, state.Down: 6})
	sink := &fakeSink{}

	ev := anomaly.New(src, sink, "local")
	ev.Now = func() time.Time { return now }

	// One bucket is not a pattern.
	if n, err := ev.Evaluate(context.Background()); err != nil || n != 0 {
		t.Fatalf("first evaluation = (%d, %v)", n, err)
	}

	now = now.Add(5 * time.Minute)
	n, err := ev.Evaluate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(sink.anomalies) != 1 {
		t.Fatalf("second evaluation = (%d, %d anomalies)", n, len(sink.anomalies))
	}
	got := sink.anomalies[0]
	if got.Kind != "health_floor" || got.Buckets != 2 {
		t.Fatalf("anomaly = %+v, want a two-bucket health_floor", got)
	}
	if got.Baseline != 0.90 {
		t.Fatalf("baseline = %v, want the global floor 0.90", got.Baseline)
	}
	if diff := got.Observed - 0.85; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("observed = %v, want 0.85", got.Observed)
	}
}

func TestEvaluateLearningFloorIsScopeSpecific(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		scope    string
		value    string
		up       int
		down     int
		fires    bool
		baseline float64
	}{
		{"global below 0.90", "global", "", 34, 6, true, 0.90},
		{"global above 0.90", "global", "", 37, 3, false, 0},
		{"sector below 0.75", "sector", "airline", 28, 12, true, 0.75},
		{"sector above 0.75", "sector", "airline", 32, 8, false, 0},
		// No documented floor for a country or ASN during LEARNING, so the
		// detector stays silent rather than inventing one.
		{"country has no floor", "country", "US", 5, 35, false, 0},
		{"asn has no floor", "asn", "64500", 5, 35, false, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
			src := learningSource(tc.scope, tc.value, map[state.State]int{
				state.Up: tc.up, state.Down: tc.down,
			})
			sink := &fakeSink{}

			ev := anomaly.New(src, sink, "local")
			ev.Now = func() time.Time { return now }

			if n, _ := ev.Evaluate(context.Background()); n != 0 {
				t.Fatalf("first evaluation fired")
			}
			now = now.Add(5 * time.Minute)
			n, err := ev.Evaluate(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if tc.fires {
				if n != 1 || len(sink.anomalies) != 1 {
					t.Fatalf("second evaluation = (%d, %d anomalies), want one", n, len(sink.anomalies))
				}
				if sink.anomalies[0].Kind != "health_floor" ||
					sink.anomalies[0].Baseline != tc.baseline {
					t.Fatalf("anomaly = %+v", sink.anomalies[0])
				}
				return
			}
			if n != 0 || len(sink.anomalies) != 0 {
				t.Fatalf("second evaluation emitted %+v", sink.anomalies)
			}
		})
	}
}
