// Package incidents turns correlated failures into incidents. Detection is a
// single deterministic evaluation: observations in, at most one incident out.
package incidents

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/Greg-2600/netweather/internal/correlate"
	"github.com/Greg-2600/netweather/internal/store"
)

// Source supplies the current observations.
type Source interface {
	Observations(ctx context.Context, vantageID string) ([]store.Observation, error)
}

// Sink records incidents and answers whether one is already open.
type Sink interface {
	HasOpenIncident(ctx context.Context, scope, value string) (bool, error)
	OpenIncident(ctx context.Context, inc store.Incident, targetIDs []string, attrs []store.Attribution) error
}

// Evaluate runs one detection cycle. It returns the opened incident, or false
// when nothing cleared the gates or the cluster was already open.
func Evaluate(ctx context.Context, src Source, sink Sink, vantage string, now time.Time) (store.Incident, bool, error) {
	observations, err := src.Observations(ctx, vantage)
	if err != nil {
		return store.Incident{}, false, err
	}

	correlated := make([]correlate.Observation, 0, len(observations))
	for _, o := range observations {
		correlated = append(correlated, correlate.Observation{
			TargetID: o.TargetID,
			CDN:      o.CDN,
			ASN:      o.ASN,
			Prefix24: o.Prefix24,
			Country:  o.Country,
			Sector:   o.Sector,
			Stage:    o.Stage,
			Failing:  o.Failing,
		})
	}

	ranked := correlate.Rank(correlated)
	var winner correlate.Candidate
	found := false
	for _, c := range ranked {
		if c.Passes() {
			winner = c
			found = true
			break
		}
	}
	if !found {
		return store.Incident{}, false, nil
	}

	alreadyOpen, err := sink.HasOpenIncident(ctx, string(winner.Dimension), winner.Value)
	if err != nil {
		return store.Incident{}, false, err
	}
	if alreadyOpen {
		return store.Incident{}, false, nil
	}

	targetIDs := make([]string, 0, winner.Failing)
	for _, o := range observations {
		if o.Failing {
			targetIDs = append(targetIDs, o.TargetID)
		}
	}

	attrs := make([]store.Attribution, 0, len(ranked))
	for i, c := range ranked {
		attrs = append(attrs, store.Attribution{
			Rank:    i + 1,
			Kind:    string(c.Dimension),
			Value:   c.Value,
			Failing: c.Failing,
			Total:   c.Total,
			Share:   c.Share,
			Score:   c.Score,
		})
	}

	inc := store.Incident{
		ID:         NewID(),
		OpenedAt:   now.UTC(),
		Verdict:    "INCIDENT",
		Scope:      string(winner.Dimension),
		ScopeValue: winner.Value,
		Headline: fmt.Sprintf("%d of %d targets failing on %s=%s",
			winner.Failing, winner.Total, winner.Dimension, winner.Value),
		Confidence: confidence(winner),
	}

	if err := sink.OpenIncident(ctx, inc, targetIDs, attrs); err != nil {
		return store.Incident{}, false, err
	}
	return inc, true, nil
}

func confidence(c correlate.Candidate) string {
	if c.Failing >= 5 && c.Share >= 0.80 {
		return "high"
	}
	return "medium"
}

// NewID returns a short, sortable-enough identifier. It is hex rather than a
// ULID; the only requirement is uniqueness and readability in a URL.
func NewID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("inc-%d", time.Now().UnixNano())
	}
	return "inc-" + hex.EncodeToString(b[:])
}

// Loop evaluates every interval until ctx is done. A single failed evaluation is
// logged and retried on the next tick; it never stops the loop.
func Loop(ctx context.Context, src Source, sink Sink, vantage string, every time.Duration, logger *slog.Logger) {
	if every <= 0 {
		every = 5 * time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}

	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			inc, opened, err := Evaluate(ctx, src, sink, vantage, time.Now())
			if err != nil {
				logger.Warn("incident evaluation failed",
					"event", "incident_failed", "component", "incidents", "error", err)
				continue
			}
			if opened {
				logger.Info("incident opened",
					"event", "incident_opened",
					"component", "incidents",
					"incident_id", inc.ID,
					"scope", inc.Scope,
					"value", inc.ScopeValue,
					"headline", inc.Headline,
				)
			}
		}
	}
}
