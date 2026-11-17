package baseline_test

import (
	"math"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/baseline"
)

func TestKey(t *testing.T) {
	t.Parallel()

	monday := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC) // Monday
	if got := baseline.Key(monday); got != 0 {
		t.Fatalf("Monday 00:00 = %d, want 0", got)
	}
	sunday := time.Date(2026, 11, 22, 23, 0, 0, 0, time.UTC)
	if got := baseline.Key(sunday); got != 167 {
		t.Fatalf("Sunday 23:00 = %d, want 167", got)
	}

	// A local timestamp is converted to UTC, not interpreted locally.
	local := time.Date(2026, 11, 16, 1, 0, 0, 0, time.FixedZone("X", 3600))
	if got := baseline.Key(local); got != 0 {
		t.Fatalf("01:00+01:00 = %d, want 0", got)
	}
}

func TestComputeMedianAndMAD(t *testing.T) {
	t.Parallel()

	// Median 5; absolute deviations 4,3,2,1,0,1,2,3 -> median 2.
	samples := []float64{1, 2, 3, 4, 5, 6, 7, 8}
	got := baseline.Compute(samples)
	if got.N != 8 {
		t.Fatalf("N = %d", got.N)
	}
	if math.Abs(got.Median-4.5) > 1e-9 {
		// even count: (4+5)/2
		t.Fatalf("median = %v", got.Median)
	}
}

func TestComputeOddAndEmpty(t *testing.T) {
	t.Parallel()

	if got := baseline.Compute(nil); got.N != 0 || got.Median != 0 {
		t.Fatalf("empty = %+v", got)
	}
	got := baseline.Compute([]float64{1, 3, 100})
	if got.Median != 3 {
		t.Fatalf("median = %v", got.Median)
	}
}

func TestUsableNeedsTwentyOneSamples(t *testing.T) {
	t.Parallel()

	if (baseline.Stats{N: 20}).Usable() {
		t.Fatal("20 samples should be learning")
	}
	if !(baseline.Stats{N: 21}).Usable() {
		t.Fatal("21 samples should be usable")
	}
}

func TestBandClamps(t *testing.T) {
	t.Parallel()

	s := baseline.Stats{Median: 0.99, MAD: 0.1}
	if got := s.Upper(3); got != 1 {
		t.Fatalf("upper = %v, want 1", got)
	}
	s = baseline.Stats{Median: 0.01, MAD: 0.1}
	if got := s.Lower(3); got != 0 {
		t.Fatalf("lower = %v, want 0", got)
	}
}

func TestHistoryWindowPerBucket(t *testing.T) {
	t.Parallel()

	h := baseline.NewHistory()
	base := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 40; i++ {
		h.Add(base.AddDate(0, 0, 7*i), 0.9)
	}
	stats, usable := h.Stats(base.AddDate(0, 0, 7*39))
	if stats.N != baseline.Window {
		t.Fatalf("N = %d, want the window %d", stats.N, baseline.Window)
	}
	if !usable {
		t.Fatal("28 samples should be usable")
	}

	other := base.Add(time.Hour)
	if _, ok := h.Stats(other); ok {
		t.Fatal("a different hour-of-week bucket must be separate")
	}
}

func TestDetectorTriggersAfterTwoBuckets(t *testing.T) {
	t.Parallel()

	s := baseline.Stats{N: 28, Median: 0.90, MAD: 0.01}
	d := baseline.NewDetector()

	if ok, reason := d.Push(0.80, s); ok || reason != "" {
		t.Fatalf("one bucket triggered: %v %s", ok, reason)
	}
	ok, reason := d.Push(0.80, s)
	if !ok {
		t.Fatalf("two buckets did not trigger (%s)", reason)
	}
}

func TestDetectorRequiresConsecutiveBuckets(t *testing.T) {
	t.Parallel()

	s := baseline.Stats{N: 28, Median: 0.90, MAD: 0.01}
	d := baseline.NewDetector()

	d.Push(0.80, s)
	d.Push(0.95, s) // back inside
	if ok, _ := d.Push(0.80, s); ok {
		t.Fatal("triggered without two consecutive low buckets")
	}
}

