// Package baseline is the robust statistics behind anomaly detection: an
// hour-of-week median/MAD band, the two-bucket trigger, and the both-direction
// atypical-day rule (DESIGN.md §7.2).
//
// It is pure. Samples come in as floats; the caller decides which scope and
// which bucket. That makes the maths testable without a database or a clock.
package baseline

import (
	"math"
	"sort"
	"time"
)

const (
	// Window is the rolling history per hour-of-week bucket: four weeks.
	Window = 28
	// MinSamples is the smallest usable history (21 of 28).
	MinSamples = 21
	// DefaultK is the robust sigma multiplier.
	DefaultK = 3.0
	// MADToSigma converts a median absolute deviation into a robust sigma.
	MADToSigma = 1.4826
)

// Key maps an instant to its hour-of-week bucket: Monday 00:00 UTC is 0 and
// Sunday 23:00 is 167. UTC is used so DST cannot duplicate or skip a bucket.
func Key(t time.Time) int {
	u := t.UTC()
	day := (int(u.Weekday()) + 6) % 7 // Monday=0
	return day*24 + u.Hour()
}

// Stats is a robust baseline for one bucket.
type Stats struct {
	N      int
	Median float64
	MAD    float64
}

// Sigma is the robust standard deviation estimate.
func (s Stats) Sigma() float64 { return MADToSigma * s.MAD }

// Usable reports whether the bucket has enough samples to judge against.
func (s Stats) Usable() bool { return s.N >= MinSamples }

// Lower is median - k*sigma, clamped to the possible health range.
func (s Stats) Lower(k float64) float64 { return clamp01(s.Median - k*s.Sigma()) }

// Upper is median + k*sigma, clamped to the possible health range.
func (s Stats) Upper(k float64) float64 { return clamp01(s.Median + k*s.Sigma()) }

// Compute returns the median and median absolute deviation of samples.
func Compute(samples []float64) Stats {
	n := len(samples)
	if n == 0 {
		return Stats{}
	}

	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	med := medianSorted(sorted)

	dev := make([]float64, n)
	for i, v := range sorted {
		dev[i] = math.Abs(v - med)
	}
	sort.Float64s(dev)

	return Stats{N: n, Median: med, MAD: medianSorted(dev)}
}

func medianSorted(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}

// History keeps the last Window samples per bucket.
type History struct {
	samples map[int][]float64
}

// NewHistory returns an empty history.
func NewHistory() *History {
	return &History{samples: make(map[int][]float64)}
}

// Add records a sample, trimming the bucket to the rolling window.
func (h *History) Add(t time.Time, health float64) {
	if h.samples == nil {
		h.samples = make(map[int][]float64)
	}
	k := Key(t)
	s := append(h.samples[k], health)
	if len(s) > Window {
		s = s[len(s)-Window:]
	}
	h.samples[k] = s
}

// Stats returns the bucket baseline and whether it is usable.
func (h *History) Stats(t time.Time) (Stats, bool) {
	s, ok := h.samples[Key(t)]
	if !ok {
		return Stats{}, false
	}
	st := Compute(s)
	return st, st.Usable()
}

// Detector implements the two-consecutive-bucket trigger.
type Detector struct {
	K       float64 // sigma multiplier; 0 means DefaultK
	MinDrop float64 // absolute drop in health points; 0 means 0.02
	last    []float64
}

// NewDetector returns a detector with the documented defaults.
func NewDetector() *Detector {
	return &Detector{K: DefaultK, MinDrop: 0.02}
}

// Push records one evaluation and reports whether it completes a trigger.
func (d *Detector) Push(health float64, s Stats) (bool, string) {
	if !s.Usable() {
		return false, "learning"
	}

	k := d.K
	if k == 0 {
		k = DefaultK
	}
	minDrop := d.MinDrop
	if minDrop == 0 {
		minDrop = 0.02
	}

	d.last = append(d.last, health)
	if len(d.last) > 2 {
		d.last = d.last[1:]
	}
	if len(d.last) < 2 {
		return false, ""
	}

	lower := s.Lower(k)
	if d.last[0] >= lower || d.last[1] >= lower {
		return false, ""
	}
	if s.Median-health < minDrop {
		return false, ""
	}
	return true, "below baseline for two consecutive buckets"
}

// Reset clears the consecutive-bucket state.
func (d *Detector) Reset() { d.last = nil }

// DayWidener implements the both-direction atypical-day rule: if the first
// three evaluations of a UTC day fall outside the band in both directions, the
// day is atypical and the band widens to twice the MAD (6*MAD) until midnight.
type DayWidener struct {
	date     string
	observed int
	above    bool
	below    bool
	widened  bool
}

// Observe records one evaluation and returns the band to use.
func (w *DayWidener) Observe(t time.Time, health float64, s Stats) (Stats, bool) {
	date := t.UTC().Format("2006-01-02")
	if date != w.date {
		*w = DayWidener{date: date}
	}

	if w.widened {
		return widen(s), true
	}
	if w.observed >= 3 {
		return s, false
	}

	switch {
	case health < s.Lower(DefaultK):
		w.below = true
	case health > s.Upper(DefaultK):
		w.above = true
	}
	w.observed++

	if w.observed == 3 && w.above && w.below {
		w.widened = true
		return widen(s), true
	}
	return s, false
}

// Widened reports whether the current day has been marked atypical.
func (w *DayWidener) Widened() bool { return w.widened }

func widen(s Stats) Stats {
	s.MAD *= 2 // 3*MAD becomes 6*MAD
	return s
}
