package api

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Greg-2600/netweather/internal/baseline"
	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/logging"
	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
	"github.com/Greg-2600/netweather/internal/streamlimit"
)

// Control is the probe kill switch. The agent implements it; a nil Control means
// the control endpoints are unavailable rather than silently successful.
type Control interface {
	Pause()
	Resume()
	Paused() bool
}

// ProbeReader is the read side the API needs for target history.
type ProbeReader interface {
	RecentProbes(ctx context.Context, targetID, vantageID string, limit int) ([]store.ProbeRow, error)
	TargetExists(ctx context.Context, targetID string) (bool, error)
	Series(ctx context.Context, targetID, vantageID, resolution string, from, to time.Time) ([]store.Rollup, error)
}

// IncidentReader is the read side of the incident log.
type IncidentReader interface {
	Incidents(ctx context.Context, openOnly bool, limit int) ([]store.Incident, error)
	IncidentByID(ctx context.Context, id string) (store.Incident, []store.Attribution, []string, error)
}

// CorroborationReader returns external feed events that overlap an incident.
type CorroborationReader interface {
	Corroboration(ctx context.Context, inc store.Incident, now time.Time, limit int) ([]store.ExternalEvent, error)
}

// ScopeReader is the read side for scope health and coverage.
type ScopeReader interface {
	ScopeStates(ctx context.Context, vantageID, scope, value string) (map[state.State]int, int, error)
	ScopeHistory(ctx context.Context, scope, value string, at time.Time) ([]float64, error)
}

// AnomalyReader is the read side of the baseline-relative detector.
type AnomalyReader interface {
	Anomalies(ctx context.Context, limit int) ([]store.Anomaly, error)
}

// TargetReader is the single-target detail read.
type TargetReader interface {
	TargetByID(ctx context.Context, vantageID, id string) (store.TargetRow, error)
}

// CoverageReader builds the blind-spot panel.
type CoverageReader interface {
	Coverage(ctx context.Context, vantageID string, now time.Time) (store.Coverage, error)
}

// CoverageHistoryReader reads daily coverage snapshots: the trend a point-in-time
// panel cannot show.
type CoverageHistoryReader interface {
	CoverageHistory(ctx context.Context, scope, value string, days int) ([]store.CoveragePoint, error)
}

// SelfCheckReader reads the latest self-health results.
type SelfCheckReader interface {
	LatestSelfChecks(ctx context.Context) ([]store.SelfCheck, error)
}

// Exporter streams observations for the export endpoint. The callback shape is
// the whole point: an export never materialises its result set.
type Exporter interface {
	ExportProbes(ctx context.Context, from, to time.Time, fn func(store.ExportRow) error) error
}

// FeedReader is the read side of external corroboration feeds: their health and
// their most recent events.
type FeedReader interface {
	FeedStates(ctx context.Context) ([]store.FeedState, error)
	ExternalEvents(ctx context.Context, limit int) ([]store.ExternalEvent, error)
}

// DebugScopes is the time-boxed verbose-logging registry.
type DebugScopes interface {
	Add(sc logging.Scope, ttl time.Duration) (logging.Scope, error)
	Remove(id string) bool
	List() []logging.Scope
}

// Deps is everything the HTTP layer needs. It is deliberately small: the API
// reads state, it does not own it.
type Deps struct {
	Logger    *slog.Logger
	Ring      *logging.Ring
	Version   string
	Commit    string
	StartedAt time.Time
	// Source supplies the board. Nil means "not configured": the endpoint
	// answers 503 rather than inventing an empty board.
	Source board.Source
	// Watcher drives the SSE stream. Nil keeps /events heartbeat-only.
	Watcher board.Watcher
	// IncidentWatcher signals incident changes on the same stream. Nil disables
	// incident events without affecting state events.
	IncidentWatcher board.Watcher
	// Control is the pause/resume switch.
	Control Control
	// Probes reads recent observations for one target. Nil disables the route.
	Probes ProbeReader
	// Incidents reads the incident log. Nil disables the routes.
	Incidents IncidentReader
	// Corroboration reads external feed events for an incident. Nil omits the
	// section rather than pretending no earlier event existed.
	Corroboration CorroborationReader
	// Scopes reads scope health/coverage. Nil disables the route.
	Scopes ScopeReader
	// Anomalies reads detector output. Nil disables the route.
	Anomalies AnomalyReader
	// Targets reads one target with its current state. Nil disables the route.
	Targets TargetReader
	// Coverage builds the blind-spot panel. Nil disables the route.
	Coverage CoverageReader
	// CoverageHistory reads daily coverage snapshots. Nil disables the route.
	CoverageHistory CoverageHistoryReader
	// SelfChecks reads the latest self-health results. Nil disables the route.
	SelfChecks SelfCheckReader
	// Exporter streams observations. Nil disables the route.
	Exporter Exporter
	// Feeds reads external corroboration. Nil disables the route.
	Feeds FeedReader
	// Debug manages time-boxed verbose logging. Nil disables the routes.
	Debug DebugScopes
	// Tracer runs on-demand traceroute. Nil disables the trace action.
	Tracer Tracer
	// Resolver turns a target hostname into addresses for a trace.
	Resolver HostResolver
	// StreamLimit caps concurrent SSE connections. Nil uses the defaults.
	StreamLimit *streamlimit.Limiter
	// Metrics is the Prometheus handler. Nil keeps /metrics unmounted, which is
	// the default: exposing it is an operator decision.
	Metrics http.Handler
	// Vantage is the vantage the read endpoints report on.
	Vantage string
	// Fallback serves everything that is not /healthz or /api/. It is the UI.
	// Nil means "JSON 404", which is the right default for tests.
	Fallback http.Handler
}

type server struct {
	deps     Deps
	authMode string
	// traceBudget is per-instance, not per-client: the operator's egress IP is
	// what a target's abuse desk sees.
	traceBudget *traceGovernor
	// idempotency replays action responses for a repeated Idempotency-Key.
	idempotency *idempotencyStore
	// streams caps concurrent SSE connections. nil means "unlimited", which
	// only a test should ask for.
	streams *streamlimit.Limiter
}