func TestDetectorRequiresTheAbsoluteDrop(t *testing.T) {
	t.Parallel()

	// A tight band can put 0.895 below the lower edge while the drop is only
	// one percentage point, under the two-point floor.
	s := baseline.Stats{N: 28, Median: 0.90, MAD: 0.001}
	d := baseline.NewDetector()

	d.Push(0.895, s)
	if ok, _ := d.Push(0.895, s); ok {
		t.Fatal("triggered on a sub-threshold drop")
	}
}

func TestDetectorIsSilentWhileLearning(t *testing.T) {
	t.Parallel()

	s := baseline.Stats{N: 10, Median: 0.9, MAD: 0.01}
	d := baseline.NewDetector()

	if ok, reason := d.Push(0.1, s); ok || reason != "learning" {
		t.Fatalf("learning = %v %q", ok, reason)
	}
}

func TestDetectorReset(t *testing.T) {
	t.Parallel()

	s := baseline.Stats{N: 28, Median: 0.90, MAD: 0.01}
	d := baseline.NewDetector()
	d.Push(0.80, s)
	d.Reset()
	if ok, _ := d.Push(0.80, s); ok {
		t.Fatal("Reset did not clear the pending bucket")
	}
}

func TestDayWidenerBothDirections(t *testing.T) {
	t.Parallel()

	s := baseline.Stats{N: 28, Median: 0.90, MAD: 0.01}
	var w baseline.DayWidener
	day := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)

	if _, widened := w.Observe(day, 0.80, s); widened {
		t.Fatal("widened on the first excursion")
	}
	if _, widened := w.Observe(day.Add(time.Hour), 0.80, s); widened {
		t.Fatal("widened on two low excursions")
	}
	band, widened := w.Observe(day.Add(2*time.Hour), 0.99, s)
	if !widened || !w.Widened() {
		t.Fatal("both directions did not widen the day")
	}
	if band.MAD <= s.MAD {
		t.Fatalf("band not widened: %+v", band)
	}

	// The widened band persists for the rest of the day.
	if _, widened := w.Observe(day.Add(5*time.Hour), 0.5, s); !widened {
		t.Fatal("widening did not persist")
	}

	// A new day resets.
	if _, widened := w.Observe(day.AddDate(0, 0, 1), 0.80, s); widened {
		t.Fatal("widening leaked into the next day")
	}
}

func TestHistoryZeroValueWorks(t *testing.T) {
	t.Parallel()

	var h baseline.History
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	h.Add(now, 0.9)

	stats, ok := h.Stats(now)
	if ok {
		t.Fatal("one sample must not be usable")
	}
	if stats.N != 1 || stats.Median != 0.9 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestDetectorZeroValueUsesDefaults(t *testing.T) {
	t.Parallel()

	var d baseline.Detector
	s := baseline.Stats{N: 28, Median: 0.90, MAD: 0.01}

	d.Push(0.80, s)
	if ok, _ := d.Push(0.80, s); !ok {
		t.Fatal("zero-value detector did not apply the default K and MinDrop")
	}
}

func TestDayWidenerStopsTrackingAfterThree(t *testing.T) {
	t.Parallel()

	s := baseline.Stats{N: 28, Median: 0.90, MAD: 0.01}
	var w baseline.DayWidener
	day := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)

	for i := 0; i < 3; i++ {
		if _, widened := w.Observe(day.Add(time.Duration(i)*time.Hour), 0.80, s); widened {
			t.Fatal("a one-directional drop widened the day")
		}
	}
	// The fourth observation is outside the three-bucket window and must not
	// retroactively widen the day.
	if _, widened := w.Observe(day.Add(3*time.Hour), 0.99, s); widened {
		t.Fatal("widened after the three-bucket window")
	}
}

func TestDayWidenerOneDirectionIsNotAtypical(t *testing.T) {
	t.Parallel()

	s := baseline.Stats{N: 28, Median: 0.90, MAD: 0.01}
	var w baseline.DayWidener
	day := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)

	for i := 0; i < 3; i++ {
		if _, widened := w.Observe(day.Add(time.Duration(i)*time.Hour), 0.80, s); widened {
			t.Fatal("a one-directional drop widened the day")
		}
	}
}
