package ihi

import (
	"context"
	"strings"
	"time"

	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
)

// Component weights from DESIGN.md §11. They sum to 1 so the score is a
// weighted mean, and they are named here rather than at the call site so the
// design document and the code cannot drift silently.
const (
	WeightOwnHealth = 0.35
	WeightExternal  = 0.25
	WeightBGP       = 0.15
	WeightCorridor  = 0.15
	WeightTamper    = 0.10
)

// DefaultWindow is the lookback for external events and tamper signals. It
// matches the incident window and the world overlay: a day of evidence.
const DefaultWindow = 24 * time.Hour

// tamperPenalty is the score lost per divergence verdict in the window. Ten
// verdicts reach zero; past that the component cannot go lower, which is what
// keeps one noisy resolver from dominating the index.
const tamperPenalty = 20.0

// Store is the narrow read surface the builder needs. Keeping it an interface
// means the composition is testable without a database, and the store package
// does not have to know the index exists.
type Store interface {
	Coverage(ctx context.Context, vantageID string, now time.Time) (store.Coverage, error)
	StateCounts(ctx context.Context, vantageID string) (map[state.State]int, error)
	ExternalEvents(ctx context.Context, limit int) ([]store.ExternalEvent, error)
	CorridorStatus(ctx context.Context) ([]store.CorridorStatus, error)
	RecentDNSDivergences(ctx context.Context, limit int) ([]store.DNSDivergenceRecord, error)
	// FeedStates is how the index knows whether the external evidence is
	// current. A dead feed must be dropped rather than scored as quiet
	// (DESIGN.md §8.7).
	FeedStates(ctx context.Context) ([]store.FeedState, error)
}

// Builder assembles the components from live state and computes the index.
type Builder struct {
	Store   Store
	Vantage string
	Now     func() time.Time
	Window  time.Duration
}

// Build reads every component and returns the index. A component that cannot be
// read is not an error: it is dropped with the read's reason, because one
// unavailable panel must not take the summary down with it.
func (b *Builder) Build(ctx context.Context) Index {
	now := time.Now
	if b.Now != nil {
		now = b.Now
	}
	at := now()
	window := b.Window
	if window <= 0 {
		window = DefaultWindow
	}

	// Feed health gates the external components: a poller that has stopped
	// answering is not evidence that nothing is happening (DESIGN.md §8.7).
	feeds := b.feedStates(ctx, at)

	components := []Component{
		b.ownHealth(ctx, at),
		b.external(ctx, at, window, feeds),
		b.bgp(ctx, at, window, feeds),
		b.corridors(ctx, at),
		b.tamper(ctx, at, window),
	}
	return Compute(components, Options{Now: at})
}

// feedStaleAfter is the board's staleness threshold, reused so the index and the
// feed strip never disagree about a dead source.
const feedStaleAfter = 30 * time.Minute

// feedStates returns the health of each feed keyed by name. A failed read is an
// empty map, which drops the external components with a stated reason.
func (b *Builder) feedStates(ctx context.Context, at time.Time) map[string]string {
	states, err := b.Store.FeedStates(ctx)
	if err != nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(states))
	for _, f := range states {
		// A known, healthy feed is present with an empty reason. Absence from
		// the map means the feed is not configured, which is a different
		// statement: silence from a running feed is evidence, silence from a
		// feed that is not running is not.
		out[f.Feed] = ""
		switch {
		case f.LastOKAt.IsZero():
			out[f.Feed] = "feed has never succeeded"
		case at.Sub(f.LastOKAt) > feedStaleAfter:
			out[f.Feed] = "feed is stale"
		case f.ConsecutiveFailures > 0:
			out[f.Feed] = "feed is failing"
		}
	}
	return out
}

// feedReason names why a feed cannot support a component, or "" when it can.
// A feed that is not configured at all is also a reason: the core works with
// zero feeds, but the index must not call the missing evidence healthy.
func feedReason(feeds map[string]string, feed string) string {
	if reason, ok := feeds[feed]; ok {
		return reason
	}
	return "feed is not enabled"
}

func dropped(name string, weight float64, at time.Time, coverage float64, reason string) Component {
	return Component{Name: name, Weight: weight, At: at, Coverage: coverage, Reason: reason}
}

// ownHealth is the share of judgeable targets that are up, weighted by how much
// of the fleet is measurable at all. A target that is blocked or unmeasurable
// is not counted as either healthy or failed: it is not evidence.
func (b *Builder) ownHealth(ctx context.Context, at time.Time) Component {
	const name = "Own health"

	coverage, err := b.Store.Coverage(ctx, b.Vantage, at)
	if err != nil {
		return dropped(name, WeightOwnHealth, at, 1, "unavailable: "+err.Error())
	}
	measurable := 0.0
	if coverage.Totals.Total > 0 {
		measurable = float64(coverage.Totals.Measurable) / float64(coverage.Totals.Total)
	}

	counts, err := b.Store.StateCounts(ctx, b.Vantage)
	if err != nil {
		return dropped(name, WeightOwnHealth, at, measurable, "unavailable: "+err.Error())
	}

	healthy := float64(counts[state.Up] + counts[state.UpReachability])
	healthy += 0.5 * float64(counts[state.Degraded])
	judged := counts[state.Up] + counts[state.UpReachability] + counts[state.Degraded] + counts[state.Down]
	if judged == 0 {
		return dropped(name, WeightOwnHealth, at, measurable, "no judgeable targets")
	}

	return Component{
		Name:     name,
		Weight:   WeightOwnHealth,
		Score:    100 * healthy / float64(judged),
		At:       at,
		OK:       true,
		Coverage: measurable,
	}
}

