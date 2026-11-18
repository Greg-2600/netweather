package web

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"net/netip"
	"time"

	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/ihi"
	"github.com/Greg-2600/netweather/internal/logging"
	"github.com/Greg-2600/netweather/internal/probe"
	"github.com/Greg-2600/netweather/internal/store"
	"github.com/Greg-2600/netweather/internal/streamlimit"
	"github.com/Greg-2600/netweather/internal/tracebudget"
	"github.com/Greg-2600/netweather/internal/verdict"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Deps is what the UI needs. Source is required; Watcher is optional, and
// without it the page simply does not receive live deltas.
type Deps struct {
	Logger    *slog.Logger
	Source    board.Source
	Watcher   board.Watcher
	History   History
	Incidents IncidentHistory
	// IncidentProbes reads the probes pinned around an incident. Nil means the
	// panel states that the archive is not wired.
	IncidentProbes IncidentProbeReader
	// Corroboration supplies external feed events for an incident.
	Corroboration Corroboration
	// Annotator records the operator's ground truth on an incident. Nil hides
	// the annotation form rather than showing a form that cannot save.
	Annotator IncidentAnnotator
	// Feeds supplies feed health for the board's staleness strip. Nil means the
	// strip is omitted rather than shown as healthy.
	Feeds FeedHealth
	// TargetCorroboration supplies feed events for a target's country or ASN.
	TargetCorroboration TargetCorroboration
	// Provider supplies events from a target's declared status page.
	Provider ProviderStatus
	// CountryEvents supplies the world-view feed overlay.
	CountryEvents CountryEvents
	// Corridors supplies the corridor page. Nil shows a note, not an empty page.
	Corridors CorridorStatus
	// Health computes the Internet Health Index for the board chip and the
	// /ihi page. Nil omits both rather than showing a zero.
	Health HealthIndex
	// Verdict computes the one global verdict for the top bar. Nil omits the
	// badge rather than inventing an OK.
	Verdict VerdictSource
	// Anomalies supplies detector advisories for the board. Nil omits the
	// coverage advisory rather than implying coverage is steady.
	Anomalies AnomalyReader
	// Vantages supplies every vantage's state for a target, so the drawer can
	// show a regional disagreement rather than calling it global.
	Vantages VantageStates
	// TargetControl stops probing one target at runtime. It is the abuse-response
	// path from ACCEPTABLE_USE.md §7: the row and its history stay, the scheduler
	// drops the target on the next tick, and a config reload can bring it back.
	// Nil disables the action with a stated reason rather than a silent success.
	TargetControl TargetControl
	// Provenance supplies where a target came from, so the drawer can show the
	// seed or config payload that produced it (THREAT_MODEL.md T35). Nil omits
	// the panel rather than implying the target has no source.
	Provenance ProvenanceReader
	// Fleet supplies the overview line's judged health and learned band. Nil
	// omits the line rather than inventing a band.
	Fleet FleetReader
	// VantageChanges supplies the newest egress move, so the board can explain
	// why its baselines reset. Nil omits the note.
	VantageChanges VantageChangeReader
	// ConfigReload reports the last rejected config edit, so the board can say
	// that an edit did not take effect (THREAT_MODEL.md T36). Nil omits it.
	ConfigReload ConfigReloadStatus
	// DNSObservations supplies the board's collapsed DNS divergence strip.
	DNSObservations DNSObservations
	// Logs supplies the ring and logging setup for the diagnostics page and
	// the drawer's Why panel (LOGGING.md §9).
	Logs LogDiagnostics
	// Budget reports the direct vantage's daily egress allowance so the board
	// can explain a hard stop instead of showing stale tiles as if they were
	// fresh (ACCEPTABLE_USE.md §4). Nil means no budget.
	Budget BudgetState
	// TorBudget reports the Tor vantage's daily volume allowance. Nil means Tor
	// is not configured; a configured budget is shown so a paused corroboration
	// source is visible (DESIGN.md §5.7).
	TorBudget BudgetState
	// IncidentWatcher wakes the UI stream when an incident opens or closes.
	IncidentWatcher board.Watcher
	Coverage        Coverage
	CoverageTrend   CoverageHistory
	Series          Series
	SeriesBatch     SeriesBatch
	Changes         StateChanges
	// TraceEnabled gates the drawer's on-demand traceroute, the same switch the
	// API uses. False renders the reason instead of a button that cannot work.
	TraceEnabled bool
	// Tracer runs one walk and Resolver turns the target host into an address.
	// Both nil disables the action with a stated reason.
	Tracer   Tracer
	Resolver HostResolver
	// TraceBudget is the instance-wide trace governor shared with the API.
	// Nil disables the action rather than creating a second, additive budget.
	TraceBudget *tracebudget.Governor
	// StreamLimit caps concurrent SSE connections. Nil uses the defaults.
	StreamLimit *streamlimit.Limiter
	// TrustedProxies are the peers whose X-Forwarded-For is believed for
	// per-client limits and log lines. Empty means trust nobody, which is the
	// safe default (NETWORKING.md §7.3).
	TrustedProxies []netip.Prefix
	Control        Control
	Vantage        string
	Token          string
	SecureCookies  bool
	Version        string
	StartedAt      time.Time
}

