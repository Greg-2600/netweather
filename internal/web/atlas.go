package web

import (
	"context"
	"net/http"
	"strings"

	"github.com/Greg-2600/netweather/internal/region"
)

// atlasView is the read model for /atlas: every existing page's data in one
// map-first view. Each field is produced by the same builder its own page uses,
// so a number on the atlas cannot drift from the number on /map, /world, /hubs,
// /corridors, /coverage, /incidents, /feeds, /ihi, or /diagnostics.
type atlasView struct {
	Insecure bool
	NoAuth   bool
	Version  string
	CSRF     string

	Board       boardView
	Map         mapView
	World       worldView
	Hubs        hubsView
	Corridors   corridorsView
	Coverage    coverageView
	Incidents   incidentListView
	Feeds       feedsView
	IHI         ihiView
	Diagnostics diagnosticsView

	// Optional readers are stated, never rendered as a healthy empty result.
	CoverageNote  string
	IncidentsNote string
	FeedsNote     string

	// RecordSample keeps the diagnostics section bounded on a page that already
	// renders the whole fleet; the full ring stays on /diagnostics.
	RecordSample []diagnosticsRecord
}

// buildAtlasView assembles the combined page from the shared builders. A hard
// failure is only possible for the board snapshot, which every section needs;
// optional providers degrade to an explicit note instead of an empty panel.
func (s *server) buildAtlasView(ctx context.Context, params boardParams) (atlasView, error) {
	view := atlasView{Version: s.deps.Version}

	boardData, err := s.buildBoardView(ctx, params)
	if err != nil {
		return atlasView{}, err
	}
	view.Board = boardData

	mapData, err := s.buildMapData(ctx, "health")
	if err != nil {
		return atlasView{}, err
	}
	view.Map = mapData

	worldData, err := s.buildWorldData(ctx)
	if err != nil {
		return atlasView{}, err
	}
	view.World = worldData

	hubsData, err := s.buildHubsData(ctx)
	if err != nil {
		return atlasView{}, err
	}
	view.Hubs = hubsData

	corridorData, err := s.buildCorridorsData(ctx)
	if err != nil {
		// The corridor builder records provider failures as notes. Keep the
		// section renderable if a future hard failure appears.
		corridorData.Note = "Corridors are temporarily unavailable."
	}
	view.Corridors = corridorData

	coverageData, err := s.buildCoverageData(ctx)
	if err != nil {
		view.CoverageNote = "Coverage is not configured or temporarily unavailable on this instance."
	} else {
		view.Coverage = coverageData
	}

	incidentData, err := s.buildIncidentsData(ctx)
	if err != nil {
		view.IncidentsNote = "The incident log is not configured or temporarily unavailable on this instance."
	} else {
		view.Incidents = incidentData
	}

	if s.deps.Feeds == nil {
		view.FeedsNote = "Feed health is not configured on this instance."
	} else {
		feedData, err := s.buildFeedsData(ctx, 12)
		if err != nil {
			view.FeedsNote = "Feed health is temporarily unavailable."
		} else {
			view.Feeds = feedData
		}
	}

	view.IHI = s.buildIHIView(ctx)
	view.Diagnostics = s.buildDiagnosticsView()
	const recordSample = 10
	if len(view.Diagnostics.Records) > recordSample {
		view.RecordSample = view.Diagnostics.Records[:recordSample]
	} else {
		view.RecordSample = view.Diagnostics.Records
	}

	return view, nil
}

// handleAtlas renders the map-first combined view. It is session-protected by
// the route in New, and uses the bounded inline-style CSP the other projection
// pages use: marker percentages are the only inline styles in the document.
func (s *server) handleAtlas(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	focus := false
	switch q.Get("focus") {
	case "problems", "problem":
		focus = true
	}
	view, err := s.buildAtlasView(r.Context(), boardParams{
		Group:   normaliseGroupMode(q.Get("group")),
		Focus:   focus,
		Country: strings.ToUpper(strings.TrimSpace(q.Get("country"))),
		Region:  region.Normalise(q.Get("region")),
		Density: normaliseDensity(q.Get("density")),
		Show:    parseShow(q.Get("show")),
	})
	if err != nil {
		s.deps.Logger.Error("atlas snapshot failed",
			"event", "atlas_failed", "component", "http",
			"request_id", requestIDFrom(r), "error", err)
		s.renderError(w, r, http.StatusServiceUnavailable,
			"The atlas could not be read. The server log has the details for this request.")
		return
	}
	csrf, _ := s.auth.csrfFrom(r)
	view.CSRF = csrf
	view.Insecure = s.insecure(r)
	view.NoAuth = s.auth.disabled()
	view.Board.CSRF = csrf
	view.Board.Insecure = view.Insecure
	view.Board.NoAuth = view.NoAuth
	view.Board.Version = view.Version
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; style-src 'self' 'unsafe-inline'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	_ = s.tmpl.ExecuteTemplate(w, "atlas.html", view)
}
