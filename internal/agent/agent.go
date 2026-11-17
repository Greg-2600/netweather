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
	"sync"
	"sync/atomic"
	"time"

	"github.com/Greg-2600/netweather/internal/classify"
	"github.com/Greg-2600/netweather/internal/probe"
	"github.com/Greg-2600/netweather/internal/schedule"
	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
)

// Target is one configured probe.
type Target struct {
	ID         string
	Name       string
	Host       string
	URL        string
	Address    string // host:port for tls/tcp/dns
	Query      string // qname for dns
	Instrument string // http, tls, tcp, dns
	Vantage    string
	Tier       schedule.Tier
	Interval   time.Duration
	Expect     classify.Expected
	ICMPAnchor bool
}

// Runner executes one target with the right instrument. agent.Instruments is the
// production implementation; tests inject a fake.
type Runner interface {
	Run(ctx context.Context, t Target) probe.Result
}

// Instruments dispatches by target type. A nil instrument is a configuration
// error, not a silent success.
type Instruments struct {
	HTTP *probe.HTTP
	TLS  *probe.TLS
	TCP  *probe.TCP
	DNS  *probe.DNS
}

// Run executes the instrument named by the target.
func (i Instruments) Run(ctx context.Context, t Target) probe.Result {
	switch t.Instrument {
	case "tls":
		if i.TLS == nil {
			return probe.Result{Err: fmt.Errorf("agent: tls instrument not configured")}
		}
		r := i.TLS.Do(ctx, t.Address)
		return probe.Result{Latency: r.Latency, ErrorClass: r.ErrorClass, Err: r.Err}
	case "tcp":
		if i.TCP == nil {
			return probe.Result{Err: fmt.Errorf("agent: tcp instrument not configured")}
		}
		r := i.TCP.Do(ctx, t.Address)
		return probe.Result{Latency: r.Latency, ErrorClass: r.ErrorClass, Err: r.Err}
	case "dns":
		if i.DNS == nil {
			return probe.Result{Err: fmt.Errorf("agent: dns instrument not configured")}
		}
		r := i.DNS.Do(ctx, t.Address, t.Query)
		return probe.Result{Latency: r.Latency, ErrorClass: r.ErrorClass, Err: r.Err}
	default:
		if i.HTTP == nil {
			return probe.Result{Err: fmt.Errorf("agent: http instrument not configured")}
		}
		return i.HTTP.Do(ctx, t.URL)
	}
}

// Sink persists a batch. store.Store satisfies it.
type Sink interface {
	Record(ctx context.Context, probes []store.Probe) error
}

// Agent runs due targets and records the results.
type Agent struct {
	Runner  Runner
	Sink    Sink
	Limiter *schedule.Limiter
	Now     func() time.Time
	Rand    func() float64
	Logger  *slog.Logger
	// Pace is the minimum gap between two probes in one Step. It is zero for
	// the direct vantage and non-zero for Tor, where bursty traffic is both
	// abusive and a fingerprint.
	Pace time.Duration

	mu      sync.RWMutex
	targets []Target
	next    map[string]time.Time
	primed  bool
	paused  atomic.Bool
}

// New builds an agent. Now and Rand default to the real clock and math/rand;
// tests override them before the first Step.
func New(targets []Target, runner Runner, sink Sink) *Agent {
	return &Agent{
		targets: targets,
		Runner:  runner,
		Sink:    sink,
		Limiter: schedule.NewLimiter(),
		Now:     time.Now,
		Rand:    rand.Float64,
		next:    make(map[string]time.Time, len(targets)),
	}
}

// SetTargets replaces the target set at runtime. Existing schedules are kept;
// removed targets lose their next-run entry, and new targets are spread across
// their first interval rather than fired immediately.
func (a *Agent) SetTargets(targets []Target) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.targets = targets

	live := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		live[t.ID] = struct{}{}
	}
	for id := range a.next {
		if _, ok := live[id]; !ok {
			delete(a.next, id)
		}
	}

	if !a.primed {
		return
	}
	now := a.now()
	for _, t := range targets {
		if _, ok := a.next[t.ID]; ok {
			continue
		}
		a.next[t.ID] = now.Add(schedule.FirstRun(t.Interval, a.Rand))
	}
}

