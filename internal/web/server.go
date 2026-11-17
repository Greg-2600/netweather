package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Greg-2600/netweather/internal/board"
)

type server struct {
	deps Deps
	auth *authenticator
	tmpl *template.Template
	now  func() time.Time
}

// New builds the browser-facing handler: login, board, target detail, and the
// SSE stream. The API is mounted separately; the two share the read model.
func New(deps Deps) (http.Handler, error) {
	if deps.Source == nil {
		return nil, errNoSource
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	tmpl, err := loadTemplates()
	if err != nil {
		return nil, fmt.Errorf("web: parse templates: %w", err)
	}

	s := &server{
		deps: deps,
		auth: newAuthenticator(deps.Token, deps.SecureCookies),
		tmpl: tmpl,
		now:  time.Now,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "User-agent: *\nDisallow: /\n")
	})
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.requireSession(http.HandlerFunc(s.handleLogout)).ServeHTTP)
	mux.Handle("/static/", http.FileServer(http.FS(staticFS)))
	mux.Handle("/events", s.requireSession(http.HandlerFunc(s.handleEvents)))
	mux.HandleFunc("/targets/", s.requireSession(http.HandlerFunc(s.handleTarget)).ServeHTTP)
	mux.HandleFunc("/incidents", s.requireSession(http.HandlerFunc(s.handleIncidents)).ServeHTTP)
	mux.HandleFunc("/incidents/", s.requireSession(http.HandlerFunc(s.handleIncident)).ServeHTTP)
	mux.HandleFunc("/pause", s.requireSession(http.HandlerFunc(s.handlePause)).ServeHTTP)
	mux.HandleFunc("/resume", s.requireSession(http.HandlerFunc(s.handleResume)).ServeHTTP)
	mux.Handle("/", s.requireSession(http.HandlerFunc(s.handleBoard)))
	return mux, nil
}

// --- login -----------------------------------------------------------------

