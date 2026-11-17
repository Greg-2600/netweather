package api

import (
	"net/http"
	"time"
)

// ihiResponse is the Internet Health Index as a client sees it. Parts carry
// their effective weight and the time of the evidence, so a reader can tell a
// quiet internet from a summary built on old data.
type ihiResponse struct {
	Score   float64      `json:"score"`
	Parts   []ihiPart    `json:"parts"`
	Dropped []ihiDropped `json:"dropped"`
}

type ihiPart struct {
	Name   string  `json:"name"`
	Score  float64 `json:"score"`
	Weight float64 `json:"weight"`
	At     string  `json:"at"`
}

type ihiDropped struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// handleIHI serves DESIGN.md §11. It is a summary, not a detector: it changes
// nothing, and a client that wants the components always gets them, including
// the ones that were left out and why.
func (s *server) handleIHI(w http.ResponseWriter, r *http.Request) {
	if s.deps.IHI == nil {
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "the health index is not configured")
		return
	}

	index := s.deps.IHI.Build(r.Context())
	resp := ihiResponse{
		Score:   index.Score,
		Parts:   make([]ihiPart, 0, len(index.Parts)),
		Dropped: make([]ihiDropped, 0, len(index.Dropped)),
	}
	for _, part := range index.Parts {
		resp.Parts = append(resp.Parts, ihiPart{
			Name:   part.Name,
			Score:  part.Score,
			Weight: part.Weight,
			At:     part.At.UTC().Format(time.RFC3339),
		})
	}
	for _, dropped := range index.Dropped {
		resp.Dropped = append(resp.Dropped, ihiDropped{Name: dropped.Name, Reason: dropped.Reason})
	}

	writeData(w, r, resp, nil)
}
