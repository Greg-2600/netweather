// Package metrics exposes aggregate Prometheus text. It deliberately has no
// per-target labels: labels multiplied across the fleet are a cardinality
// incident, and the point of the endpoint is fleet-level health, not a second
// board.
package metrics

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	"github.com/Greg-2600/netweather/internal/state"
	"github.com/Greg-2600/netweather/internal/store"
)

// Source is the aggregate data the endpoint needs.
type Source interface {
	StateCounts(ctx context.Context, vantageID string) (map[state.State]int, error)
	OpenIncidentCount(ctx context.Context) (int, error)
	// FleetStats supplies the DESIGN.md §6.4 view: class counts, scheduled
	// rate, blocked share, and coverage. It carries no per-target data.
	FleetStats(ctx context.Context, vantageID string) (store.FleetStats, error)
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
		fleet, err := src.FleetStats(r.Context(), vantage)
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

		// DESIGN.md §6.4: enough to judge an interval choice without per-target
		// labels. Classes are sorted so two scrapes diff cleanly.
		_, _ = fmt.Fprintln(w, "# HELP netweather_targets_class Enabled targets by class.")
		_, _ = fmt.Fprintln(w, "# TYPE netweather_targets_class gauge")
		classes := make([]string, 0, len(fleet.ByClass))
		for class := range fleet.ByClass {
			classes = append(classes, class)
		}
		sort.Strings(classes)
		for _, class := range classes {
			_, _ = fmt.Fprintf(w, "netweather_targets_class{class=%q} %d\n", class, fleet.ByClass[class])
		}

		_, _ = fmt.Fprintln(w, "# HELP netweather_probe_rate_per_second Scheduled probes per second (sum of 1/interval).")
		_, _ = fmt.Fprintln(w, "# TYPE netweather_probe_rate_per_second gauge")
		_, _ = fmt.Fprintf(w, "netweather_probe_rate_per_second %g\n", fleet.EffectivePerSecond)

		blockedShare := 0.0
		if fleet.Total > 0 {
			blockedShare = float64(fleet.Blocked) / float64(fleet.Total)
		}
		_, _ = fmt.Fprintln(w, "# HELP netweather_blocked_share Share of enabled targets currently BLOCKED.")
		_, _ = fmt.Fprintln(w, "# TYPE netweather_blocked_share gauge")
		_, _ = fmt.Fprintf(w, "netweather_blocked_share %g\n", blockedShare)

		_, _ = fmt.Fprintln(w, "# HELP netweather_unmeasurable_targets Enabled targets currently UNMEASURABLE.")
		_, _ = fmt.Fprintln(w, "# TYPE netweather_unmeasurable_targets gauge")
		_, _ = fmt.Fprintf(w, "netweather_unmeasurable_targets %d\n", fleet.Unmeasurable)

		_, _ = fmt.Fprintln(w, "# HELP netweather_asn_coverage Distinct learned origin ASNs.")
		_, _ = fmt.Fprintln(w, "# TYPE netweather_asn_coverage gauge")
		_, _ = fmt.Fprintf(w, "netweather_asn_coverage %d\n", fleet.ASNCount)

		_, _ = fmt.Fprintln(w, "# HELP netweather_countries Distinct learned countries.")
		_, _ = fmt.Fprintln(w, "# TYPE netweather_countries gauge")
		_, _ = fmt.Fprintf(w, "netweather_countries %d\n", fleet.CountryCount)

		_, _ = fmt.Fprintln(w, "# HELP netweather_top_asn_share Share of enabled targets in the most common ASN.")
		_, _ = fmt.Fprintln(w, "# TYPE netweather_top_asn_share gauge")
		_, _ = fmt.Fprintf(w, "netweather_top_asn_share %g\n", fleet.TopASNShare)
		if fleet.TopASN > 0 {
			_, _ = fmt.Fprintln(w, "# HELP netweather_top_asn The most common origin ASN.")
			_, _ = fmt.Fprintln(w, "# TYPE netweather_top_asn gauge")
			_, _ = fmt.Fprintf(w, "netweather_top_asn{asn=%q,name=%q} %g\n",
				fmt.Sprintf("%d", fleet.TopASN), fleet.TopASNName, fleet.TopASNShare)
		}
	})
}
