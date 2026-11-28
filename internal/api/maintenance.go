package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Greg-2600/netweather/internal/store"
)

// maintenanceItem is the wire shape of one declared window. State is derived
// from completed_at on read, so a window cannot claim to be complete while its
// completion timestamp is missing.
type maintenanceItem struct {
	ID          int64  `json:"id"`
	Components  string `json:"components"`
	Reason      string `json:"reason"`
	StartsAt    string `json:"starts_at"`
	EndsAt      string `json:"ends_at"`
	State       string `json:"state"`
	CompletedAt string `json:"completed_at,omitempty"`
}

func toMaintenanceItem(w store.MaintenanceWindow) maintenanceItem {
	item := maintenanceItem{
		ID:         w.ID,
		Components: w.Components,
		Reason:     w.Reason,
		StartsAt:   w.StartsAt.UTC().Format(time.RFC3339),
		EndsAt:     w.EndsAt.UTC().Format(time.RFC3339),
		State:      "planned",
	}
	if !w.CompletedAt.IsZero() {
		item.State = "completed"
		item.CompletedAt = w.CompletedAt.UTC().Format(time.RFC3339)
	}
	return item
}

// handleMaintenanceCollection lists declared windows and accepts new ones.
// Methods are gated here because the same path serves both a read and an
// operator action.
func (s *server) handleMaintenanceCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.handleMaintenanceList(w, r)
	case http.MethodPost:
		s.handleMaintenanceCreate(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, r, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
			r.Method+" is not allowed on "+r.URL.Path)
	}
}

func (s *server) handleMaintenanceList(w http.ResponseWriter, r *http.Request) {
	if s.deps.Maintenance == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "maintenance windows are not configured")
		return
	}
	limit, err := parseLimit(r.URL.Query().Get("limit"), 50, 200)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, err.Error())
		return
	}
	rows, err := s.deps.Maintenance.MaintenanceWindows(r.Context(), limit)
	if err != nil {
		s.deps.Logger.Error("maintenance window list failed", "event", "maintenance_list_failed",
			"component", "http", "error", err)
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "maintenance windows unavailable")
		return
	}
	out := make([]maintenanceItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, toMaintenanceItem(row))
	}
	writeData(w, r, out, &Meta{Count: len(out), Limit: limit})
}

type maintenanceRequest struct {
	Components string `json:"components"`
	Reason     string `json:"reason"`
	StartsAt   string `json:"starts_at"`
	EndsAt     string `json:"ends_at"`
}

func (s *server) handleMaintenanceCreate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Maintenance == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "maintenance windows are not configured")
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeError(w, r, http.StatusUnsupportedMediaType, CodeInvalidArgument,
			"Content-Type must be application/json")
		return
	}
	var req maintenanceRequest
	if err := decodeJSONBody(w, r, maxActionBody, &req); err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeError(w, r, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, "request body is too large")
			return
		}
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, "invalid JSON body: "+err.Error())
		return
	}
	components := strings.TrimSpace(req.Components)
	reason := strings.TrimSpace(req.Reason)
	if components == "" || len(components) > 200 {
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, "components must be 1-200 characters")
		return
	}
	if reason == "" || len(reason) > 500 {
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, "reason must be 1-500 characters")
		return
	}
	startsAt, err := time.Parse(time.RFC3339, req.StartsAt)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, "starts_at must be an RFC3339 timestamp")
		return
	}
	endsAt, err := time.Parse(time.RFC3339, req.EndsAt)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, "ends_at must be an RFC3339 timestamp")
		return
	}
	if !endsAt.After(startsAt) || endsAt.Sub(startsAt) > 30*24*time.Hour {
		writeError(w, r, http.StatusBadRequest, CodeInvalidArgument,
			"ends_at must be after starts_at and within 30 days")
		return
	}

	id, err := s.deps.Maintenance.CreateMaintenanceWindow(r.Context(), components, reason, startsAt, endsAt)
	if err != nil {
		s.deps.Logger.Error("maintenance window create failed", "event", "maintenance_create_failed",
			"component", "http", "error", err)
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "maintenance window unavailable")
		return
	}
	s.deps.Logger.Info("maintenance window created",
		"event", "maintenance_window_created", "component", "http",
		"request_id", requestIDFrom(r), "maintenance_id", id)
	writeData(w, r, map[string]any{
		"id":         id,
		"components": components,
		"reason":     reason,
		"starts_at":  startsAt.UTC().Format(time.RFC3339),
		"ends_at":    endsAt.UTC().Format(time.RFC3339),
		"state":      "planned",
	}, nil)
}

// handleMaintenanceSubtree routes the completion action. Unknown suffixes are
// a 404 rather than a silently accepted write.
func (s *server) handleMaintenanceSubtree(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/complete") {
		s.handleMaintenanceComplete(w, r)
		return
	}
	writeError(w, r, http.StatusNotFound, CodeNotFound, "no such maintenance window path")
}

func (s *server) handleMaintenanceComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, r, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
			r.Method+" is not allowed on "+r.URL.Path)
		return
	}
	if s.deps.Maintenance == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "maintenance windows are not configured")
		return
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/maintenance/"), "/complete")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 || strings.Contains(raw, "/") {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "no such maintenance window")
		return
	}
	at := time.Now().UTC()
	if err := s.deps.Maintenance.CompleteMaintenanceWindow(r.Context(), id, at); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, r, http.StatusNotFound, CodeNotFound, "no such maintenance window")
			return
		}
		s.deps.Logger.Error("maintenance window completion failed", "event", "maintenance_complete_failed",
			"component", "http", "maintenance_id", id, "error", err)
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "maintenance window unavailable")
		return
	}
	s.deps.Logger.Info("maintenance window completed",
		"event", "maintenance_window_completed", "component", "http",
		"request_id", requestIDFrom(r), "maintenance_id", id)
	writeData(w, r, map[string]any{
		"id":           id,
		"state":        "completed",
		"completed_at": at.Format(time.RFC3339),
	}, nil)
}