// New builds the HTTP handler. It validates the exposure guardrail before it
// returns, so a misconfigured listener fails at startup rather than at the first
// unauthenticated request.
func New(cfg Config, deps Deps) (http.Handler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.StartedAt.IsZero() {
		deps.StartedAt = time.Now()
	}
	s := &server{
		deps:        deps,
		authMode:    cfg.AuthMode(),
		traceBudget: newTraceGovernor(),
		idempotency: newIdempotencyStore(),
		streams:     deps.StreamLimit,
	}
	if s.streams == nil {
		s.streams = streamlimit.New(streamlimit.DefaultTotal, streamlimit.DefaultPerIP)
	}

	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/api/v1/board", onlyGET(s.handleBoard))
	apiMux.HandleFunc("/api/v1/targets", onlyGET(s.handleTargets))
	apiMux.HandleFunc("/api/v1/control", onlyGET(s.handleControl))
	apiMux.HandleFunc("/api/v1/pause", onlyPOST(s.handlePause))
	apiMux.HandleFunc("/api/v1/resume", onlyPOST(s.handleResume))
	apiMux.HandleFunc("/api/v1/meta", onlyGET(s.handleMeta))
	apiMux.HandleFunc("/api/v1/logs", onlyGET(s.handleLogs))
	apiMux.HandleFunc("/api/v1/targets/", s.handleTargetSubtree)
	apiMux.HandleFunc("/api/v1/scopes/", onlyGET(s.handleScope))
	apiMux.HandleFunc("/api/v1/anomalies", onlyGET(s.handleAnomalies))
	apiMux.HandleFunc("/api/v1/coverage", onlyGET(s.handleCoverage))
	apiMux.HandleFunc("/api/v1/coverage/history", onlyGET(s.handleCoverageHistory))
	apiMux.HandleFunc("/api/v1/selfcheck", onlyGET(s.handleSelfCheck))
	apiMux.HandleFunc("/api/v1/export", onlyGET(s.handleExport))
	apiMux.HandleFunc("/api/v1/events", onlyGET(s.handleEvents))
	apiMux.HandleFunc("/api/v1/feeds", onlyGET(s.handleFeeds))
	apiMux.HandleFunc("/api/v1/debug/scopes", s.handleDebugScopes)
	apiMux.HandleFunc("/api/v1/debug/scopes/", s.handleDebugScopeByID)
	apiMux.HandleFunc("/api/v1/incidents", onlyGET(s.handleIncidents))
	apiMux.HandleFunc("/api/v1/incidents/", onlyGET(s.handleIncidentByID))
	apiMux.HandleFunc("/", s.handleNotFound)

	// 60 requests/minute sustained, 20 burst (docs/API.md §8).
	limiter := newBucket(1, 20, time.Now)

	root := http.NewServeMux()
	root.HandleFunc("/healthz", onlyGET(s.handleHealthz))
	root.Handle("/api/", withRateLimit(limiter, withBearerAuth(cfg.Token, withCompression(withIdempotency(s.idempotency, apiMux)))))
	if deps.Metrics != nil {
		// Aggregate metrics still describe the operator's fleet, so they get the
		// same credential as the API.
		root.Handle("/metrics", withBearerAuth(cfg.Token, deps.Metrics))
	}
	if deps.Fallback != nil {
		root.Handle("/", deps.Fallback)
	} else {
		root.HandleFunc("/", s.handleNotFound)
	}

	return withRequestID(
		withAccessLog(deps.Logger,
			withSecurityHeaders(
				withRecover(deps.Logger, root),
			),
		),
	), nil
}

func onlyGET(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, r, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
				fmt.Sprintf("%s is not allowed on %s", r.Method, r.URL.Path))
			return
		}
		h(w, r)
	}
}

func onlyPOST(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeError(w, r, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
				fmt.Sprintf("%s is not allowed on %s", r.Method, r.URL.Path))
			return
		}
		h(w, r)
	}
}

func (s *server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotFound, CodeNotFound, "no such endpoint")
}

// healthResponse is intentionally bare: orchestrators should not unwrap an
// envelope, and a health probe must never disclose target data.
type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	UptimeS int64  `json:"uptime_s"`
}

func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	// When the store exists, a failed read must return 503 here. Until then the
	// process being able to answer is the whole check.
	writeJSON(w, http.StatusOK, healthResponse{
		Status:  "ok",
		Version: s.deps.Version,
		Commit:  s.deps.Commit,
		UptimeS: int64(time.Since(s.deps.StartedAt).Seconds()),
	})
}

type ringInfo struct {
	Records  int `json:"records"`
	Capacity int `json:"capacity"`
}

type metaResponse struct {
	Version      string   `json:"version"`
	Commit       string   `json:"commit"`
	API          string   `json:"api"`
	StartedAt    string   `json:"started_at"`
	UptimeS      int64    `json:"uptime_s"`
	Auth         string   `json:"auth"`
	Capabilities []string `json:"capabilities"`
	Ring         ringInfo `json:"ring"`
}

// capabilities is the runtime truth. A client should feature-detect here rather
// than assume an endpoint from docs/API.md exists.
var capabilities = []string{"health", "meta", "logs", "board", "targets", "control", "probes", "series", "incidents", "scopes", "anomalies", "coverage", "selfcheck", "export", "events", "feeds", "debug", "coverage_history", "trace"}

func (s *server) handleMeta(w http.ResponseWriter, r *http.Request) {
	info := ringInfo{}
	if s.deps.Ring != nil {
		info = ringInfo{Records: s.deps.Ring.Len(), Capacity: s.deps.Ring.Capacity()}
	}

	auth := s.authMode

	writeData(w, r, metaResponse{
		Version:      s.deps.Version,
		Commit:       s.deps.Commit,
		API:          "v1",
		StartedAt:    s.deps.StartedAt.UTC().Format(time.RFC3339Nano),
		UptimeS:      int64(time.Since(s.deps.StartedAt).Seconds()),
		Auth:         auth,
		Capabilities: capabilities,
		Ring:         info,
	}, nil)
}

type targetItem struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Host    string `json:"host"`
	Class   string `json:"class,omitempty"`
	Sector  string `json:"sector,omitempty"`
	Country string `json:"country,omitempty"`
	State   string `json:"state"`
	Since   string `json:"since,omitempty"`
	// Certificate observations from the last TLS probe. Absent for non-TLS
	// targets, never guessed.
	CertIssuer   string `json:"cert_issuer,omitempty"`
	CertNotAfter string `json:"cert_not_after,omitempty"`
}

// handleTargets is the filtered list. Unknown class/sector/country values are
// data and simply match nothing; an unknown state is a client error.
func (s *server) handleTargets(w http.ResponseWriter, r *http.Request) {
	if s.deps.Source == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "targets are not configured")
		return
	}

	q := r.URL.Query()
	stateFilter := strings.ToLower(strings.TrimSpace(q.Get("state")))
	if stateFilter != "" && !validState(stateFilter) {
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument,
			"state must be one of the eight target states")
		return
	}
	classFilter := strings.TrimSpace(q.Get("class"))
	sectorFilter := strings.TrimSpace(q.Get("sector"))
	countryFilter := strings.TrimSpace(q.Get("country"))

	targets, err := s.deps.Source.Snapshot(r.Context())
	if err != nil {
		s.deps.Logger.Error("target list failed", "event", "target_list_failed", "component", "http", "error", err)
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "targets unavailable")
		return
	}

	out := make([]targetItem, 0, len(targets))
	for _, t := range targets {
		if classFilter != "" && !strings.EqualFold(t.Class, classFilter) {
			continue
		}
		if sectorFilter != "" && !strings.EqualFold(t.Sector, sectorFilter) {
			continue
		}
		if countryFilter != "" && !strings.EqualFold(t.Country, countryFilter) {
			continue
		}
		if stateFilter != "" && string(t.State) != stateFilter {
			continue
		}

		item := targetItem{
			ID: t.ID, Name: t.Name, Host: t.Host, Class: t.Class,
			Sector: t.Sector, Country: t.Country, State: string(t.State),
			CertIssuer: t.CertIssuer, CertNotAfter: certNotAfterJSON(t.CertNotAfter),
		}
		if !t.Since.IsZero() {
			item.Since = t.Since.UTC().Format(time.RFC3339)
		}
		out = append(out, item)
	}

	writeData(w, r, out, &Meta{Count: len(out)})
}

