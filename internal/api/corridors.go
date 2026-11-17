package api

import (
	"net/http"
	"time"
)

// corridorItem is one corridor's current state. The API reports the latest
// sample and the open event, not a time series: a corridor's history is a page,
// and a scrollable API is how a monitoring endpoint becomes a database dump.
type corridorItem struct {
	ID   string `json:"id"`
	From string `json:"from"`
	To   string `json:"to"`
	// Latest is absent until the first sample lands.
	Latest *corridorSampleItem `json:"latest,omitempty"`
	// OpenEvent is present only while a step change is open.
	OpenEvent *corridorEventItem `json:"open_event,omitempty"`
}

type corridorSampleItem struct {
	TS    string  `json:"ts"`
	RTTMS float64 `json:"rtt_ms,omitempty"`
	Loss  float64 `json:"loss"`
	OK    bool    `json:"ok"`
}

type corridorEventItem struct {
	Kind      string `json:"kind"`
	StartedAt string `json:"started_at"`
	Detail    string `json:"detail,omitempty"`
}

// handleCorridors lists the configured corridors with their newest observation
// and any open step change (DESIGN.md §9). It is evidence about a path, never a
// target state.
func (s *server) handleCorridors(w http.ResponseWriter, r *http.Request) {
	if s.deps.Corridors == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "corridors are not configured")
		return
	}

	statuses, err := s.deps.Corridors.CorridorStatus(r.Context())
	if err != nil {
		s.deps.Logger.Error("corridor list failed",
			"event", "corridor_list_failed", "component", "http", "error", err)
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "corridors unavailable")
		return
	}

	out := make([]corridorItem, 0, len(statuses))
	for _, status := range statuses {
		item := corridorItem{
			ID: status.Corridor.ID, From: status.Corridor.From, To: status.Corridor.To,
		}
		if status.Latest != nil {
			item.Latest = &corridorSampleItem{
				TS:    status.Latest.TS.UTC().Format(time.RFC3339),
				RTTMS: status.Latest.RTTMS,
				Loss:  status.Latest.Loss,
				OK:    status.Latest.OK,
			}
		}
		if status.Open != nil {
			item.OpenEvent = &corridorEventItem{
				Kind:      status.Open.Kind,
				StartedAt: status.Open.StartedAt.UTC().Format(time.RFC3339),
				Detail:    status.Open.Detail,
			}
		}
		out = append(out, item)
	}

	writeData(w, r, out, &Meta{Count: len(out)})
}