// Coverage supplies the blind-spot panel. Nil means the page says so rather
// than rendering an empty panel that looks like full visibility.
type Coverage interface {
	Coverage(ctx context.Context, vantageID string, now time.Time) (store.Coverage, error)
}

// CoverageHistory supplies the daily trend. Nil means the page says history is
// not configured rather than drawing an empty chart.
type CoverageHistory interface {
	CoverageHistory(ctx context.Context, scope, value string, days int) ([]store.CoveragePoint, error)
}

// Series supplies rollup buckets for the target drawer's 24-hour strip.
type Series interface {
	Series(ctx context.Context, targetID, vantageID, resolution string, from, to time.Time) ([]store.Rollup, error)
}

// SeriesBatch supplies many targets' history in one query. It is optional so a
// small deployment or a test can render the board without it; the comfortable
// density omits the sparkline rather than issuing one query per tile.
type SeriesBatch interface {
	SeriesForTargets(ctx context.Context, targetIDs []string, vantageID, resolution string, from, to time.Time) (map[string][]store.Rollup, error)
}

// StateChanges supplies the target drawer's transition history.
type StateChanges interface {
	StateChanges(ctx context.Context, targetID, vantageID string, limit int) ([]store.StateChange, error)
}

// History supplies the drawer's recent observations. Nil means the page says so
// instead of pretending the target has never been probed.
type History interface {
	RecentProbes(ctx context.Context, targetID, vantageID string, limit int) ([]store.ProbeRow, error)
}

// IncidentHistory supplies the incident screens.
type IncidentHistory interface {
	Incidents(ctx context.Context, openOnly bool, limit int) ([]store.Incident, error)
	IncidentByID(ctx context.Context, id string) (store.Incident, []store.Attribution, []string, error)
}

// IncidentProbeReader reads the probes pinned around an incident (SCHEMA.md
// §5). Nil means the panel says the archive is not wired rather than showing an
// empty table.
type IncidentProbeReader interface {
	IncidentProbes(ctx context.Context, incidentID string, limit int) ([]store.ProbeRow, error)
}

// Corroboration supplies external feed events for an incident. Nil omits the
// panel rather than implying nothing else was happening.
type Corroboration interface {
	Corroboration(ctx context.Context, inc store.Incident, now time.Time, limit int) ([]store.ExternalEvent, error)
}

// FeedHealth supplies feed health for the board's staleness strip. A feed that
// has stopped answering must be visible on the board, not only in the API
// (DESIGN.md P5).
type FeedHealth interface {
	FeedStates(ctx context.Context) ([]store.FeedState, error)
}

// TargetCorroboration supplies feed events for a target's country or ASN. Nil
// means the drawer says so rather than implying nothing is happening.
type TargetCorroboration interface {
	ScopeCorroboration(ctx context.Context, country string, asn int, since time.Time, limit int) ([]store.ExternalEvent, error)
}

// ProviderStatus supplies events from a target's declared status page. It is a
// provider match, unlike TargetCorroboration's country/ASN match (DESIGN.md
// §10.5). Nil means the drawer says so rather than implying the provider is
// healthy.
type ProviderStatus interface {
	ProviderCorroboration(ctx context.Context, statusURL string, since time.Time, limit int) ([]store.ExternalEvent, error)
}

// CountryEvents supplies one feed event per country for the world view overlay.
// Nil means the tiles render without an overlay rather than with a fake one.
type CountryEvents interface {
	RecentCountryEvents(ctx context.Context, since time.Time) (map[string]store.ExternalEvent, error)
}

// CorridorStatus supplies the corridor page: each corridor's latest sample,
// open step change, and the samples behind its sparkline (DESIGN.md §9/§10.4).
type CorridorStatus interface {
	CorridorStatus(ctx context.Context) ([]store.CorridorStatus, error)
	CorridorSeries(ctx context.Context, since time.Time) (map[string][]store.CorridorSample, error)
}

// HealthIndex computes the Internet Health Index (DESIGN.md §11). Nil omits the
// board chip and the page rather than rendering a zero as if it were health.
type HealthIndex interface {
	Build(ctx context.Context) ihi.Index
}