func validState(s string) bool {
	switch state.State(s) {
	case state.Up, state.UpReachability, state.Degraded, state.Down,
		state.Blocked, state.Unknown, state.ConfigError, state.Unmeasurable:
		return true
	default:
		return false
	}
}

type boardItem struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Host    string `json:"host"`
	Sector  string `json:"sector,omitempty"`
	Country string `json:"country,omitempty"`
	State   string `json:"state"`
	Since   string `json:"since,omitempty"`
}

// handleBoard is the machine-readable grid: one entry per enabled target, in the
// source's order. It never filters by state; a client that wants only failures
// can filter a list it already has.
func (s *server) handleBoard(w http.ResponseWriter, r *http.Request) {
	if s.deps.Source == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "board source is not configured")
		return
	}

	targets, err := s.deps.Source.Snapshot(r.Context())
	if err != nil {
		s.deps.Logger.Error("board snapshot failed",
			"event", "board_failed", "component", "http", "error", err)
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "board unavailable")
		return
	}

	out := make([]boardItem, 0, len(targets))
	for _, t := range targets {
		item := boardItem{
			ID:      t.ID,
			Name:    t.Name,
			Host:    t.Host,
			Sector:  t.Sector,
			Country: t.Country,
			State:   string(t.State),
		}
		if !t.Since.IsZero() {
			item.Since = t.Since.UTC().Format(time.RFC3339)
		}
		out = append(out, item)
	}

	writeData(w, r, out, &Meta{Count: len(out)})
}

