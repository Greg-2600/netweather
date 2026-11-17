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
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Greg-2600/netweather/internal/classify"
	"github.com/Greg-2600/netweather/internal/dnsdiv"
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
	// Resolvers is the optional set of DNS servers to ask. Empty or a single
	// entry means "just Address"; two or more enable the divergence check
	// (CLASSIFIER.md §3.2).
	Resolvers  []string
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
	NTP  *probe.NTP
	ICMP *probe.ICMP
}

// Run executes the instrument named by the target.
func (i Instruments) Run(ctx context.Context, t Target) probe.Result {
	switch t.Instrument {
	case "tls":
		if i.TLS == nil {
			return probe.Result{Err: fmt.Errorf("agent: tls instrument not configured")}
		}
		r := i.TLS.Do(ctx, t.Address)
		return probe.Result{
			Latency: r.Latency, ErrorClass: r.ErrorClass, Err: r.Err,
			CertIssuer: r.Issuer, CertNotAfter: r.NotAfter,
			CertChainLength: r.ChainLength, CertIntermediateSPKI: r.IntermediateSPKI,
			CertLeafFingerprint: r.LeafFingerprint,
		}
	case "tcp", "tcpping":
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
		return probe.Result{
			Latency: r.Latency, ErrorClass: r.ErrorClass, Err: r.Err,
			Addresses: r.Addresses, CNAMEs: r.CNAMEs,
		}
	case "ntp":
		if i.NTP == nil {
			return probe.Result{Err: fmt.Errorf("agent: ntp instrument not configured")}
		}
		r := i.NTP.Do(ctx, t.Address)
		return probe.Result{Latency: r.RTT, Offset: r.Offset, ErrorClass: r.ErrorClass, Err: r.Err}
	case "icmp":
		if i.ICMP == nil {
			return probe.Result{Err: fmt.Errorf("agent: icmp instrument not configured")}
		}
		r := i.ICMP.Do(ctx, t.Address)
		return probe.Result{Latency: r.RTT, Loss: r.Lost, ErrorClass: r.ErrorClass, Err: r.Err}
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

// Pins learns certificate issuers and chain shapes. store.Store satisfies it.
// Nil disables pinning, which is the right default for a vantage that has no
// business pinning someone else's names.
type Pins interface {
	ObserveCert(ctx context.Context, obs store.CertObservation, at time.Time) (changed bool, advisory string, err error)
	// ObserveLeaf records the leaf fingerprint and reports whether it moved.
	// A leaf change is informational: leaves rotate on a schedule.
	ObserveLeaf(ctx context.Context, targetID, fingerprint string, at time.Time) (changed bool, err error)
}

// Divergence is the per-resolver DNS baseline store. store.Store satisfies it.
// Nil disables the divergence check, which is the right default for a vantage
// that does not configure more than one resolver.
type Divergence interface {
	RecordDNSAnswer(ctx context.Context, targetID string, ans dnsdiv.Answer) error
	DNSBaselines(ctx context.Context, targetID string, since time.Time) (map[string]dnsdiv.Baseline, error)
	RecordDNSDivergence(ctx context.Context, targetID string, v dnsdiv.Verdict, at time.Time) error
}

// Agent runs due targets and records the results.
type Agent struct {
	Runner  Runner
	Sink    Sink
	Limiter *schedule.Limiter
	Now     func() time.Time
	Rand    func() float64
	Logger  *slog.Logger
	// Pins learns certificate issuers and flags a change once. Nil disables it.
	Pins Pins
	// Pace is the minimum gap between two probes in one Step. It is zero for
	// the direct vantage and non-zero for Tor, where bursty traffic is both
	// abusive and a fingerprint.
	Pace time.Duration
	// DNSBlocked reports whether this vantage's egress port 53 is known to be
	// blocked. Nil means unknown, and an unknown path is treated as working:
	// a missing health signal must not suppress real DOWN states.
	DNSBlocked func() bool
	// MTUBlocked reports whether this vantage's egress MTU is below the floor
	// where a large handshake can be black-holed. Nil means unknown, treated as
	// working for the same reason as DNSBlocked.
	MTUBlocked func() bool
	// Suspend reports whether a self-imposed limit (a daily volume budget, for
	// example) has stopped this agent. It is not the operator's pause: the
	// kill switch is Pause/Resume, and this is the vantage's own allowance.
	Suspend func() bool
	// DNSQuery asks one resolver for one name. It is the seam the divergence
	// path uses to probe every configured resolver; nil disables divergence.
	DNSQuery func(ctx context.Context, server, name string) probe.DNSResult
	// Divergence reads and writes the per-resolver DNS baselines. Nil disables
	// divergence.
	Divergence Divergence
	// LookupASN resolves an address's origin ASN for a new answer. Nil leaves a
	// new address weak, which never carries a verdict on its own.
	LookupASN func(ctx context.Context, addr netip.Addr) (int, error)
	// DNSQueryNS asks one resolver for a name's NS set: the parent's
	// delegation. Nil leaves the delegation signal unlearned; the address and
	// RCODE signals still apply.
	DNSQueryNS func(ctx context.Context, server, name string) ([]string, error)

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
	if a.Suspend != nil && a.Suspend() {
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

		if a.Pins != nil && t.Instrument == "tls" && res.CertIssuer != "" {
			changed, advisory, err := a.Pins.ObserveCert(ctx, store.CertObservation{
				TargetID: t.ID, Issuer: res.CertIssuer,
				ChainLength: res.CertChainLength, IntermediateSPKI: res.CertIntermediateSPKI,
			}, now)
			switch {
			case err != nil:
				a.logger().Debug("certificate pin check failed",
					"event", "cert_pin_failed", "component", "agent",
					"target", t.Host, "error", err)
			case changed:
				// A new issuer is accepted and flagged once, never TAMPER on
				// arrival: CDN-fronted sites rotate CAs legitimately.
				res.ErrorClass = "tls_issuer_unexpected"
				res.Err = fmt.Errorf("agent: issuer changed for %s", t.Host)
			case advisory != "":
				// A chain-shape change is informational by design: an
				// intermediate rotation is normal, and the chain is not a
				// verdict (CLASSIFIER.md §3.5).
				a.logger().Info("certificate chain changed",
					"event", "cert_chain_changed", "component", "agent",
					"target", t.Host, "detail", advisory)
			}
		}

		if a.Pins != nil && t.Instrument == "tls" && res.CertLeafFingerprint != "" {
			leafChanged, err := a.Pins.ObserveLeaf(ctx, t.ID, res.CertLeafFingerprint, now)
			switch {
			case err != nil:
				a.logger().Debug("leaf fingerprint check failed",
					"event", "cert_leaf_failed", "component", "agent",
					"target", t.Host, "error", err)
			case leafChanged:
				// Leaf rotation is expected, so this is informational only
				// (CLASSIFIER.md §3.5 step 4).
				a.logger().Info("leaf certificate changed",
					"event", "cert_leaf_changed", "component", "agent",
					"target", t.Host)
			}
		}

		if t.Instrument == "dns" && len(t.Resolvers) > 1 && a.DNSQuery != nil && a.Divergence != nil {
			res = a.divergeDNS(ctx, t, res, now)
		}

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

// divergeDNS asks every configured resolver, records each answer against that
// resolver's own baseline, and reports the ladder's verdict.
//
// The recorded probe stays the primary resolver's, with one exception: if the
// primary failed while another resolver answered, that is our resolver or path
// talking, not the name disappearing, so it becomes a divergence advisory
// instead of DOWN (CLASSIFIER.md §3.2).
func (a *Agent) divergeDNS(ctx context.Context, t Target, primary probe.Result, now time.Time) probe.Result {
	// The baselines are read first: a new address needs the resolver's learned
	// ASN and hosting suffix before the answer can be judged.
	baselines, err := a.Divergence.DNSBaselines(ctx, t.ID, now.Add(-dnsBaselineWindow))
	if err != nil {
		a.logger().Debug("dns baseline read failed",
			"event", "dns_baseline_read_failed", "component", "agent",
			"target", t.Host, "error", err)
		baselines = map[string]dnsdiv.Baseline{}
	}

	answers := make([]dnsdiv.Answer, 0, len(t.Resolvers))
	answered := 0
	for _, server := range t.Resolvers {
		r := a.DNSQuery(ctx, server, t.Query)
		ans := dnsdiv.Answer{
			Resolver:  server,
			At:        now,
			Addresses: dnsAddresses(r),
			Failure:   dnsFailure(r),
		}
		if ans.Failure == "" {
			answered++
			ans.CNAMESuffixes = dnsSuffixes(r.CNAMEs)
			ans.ASNs = a.lookupNewASNs(ctx, r.Addresses, baselines[server])
			if a.DNSQueryNS != nil {
				names, err := a.DNSQueryNS(ctx, server, t.Query)
				if err != nil {
					a.logger().Debug("dns delegation query failed",
						"event", "dns_delegation_failed", "component", "agent",
						"target", t.Host, "resolver", server, "error", err)
				} else {
					ans.Delegations = names
				}
			}
		}
		answers = append(answers, ans)
		if err := a.Divergence.RecordDNSAnswer(ctx, t.ID, ans); err != nil {
			a.logger().Debug("dns baseline write failed",
				"event", "dns_baseline_failed", "component", "agent",
				"target", t.Host, "resolver", server, "error", err)
		}
	}

	verdict := dnsdiv.Evaluate(answers, baselines)
	if verdict.Kind != "" {
		if err := a.Divergence.RecordDNSDivergence(ctx, t.ID, verdict, now); err != nil {
			a.logger().Debug("dns divergence write failed",
				"event", "dns_divergence_write_failed", "component", "agent",
				"target", t.Host, "error", err)
		}
		a.logger().Warn("dns divergence",
			"event", "dns_divergence", "component", "agent",
			"target", t.Host, "verdict", verdict.Kind,
			"confidence", verdict.Confidence, "detail", verdict.Detail)
	}

	if primary.Failed() && answered > 0 {
		primary.ErrorClass = "dns_divergence"
		primary.Err = fmt.Errorf("agent: %s failed while %d resolver(s) answered", t.Resolvers[0], answered)
	}
	return primary
}

// dnsASNLookupsPerProbe caps origin lookups: a resolver returning thirty new
// addresses must not turn one probe into thirty TXT queries.
const dnsASNLookupsPerProbe = 4

// lookupNewASNs resolves the origin ASN only for addresses this resolver has
// never returned. A known address already has its ASN in the baseline, and the
// cap keeps a churny CDN from becoming a lookup amplifier.
func (a *Agent) lookupNewASNs(ctx context.Context, addrs []netip.Addr, base dnsdiv.Baseline) []int {
	if a.LookupASN == nil {
		return nil
	}
	out := make([]int, 0, len(addrs))
	seen := make(map[int]struct{}, len(addrs))
	for _, addr := range addrs {
		if _, known := base.Addresses[addr.String()]; known {
			continue
		}
		if len(out) >= dnsASNLookupsPerProbe {
			break
		}
		asn, err := a.LookupASN(ctx, addr)
		if err != nil || asn <= 0 {
			continue
		}
		if _, dup := seen[asn]; dup {
			continue
		}
		seen[asn] = struct{}{}
		out = append(out, asn)
	}
	return out
}

// dnsSuffixes reduces the CNAME chain to the distinct registrable suffixes the
// divergence rule compares. The suffixes are a hosting label, never a verdict.
func dnsSuffixes(cnames []string) []string {
	out := make([]string, 0, len(cnames))
	seen := make(map[string]struct{}, len(cnames))
	for _, name := range cnames {
		suffix := dnsdiv.RegistrableSuffix(name)
		if suffix == "" {
			continue
		}
		if _, dup := seen[suffix]; dup {
			continue
		}
		seen[suffix] = struct{}{}
		out = append(out, suffix)
	}
	return out
}

// dnsBaselineWindow is the seven days CLASSIFIER.md §3.2 names.
const dnsBaselineWindow = 7 * 24 * time.Hour

func dnsAddresses(r probe.DNSResult) []string {
	if r.Failed() {
		return nil
	}
	out := make([]string, 0, len(r.Addresses))
	for _, addr := range r.Addresses {
		out = append(out, addr.String())
	}
	return out
}

// dnsFailure maps the instrument's class to the divergence engine's vocabulary.
func dnsFailure(r probe.DNSResult) string {
	if r.ErrorClass == "" {
		return ""
	}
	switch class := strings.TrimPrefix(r.ErrorClass, "dns_"); class {
	case "nxdomain", "nodata", "servfail", "refused", "timeout":
		return class
	default:
		return "error"
	}
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
	dnsBlocked := false
	if t.Instrument == "dns" && a.DNSBlocked != nil {
		dnsBlocked = a.DNSBlocked()
	}
	mtuBlocked := false
	if a.MTUBlocked != nil {
		mtuBlocked = a.MTUBlocked()
	}
	decision := decisionFor(t, res, dnsBlocked, mtuBlocked)

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
// handshake is reachability, not a missing HTTP status. dnsBlocked is the
// vantage's port-53 health and mtuBlocked its egress MTU health: a transport
// failure under either measures us, not the target.
func decisionFor(t Target, res probe.Result, dnsBlocked, mtuBlocked bool) classify.Result {
	expect := t.Expect
	if dnsBlocked {
		expect.DNSBlocked = true
	}
	if mtuBlocked {
		expect.MTUBlocked = true
	}
	// NTP is self-health: it can warn about this vantage's clock, but it never
	// says a target is up or down (CLASSIFIER.md §3.6).
	if t.Instrument == "ntp" {
		if res.Err != nil || res.ErrorClass != "" {
			return classify.Decide(expect, classify.Response{
				Err: res.Err, ErrorClass: res.ErrorClass,
			})
		}
		return classify.Result{
			State:      state.Unknown,
			Confidence: classify.Low,
			Reason:     "clock offset " + res.Offset.Round(time.Millisecond).String(),
		}
	}

	if t.Instrument != "" && t.Instrument != "http" && res.Err == nil && res.ErrorClass == "" {
		return classify.Result{
			State:      state.UpReachability,
			Confidence: classify.Low,
			Reason:     "reachability only",
		}
	}
	return classify.Decide(expect, classify.Response{
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
	case strings.HasPrefix(class, "ntp_"):
		return "ntp"
	}
	switch t.Instrument {
	case "tls":
		return "tls"
	case "tcp", "tcpping":
		return "tcp"
	case "ntp":
		return "ntp"
	default:
		return "http"
	}
}
