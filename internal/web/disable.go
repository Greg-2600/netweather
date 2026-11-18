package web

import (
	"net/http"
	"strings"
)

// handleTargetDisable stops probing one target from the drawer. It is the
// abuse-response path from ACCEPTABLE_USE.md §7: the row and its history stay,
// the live scheduler drops the target on the next tick, and a config reload can
// bring it back. It is never a delete.
//
// It is a separate POST under /targets/{id}/disable, with its own method and
// CSRF checks, and it is dispatched before the page's GET guard in handleTarget.
func (s *server) handleTargetDisable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		s.renderError(w, r, http.StatusMethodNotAllowed,
			"Disabling a target is an action; use the button on the target page.")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxActionBody)
	if err := r.ParseForm(); err != nil || !s.auth.csrfMatches(r, r.PostFormValue("csrf")) {
		s.renderError(w, r, http.StatusForbidden,
			"The request could not be verified. Reload the target page and try again.")
		return
	}

	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/targets/"), "/disable")
	if id == "" || strings.Contains(id, "/") {
		s.renderError(w, r, http.StatusNotFound, "No such target.")
		return
	}

	if s.deps.TargetControl == nil {
		s.renderError(w, r, http.StatusServiceUnavailable,
			"Runtime target control is not configured on this instance.")
		return
	}

	found, err := s.deps.TargetControl.DisableTarget(r.Context(), id)
	if err != nil {
		s.deps.Logger.Error("target disable failed",
			"event", "target_disable_failed", "component", "http",
			"target_id", id, "request_id", requestIDFrom(r), "error", err)
		s.renderError(w, r, http.StatusServiceUnavailable,
			"The target could not be disabled. The server log has the details.")
		return
	}
	if !found {
		s.renderError(w, r, http.StatusNotFound, "No such target.")
		return
	}

	s.deps.Logger.Warn("target disabled from the dashboard",
		"event", "target_disabled", "component", "http",
		"target_id", id, "request_id", requestIDFrom(r))

	http.Redirect(w, r, "/", http.StatusSeeOther)
}