type probeItem struct {
	TS         string `json:"ts"`
	State      string `json:"state"`
	Stage      string `json:"stage"`
	ErrorClass string `json:"error_class,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
	LatencyMS  int    `json:"latency_ms,omitempty"`
	Confidence string `json:"confidence"`
	Detail     string `json:"detail,omitempty"`
}

// handleTargetProbes serves /api/v1/targets/{id}/probes: the newest observations
// for one target, newest first. It is the drawer's data.
// handleTargetSubtree dispatches the target subtree. Its reads are GET-only,
// but the trace action is POST, so one method gate cannot cover both.
func (s *server) handleTargetSubtree(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/trace") {
		s.handleTrace(w, r)
		return
	}
	onlyGET(s.handleTargetProbes)(w, r)
}

func (s *server) handleTargetProbes(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/targets/")
	id, suffix, ok := strings.Cut(rest, "/")
	if id == "" {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "no such target")
		return
	}

	if !ok {
		s.serveTarget(w, r, id)
		return
	}

	switch suffix {
	case "probes":
		if s.deps.Probes == nil {
			writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "probe history is not configured")
			return
		}
		s.serveProbes(w, r, id)
	case "series":
		if s.deps.Probes == nil {
			writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "probe history is not configured")
			return
		}
		s.serveSeries(w, r, id)
	default:
		writeError(w, r, http.StatusNotFound, CodeNotFound, "no such endpoint")
	}
}

func (s *server) serveTarget(w http.ResponseWriter, r *http.Request, id string) {
	if s.deps.Targets == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "target detail is not configured")
		return
	}

	vantage := s.deps.Vantage
	if vantage == "" {
		vantage = "local"
	}

	row, err := s.deps.Targets.TargetByID(r.Context(), vantage, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "no such target")
		return
	}
	if err != nil {
		s.deps.Logger.Error("target detail failed", "event", "target_detail_failed", "component", "http", "error", err)
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "target unavailable")
		return
	}

	item := targetItem{
		ID: row.ID, Name: row.Name, Host: row.Host, Class: row.Class,
		Sector: row.Sector, Country: row.Country, State: string(row.State),
		CertIssuer: row.CertIssuer, CertNotAfter: certNotAfterJSON(row.CertNotAfter),
	}
	if !row.Since.IsZero() {
		item.Since = row.Since.UTC().Format(time.RFC3339)
	}

	writeData(w, r, item, nil)
}

// certNotAfterJSON formats a certificate expiry for the API, leaving it absent
// when the last probe was not TLS.
func certNotAfterJSON(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func (s *server) serveProbes(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "no such target")
		return
	}

	limit, err := parseLimit(r.URL.Query().Get("limit"), 20, 200)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, err.Error())
		return
	}

	exists, err := s.deps.Probes.TargetExists(r.Context(), id)
	if err != nil {
		s.deps.Logger.Error("target lookup failed", "event", "target_lookup_failed", "component", "http", "error", err)
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "probe history unavailable")
		return
	}
	if !exists {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "no such target")
		return
	}

	vantage := s.deps.Vantage
	if vantage == "" {
		vantage = "local"
	}

	rows, err := s.deps.Probes.RecentProbes(r.Context(), id, vantage, limit)
	if err != nil {
		s.deps.Logger.Error("probe history failed", "event", "probe_history_failed", "component", "http", "error", err)
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "probe history unavailable")
		return
	}

	out := make([]probeItem, 0, len(rows))
	for _, p := range rows {
		out = append(out, probeItem{
			TS:         p.TS.UTC().Format(time.RFC3339),
			State:      string(p.State),
			Stage:      p.Stage,
			ErrorClass: p.ErrorClass,
			HTTPStatus: p.HTTPStatus,
			LatencyMS:  p.LatencyMS,
			Confidence: p.Confidence,
			Detail:     p.Detail,
		})
	}

	writeData(w, r, out, &Meta{Count: len(out), Limit: limit})
}

type incidentItem struct {
	ID         string `json:"id"`
	OpenedAt   string `json:"opened_at"`
	ClosedAt   string `json:"closed_at,omitempty"`
	Verdict    string `json:"verdict"`
	Scope      string `json:"scope"`
	ScopeValue string `json:"scope_value,omitempty"`
	Headline   string `json:"headline"`
	Confidence string `json:"confidence"`
}

type attributionItem struct {
	Rank    int     `json:"rank"`
	Kind    string  `json:"kind"`
	Value   string  `json:"value"`
	Failing int     `json:"failing"`
	Total   int     `json:"total"`
	Share   float64 `json:"share"`
	Score   float64 `json:"score"`
}

type incidentDetail struct {
	Incident      incidentItem        `json:"incident"`
	Attribution   []attributionItem   `json:"attribution"`
	Targets       []string            `json:"targets"`
	Corroboration []corroborationItem `json:"corroboration"`
}

// corroborationItem is one external event that overlaps the incident. It is
// evidence: it never changes the verdict, it tells the reader what else was
// happening.
type corroborationItem struct {
	Feed       string  `json:"feed"`
	ExternalID string  `json:"external_id"`
	Kind       string  `json:"kind"`
	Severity   float64 `json:"severity,omitempty"`
	Country    string  `json:"country,omitempty"`
	ASN        int     `json:"asn,omitempty"`
	StartedAt  string  `json:"started_at"`
	EndedAt    string  `json:"ended_at,omitempty"`
}

func toIncidentItem(inc store.Incident) incidentItem {
	item := incidentItem{
		ID:         inc.ID,
		OpenedAt:   inc.OpenedAt.UTC().Format(time.RFC3339),
		Verdict:    inc.Verdict,
		Scope:      inc.Scope,
		ScopeValue: inc.ScopeValue,
		Headline:   inc.Headline,
		Confidence: inc.Confidence,
	}
	if !inc.ClosedAt.IsZero() {
		item.ClosedAt = inc.ClosedAt.UTC().Format(time.RFC3339)
	}
	return item
}

// handleIncidents lists incidents newest first, optionally only open ones.
func (s *server) handleIncidents(w http.ResponseWriter, r *http.Request) {
	if s.deps.Incidents == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "incidents are not configured")
		return
	}

	q := r.URL.Query()
	limit, err := parseLimit(q.Get("limit"), 50, 200)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, err.Error())
		return
	}
	openOnly := q.Get("open") == "true"

	rows, err := s.deps.Incidents.Incidents(r.Context(), openOnly, limit)
	if err != nil {
		s.deps.Logger.Error("incident list failed", "event", "incident_list_failed", "component", "http", "error", err)
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "incidents unavailable")
		return
	}

	out := make([]incidentItem, 0, len(rows))
	for _, inc := range rows {
		out = append(out, toIncidentItem(inc))
	}
	writeData(w, r, out, &Meta{Count: len(out), Limit: limit})
}

// handleIncidentByID returns one incident with its ranked attribution and the
// targets it covers.
func (s *server) handleIncidentByID(w http.ResponseWriter, r *http.Request) {
	if s.deps.Incidents == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "incidents are not configured")
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/api/v1/incidents/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "no such incident")
		return
	}

	inc, attrs, targets, err := s.deps.Incidents.IncidentByID(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "no such incident")
		return
	}
	if err != nil {
		s.deps.Logger.Error("incident load failed", "event", "incident_load_failed", "component", "http", "error", err)
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "incident unavailable")
		return
	}

	detail := incidentDetail{
		Incident:      toIncidentItem(inc),
		Attribution:   make([]attributionItem, 0, len(attrs)),
		Targets:       targets,
		Corroboration: make([]corroborationItem, 0),
	}
	for _, a := range attrs {
		detail.Attribution = append(detail.Attribution, attributionItem{
			Rank: a.Rank, Kind: a.Kind, Value: a.Value,
			Failing: a.Failing, Total: a.Total, Share: a.Share, Score: a.Score,
		})
	}

	if s.deps.Corroboration != nil {
		events, err := s.deps.Corroboration.Corroboration(r.Context(), inc, time.Now().UTC(), 20)
		if err != nil {
			// Corroboration is a bonus panel; a failed query must not hide the
			// incident itself.
			s.deps.Logger.Warn("corroboration failed",
				"event", "corroboration_failed", "component", "http",
				"incident_id", inc.ID, "error", err)
		}
		for _, e := range events {
			item := corroborationItem{
				Feed: e.Feed, ExternalID: e.ExternalID, Kind: e.Kind,
				Severity: e.Severity, Country: e.Country, ASN: e.ASN,
				StartedAt: e.StartedAt.UTC().Format(time.RFC3339),
			}
			if !e.EndedAt.IsZero() {
				item.EndedAt = e.EndedAt.UTC().Format(time.RFC3339)
			}
			detail.Corroboration = append(detail.Corroboration, item)
		}
	}

	writeData(w, r, detail, nil)
}

type seriesItem struct {
	Bucket   string `json:"bucket"`
	N        int    `json:"n"`
	Up       int    `json:"up"`
	Degraded int    `json:"degraded"`
	Down     int    `json:"down"`
	Blocked  int    `json:"blocked"`
	Unknown  int    `json:"unknown"`
}

// serveSeries returns rollup buckets for one target at 5m, 1h, or 1d resolution.
func (s *server) serveSeries(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "no such target")
		return
	}

	q := r.URL.Query()
	resolution := q.Get("resolution")
	if resolution == "" {
		resolution = "5m"
	}
	switch resolution {
	case "5m", "1h", "1d":
	default:
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument,
			"resolution must be 5m, 1h, or 1d")
		return
	}

	now := time.Now().UTC()
	from := now.Add(-24 * time.Hour)
	to := now
	if v := q.Get("from"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, "from must be RFC 3339")
			return
		}
		from = parsed
	}
	if v := q.Get("to"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, "to must be RFC 3339")
			return
		}
		to = parsed
	}
	if !from.Before(to) {
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, "from must be before to")
		return
	}

	exists, err := s.deps.Probes.TargetExists(r.Context(), id)
	if err != nil {
		s.deps.Logger.Error("target lookup failed", "event", "target_lookup_failed", "component", "http", "error", err)
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "series unavailable")
		return
	}
	if !exists {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "no such target")
		return
	}

	vantage := s.deps.Vantage
	if vantage == "" {
		vantage = "local"
	}

	rows, err := s.deps.Probes.Series(r.Context(), id, vantage, resolution, from, to)
	if err != nil {
		s.deps.Logger.Error("series failed", "event", "series_failed", "component", "http", "error", err)
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "series unavailable")
		return
	}

	out := make([]seriesItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, seriesItem{
			Bucket:   row.Bucket.UTC().Format(time.RFC3339),
			N:        row.N,
			Up:       row.Up,
			Degraded: row.Degraded,
			Down:     row.Down,
			Blocked:  row.Blocked,
			Unknown:  row.Unknown,
		})
	}

	writeData(w, r, out, &Meta{Count: len(out)})
}

type baselineView struct {
	N      int     `json:"n"`
	Median float64 `json:"median"`
	MAD    float64 `json:"mad"`
	Sigma  float64 `json:"sigma"`
	Usable bool    `json:"usable"`
}

type scopeResponse struct {
	Scope      string         `json:"scope"`
	Value      string         `json:"value,omitempty"`
	Status     string         `json:"status"`
	Mode       string         `json:"mode"`
	Health     *float64       `json:"health"`
	Coverage   float64        `json:"coverage"`
	Measurable int            `json:"measurable"`
	Judgeable  int            `json:"judgeable"`
	States     map[string]int `json:"states"`
	Baseline   *baselineView  `json:"baseline,omitempty"`
}

// handleScope reports health and coverage for global, sector, country, or ASN.
//
// It applies the coverage floors from DESIGN.md §7.2 in one place: a scope with
// too little visibility is UNKNOWN, not healthy.
func (s *server) handleScope(w http.ResponseWriter, r *http.Request) {
	if s.deps.Scopes == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "scopes are not configured")
		return
	}

	scope := strings.TrimPrefix(r.URL.Path, "/api/v1/scopes/")
	if scope == "" || strings.Contains(scope, "/") {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "no such scope")
		return
	}
	switch scope {
	case "global", "sector", "country", "asn":
	default:
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument,
			"scope must be global, sector, country, or asn")
		return
	}

	value := strings.TrimSpace(r.URL.Query().Get("value"))
	if scope != "global" && value == "" {
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, "value is required for this scope")
		return
	}
	if scope == "asn" {
		if n, err := strconv.Atoi(value); err != nil || n <= 0 {
			writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, "value must be a positive ASN")
			return
		}
	}

	vantage := s.deps.Vantage
	if vantage == "" {
		vantage = "local"
	}

	counts, judgeable, err := s.deps.Scopes.ScopeStates(r.Context(), vantage, scope, value)
	if err != nil {
		s.deps.Logger.Error("scope read failed", "event", "scope_failed", "component", "http", "error", err)
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "scope unavailable")
		return
	}

	measurable := counts[state.Up] + counts[state.UpReachability] + counts[state.Degraded] + counts[state.Down]
	healthy := counts[state.Up] + counts[state.UpReachability] + counts[state.Degraded]

	coverage := 0.0
	if judgeable > 0 {
		coverage = float64(measurable) / float64(judgeable)
	}

	status := "ok"
	var health *float64
	if judgeable == 0 || coverage < 0.5 || measurable < 8 {
		status = "unknown"
	} else {
		h := float64(healthy) / float64(measurable)
		health = &h
		if coverage < 0.8 || measurable < 20 {
			status = "low_coverage"
		}
	}

	states := make(map[string]int, 8)
	for _, st := range []state.State{
		state.Up, state.UpReachability, state.Degraded, state.Down,
		state.Blocked, state.Unknown, state.ConfigError, state.Unmeasurable,
	} {
		states[string(st)] = counts[st]
	}

	now := time.Now().UTC()
	samples, err := s.deps.Scopes.ScopeHistory(r.Context(), scope, value, now)
	if err != nil {
		s.deps.Logger.Error("scope history failed", "event", "scope_history_failed", "component", "http", "error", err)
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "scope unavailable")
		return
	}
	stats := baseline.Compute(samples)
	mode := "learning"
	if stats.Usable() {
		mode = "normal"
	}

	writeData(w, r, scopeResponse{
		Scope:      scope,
		Value:      value,
		Status:     status,
		Mode:       mode,
		Health:     health,
		Coverage:   coverage,
		Measurable: measurable,
		Judgeable:  judgeable,
		States:     states,
		Baseline: &baselineView{
			N: stats.N, Median: stats.Median, MAD: stats.MAD,
			Sigma: stats.Sigma(), Usable: stats.Usable(),
		},
	}, nil)
}

type anomalyItem struct {
	ID         int64   `json:"id"`
	TS         string  `json:"ts"`
	Scope      string  `json:"scope"`
	ScopeValue string  `json:"scope_value,omitempty"`
	Kind       string  `json:"kind"`
	Observed   float64 `json:"observed"`
	Baseline   float64 `json:"baseline"`
	MAD        float64 `json:"mad"`
	Sigma      float64 `json:"sigma"`
	Buckets    int     `json:"buckets"`
	IncidentID string  `json:"incident_id,omitempty"`
}

// handleAnomalies lists detector output, newest first. An anomaly is an
// observation against a baseline, not a verdict: an incident may follow from it.
func (s *server) handleAnomalies(w http.ResponseWriter, r *http.Request) {
	if s.deps.Anomalies == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "anomalies are not configured")
		return
	}

	limit, err := parseLimit(r.URL.Query().Get("limit"), 50, 200)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, err.Error())
		return
	}

	rows, err := s.deps.Anomalies.Anomalies(r.Context(), limit)
	if err != nil {
		s.deps.Logger.Error("anomaly list failed", "event", "anomaly_list_failed", "component", "http", "error", err)
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "anomalies unavailable")
		return
	}

	out := make([]anomalyItem, 0, len(rows))
	for _, a := range rows {
		out = append(out, anomalyItem{
			ID:         a.ID,
			TS:         a.TS.UTC().Format(time.RFC3339),
			Scope:      a.Scope,
			ScopeValue: a.ScopeValue,
			Kind:       a.Kind,
			Observed:   a.Observed,
			Baseline:   a.Baseline,
			MAD:        a.MAD,
			Sigma:      a.Sigma,
			Buckets:    a.Buckets,
			IncidentID: a.IncidentID,
		})
	}

	writeData(w, r, out, &Meta{Count: len(out), Limit: limit})
}

type coverageBucketItem struct {
	Kind         string  `json:"kind,omitempty"`
	Value        string  `json:"value"`
	Total        int     `json:"total"`
	Measurable   int     `json:"measurable"`
	Unmeasurable int     `json:"unmeasurable"`
	Coverage     float64 `json:"coverage"`
}

type staleTargetItem struct {
	ID         string `json:"id"`
	Host       string `json:"host"`
	State      string `json:"state"`
	LastProbe  string `json:"last_probe,omitempty"`
	IntervalS  int    `json:"interval_s"`
	AgeSeconds int64  `json:"age_seconds,omitempty"`
}

type coverageResponse struct {
	Total      int                  `json:"total"`
	Measurable int                  `json:"measurable"`
	Coverage   float64              `json:"coverage"`
	Classes    []coverageBucketItem `json:"classes"`
	Sectors    []coverageBucketItem `json:"sectors"`
	BlindSpots []coverageBucketItem `json:"blind_spots"`
	Stale      []staleTargetItem    `json:"stale"`
}

func toCoverageBuckets(buckets []store.CoverageBucket) []coverageBucketItem {
	out := make([]coverageBucketItem, 0, len(buckets))
	for _, b := range buckets {
		share := 0.0
		if b.Total > 0 {
			share = float64(b.Measurable) / float64(b.Total)
		}
		out = append(out, coverageBucketItem{
			Kind:  b.Kind,
			Value: b.Value, Total: b.Total, Measurable: b.Measurable,
			Unmeasurable: b.Unmeasurable, Coverage: share,
		})
	}
	return out
}

// handleCoverage answers "where are we blind?". It reports measurement, not
// health: a down target is still measured, and the panel exists to catch the
// targets that are not.
func (s *server) handleCoverage(w http.ResponseWriter, r *http.Request) {
	if s.deps.Coverage == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "coverage is not configured")
		return
	}

	vantage := s.deps.Vantage
	if vantage == "" {
		vantage = "local"
	}

	now := time.Now().UTC()
	cov, err := s.deps.Coverage.Coverage(r.Context(), vantage, now)
	if err != nil {
		s.deps.Logger.Error("coverage read failed", "event", "coverage_failed", "component", "http", "error", err)
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "coverage unavailable")
		return
	}

	out := coverageResponse{
		Total:      cov.Totals.Total,
		Measurable: cov.Totals.Measurable,
		Classes:    toCoverageBuckets(cov.Classes),
		Sectors:    toCoverageBuckets(cov.Sectors),
		BlindSpots: toCoverageBuckets(cov.BlindSpots),
		Stale:      make([]staleTargetItem, 0, len(cov.Stale)),
	}
	if cov.Totals.Total > 0 {
		out.Coverage = float64(cov.Totals.Measurable) / float64(cov.Totals.Total)
	}
	for _, st := range cov.Stale {
		item := staleTargetItem{
			ID: st.ID, Host: st.Host, State: st.State, IntervalS: st.IntervalS,
		}
		if !st.LastProbe.IsZero() {
			item.LastProbe = st.LastProbe.UTC().Format(time.RFC3339)
			item.AgeSeconds = int64(now.Sub(st.LastProbe).Seconds())
		}
		out.Stale = append(out.Stale, item)
	}

	writeData(w, r, out, nil)
}

type selfCheckItem struct {
	Check  string `json:"check"`
	OK     bool   `json:"ok"`
	Value  string `json:"value,omitempty"`
	Detail string `json:"detail,omitempty"`
	TS     string `json:"ts"`
}

// handleSelfCheck reports the newest result per self-health check. It is the
// "can the monitor see at all?" page, and it is separate from target health on
// purpose: a broken monitor must not look like a broken internet.
func (s *server) handleSelfCheck(w http.ResponseWriter, r *http.Request) {
	if s.deps.SelfChecks == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "self-check is not configured")
		return
	}

	rows, err := s.deps.SelfChecks.LatestSelfChecks(r.Context())
	if err != nil {
		s.deps.Logger.Error("self-check read failed", "event", "selfcheck_failed", "component", "http", "error", err)
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "self-check unavailable")
		return
	}

	out := make([]selfCheckItem, 0, len(rows))
	for _, c := range rows {
		out = append(out, selfCheckItem{
			Check: c.Check, OK: c.OK, Value: c.Value, Detail: c.Detail,
			TS: c.TS.UTC().Format(time.RFC3339),
		})
	}

	writeData(w, r, out, &Meta{Count: len(out)})
}

type coveragePointItem struct {
	Bucket       string  `json:"bucket"`
	Scope        string  `json:"scope"`
	ScopeValue   string  `json:"scope_value,omitempty"`
	Total        int     `json:"total"`
	Measurable   int     `json:"measurable"`
	Blocked      int     `json:"blocked"`
	Unmeasurable int     `json:"unmeasurable"`
	Coverage     float64 `json:"coverage"`
}

// handleCoverageHistory returns daily snapshots for one scope. A point-in-time
// panel cannot show "coverage has been decaying for three weeks"; this can.
func (s *server) handleCoverageHistory(w http.ResponseWriter, r *http.Request) {
	if s.deps.CoverageHistory == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "coverage history is not configured")
		return
	}

	q := r.URL.Query()
	scope := strings.TrimSpace(q.Get("scope"))
	if scope == "" {
		scope = "global"
	}
	switch scope {
	case "global", "sector", "country", "asn":
	default:
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument,
			"scope must be global, sector, country, or asn")
		return
	}

	value := strings.TrimSpace(q.Get("value"))
	if scope != "global" && value == "" {
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, "value is required for this scope")
		return
	}
	if scope == "asn" {
		if n, err := strconv.Atoi(value); err != nil || n <= 0 {
			writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, "value must be a positive ASN")
			return
		}
	}

	days, err := strconv.Atoi(q.Get("days"))
	if q.Get("days") == "" {
		days = 30
	} else if err != nil || days < 1 || days > 365 {
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, "days must be 1-365")
		return
	}

	points, err := s.deps.CoverageHistory.CoverageHistory(r.Context(), scope, value, days)
	if err != nil {
		s.deps.Logger.Error("coverage history failed", "event", "coverage_history_failed", "component", "http", "error", err)
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "coverage history unavailable")
		return
	}

	out := make([]coveragePointItem, 0, len(points))
	for _, p := range points {
		item := coveragePointItem{
			Bucket:     p.Bucket.UTC().Format("2006-01-02"),
			Scope:      p.Scope,
			ScopeValue: p.ScopeValue,
			Total:      p.Total, Measurable: p.Measurable,
			Blocked: p.Blocked, Unmeasurable: p.Unmeasurable,
		}
		if p.Total > 0 {
			item.Coverage = float64(p.Measurable) / float64(p.Total)
		}
		out = append(out, item)
	}

	writeData(w, r, out, &Meta{Count: len(out), Limit: days})
}

type controlState struct {
	Paused bool `json:"paused"`
}

// exportHeader is the CSV column contract. It is stable on purpose: an operator
// piping this into a spreadsheet should not have to rename columns on upgrade.
var exportHeader = []string{
	"target_id", "host", "vantage", "ts", "state", "stage",
	"error_class", "http_status", "latency_ms", "confidence", "detail",
}

// handleExport streams observations for a time range as CSV or JSONL. Nothing
// is buffered: the store calls back one row at a time.
func (s *server) handleExport(w http.ResponseWriter, r *http.Request) {
	if s.deps.Exporter == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "export is not configured")
		return
	}

	q := r.URL.Query()
	format := q.Get("format")
	if format == "" {
		format = "csv"
	}
	if format != "csv" && format != "jsonl" {
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, "format must be csv or jsonl")
		return
	}

	now := time.Now().UTC()
	from := now.Add(-24 * time.Hour)
	to := now

	parseTime := func(name string, dst *time.Time) bool {
		raw := q.Get(name)
		if raw == "" {
			return true
		}
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, name+" must be RFC 3339")
			return false
		}
		*dst = parsed
		return true
	}
	if !parseTime("from", &from) || !parseTime("to", &to) {
		return
	}
	if !from.Before(to) {
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, "from must be before to")
		return
	}
	// A month is the largest window that still streams politely; the CLI has no
	// such cap because the operator can wait, and a client cannot.
	if to.Sub(from) > 31*24*time.Hour {
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, "window must be 31 days or less")
		return
	}

	switch format {
	case "csv":
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="netweather-probes.csv"`)
		w.WriteHeader(http.StatusOK)

		cw := csv.NewWriter(w)
		if err := cw.Write(exportHeader); err != nil {
			return
		}
		err := s.deps.Exporter.ExportProbes(r.Context(), from, to, func(row store.ExportRow) error {
			if err := cw.Write(exportRow(row)); err != nil {
				return err
			}
			// Flush per row so a long export is visible as it runs.
			cw.Flush()
			return cw.Error()
		})
		cw.Flush()
		if err != nil {
			s.deps.Logger.Warn("export failed mid-stream",
				"event", "export_failed", "component", "http", "error", err)
		}

	case "jsonl":
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="netweather-probes.jsonl"`)
		w.WriteHeader(http.StatusOK)

		enc := json.NewEncoder(w)
		err := s.deps.Exporter.ExportProbes(r.Context(), from, to, func(row store.ExportRow) error {
			return enc.Encode(exportObject(row))
		})
		if err != nil {
			s.deps.Logger.Warn("export failed mid-stream",
				"event", "export_failed", "component", "http", "error", err)
		}
	}
}

func exportRow(row store.ExportRow) []string {
	return []string{
		row.TargetID, row.Host, row.VantageID, row.TS.UTC().Format(time.RFC3339),
		string(row.State), row.Stage, row.ErrorClass,
		strconv.Itoa(row.HTTPStatus), strconv.Itoa(row.LatencyMS),
		row.Confidence, row.Detail,
	}
}

func exportObject(row store.ExportRow) map[string]any {
	return map[string]any{
		"target_id":   row.TargetID,
		"host":        row.Host,
		"vantage":     row.VantageID,
		"ts":          row.TS.UTC().Format(time.RFC3339),
		"state":       string(row.State),
		"stage":       row.Stage,
		"error_class": row.ErrorClass,
		"http_status": row.HTTPStatus,
		"latency_ms":  row.LatencyMS,
		"confidence":  row.Confidence,
		"detail":      row.Detail,
	}
}

// sseState is one delta in the API stream. It mirrors the UI's shape on purpose:
// there is one contract, not two by accident.
type sseState struct {
	Target  string `json:"target"`
	Name    string `json:"name"`
	State   string `json:"state"`
	Text    string `json:"state_text"`
	Glyph   string `json:"glyph"`
	Class   string `json:"class"`
	Since   string `json:"since,omitempty"`
	Vantage string `json:"vantage"`
}

type sseSnapshot struct {
	Targets []sseState `json:"targets"`
}

// handleEvents is the bearer-authenticated SSE stream. It sends one snapshot on
// connect, then only changes, plus a heartbeat comment so proxies keep the
// connection.
//
// It does not implement Last-Event-ID: the board broadcast carries no replay
// buffer, so a reconnecting client refetches /api/v1/board. That is stated in
// the contract rather than silently pretending to resume.
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	// A stream with no board to report is not a stream; say so instead of
	// holding a connection open that will only ever heartbeat.
	if s.deps.Source == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "board source is not configured")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, CodeInternal, "streaming unsupported")
		return
	}

	release, ok := s.streams.Acquire(remoteHost(r))
	if !ok {
		w.Header().Set("Retry-After", "60")
		writeError(w, r, http.StatusTooManyRequests, CodeRateLimited,
			"too many concurrent event streams")
		return
	}
	defer release()

	// Normal requests have a 30s write timeout; this one outlives it.
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

	writeState := func(event string, v any) bool {
		data, err := json.Marshal(v)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return false
		}
		return true
	}

	// Subscribe before the snapshot is written. A subscriber that only attaches
	// afterwards can miss a change that happens in the gap, which is a real
	// lost-wakeup for a client and a hang for a test that notifies as soon as it
	// reads the snapshot.
	var ch <-chan struct{}
	if s.deps.Watcher != nil {
		boardCh, cancel := s.deps.Watcher.Subscribe()
		defer cancel()
		ch = boardCh
	}

	var incidentCh <-chan struct{}
	if s.deps.IncidentWatcher != nil {
		incCh, incCancel := s.deps.IncidentWatcher.Subscribe()
		defer incCancel()
		incidentCh = incCh
	}

	last, err := s.streamSnapshot(r.Context())
	if err != nil {
		_ = writeState("error", map[string]string{"message": "board unavailable"})
		flusher.Flush()
		return
	}
	if !writeState("snapshot", sseSnapshot{Targets: last.list()}) {
		return
	}
	flusher.Flush()

	if ch == nil {
		s.heartbeat(w, r, flusher)
		return
	}

	emitIncidents := func(ctx context.Context) bool {
		if s.deps.Incidents == nil {
			return true
		}
		open, err := s.deps.Incidents.Incidents(ctx, true, 50)
		if err != nil {
			s.deps.Logger.Warn("incident stream read failed",
				"event", "incident_stream_failed", "component", "http", "error", err)
			return true
		}
		for _, inc := range open {
			if !writeState("incident", incidentEvent{
				ID: inc.ID, Verdict: inc.Verdict, Scope: inc.Scope,
				ScopeValue: inc.ScopeValue, Headline: inc.Headline,
				Confidence: inc.Confidence,
				OpenedAt:   inc.OpenedAt.UTC().Format(time.RFC3339),
			}) {
				return false
			}
		}
		return true
	}

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
		case <-incidentCh:
			// Open incidents are re-sent whole; a client upserts by id. A close
			// is not sent as an event because the stream carries current state,
			// not a diff, and guessing would be worse than a refetch.
			if !emitIncidents(r.Context()) {
				return
			}
			flusher.Flush()
		case <-ch:
			current, err := s.streamSnapshot(r.Context())
			if err != nil {
				return
			}
			for _, id := range current.ids() {
				next := current[id]
				if prev, seen := last[id]; seen && prev.State == next.State {
					continue
				}
				if !writeState("state", next) {
					return
				}
			}
			last = current
			flusher.Flush()
		}
	}
}

// incidentEvent is the wire shape for an incident on the stream.
type incidentEvent struct {
	ID         string `json:"id"`
	Verdict    string `json:"verdict"`
	Scope      string `json:"scope"`
	ScopeValue string `json:"scope_value,omitempty"`
	Headline   string `json:"headline"`
	Confidence string `json:"confidence"`
	OpenedAt   string `json:"opened_at"`
}

// sseStates is the board keyed by host, because that is the stable identity a
// script has without reading target ids first.
type sseStates map[string]sseState

func (m sseStates) ids() []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (m sseStates) list() []sseState {
	out := make([]sseState, 0, len(m))
	for _, id := range m.ids() {
		out = append(out, m[id])
	}
	return out
}

func (s *server) streamSnapshot(ctx context.Context) (sseStates, error) {
	targets, err := s.deps.Source.Snapshot(ctx)
	if err != nil {
		return nil, err
	}

	out := make(sseStates, len(targets))
	for _, t := range targets {
		state := sseState{
			Target: t.Host, Name: t.Name, State: string(t.State),
			Text: t.State.Text(), Glyph: t.State.Glyph(), Class: t.State.CSSClass(),
			Vantage: s.vantage(),
		}
		if !t.Since.IsZero() {
			state.Since = t.Since.UTC().Format(time.RFC3339)
		}
		out[t.Host] = state
	}
	return out, nil
}

func (s *server) vantage() string {
	if s.deps.Vantage == "" {
		return "local"
	}
	return s.deps.Vantage
}

func (s *server) heartbeat(w http.ResponseWriter, r *http.Request, flusher http.Flusher) {
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

type feedStateItem struct {
	Feed                string `json:"feed"`
	LastOKAt            string `json:"last_ok_at,omitempty"`
	LastErr             string `json:"last_err,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	NextPollAt          string `json:"next_poll_at,omitempty"`
	Stale               bool   `json:"stale"`
}

