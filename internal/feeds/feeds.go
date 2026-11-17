// Package feeds polls external corroboration sources (IODA today) and records
// their events plus how the feed itself is doing.
//
// The split matters: an outage from IODA is evidence, not a verdict, and a
// feed that has silently stopped answering must be visible as stale rather than
// as "no outages".
package feeds

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Greg-2600/netweather/internal/store"
)

// Event is one feed observation, independent of the feed's wire format.
type Event struct {
	ExternalID string
	Kind       string
	Severity   float64
	Country    string
	ASN        int
	StartedAt  time.Time
	EndedAt    time.Time
	ObservedAt time.Time
	Payload    string
}

// Fetcher pulls events. Implementations own their wire format.
type Fetcher interface {
	Fetch(ctx context.Context) ([]Event, error)
}

// Sink records events and feed health. store.Store satisfies it.
type Sink interface {
	UpsertExternalEvents(ctx context.Context, events []store.ExternalEvent) error
	RecordFeedSuccess(ctx context.Context, feed string, at time.Time) error
	RecordFeedFailure(ctx context.Context, feed, detail string, at time.Time) error
	SetFeedNextPoll(ctx context.Context, feed string, at time.Time) error
}

// Poller is one feed's polling cycle.
type Poller struct {
	Feed   string
	Source Fetcher
	Sink   Sink
	Every  time.Duration
	Now    func() time.Time
}

// Poll runs one cycle. A fetch failure is recorded and returned; a sink failure
// is returned without pretending the feed succeeded.
func (p *Poller) Poll(ctx context.Context) (int, error) {
	now := p.now()

	events, err := p.Source.Fetch(ctx)
	if err != nil {
		detail := err.Error()
		if len(detail) > 200 {
			detail = detail[:200]
		}
		if recErr := p.Sink.RecordFeedFailure(ctx, p.Feed, detail, now); recErr != nil {
			return 0, fmt.Errorf("feeds: %s failure not recorded: %w", p.Feed, recErr)
		}
		return 0, err
	}

	batch := make([]store.ExternalEvent, 0, len(events))
	for _, e := range events {
		batch = append(batch, store.ExternalEvent{
			Feed: p.Feed, ExternalID: e.ExternalID, Kind: e.Kind,
			Severity: e.Severity, Country: e.Country, ASN: e.ASN,
			StartedAt: e.StartedAt, EndedAt: e.EndedAt,
			ObservedAt: e.ObservedAt, Payload: e.Payload,
		})
	}

	if err := p.Sink.UpsertExternalEvents(ctx, batch); err != nil {
		return 0, err
	}
	if err := p.Sink.RecordFeedSuccess(ctx, p.Feed, now); err != nil {
		return 0, err
	}
	_ = p.Sink.SetFeedNextPoll(ctx, p.Feed, now.Add(p.every()))
	return len(events), nil
}

func (p *Poller) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

func (p *Poller) every() time.Duration {
	if p.Every <= 0 {
		return 10 * time.Minute
	}
	return p.Every
}

// Loop polls until ctx is done, logging each cycle. A failed poll does not stop
// the loop: an external feed being down is exactly when monitoring matters.
func Loop(ctx context.Context, p *Poller, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}

	ticker := time.NewTicker(p.every())
	defer ticker.Stop()

	// Poll immediately: waiting ten minutes after startup hides a broken feed.
	poll := func() {
		n, err := p.Poll(ctx)
		if err != nil {
			logger.Warn("feed poll failed",
				"event", "feed_failed", "component", "feeds", "feed", p.Feed, "error", err)
			return
		}
		logger.Info("feed polled",
			"event", "feed_ok", "component", "feeds", "feed", p.Feed, "events", n)
	}
	poll()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			poll()
		}
	}
}
