package web

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/Greg-2600/netweather/internal/board"
)

// webTraceTimeout bounds one drawer walk. The API uses the same ceiling; a
// trace that outlives a minute is not a useful hint any more.
const webTraceTimeout = 60 * time.Second

// traceHopView is one row of the drawer's trace table.
type traceHopView struct {
	TTL     int
	Addr    string
	RTT     string
	Timeout bool
}

// tracePageView is the drawer's on-demand trace result: a short page rather
// than a rewrite of the target page, so the action cannot disturb the board's
// state or the drawer's many panels.
type tracePageView struct {
	Insecure    bool
	NoAuth      bool
	ID          string
	Name        string
	Host        string
	Destination string
	Reached     bool
	Hops        []traceHopView
	Note        string
}

// handleTraceAction runs one on-demand traceroute from the drawer.
//
// It shares the API's instance-wide budget, so the documented ceiling is ten
// runs per hour for the process rather than ten per surface (THREAT_MODEL.md
// T29). A disabled capability, an exhausted budget, and an unresolvable host
// are stated on the page instead of returning a bare 5xx.
func (s *server) handleTraceAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		s.renderError(w, r, http.StatusMethodNotAllowed,
			"A traceroute is an action; use the button on the target page.")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxActionBody)
	if err := r.ParseForm(); err != nil || !s.auth.csrfMatches(r, r.PostFormValue("csrf")) {
		s.renderError(w, r, http.StatusBadRequest,
			"The request could not be verified. Reload the target page and try again.")
		return
	}

	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/targets/"), "/trace")
	if id == "" || strings.Contains(id, "/") {
		s.renderError(w, r, http.StatusNotFound, "No such target.")
		return
	}

	targets, err := s.deps.Source.Snapshot(r.Context())
	if err != nil {
		s.deps.Logger.Error("trace target snapshot failed",
			"event", "trace_target_snapshot_failed", "component", "http",
			"target_id", id, "request_id", requestIDFrom(r), "error", err)
		s.renderError(w, r, http.StatusServiceUnavailable,
			"The target could not be read. The server log has the details for this request.")
		return
	}

	var target board.Target
	found := false
	for _, t := range targets {
		if t.ID == id {
			target, found = t, true
			break
		}
	}
	if !found {
		s.renderError(w, r, http.StatusNotFound, "No such target.")
		return
	}

	view := tracePageView{ID: target.ID, Name: target.Name, Host: target.Host}

	switch {
	case !s.deps.TraceEnabled || s.deps.Tracer == nil || s.deps.Resolver == nil:
		view.Note = "Traceroute is disabled on this instance. Set NETWEATHER_TRACE_ENABLED=true and grant CAP_NET_RAW (docs/IMAGE.md §5)."
	case s.deps.TraceBudget == nil:
		view.Note = "Traceroute has no budget configured, so this instance will not run one."
	default:
		s.runTracePage(w, r, target, &view)
	}

	s.renderTrace(w, r, view)
}

// runTracePage spends one budget unit and, when it can, runs the walk.
func (s *server) runTracePage(w http.ResponseWriter, r *http.Request, target board.Target, view *tracePageView) {
	if ok, retry := s.deps.TraceBudget.Allow(); !ok {
		wait := retry.Round(time.Minute)
		if wait < time.Minute {
			wait = time.Minute
		}
		view.Note = fmt.Sprintf(
			"Traceroute is limited to ten runs per hour across the instance. Try again in about %s.", wait)
		return
	}

	addrs, err := s.deps.Resolver.CheckHost(r.Context(), target.Host)
	if err != nil || len(addrs) == 0 {
		view.Note = "The target host could not be resolved for a trace."
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), webTraceTimeout)
	defer cancel()

	res := s.deps.Tracer.Run(ctx, addrs[0])
	if res.Destination.IsValid() {
		view.Destination = res.Destination.String()
	}
	view.Reached = res.Reached
	for _, hop := range res.Hops {
		row := traceHopView{TTL: hop.TTL, Timeout: !hop.Addr.IsValid()}
		if hop.Addr.IsValid() {
			row.Addr = hop.Addr.String()
			row.RTT = hop.RTT.Round(time.Millisecond).String()
		}
		view.Hops = append(view.Hops, row)
	}

	s.deps.Logger.Info("traceroute",
		"event", "traceroute", "component", "http", "surface", "ui",
		"target", target.Host, "hops", len(view.Hops), "reached", view.Reached,
		"request_id", requestIDFrom(r))

	if res.Err != nil {
		view.Note = "Traceroute could not start: " + res.Err.Error()
		return
	}
	view.Note = traceCaveat(view.Hops, view.Reached)
}

// traceCaveat renders NETWORKING.md §8's container artifacts in words: a
// private first hop is the local gateway, and a walk that stops there is a
// NAT/conntrack artifact rather than a path failure.
func traceCaveat(hops []traceHopView, reached bool) string {
	note := ""
	if len(hops) > 0 && !hops[0].Timeout {
		if addr, err := netip.ParseAddr(hops[0].Addr); err == nil &&
			(addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast()) {
			note = "hop 1 is a private address, likely the local gateway rather than a wide-area hop"
		}
	}
	if !reached && len(hops) <= 1 {
		if note != "" {
			note += "; "
		}
		note += "the walk stopped after the first hop; in a container this is a NAT/conntrack artifact, not a path failure"
	}
	return note
}

func (s *server) renderTrace(w http.ResponseWriter, r *http.Request, view tracePageView) {
	view.Insecure = s.insecure(r)
	view.NoAuth = s.auth.disabled()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	_ = s.tmpl.ExecuteTemplate(w, "trace.html", view)
}
