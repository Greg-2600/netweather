package api

import (
	"context"
	"encoding/json"
	"time"
)

// trackEvents is the single writer of the replay ring. It diffs the board on
// every wake-up and records incident changes, so a client that was disconnected
// still finds the missed events when it reconnects with Last-Event-ID.
//
// It subscribes before taking its first snapshot. A change that lands in the
// gap would otherwise be recorded twice (snapshot plus delta) at worst; the
// other order loses it entirely, and a duplicate state is harmless because
// clients upsert by target.
func (s *server) trackEvents(ctx context.Context) {
	boardCh, cancelBoard := s.deps.Watcher.Subscribe()
	defer cancelBoard()

	var incidentCh <-chan struct{}
	if s.deps.IncidentWatcher != nil {
		incCh, cancelIncidents := s.deps.IncidentWatcher.Subscribe()
		defer cancelIncidents()
		incidentCh = incCh
	}

	last := sseStates{}
	if snap, err := s.streamSnapshot(ctx); err == nil {
		last = snap
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-incidentCh:
			s.logIncidents(ctx)
		case <-boardCh:
			next, err := s.streamSnapshot(ctx)
			if err != nil {
				// A read failure is a gap in our own history. Leave `last`
				// untouched so the next successful read still produces the
				// deltas instead of swallowing them.
				continue
			}
			s.logBoardDelta(last, next)
			last = next
		}
	}
}

// logBoardDelta records what changed. A change in the set of targets is a full
// snapshot, because a delta stream cannot express a removal; otherwise each
// changed target is one state event.
func (s *server) logBoardDelta(prev, next sseStates) {
	if !sameTargetSet(prev, next) {
		s.appendJSON("snapshot", sseSnapshot{Targets: next.list()})
		return
	}
	for _, id := range next.ids() {
		if prev[id].State == next[id].State {
			continue
		}
		s.appendJSON("state", next[id])
	}
}

// logIncidents records the open incidents after a change. Closures are not
// pushed because the stream carries current state, not a diff; a client that
// needs closures refetches.
func (s *server) logIncidents(ctx context.Context) {
	if s.deps.Incidents == nil {
		return
	}
	open, err := s.deps.Incidents.Incidents(ctx, true, 50)
	if err != nil {
		s.deps.Logger.Warn("incident stream read failed",
			"event", "incident_stream_failed", "component", "http", "error", err)
		return
	}
	for _, inc := range open {
		s.appendJSON("incident", incidentEvent{
			ID: inc.ID, Verdict: inc.Verdict, Scope: inc.Scope,
			ScopeValue: inc.ScopeValue, Headline: inc.Headline,
			Confidence: inc.Confidence,
			OpenedAt:   inc.OpenedAt.UTC().Format(time.RFC3339),
		})
	}
}

func (s *server) appendJSON(event string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.events.append(event, data)
}

func sameTargetSet(a, b sseStates) bool {
	if len(a) != len(b) {
		return false
	}
	for id := range a {
		if _, ok := b[id]; !ok {
			return false
		}
	}
	return true
}
