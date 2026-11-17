// Package metrics exposes aggregate Prometheus text. It deliberately has no
// per-target labels: 750 targets times several labels is a cardinality incident,
// and the point of the endpoint is fleet-level health, not a second board.
package metrics

import (
	"context"
	"fmt"
	"net/http"

	"github.com/Greg-2600/netweather/internal/state"
)

// Source is the aggregate data the endpoint needs.
type Source interface {
	StateCounts(ctx context.Context, vantageID string) (map[state.State]int, error)
	OpenIncidentCount(ctx context.Context) (int, error)
}

var stateOrder = []state.State{
	state.Up, state.UpReachability, state.Degraded, state.Down,
	state.Blocked, state.Unknown, state.ConfigError, state.Unmeasurable,
}

// Handler returns the /metrics handler for one vantage.
func Handler(src Source, vantage string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counts, err := src.StateCounts(r.Context(), vantage)
		if err != nil {
			http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
			return
		}
		open, err := src.OpenIncidentCount(r.Context())
		if err != nil {
			http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		_, _ = fmt.Fprintln(w, "# HELP netweather_targets Number of targets by state.")
		_, _ = fmt.Fprintln(w, "# TYPE netweather_targets gauge")
		for _, s := range stateOrder {
			_, _ = fmt.Fprintf(w, "netweather_targets{state=%q} %d\n", string(s), counts[s])
		}

		_, _ = fmt.Fprintln(w, "# HELP netweather_incidents_open Unclosed incidents.")
		_, _ = fmt.Fprintln(w, "# TYPE netweather_incidents_open gauge")
		_, _ = fmt.Fprintf(w, "netweather_incidents_open %d\n", open)
	})
}
