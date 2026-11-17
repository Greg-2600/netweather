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
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Deps is what the UI needs. Source is required; Watcher is optional, and
// without it the page simply does not receive live deltas.
type Deps struct {
	Logger        *slog.Logger
	Source        board.Source
	Watcher       board.Watcher
	History       History
	Incidents     IncidentHistory
	Coverage      Coverage
	CoverageTrend CoverageHistory
	Series        Series
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