type externalEventItem struct {
	Feed       string  `json:"feed"`
	ExternalID string  `json:"external_id"`
	Kind       string  `json:"kind"`
	Severity   float64 `json:"severity,omitempty"`
	Country    string  `json:"country,omitempty"`
	ASN        int     `json:"asn,omitempty"`
	StartedAt  string  `json:"started_at"`
	EndedAt    string  `json:"ended_at,omitempty"`
	ObservedAt string  `json:"observed_at"`
}

type feedsResponse struct {
	Feeds  []feedStateItem     `json:"feeds"`
	Events []externalEventItem `json:"events"`
}

// handleFeeds reports feed health and recent external events. Staleness is
// computed here, not stored: a feed is stale when its last success is older than
// three default poll intervals, and a stale feed's events are evidence with an
// expiry date, which the client can see.
func (s *server) handleFeeds(w http.ResponseWriter, r *http.Request) {
	if s.deps.Feeds == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "feeds are not configured")
		return
	}

	limit, err := parseLimit(r.URL.Query().Get("limit"), 50, 200)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, err.Error())
		return
	}

	states, err := s.deps.Feeds.FeedStates(r.Context())
	if err != nil {
		s.deps.Logger.Error("feed states failed", "event", "feed_states_failed", "component", "http", "error", err)
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "feeds unavailable")
		return
	}
	events, err := s.deps.Feeds.ExternalEvents(r.Context(), limit)
	if err != nil {
		s.deps.Logger.Error("external events failed", "event", "feed_events_failed", "component", "http", "error", err)
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "feeds unavailable")
		return
	}

	now := time.Now().UTC()
	staleAfter := 30 * time.Minute

	out := feedsResponse{
		Feeds:  make([]feedStateItem, 0, len(states)),
		Events: make([]externalEventItem, 0, len(events)),
	}
	for _, st := range states {
		item := feedStateItem{
			Feed: st.Feed, LastErr: st.LastErr,
			ConsecutiveFailures: st.ConsecutiveFailures,
		}
		if !st.LastOKAt.IsZero() {
			item.LastOKAt = st.LastOKAt.UTC().Format(time.RFC3339)
			item.Stale = now.Sub(st.LastOKAt) > staleAfter
		} else {
			item.Stale = true
		}
		if !st.NextPollAt.IsZero() {
			item.NextPollAt = st.NextPollAt.UTC().Format(time.RFC3339)
		}
		out.Feeds = append(out.Feeds, item)
	}
	for _, e := range events {
		item := externalEventItem{
			Feed: e.Feed, ExternalID: e.ExternalID, Kind: e.Kind,
			Severity: e.Severity, Country: e.Country, ASN: e.ASN,
			StartedAt:  e.StartedAt.UTC().Format(time.RFC3339),
			ObservedAt: e.ObservedAt.UTC().Format(time.RFC3339),
		}
		if !e.EndedAt.IsZero() {
			item.EndedAt = e.EndedAt.UTC().Format(time.RFC3339)
		}
		out.Events = append(out.Events, item)
	}

	writeData(w, r, out, &Meta{Count: len(out.Events), Limit: limit})
}

