package web

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"time"

	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/store"
	"github.com/Greg-2600/netweather/internal/streamlimit"
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
	// CountryEvents supplies the world-view feed overlay.
	CountryEvents CountryEvents
	// Corridors supplies the corridor page. Nil shows a note, not an empty page.
	Corridors CorridorStatus
	// IncidentWatcher wakes the UI stream when an incident opens or closes.
	IncidentWatcher board.Watcher
	Coverage        Coverage
	CoverageTrend   CoverageHistory
	Series          Series
	SeriesBatch     SeriesBatch
	Changes         StateChanges
	// StreamLimit caps concurrent SSE connections. Nil uses the defaults.
	StreamLimit   *streamlimit.Limiter
	Control       Control
	Vantage       string
	Token         string
	SecureCookies bool
	Version       string
	StartedAt     time.Time
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
