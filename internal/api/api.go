package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Greg-2600/netweather/internal/board"
	"github.com/Greg-2600/netweather/internal/logging"
)

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
	// Fallback serves everything that is not /healthz or /api/. It is the UI.
	// Nil means "JSON 404", which is the right default for tests.
	Fallback http.Handler
}

type server struct {
	deps     Deps
	authMode string
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
	s := &server{deps: deps, authMode: cfg.AuthMode()}

	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/api/v1/board", onlyGET(s.handleBoard))
	apiMux.HandleFunc("/api/v1/meta", onlyGET(s.handleMeta))
	apiMux.HandleFunc("/api/v1/logs", onlyGET(s.handleLogs))
	apiMux.HandleFunc("/", s.handleNotFound)

	// 60 requests/minute sustained, 20 burst (docs/API.md §8).
	limiter := newBucket(1, 20, time.Now)

	root := http.NewServeMux()
	root.HandleFunc("/healthz", onlyGET(s.handleHealthz))
	root.Handle("/api/", withRateLimit(limiter, withBearerAuth(cfg.Token, apiMux)))
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
var capabilities = []string{"health", "meta", "logs", "board"}

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
