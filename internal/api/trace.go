package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Greg-2600/netweather/internal/probe"
)

// Tracer runs one on-demand traceroute. probe.Trace satisfies it.
type Tracer interface {
	Run(ctx context.Context, dst netip.Addr) probe.TraceResult
}

// HostResolver turns a target hostname into addresses. netx.Dialer satisfies
// it, so a trace resolves through the same guarded path probes use.
type HostResolver interface {
	CheckHost(ctx context.Context, host string) ([]netip.Addr, error)
}

// traceLimit is the global ceiling, not per client: traceroute is the most
// visible thing this binary can do, and it is on-demand (DESIGN.md §5.4).
const (
	traceLimit  = 10
	traceWindow = time.Hour
	traceBudget = 60 * time.Second
)

// traceGovernor is a fixed-window counter. A caller gets a retry hint when the
// window is exhausted, because "try later" without a time is not an answer.
type traceGovernor struct {
	mu      sync.Mutex
	started time.Time
	count   int
	now     func() time.Time
}

func newTraceGovernor() *traceGovernor {
	return &traceGovernor{now: time.Now}
}

func (g *traceGovernor) allow() (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	if g.started.IsZero() || now.Sub(g.started) >= traceWindow {
		g.started = now
		g.count = 0
	}
	if g.count >= traceLimit {
		return false, traceWindow - now.Sub(g.started)
	}
	g.count++
	return true, 0
}

type traceHopItem struct {
	TTL     int    `json:"ttl"`
	Addr    string `json:"addr,omitempty"`
	RTTMS   int64  `json:"rtt_ms,omitempty"`
	Timeout bool   `json:"timeout"`
}

type traceResponse struct {
	Target      string         `json:"target"`
	Host        string         `json:"host"`
	Destination string         `json:"destination"`
	Reached     bool           `json:"reached"`
	Hops        []traceHopItem `json:"hops"`
}

// handleTrace runs an on-demand traceroute for one target.
//
// It is deliberately narrow: IPv4 ICMP, ten runs per hour across the whole
// instance, and a visible reason when the capability is missing rather than a
// 500. A trace is a hint about where a path bends; it never changes a target's
// state.
func (s *server) handleTrace(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, r, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
			r.Method+" is not allowed on "+r.URL.Path)
		return
	}

	if !s.deps.TraceEnabled {
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable,
			"traceroute is disabled; set NETWEATHER_TRACE_ENABLED=true and grant CAP_NET_RAW")
		return
	}

	if s.deps.Tracer == nil || s.deps.Resolver == nil || s.deps.Targets == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable,
			"traceroute is not configured on this instance")
		return
	}

	if ok, retry := s.traceBudget.allow(); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		writeError(w, r, http.StatusTooManyRequests, CodeRateLimited,
			fmt.Sprintf("traceroute is limited to %d runs per hour", traceLimit))
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/api/v1/targets/")
	id = strings.TrimSuffix(id, "/trace")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "no such target")
		return
	}

	vantage := s.vantage()
	row, err := s.deps.Targets.TargetByID(r.Context(), vantage, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "no such target")
		return
	}
	if err != nil {
		s.deps.Logger.Error("trace target lookup failed",
			"event", "trace_target_failed", "component", "http", "error", err)
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "target unavailable")
		return
	}

	addrs, err := s.deps.Resolver.CheckHost(r.Context(), row.Host)
	if err != nil || len(addrs) == 0 {
		s.deps.Logger.Warn("trace resolve failed",
			"event", "trace_resolve_failed", "component", "http",
			"target", row.Host, "error", err)
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable,
			"target does not resolve right now")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), traceBudget)
	defer cancel()

	result := s.deps.Tracer.Run(ctx, addrs[0])
	if result.Failed() {
		// "Disabled with a visible reason, not an error" (DESIGN.md §5.5).
		if result.ErrorClass == "trace_unsupported" || result.ErrorClass == "trace_config" {
			writeError(w, r, http.StatusBadRequest, CodeInvalidArgument, result.Err.Error())
			return
		}
		s.deps.Logger.Warn("trace unavailable",
			"event", "trace_unavailable", "component", "http",
			"target", row.Host, "error", result.Err)
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable,
			"traceroute is unavailable: "+errText(result.Err))
		return
	}

	out := traceResponse{
		Target:      row.Name,
		Host:        row.Host,
		Destination: result.Destination.String(),
		Reached:     result.Reached,
		Hops:        make([]traceHopItem, 0, len(result.Hops)),
	}
	for _, hop := range result.Hops {
		item := traceHopItem{TTL: hop.TTL, Timeout: !hop.Addr.IsValid()}
		if hop.Addr.IsValid() {
			item.Addr = hop.Addr.String()
			item.RTTMS = hop.RTT.Milliseconds()
		}
		out.Hops = append(out.Hops, item)
	}

	s.deps.Logger.Info("traceroute",
		"event", "traceroute",
		"component", "http",
		"request_id", requestIDFrom(r),
		"target", row.Host,
		"destination", out.Destination,
		"hops", len(out.Hops),
		"reached", out.Reached,
	)

	writeData(w, r, out, nil)
}

func errText(err error) string {
	if err == nil {
		return "no detail"
	}
	return err.Error()
}
