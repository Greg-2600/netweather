package corridor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/corridor"
)

func baseSamples(n int, rtt, loss float64) []corridor.Sample {
	started := time.Date(2026, 11, 17, 6, 0, 0, 0, time.UTC)
	out := make([]corridor.Sample, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, corridor.Sample{
			TS:    started.Add(time.Duration(i) * 5 * time.Minute),
			RTTMS: rtt, Loss: loss, OK: true,
		})
	}
	return out
}

func TestFitIgnoresFailedRTTsButKeepsTheirLoss(t *testing.T) {
	t.Parallel()

	samples := []corridor.Sample{
		{RTTMS: 100, Loss: 0, OK: true},
		{RTTMS: 120, Loss: 0, OK: true},
		{RTTMS: 0, Loss: 100, OK: false},
		{RTTMS: 110, Loss: 20, OK: true},
	}
	got := corridor.Fit(samples)
	if got.N != 4 {
		t.Fatalf("N = %d, want every sample counted", got.N)
	}
	if got.MedianMS != 110 {
		t.Fatalf("median rtt = %v, want 110 (failures excluded)", got.MedianMS)
	}
	if got.MedianLoss != 10 {
		t.Fatalf("median loss = %v, want 10", got.MedianLoss)
	}
	if got.SigmaMS <= 0 {
		t.Fatalf("sigma = %v", got.SigmaMS)
	}
}

func TestEvaluateNeedsABaselineAndTwoSamples(t *testing.T) {
	t.Parallel()

	if got := corridor.Evaluate(nil); got.Step {
		t.Fatal("an empty history produced a step")
	}
	if got := corridor.Evaluate(baseSamples(4, 100, 0)); got.Step {
		t.Fatal("a short history produced a step")
	}

	// One spike is noise, not a step.
	history := baseSamples(corridor.MinSamples+1, 100, 0)
	history = append(history, corridor.Sample{RTTMS: 400, Loss: 0, OK: true})
	if got := corridor.Evaluate(history); got.Step {
		t.Fatalf("a single spike was a step: %+v", got)
	}
}

func TestEvaluateFlagsTwoConsecutiveRTTSteps(t *testing.T) {
	t.Parallel()

	history := baseSamples(corridor.MinSamples+2, 100, 0)
	history[len(history)-2] = corridor.Sample{RTTMS: 180, Loss: 0, OK: true}
	history[len(history)-1] = corridor.Sample{RTTMS: 190, Loss: 0, OK: true}

	got := corridor.Evaluate(history)
	if !got.Step || got.Kind != "rtt_step" {
		t.Fatalf("verdict = %+v, want an rtt step", got)
	}
	if got.Detail == "" {
		t.Fatal("step had no detail")
	}
}

func TestEvaluateIgnoresASmallStatisticallyLargeMove(t *testing.T) {
	t.Parallel()

	// A very stable path has a tiny sigma; a 5ms move must not alert.
	history := baseSamples(corridor.MinSamples+2, 100.0, 0)
	history[len(history)-2] = corridor.Sample{RTTMS: 105, Loss: 0, OK: true}
	history[len(history)-1] = corridor.Sample{RTTMS: 106, Loss: 0, OK: true}

	if got := corridor.Evaluate(history); got.Step {
		t.Fatalf("a 5ms move on a stable path alerted: %+v", got)
	}
}

func TestEvaluateFlagsTwoConsecutiveLossSteps(t *testing.T) {
	t.Parallel()

	history := baseSamples(corridor.MinSamples+2, 100, 0)
	history[len(history)-2] = corridor.Sample{RTTMS: 100, Loss: 20, OK: true}
	history[len(history)-1] = corridor.Sample{RTTMS: 100, Loss: 30, OK: true}

	got := corridor.Evaluate(history)
	if !got.Step || got.Kind != "loss_step" {
		t.Fatalf("verdict = %+v, want a loss step", got)
	}
}

func TestEvaluateKeepsASustainedBaselineStable(t *testing.T) {
	t.Parallel()

	history := baseSamples(corridor.MinSamples+2, 100, 0)
	// Replace the whole window with the same value: no step, no noise.
	for i := range history {
		history[i].RTTMS = 100
	}
	if got := corridor.Evaluate(history); got.Step {
		t.Fatalf("a flat window alerted: %+v", got)
	}
}

type fakeProber struct {
	latency time.Duration
	fail    bool
	calls   []string
	err     error
}

func (f *fakeProber) Probe(_ context.Context, address string) (time.Duration, bool) {
	f.calls = append(f.calls, address)
	if f.err != nil {
		return 0, false
	}
	if f.fail {
		return 0, false
	}
	return f.latency, true
}

func TestMeasureTakesTheMedianPerEndpoint(t *testing.T) {
	t.Parallel()

	prober := &fakeProber{latency: 40 * time.Millisecond}
	got := corridor.Measure(context.Background(), []string{"a.example:443", "b.example:443"}, prober)

	if !got.OK || got.RTTMS != 40 {
		t.Fatalf("sample = %+v", got)
	}
	if got.Loss != 0 {
		t.Fatalf("loss = %v, want 0", got.Loss)
	}
	if len(prober.calls) != 2*corridor.Attempts {
		t.Fatalf("calls = %d, want %d", len(prober.calls), 2*corridor.Attempts)
	}
}

func TestMeasureCountsEveryFailedAttempt(t *testing.T) {
	t.Parallel()

	prober := &fakeProber{err: errors.New("refused")}
	got := corridor.Measure(context.Background(), []string{"a.example:443", "b.example:443"}, prober)

	if got.OK {
		t.Fatalf("a total failure produced an RTT: %+v", got)
	}
	if got.Loss != 100 {
		t.Fatalf("loss = %v, want 100", got.Loss)
	}
	if got.RTTMS != 0 {
		t.Fatalf("rtt = %v, want 0", got.RTTMS)
	}
}

func TestMeasureHonoursCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	prober := &fakeProber{latency: time.Millisecond}
	got := corridor.Measure(ctx, []string{"a.example:443"}, prober)
	if len(prober.calls) != 0 {
		t.Fatalf("cancelled measurement still probed %d times", len(prober.calls))
	}
	if got.OK {
		t.Fatalf("cancelled measurement produced a sample: %+v", got)
	}
}