type debugScopeRequest struct {
	Target     string `json:"target"`
	Vantage    string `json:"vantage"`
	Component  string `json:"component"`
	ErrorClass string `json:"error_class"`
	Level      string `json:"level"`
	TTL        string `json:"ttl"`
}

// handleDebugScopes lists and creates time-boxed verbose sessions. There is no
// "trace everything" body: a scope needs at least one selector.
func (s *server) handleDebugScopes(w http.ResponseWriter, r *http.Request) {
	if s.deps.Debug == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "debug scopes are not configured")
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		list := s.deps.Debug.List()
		writeData(w, r, list, &Meta{Count: len(list)})

	case http.MethodPost:
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			writeError(w, r, http.StatusUnsupportedMediaType, CodeInvalidArgument, "Content-Type must be application/json")
			return
		}

		var req debugScopeRequest
		dec := json.NewDecoder(io.LimitReader(r.Body, 8<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, "invalid JSON body: "+err.Error())
			return
		}
		if req.Level == "" {
			req.Level = "debug"
		}

		ttl, err := time.ParseDuration(req.TTL)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, "ttl must be a duration such as 10m")
			return
		}

		scope, err := s.deps.Debug.Add(logging.Scope{
			Target: req.Target, Vantage: req.Vantage,
			Component: req.Component, ErrorClass: req.ErrorClass,
			Level: req.Level,
		}, ttl)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, err.Error())
			return
		}

		s.deps.Logger.Info("debug scope enabled",
			"event", "debug_scope_enabled",
			"component", "http",
			"request_id", requestIDFrom(r),
			"scope_id", scope.ID,
			"target", scope.Target,
			"component_scope", scope.Component,
			"error_class", scope.ErrorClass,
			"level", scope.Level,
			"expires_at", scope.ExpiresAt.Format(time.RFC3339),
		)

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": scope,
			"meta": map[string]string{"request_id": requestIDFrom(r)},
		})

	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		writeError(w, r, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
			r.Method+" is not allowed on "+r.URL.Path)
	}
}