// external is the highest-severity corroborated outage in the window. No event
// is a healthy 100, because this component answers "is something large
// happening", and silence from a healthy IODA feed is the answer "not that we
// know of". A missing or dead feed is not silence: the component is dropped.
func (b *Builder) external(ctx context.Context, at time.Time, window time.Duration, feeds map[string]string) Component {
	return b.eventComponent(ctx, at, window, "External outage severity", WeightExternal, "ioda",
		func(store.ExternalEvent) bool { return true }, feeds)
}

// bgp is the same shape restricted to routing events: a hijack or a leak is a
// different failure from an outage, and DESIGN.md weights it separately. Its
// evidence comes from Radar, so its gate is Radar's health.
func (b *Builder) bgp(ctx context.Context, at time.Time, window time.Duration, feeds map[string]string) Component {
	return b.eventComponent(ctx, at, window, "BGP stability", WeightBGP, "radar",
		func(e store.ExternalEvent) bool {
			kind := strings.ToLower(e.Kind)
			return strings.Contains(kind, "hijack") || strings.Contains(kind, "leak")
		}, feeds)
}

func (b *Builder) eventComponent(ctx context.Context, at time.Time, window time.Duration,
	name string, weight float64, feed string,
	keep func(store.ExternalEvent) bool, feeds map[string]string,
) Component {
	// A feed that has never answered, has gone stale, or is failing cannot
	// support a score. Dropping it is the difference between "nothing is
	// happening" and "we do not know whether anything is happening".
	if reason := feedReason(feeds, feed); reason != "" {
		return dropped(name, weight, at, 1, reason)
	}

	events, err := b.Store.ExternalEvents(ctx, 500)
	if err != nil {
		return dropped(name, weight, at, 1, "unavailable: "+err.Error())
	}

	worst := 0.0
	for _, e := range events {
		if !keep(e) || !inWindow(e, at, window) {
			continue
		}
		if e.Severity > worst {
			worst = e.Severity
		}
	}
	// Severity is 0-1 upstream; clamp before it reaches the score so a feed
	// that sends 8.7 cannot push the index negative.
	if worst > 1 {
		worst = 1
	}

	// The timestamp is when the evidence was read, not when the newest event
	// happened: "no event in the last day" is a current answer, and an event
	// from three hours ago is still inside the window. A feed that has stopped
	// answering is a feed-health problem, shown separately, not something this
	// component can infer from an absence.
	return Component{
		Name: name, Weight: weight, Score: 100 * (1 - worst),
		At: at, OK: true, Coverage: 1,
	}
}

// corridors scores the share of configured paths with no open step change. A
// deployment without corridors drops the component rather than scoring it 100:
// not measuring a path is not the same as it being healthy.
func (b *Builder) corridors(ctx context.Context, at time.Time) Component {
	const name = "Corridor stability"

	statuses, err := b.Store.CorridorStatus(ctx)
	if err != nil {
		return dropped(name, WeightCorridor, at, 1, "unavailable: "+err.Error())
	}
	if len(statuses) == 0 {
		return dropped(name, WeightCorridor, at, 1, "no corridors configured")
	}

	healthy := 0
	newest := time.Time{}
	for _, status := range statuses {
		if status.Open == nil {
			healthy++
		}
		if status.Latest != nil && status.Latest.TS.After(newest) {
			newest = status.Latest.TS
		}
	}
	if newest.IsZero() {
		return dropped(name, WeightCorridor, at, 1, "no corridor samples")
	}

	return Component{
		Name: name, Weight: WeightCorridor,
		Score: 100 * float64(healthy) / float64(len(statuses)),
		At:    newest, OK: true, Coverage: 1,
	}
}

// tamper scores DNS divergence verdicts in the window. Cert-issuer changes are
// enforced and shown in the drawer, but they are stored as pins, not as a dated
// signal series, so they cannot feed a windowed component honestly; DESIGN.md
// §11 states that limit instead of counting a table that does not exist.
func (b *Builder) tamper(ctx context.Context, at time.Time, window time.Duration) Component {
	const name = "Tamper signals"

	records, err := b.Store.RecentDNSDivergences(ctx, 200)
	if err != nil {
		return dropped(name, WeightTamper, at, 1, "unavailable: "+err.Error())
	}

	signals := 0
	for _, rec := range records {
		if rec.TS.Before(at.Add(-window)) {
			continue
		}
		verdict := strings.ToUpper(rec.Verdict)
		if verdict != "TAMPER" && verdict != "DNS_ANOMALY" {
			continue
		}
		signals++
	}

	score := 100 - tamperPenalty*float64(signals)
	if score < 0 {
		score = 0
	}
	// Read time, like the event components: the count is over a fixed window,
	// so the answer is current even when the newest verdict is hours old.
	return Component{
		Name: name, Weight: WeightTamper, Score: score,
		At: at, OK: true, Coverage: 1,
	}
}

// inWindow accepts an event that started or ended inside the lookback. A long
// outage that began yesterday and is still open is the most important kind of
// event, so an ended-before check alone would hide it.
func inWindow(e store.ExternalEvent, at time.Time, window time.Duration) bool {
	cutoff := at.Add(-window)
	if !e.EndedAt.IsZero() {
		return !e.EndedAt.Before(cutoff)
	}
	if !e.StartedAt.IsZero() {
		return !e.StartedAt.Before(cutoff)
	}
	return !e.ObservedAt.Before(cutoff)
}