// VerdictSource computes the one global verdict from DESIGN.md §3.3. Nil omits
// the top-bar badge rather than inventing a calm OK.
type VerdictSource interface {
	Build(ctx context.Context) verdict.Result
}

// AnomalyReader supplies detector output for the board's advisories. A coverage
// collapse is an advisory, never an incident (DESIGN.md §7.2), so the board has
// to be able to say it without turning a tile red.
type AnomalyReader interface {
	Anomalies(ctx context.Context, limit int) ([]store.Anomaly, error)
}

// VantageStates supplies every vantage's confirmed state for a target. It is
// how a REGIONAL disagreement is shown rather than called global (DESIGN.md
// §7.1). Nil omits the panel rather than implying one path.
type VantageStates interface {
	TargetVantages(ctx context.Context, targetID string) ([]store.VantageState, error)
}

// ProvenanceReader supplies where a target came from. The drawer shows the
// source and payload hash so a poisoned list can be traced (THREAT_MODEL.md
// T35). Nil omits the panel.
type ProvenanceReader interface {
	TargetSources(ctx context.Context, targetID string) ([]store.TargetSource, error)
}

// TargetControl stops probing one target at runtime. probe netx/agent satisfy
// it through the closure in cmd/netweather. Nil disables the action with a
// stated reason rather than a silent success.
type TargetControl interface {
	DisableTarget(ctx context.Context, id string) (bool, error)
}

// TargetControlFunc adapts a plain function to TargetControl, the way
// http.HandlerFunc adapts a function to http.Handler.
type TargetControlFunc func(ctx context.Context, id string) (bool, error)

func (f TargetControlFunc) DisableTarget(ctx context.Context, id string) (bool, error) {
	return f(ctx, id)
}

// Tracer runs one on-demand traceroute. probe.Trace satisfies it.
type Tracer interface {
	Run(ctx context.Context, dst netip.Addr) probe.TraceResult
}

// HostResolver turns a target hostname into addresses for a trace. netx.Dialer
// satisfies it, so a trace resolves through the same guarded path as probes.
type HostResolver interface {
	CheckHost(ctx context.Context, host string) ([]netip.Addr, error)
}

// FleetReader supplies the overview line from DESIGN.md §10.1. Nil omits the
// line; a nil band is shown as "learning n/28" rather than a guessed range.
type FleetReader interface {
	FleetHealth(ctx context.Context, vantageID string, now time.Time) (store.FleetHealth, error)
}

// VantageChangeReader supplies the newest detected egress move so the board can
// say why its baselines reset (DESIGN.md §7.5). Nil omits the note.
type VantageChangeReader interface {
	LatestVantageChange(ctx context.Context) (store.VantageChange, bool, error)
}

// ConfigReloadStatus reports the last rejected config reload. It exists so a
// typo is visible on the board and not only in the log (THREAT_MODEL.md T36).
type ConfigReloadStatus interface {
	LastReloadFailure() (time.Time, string, bool)
}

// DNSObservations supplies the board's collapsed DNS divergence strip. Nil
// omits the strip rather than implying nothing was observed.
type DNSObservations interface {
	RecentDNSDivergences(ctx context.Context, limit int) ([]store.DNSDivergenceRecord, error)
}

// LogDiagnostics supplies the in-memory ring and the logging setup for the
// in-app diagnostics surfaces (LOGGING.md §9). Nil omits those surfaces; the
// release image has no shell, so an instance that never wires it has no way to
// read its own logs but the host's.
type LogDiagnostics interface {
	LogRecords(minLevel slog.Level, contains string, limit int) []logging.Record
	Diagnostics() logging.Diagnostics
	ActiveScopes() []logging.Scope
	// Subscribe feeds the SSE log stream. A closed channel means no ring.
	Subscribe() (<-chan logging.Record, func())
}

// BudgetState reports the direct vantage's daily egress allowance. Nil means
// no budget is configured. A spent budget is a hard stop, so the board says so
// rather than leaving stale tiles unexplained (ACCEPTABLE_USE.md §4).
type BudgetState interface {
	Used() int64
	Allowance() int64
	Exhausted() bool
}

// IncidentAnnotator records the operator's ground truth for an incident.
type IncidentAnnotator interface {
	AnnotateIncident(ctx context.Context, id, annotation string, at time.Time) error
}

// Control is the probe kill switch, shared with the API.
type Control interface {
	Pause()
	Resume()
	Paused() bool
}

func loadTemplates() (*template.Template, error) {
	return template.ParseFS(templateFS, "templates/*.html")
}

var errNoSource = errors.New("web: Source is required")