// handleDebugScopeByID ends one session early.
func (s *server) handleDebugScopeByID(w http.ResponseWriter, r *http.Request) {
	if s.deps.Debug == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "debug scopes are not configured")
		return
	}
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		writeError(w, r, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
			r.Method+" is not allowed on "+r.URL.Path)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/api/v1/debug/scopes/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "no such scope")
		return
	}
	if !s.deps.Debug.Remove(id) {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "no such scope")
		return
	}

	s.deps.Logger.Info("debug scope disabled",
		"event", "debug_scope_disabled", "component", "http",
		"request_id", requestIDFrom(r), "scope_id", id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleControl(w http.ResponseWriter, r *http.Request) {
	if s.deps.Control == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "control is not configured")
		return
	}
	writeData(w, r, controlState{Paused: s.deps.Control.Paused()}, nil)
}

func (s *server) handlePause(w http.ResponseWriter, r *http.Request)  { s.setPaused(w, r, true) }
func (s *server) handleResume(w http.ResponseWriter, r *http.Request) { s.setPaused(w, r, false) }

func (s *server) setPaused(w http.ResponseWriter, r *http.Request, paused bool) {
	if s.deps.Control == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "control is not configured")
		return
	}

	if paused {
		s.deps.Control.Pause()
	} else {
		s.deps.Control.Resume()
	}

	// The kill switch is an auditable action: who asked, and for what.
	s.deps.Logger.Info("probe control",
		"event", "probe_control",
		"component", "http",
		"request_id", requestIDFrom(r),
		"paused", paused,
	)

	writeData(w, r, controlState{Paused: paused}, nil)
}