type loginView struct {
	Error string
	CSRF  string
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	now := s.now()

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if _, ok := s.auth.sessionFrom(r); ok {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		csrf := randomToken()
		s.auth.setCSRFCookie(w, csrf, now)
		s.renderLogin(w, http.StatusOK, loginView{CSRF: csrf})

	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			s.renderLogin(w, http.StatusBadRequest, loginView{Error: "Malformed form."})
			return
		}
		if !s.auth.limit.Allow(clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			s.renderLogin(w, http.StatusTooManyRequests, loginView{Error: "Too many attempts. Wait a minute."})
			return
		}
		if !s.auth.csrfMatches(r, r.PostFormValue("csrf")) {
			csrf := randomToken()
			s.auth.setCSRFCookie(w, csrf, now)
			s.renderLogin(w, http.StatusBadRequest, loginView{Error: "Session expired. Try again.", CSRF: csrf})
			return
		}
		if !s.auth.checkToken(r.PostFormValue("token")) {
			s.deps.Logger.Warn("login failed",
				"event", "login_failed", "component", "http", "client", clientIP(r))
			s.renderLogin(w, http.StatusUnauthorized, loginView{
				Error: "That token is not correct.",
				CSRF:  r.PostFormValue("csrf"),
			})
			return
		}
		s.auth.setSessionCookie(w, s.auth.issueSession(now), now)
		s.deps.Logger.Info("login ok", "event", "login_ok", "component", "http", "client", clientIP(r))
		http.Redirect(w, r, "/", http.StatusSeeOther)

	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) renderLogin(w http.ResponseWriter, status int, view loginView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.WriteHeader(status)
	_ = s.tmpl.ExecuteTemplate(w, "login.html", view)
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil || !s.auth.csrfMatches(r, r.PostFormValue("csrf")) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.auth.clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- probe control ---------------------------------------------------------

func (s *server) handlePause(w http.ResponseWriter, r *http.Request)  { s.setPaused(w, r, true) }
func (s *server) handleResume(w http.ResponseWriter, r *http.Request) { s.setPaused(w, r, false) }

func (s *server) setPaused(w http.ResponseWriter, r *http.Request, paused bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.deps.Control == nil {
		http.Error(w, "probe control is not configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil || !s.auth.csrfMatches(r, r.PostFormValue("csrf")) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if paused {
		s.deps.Control.Pause()
	} else {
		s.deps.Control.Resume()
	}

	s.deps.Logger.Info("probe control",
		"event", "probe_control", "component", "http", "paused", paused)

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// --- board -----------------------------------------------------------------

type tileView struct {
	ID     string
	Name   string
	Host   string
	Sector string
	State  string
	Text   string
	Glyph  string
	Class  string
	Since  string
	Aria   string
}

type countView struct {
	State string
	Label string
	Glyph string
	Class string
	Count int
}

type boardView struct {
	Version string
	CSRF    string
	Targets []tileView
	Counts  []countView
	Paused  bool
	Control bool
}

func (s *server) handleBoard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	targets, err := s.deps.Source.Snapshot(r.Context())
	if err != nil {
		s.deps.Logger.Error("board snapshot failed", "event", "board_failed", "component", "http", "error", err)
		http.Error(w, "board unavailable", http.StatusServiceUnavailable)
		return
	}
	// Failures first: the grid answers "what is down?", not "what is alphabetical".
	targets = board.Order(targets)

	csrf, _ := s.auth.csrfFrom(r)
	view := boardView{
		Version: s.deps.Version,
		CSRF:    csrf,
		Targets: make([]tileView, 0, len(targets)),
		Control: s.deps.Control != nil,
	}
	if s.deps.Control != nil {
		view.Paused = s.deps.Control.Paused()
	}
	counts := make(map[board.State]int)
	for _, t := range targets {
		counts[t.State]++
		view.Targets = append(view.Targets, toTile(t))
	}

	for _, st := range []board.State{
		board.StateDown, board.StateDegraded, board.StateBlocked,
		board.StateUnknown, board.StateConfigError, board.StateUnmeasurable,
		board.StateUpReachability, board.StateUp,
	} {
		if n := counts[st]; n > 0 {
			view.Counts = append(view.Counts, countView{
				State: string(st), Label: st.Text(), Glyph: st.Glyph(), Class: st.CSSClass(), Count: n,
			})
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	_ = s.tmpl.ExecuteTemplate(w, "board.html", view)
}

func toTile(t board.Target) tileView {
	since := ""
	if !t.Since.IsZero() {
		since = t.Since.UTC().Format("15:04 MST")
	}

	aria := fmt.Sprintf("%s (%s): %s", t.Name, t.Host, t.State.Text())
	if since != "" {
		aria += ", since " + since
	}

	return tileView{
		ID: t.ID, Name: t.Name, Host: t.Host, Sector: t.Sector,
		State: string(t.State), Text: t.State.Text(), Glyph: t.State.Glyph(),
		Class: t.State.CSSClass(), Since: since, Aria: aria,
	}
}

type targetView struct {
	Tile    tileView
	Probes  []probeView
	History bool
}

type probeView struct {
	TS         string
	State      string
	Text       string
	Glyph      string
	Stage      string
	ErrorClass string
	HTTPStatus int
	Latency    string
	Confidence string
	Detail     string
}

func (s *server) handleTarget(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := r.URL.Path[len("/targets/"):]
	targets, err := s.deps.Source.Snapshot(r.Context())
	if err != nil {
		http.Error(w, "board unavailable", http.StatusServiceUnavailable)
		return
	}
	for _, t := range targets {
		if t.ID != id {
			continue
		}

		view := targetView{Tile: toTile(t)}
		if s.deps.History != nil {
			view.History = true

			vantage := s.deps.Vantage
			if vantage == "" {
				vantage = "local"
			}
			rows, err := s.deps.History.RecentProbes(r.Context(), t.ID, vantage, 20)
			if err != nil {
				s.deps.Logger.Warn("probe history failed",
					"event", "history_failed", "component", "http", "target", t.Host, "error", err)
			}
			for _, row := range rows {
				latency := "—"
				if row.LatencyMS > 0 {
					latency = fmt.Sprintf("%d ms", row.LatencyMS)
				}
				view.Probes = append(view.Probes, probeView{
					TS:         row.TS.UTC().Format("15:04:05"),
					State:      string(row.State),
					Text:       row.State.Text(),
					Glyph:      row.State.Glyph(),
					Stage:      row.Stage,
					ErrorClass: row.ErrorClass,
					HTTPStatus: row.HTTPStatus,
					Latency:    latency,
					Confidence: row.Confidence,
					Detail:     row.Detail,
				})
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		_ = s.tmpl.ExecuteTemplate(w, "target.html", view)
		return
	}
	http.Error(w, "no such target", http.StatusNotFound)
}

// --- incidents -------------------------------------------------------------

type incidentRow struct {
	ID           string
	OpenedAt     string
	Scope        string
	ScopeValue   string
	Headline     string
	Confidence   string
	Open         bool
	ClosedAt     string
	ClosedReason string
}

type incidentListView struct {
	Incidents []incidentRow
}

type attributionView struct {
	Rank    int
	Kind    string
	Value   string
	Failing int
	Total   int
	Share   string
	Score   string
}

type incidentDetailView struct {
	ID           string
	Headline     string
	Scope        string
	ScopeValue   string
	OpenedAt     string
	Confidence   string
	Closed       bool
	ClosedAt     string
	ClosedReason string
	Attribution  []attributionView
	Targets      []string
}

func (s *server) handleIncidents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.deps.Incidents == nil {
		http.Error(w, "incident log is not configured", http.StatusServiceUnavailable)
		return
	}

	rows, err := s.deps.Incidents.Incidents(r.Context(), false, 100)
	if err != nil {
		s.deps.Logger.Error("incident list failed", "event", "incident_list_failed", "component", "http", "error", err)
		http.Error(w, "incident log unavailable", http.StatusServiceUnavailable)
		return
	}

	view := incidentListView{Incidents: make([]incidentRow, 0, len(rows))}
	for _, inc := range rows {
		row := incidentRow{
			ID: inc.ID, OpenedAt: inc.OpenedAt.UTC().Format("2006-01-02 15:04"),
			Scope: inc.Scope, ScopeValue: inc.ScopeValue, Headline: inc.Headline,
			Confidence: inc.Confidence, Open: inc.ClosedAt.IsZero(),
		}
		if !inc.ClosedAt.IsZero() {
			row.ClosedAt = inc.ClosedAt.UTC().Format("2006-01-02 15:04")
			row.ClosedReason = inc.ClosedReason
		}
		view.Incidents = append(view.Incidents, row)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	_ = s.tmpl.ExecuteTemplate(w, "incidents.html", view)
}

func (s *server) handleIncident(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.deps.Incidents == nil {
		http.Error(w, "incident log is not configured", http.StatusServiceUnavailable)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/incidents/")
	if id == "" || strings.Contains(id, "/") {
		http.Error(w, "no such incident", http.StatusNotFound)
		return
	}

	inc, attrs, targets, err := s.deps.Incidents.IncidentByID(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "no such incident", http.StatusNotFound)
		return
	}
	if err != nil {
		s.deps.Logger.Error("incident load failed", "event", "incident_load_failed", "component", "http", "error", err)
		http.Error(w, "incident unavailable", http.StatusServiceUnavailable)
		return
	}

	view := incidentDetailView{
		ID: inc.ID, Headline: inc.Headline, Scope: inc.Scope, ScopeValue: inc.ScopeValue,
		OpenedAt:   inc.OpenedAt.UTC().Format("2006-01-02 15:04"),
		Confidence: inc.Confidence, Closed: !inc.ClosedAt.IsZero(),
		Targets: targets,
	}
	if !inc.ClosedAt.IsZero() {
		view.ClosedAt = inc.ClosedAt.UTC().Format("2006-01-02 15:04")
		view.ClosedReason = inc.ClosedReason
	}
	for _, a := range attrs {
		view.Attribution = append(view.Attribution, attributionView{
			Rank: a.Rank, Kind: a.Kind, Value: a.Value,
			Failing: a.Failing, Total: a.Total,
			Share: fmt.Sprintf("%.0f%%", a.Share*100),
			Score: fmt.Sprintf("%.3f", a.Score),
		})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	_ = s.tmpl.ExecuteTemplate(w, "incident.html", view)
}

// --- events ----------------------------------------------------------------

type stateEvent struct {
	Target string `json:"target"`
	Name   string `json:"name"`
	State  string `json:"state"`
	Text   string `json:"state_text"`
	Glyph  string `json:"glyph"`
	Class  string `json:"class"`
	Since  string `json:"since,omitempty"`
}

// handleEvents streams state deltas only. The page renders the full board; the
// stream never resends it, so a slow client cannot amplify one change into a
// whole grid.
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// The server sets WriteTimeout for normal requests; SSE outlives it.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	if _, err := io.WriteString(w, "retry: 3000\n\n"); err != nil {
		return
	}
	flusher.Flush()

	watcher := s.deps.Watcher
	if watcher == nil {
		s.heartbeatOnly(w, r, flusher)
		return
	}

	ch, cancel := watcher.Subscribe()
	defer cancel()

	last := s.stateMap(r.Context())
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-ch:
			current := s.stateMap(r.Context())
			ids := make([]string, 0, len(current))
			for id := range current {
				ids = append(ids, id)
			}
			sort.Strings(ids)

			for _, id := range ids {
				t := current[id]
				if prev, seen := last[id]; seen && prev.State == t.State {
					continue
				}
				since := ""
				if !t.Since.IsZero() {
					since = t.Since.UTC().Format("15:04 MST")
				}
				data, err := json.Marshal(stateEvent{
					Target: t.Host, Name: t.Name, State: string(t.State),
					Text: t.State.Text(), Glyph: t.State.Glyph(),
					Class: t.State.CSSClass(), Since: since,
				})
				if err != nil {
					continue
				}
				if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", data); err != nil {
					return
				}
			}
			last = current
			flusher.Flush()
		}
	}
}

func (s *server) heartbeatOnly(w http.ResponseWriter, r *http.Request, flusher http.Flusher) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *server) stateMap(ctx context.Context) map[string]board.Target {
	targets, err := s.deps.Source.Snapshot(ctx)
	if err != nil {
		return map[string]board.Target{}
	}
	out := make(map[string]board.Target, len(targets))
	for _, t := range targets {
		out[t.ID] = t
	}
	return out
}
