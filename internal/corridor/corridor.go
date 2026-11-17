// Package corridor infers intercontinental path health from a pair of
// endpoints (DESIGN.md §9).
//
// There is no live cable-health API, so the only honest signal is a step change
// against a corridor's own rolling baseline: an absolute RTT says nothing, and
// comparing two corridors says even less. The package is pure: sampling is
// behind the Prober seam, and time is passed in on the samples.
package corridor

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"
)

const (
	// Window is the documented baseline lookback.
	Window = 6 * time.Hour
	// MinSamples is the smallest baseline worth judging against.
	MinSamples = 6
	// LossAlertPercent is "loss jumps from ~0 to >= 5%" from DESIGN.md §9.
	LossAlertPercent = 5
	// Attempts is how many TCP connects each endpoint gets per sample. The
	// corridor's RTT is the median of those attempts.
	Attempts = 5
)

// Sample is one corridor observation. Loss is a percentage, not a fraction.
type Sample struct {
	TS    time.Time
	RTTMS float64
	Loss  float64
	OK    bool
}

// Baseline is the rolling reference a sample is judged against.
type Baseline struct {
	MedianMS   float64
	SigmaMS    float64
	MedianLoss float64
	N          int
}

// Fit computes the baseline over the provided history. Only successful samples
// contribute to the RTT statistic; loss uses every sample because a loss step is
// exactly the thing a success-only baseline would hide.
func Fit(samples []Sample) Baseline {
	b := Baseline{N: len(samples)}

	rtts := make([]float64, 0, len(samples))
	losses := make([]float64, 0, len(samples))
	for _, s := range samples {
		losses = append(losses, s.Loss)
		if s.OK {
			rtts = append(rtts, s.RTTMS)
		}
	}
	if len(rtts) > 0 {
		b.MedianMS = median(rtts)
		b.SigmaMS = stddev(rtts, b.MedianMS)
	}
	if len(losses) > 0 {
		b.MedianLoss = median(losses)
	}
	return b
}

// Verdict is a possible step change.
type Verdict struct {
	Step   bool
	Kind   string // "rtt_step" or "loss_step"
	Detail string
}

// Evaluate decides whether the newest sample is a step change. The last two
// samples must both be out of band: a single spike is noise, and a corridor
// alert that fires on one sample is a corridor alert nobody reads.
func Evaluate(history []Sample) Verdict {
	if len(history) < MinSamples+2 {
		return Verdict{}
	}

	baseline := Fit(history[:len(history)-2])
	prev := history[len(history)-2]
	current := history[len(history)-1]

	switch {
	case lossStep(prev, baseline) && lossStep(current, baseline):
		return Verdict{
			Step: true, Kind: "loss_step",
			Detail: fmt.Sprintf("loss %.1f%% for two samples against a %.1f%% baseline",
				current.Loss, baseline.MedianLoss),
		}
	case rttStep(prev, baseline) && rttStep(current, baseline):
		return Verdict{
			Step: true, Kind: "rtt_step",
			Detail: fmt.Sprintf("rtt %.0fms for two samples against a %.0fms baseline (±%.0fms)",
				current.RTTMS, baseline.MedianMS, 3*baseline.SigmaMS),
		}
	default:
		return Verdict{}
	}
}

func rttStep(s Sample, b Baseline) bool {
	if !s.OK || b.N < MinSamples {
		return false
	}
	delta := math.Abs(s.RTTMS - b.MedianMS)
	if b.SigmaMS > 0 && delta <= 3*b.SigmaMS {
		return false
	}
	// A statistically large move on a very stable path can still be
	// meaningless. Require a material change as well: 20ms and 25%. A flat
	// baseline has sigma 0, so the material floor is what decides there.
	return delta >= 20 && delta >= 0.25*b.MedianMS
}

func lossStep(s Sample, b Baseline) bool {
	return s.Loss >= LossAlertPercent && b.MedianLoss < 1
}

// Prober is the TCP connect seam. It returns the handshake latency and whether
// the attempt succeeded.
type Prober interface {
	Probe(ctx context.Context, address string) (time.Duration, bool)
}

// Measure samples each endpoint Attempts times and aggregates one corridor
// sample: the median RTT per endpoint, averaged across endpoints, and the
// failure rate across every attempt.
func Measure(ctx context.Context, endpoints []string, prober Prober) Sample {
	sample := Sample{TS: time.Now().UTC()}

	endpointRTTs := make([]float64, 0, len(endpoints))
	total, failed := 0, 0

	for _, endpoint := range endpoints {
		rtts := make([]float64, 0, Attempts)
		for i := 0; i < Attempts; i++ {
			if ctx.Err() != nil {
				break
			}
			total++
			latency, ok := prober.Probe(ctx, endpoint)
			if !ok {
				failed++
				continue
			}
			rtts = append(rtts, float64(latency.Microseconds())/1000)
		}
		if len(rtts) > 0 {
			endpointRTTs = append(endpointRTTs, median(rtts))
		}
	}

	if total > 0 {
		sample.Loss = 100 * float64(failed) / float64(total)
	}
	if len(endpointRTTs) > 0 {
		sample.RTTMS = mean(endpointRTTs)
		sample.OK = true
	}
	return sample
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func stddev(values []float64, centre float64) float64 {
	if len(values) < 2 {
		return 0
	}
	sum := 0.0
	for _, v := range values {
		d := v - centre
		sum += d * d
	}
	return math.Sqrt(sum / float64(len(values)-1))
}
