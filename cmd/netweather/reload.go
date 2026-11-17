package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/Greg-2600/netweather/internal/agent"
	"github.com/Greg-2600/netweather/internal/store"
)

// reloadTargets reconciles the config file into the store and the agent. Targets
// removed from the file are disabled, never deleted; history survives edits.
func reloadTargets(ctx context.Context, st *store.Store, ag *agent.Agent, lg *slog.Logger) error {
	targets, err := loadTargets(ctx, st)
	if err != nil {
		return err
	}

	live := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		live[t.ID] = struct{}{}
	}

	current, err := st.TargetIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range current {
		if _, ok := live[id]; ok {
			continue
		}
		if err := st.DisableTarget(ctx, id); err != nil {
			return err
		}
		lg.Info("target disabled by config reload",
			"event", "target_disabled", "component", "config", "target_id", id)
	}

	ag.SetTargets(targets)
	return nil
}

// configLoop polls the target file and reloads on change. Polling rather than a
// filesystem watcher because config edits are rare, the check is one stat, and
// inotify does not follow into every bind mount.
func configLoop(ctx context.Context, st *store.Store, ag *agent.Agent, lg *slog.Logger) {
	path := os.Getenv("NETWEATHER_CONFIG")
	if path == "" {
		return
	}

	var (
		lastMod  time.Time
		lastSize int64
	)
	// #nosec G703 -- path is the operator-supplied NETWEATHER_CONFIG.
	if info, err := os.Stat(path); err == nil {
		lastMod, lastSize = info.ModTime(), info.Size()
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// #nosec G703 -- path is the operator-supplied NETWEATHER_CONFIG.
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			if info.ModTime().Equal(lastMod) && info.Size() == lastSize {
				continue
			}
			lastMod, lastSize = info.ModTime(), info.Size()

			if err := reloadTargets(ctx, st, ag, lg); err != nil {
				// The old target set stays live: a typo must not stop monitoring.
				lg.Warn("config reload rejected; previous targets stay live",
					"event", "config_reload_failed", "component", "config", "error", err)
				continue
			}
			lg.Info("config reloaded",
				"event", "config_reloaded", "component", "config", "targets", ag.TargetCount())
		}
	}
}