type logRecord struct {
	TS      string            `json:"ts"`
	Level   string            `json:"level"`
	Message string            `json:"message"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

func (s *server) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	minLevel, err := parseLevel(q.Get("level"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, err.Error())
		return
	}
	since, err := parseSince(q.Get("since"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, err.Error())
		return
	}
	limit, err := parseLimit(q.Get("limit"), 100, 500)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, err.Error())
		return
	}
	target := strings.ToLower(strings.TrimSpace(q.Get("target")))

	var recs []logging.Record
	if s.deps.Ring != nil {
		recs = s.deps.Ring.Snapshot()
	}

	out := make([]logRecord, 0, min(len(recs), limit))
	for _, rec := range recs {
		if rec.Level < minLevel {
			continue
		}
		if !since.IsZero() && !rec.Time.After(since) {
			continue
		}
		if target != "" && !strings.Contains(strings.ToLower(rec.Attrs["target"]), target) {
			continue
		}
		out = append(out, logRecord{
			TS:      rec.Time.UTC().Format(time.RFC3339Nano),
			Level:   rec.LevelName,
			Message: rec.Message,
			Attrs:   rec.Attrs,
		})
	}

	if len(out) > limit {
		out = out[len(out)-limit:]
	}

	writeData(w, r, out, &Meta{Count: len(out), Limit: limit})
}

func parseLevel(s string) (slog.Level, error) {
	if strings.TrimSpace(s) == "" {
		return logging.LevelTrace, nil
	}
	lv, err := logging.ParseLevel(s)
	if err != nil {
		return 0, fmt.Errorf("level: %w", err)
	}
	return lv, nil
}

func parseSince(s string) (time.Time, error) {
	if strings.TrimSpace(s) == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("since must be RFC 3339, e.g. 2026-11-16T03:14:15Z")
	}
	return t, nil
}

func parseLimit(s string, def, max int) (int, error) {
	if strings.TrimSpace(s) == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > max {
		return 0, fmt.Errorf("limit must be between 1 and %d", max)
	}
	return n, nil
}
