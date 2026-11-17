// Package agent ties an instrument to the classifier and the store. It is the
// first end-to-end path: probe -> classify -> record.
//
// The loop is deliberately a single deterministic step rather than a goroutine
// soup, so the scheduler rules can be tested with an injected clock and no
// sleeping.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Greg-2600/netweather/internal/classify"
	"github.com/Greg-2600/netweather/internal/probe"
	"github.com/Greg-2600/netweather/internal/schedule"
	"github.com/Greg-2600/netweather/internal/store"
)

// Target is one configured probe.
type Target struct {
	ID         string
	Name       string
	Host       string
	URL        string
	Vantage    string
	Tier       schedule.Tier
	Interval   time.Duration
	Expect     classify.Expected
	ICMPAnchor bool
}

// Prober is the instrument the agent drives. probe.HTTP satisfies it.
type Prober interface {
	Do(ctx context.Context, target string) probe.Result
}

// Sink persists a batch. store.Store satisfies it.
type Sink interface {
	Record(ctx context.Context, probes []store.Probe) error
}

// Agent runs due targets and records the results.
type Agent struct {
	Targets []Target
	Prober  Prober
	Sink    Sink
	Limiter *schedule.Limiter
	Now     func() time.Time
	Rand    func() float64
	Logger  *slog.Logger

	next   map[string]time.Time
	primed bool
	paused atomic.Bool
}

// New builds an agent. Now and Rand default to the real clock and math/rand;
// tests override them before the first Step.
func New(targets []Target, prober Prober, sink Sink) *Agent {
	return &Agent{
		Targets: targets,
		Prober:  prober,
		Sink:    sink,
		Limiter: schedule.NewLimiter(),
		Now:     time.Now,
		Rand:    rand.Float64,
		next:    make(map[string]time.Time, len(targets)),
	}
}

// Step probes every target that is due now and records one batch. It returns how
// many targets ran. A paused agent does nothing and schedules nothing.
func (a *Agent) Step(ctx context.Context) (int, error) {
	if a.paused.Load() {
		return 0, nil
	}

	now := a.now()
	if !a.primed {
		a.prime(now)
		a.primed = true
	}

	batch := make([]store.Probe, 0, len(a.Targets))
	ran := 0

	for _, t := range a.Targets {
		if !a.due(t, now) {
			continue
		}

		release, ok := a.Limiter.TryAcquire(t.Host)
		if !ok {
			// Another probe for the same registrable domain is in flight.
			continue
		}

		res := a.Prober.Do(ctx, t.URL)
		release()

		p := a.toProbe(t, res, now)
		a.logProbe(t, p)
		batch = append(batch, p)
		a.next[t.ID] = now.Add(schedule.Next(t.Interval, schedule.DefaultJitter(t.Tier), a.Rand))
		ran++
	}

	if len(batch) == 0 {
		return 0, nil
	}
	if err := a.Sink.Record(ctx, batch); err != nil {
		return ran, fmt.Errorf("agent: record: %w", err)
	}
	return ran, nil
}

// NextRun reports when a target is next due. Zero means "never scheduled".
func (a *Agent) NextRun(id string) time.Time { return a.next[id] }

// Pause stops probing. The agent keeps running so the UI and API stay
// available; it simply stops emitting outbound traffic.
func (a *Agent) Pause() {
	a.paused.Store(true)
	a.logger().Warn("probing paused", "event", "probe_paused", "component", "agent")
}

// Resume re-arms probing from the next Step.
func (a *Agent) Resume() {
	a.paused.Store(false)
	a.logger().Info("probing resumed", "event", "probe_resumed", "component", "agent")
}

// Paused reports whether probing is currently suppressed.
func (a *Agent) Paused() bool { return a.paused.Load() }

func (a *Agent) now() time.Time {
	if a.Now == nil {
		return time.Now()
	}
	return a.Now()
}

func (a *Agent) logger() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}

// logProbe emits the decision record from LOGGING.md §3 at DEBUG. One line per
// probe is the right volume for an opt-in level; the state change itself is
// logged by the store/incident layer at INFO.
func (a *Agent) logProbe(t Target, p store.Probe) {
	a.logger().Debug("probe",
		"event", "probe",
		"component", "agent",
		"target", t.Host,
		"vantage", t.Vantage,
		"state", string(p.State),
		"stage", p.Stage,
		"error_class", p.ErrorClass,
		"http_status", p.HTTPStatus,
		"latency_ms", p.LatencyMS,
		"confidence", p.Confidence,
		"reason", p.Reason,
	)
}

func (a *Agent) prime(now time.Time) {
	for _, t := range a.Targets {
		if _, ok := a.next[t.ID]; ok {
			continue
		}
		// Cold start: spread first runs across the first interval so 750 targets
		// do not fire in the same second (DESIGN.md §6.5).
		a.next[t.ID] = now.Add(schedule.FirstRun(t.Interval, a.Rand))
	}
}

func (a *Agent) due(t Target, now time.Time) bool {
	at, ok := a.next[t.ID]
	if !ok {
		return true
	}
	return !now.Before(at)
}

func (a *Agent) toProbe(t Target, res probe.Result, now time.Time) store.Probe {
	decision := classify.Decide(t.Expect, classify.Response{
		Err:        res.Err,
		ErrorClass: res.ErrorClass,
		Status:     res.Status,
		Header:     res.Header,
		Body:       res.Body,
		FinalURL:   res.FinalURL,
	})

	detail := decision.Reason
	if res.Err != nil {
		detail = res.ErrorClass + ": " + res.Err.Error()
	}

	return store.Probe{
		TargetID:   t.ID,
		VantageID:  t.Vantage,
		TS:         now,
		State:      decision.State,
		Stage:      stageFor(res.ErrorClass),
		ErrorClass: res.ErrorClass,
		HTTPStatus: res.Status,
		LatencyMS:  int(res.Latency / time.Millisecond),
		Confidence: string(decision.Confidence),
		Detail:     detail,
		Reason:     decision.Reason,
	}
}

func stageFor(class string) string {
	switch {
	case strings.HasPrefix(class, "dns_"):
		return "dns"
	case strings.HasPrefix(class, "tcp_"):
		return "tcp"
	case strings.HasPrefix(class, "tls_"):
		return "tls"
	default:
		return "http"
	}
}
