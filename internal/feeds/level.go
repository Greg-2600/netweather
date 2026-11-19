package feeds

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Greg-2600/netweather/internal/store"
)

// LevelFetcher pulls a bounded set of per-country levels. Unlike Fetcher, it
// runs on a schedule and stores a small snapshot: a page view never calls the
// upstream once per country (DESIGN.md §10.3).
type LevelFetcher interface {
	FetchLevels(ctx context.Context) ([]store.CountryLevel, error)
}

// LevelSink records a level snapshot and the feed health around it.
// store.Store satisfies it, as it does Sink.
type LevelSink interface {
	UpsertCountryLevels(ctx context.Context, levels []store.CountryLevel) error
	RecordFeedSuccess(ctx context.Context, feed string, at time.Time) error
	RecordFeedFailure(ctx context.Context, feed, detail string, at time.Time) error
	SetFeedNextPoll(ctx context.Context, feed string, at time.Time) error
}

// LevelPoller is one level feed's polling cycle.
type LevelPoller struct {
	Feed   string
	Source LevelFetcher
	Sink   LevelSink
	Every  time.Duration
	Now    func() time.Time
}

// Poll runs one cycle. A failed fetch is recorded as feed failure; a failed
// write is returned without claiming the feed succeeded.
func (p *LevelPoller) Poll(ctx context.Context) (int, error) {
	now := p.now()

	levels, err := p.Source.FetchLevels(ctx)
	if err != nil {
		detail := err.Error()
		if len(detail) > 200 {
			detail = detail[:200]
		}
		if recErr := p.Sink.RecordFeedFailure(ctx, p.Feed, detail, now); recErr != nil {
			return 0, fmt.Errorf("feeds: %s level failure not recorded: %w", p.Feed, recErr)
		}
		return 0, err
	}

	if err := p.Sink.UpsertCountryLevels(ctx, levels); err != nil {
		return 0, err
	}
	if err := p.Sink.RecordFeedSuccess(ctx, p.Feed, now); err != nil {
		return 0, err
	}
	_ = p.Sink.SetFeedNextPoll(ctx, p.Feed, now.Add(p.every()))
	return len(levels), nil
}

func (p *LevelPoller) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

func (p *LevelPoller) every() time.Duration {
	if p.Every <= 0 {
		return 10 * time.Minute
	}
	return p.Every
}

// LevelLoop polls until ctx is done. A failed poll does not stop the loop: an
// upstream being down is exactly when a current level matters.
func LevelLoop(ctx context.Context, p *LevelPoller, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}

	ticker := time.NewTicker(p.every())
	defer ticker.Stop()

	poll := func() {
		n, err := p.Poll(ctx)
		if err != nil {
			logger.Warn("feed level poll failed",
				"event", "feed_failed", "component", "feeds", "feed", p.Feed, "error", err)
			return
		}
		logger.Info("feed level polled",
			"event", "feed_ok", "component", "feeds", "feed", p.Feed, "levels", n)
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