// TargetCount reports the current target set size.
func (a *Agent) TargetCount() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.targets)
}

// TargetIDs reports the current ids, for diagnostics and reload diffs.
func (a *Agent) TargetIDs() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]string, 0, len(a.targets))
	for _, t := range a.targets {
		out = append(out, t.ID)
	}
	return out
}

// Step probes every target that is due now and records one batch. It returns how
// many targets ran. A paused agent does nothing and schedules nothing.
func (a *Agent) Step(ctx context.Context) (int, error) {
	if a.paused.Load() {
		return 0, nil
	}

	now := a.now()

	a.mu.Lock()
	if !a.primed {
		a.primeLocked(now)
		a.primed = true
	}
	pending := make([]Target, 0, len(a.targets))
	for _, t := range a.targets {
		if at, ok := a.next[t.ID]; ok && now.Before(at) {
			continue
		}
		pending = append(pending, t)
	}
	a.mu.Unlock()

	batch := make([]store.Probe, 0, len(pending))
	ran := 0

	for _, t := range pending {
		release, ok := a.Limiter.TryAcquire(t.Host)
		if !ok {
			// Another probe for the same registrable domain is in flight.
			continue
		}

		res := a.Runner.Run(ctx, t)
		release()

		p := a.toProbe(t, res, now)
		a.logProbe(t, p)
		batch = append(batch, p)

		a.mu.Lock()
		a.next[t.ID] = now.Add(schedule.Next(t.Interval, schedule.DefaultJitter(t.Tier), a.Rand))
		a.mu.Unlock()
		ran++

		if a.Pace > 0 {
			select {
			case <-ctx.Done():
				return ran, ctx.Err()
			case <-time.After(a.Pace):
			}
		}
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
func (a *Agent) NextRun(id string) time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.next[id]
}

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

func (a *Agent) primeLocked(now time.Time) {
	for _, t := range a.targets {
		if _, ok := a.next[t.ID]; ok {
			continue
		}
		// Cold start: spread first runs across the first interval so 750 targets
		// do not fire in the same second (DESIGN.md §6.5).
		a.next[t.ID] = now.Add(schedule.FirstRun(t.Interval, a.Rand))
	}
}

func (a *Agent) toProbe(t Target, res probe.Result, now time.Time) store.Probe {
	decision := decisionFor(t, res)

	detail := decision.Reason
	if res.Err != nil {
		detail = res.ErrorClass + ": " + res.Err.Error()
	}

	return store.Probe{
		TargetID:   t.ID,
		VantageID:  t.Vantage,
		TS:         now,
		State:      decision.State,
		Stage:      stageFor(t, res.ErrorClass),
		ErrorClass: res.ErrorClass,
		HTTPStatus: res.Status,
		LatencyMS:  int(res.Latency / time.Millisecond),
		Confidence: string(decision.Confidence),
		Detail:     detail,
		Reason:     decision.Reason,
	}
}

// decisionFor gives non-HTTP success its own answer. A completed TCP or TLS
// handshake is reachability, not a missing HTTP status.
func decisionFor(t Target, res probe.Result) classify.Result {
	if t.Instrument != "" && t.Instrument != "http" && res.Err == nil && res.ErrorClass == "" {
		return classify.Result{
			State:      state.UpReachability,
			Confidence: classify.Low,
			Reason:     "reachability only",
		}
	}
	return classify.Decide(t.Expect, classify.Response{
		Err:        res.Err,
		ErrorClass: res.ErrorClass,
		Status:     res.Status,
		Header:     res.Header,
		Body:       res.Body,
		FinalURL:   res.FinalURL,
	})
}

func stageFor(t Target, class string) string {
	switch {
	case strings.HasPrefix(class, "dns_"):
		return "dns"
	case strings.HasPrefix(class, "tcp_"):
		return "tcp"
	case strings.HasPrefix(class, "tls_"):
		return "tls"
	}
	switch t.Instrument {
	case "tls":
		return "tls"
	case "tcp":
		return "tcp"
	default:
		return "http"
	}
}
