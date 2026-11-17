package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/Greg-2600/netweather/internal/agent"
	"github.com/Greg-2600/netweather/internal/config"
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

// configStamp is the change signal for a config path. A file's mtime and size
// are enough. A directory needs a walk: editing sites/financial/banking.txt does
// not change the directory's own mtime, so a stat on the root would miss the
// edit that matters.
func configStamp(path string) (string, error) {
	info, err := os.Stat(path) // #nosec G703 -- operator-supplied NETWEATHER_CONFIG.
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return fmt.Sprintf("%d|%d", info.ModTime().UnixNano(), info.Size()), nil
	}

	var (
		newest int64
		total  int64
		count  int
	)
	err = filepath.WalkDir(path, func(p string, entry os.DirEntry, err error) error { // #nosec G703 -- operator-supplied NETWEATHER_CONFIG.
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		fi, err := entry.Info()
		if err != nil {
			return err
		}
		if mt := fi.ModTime().UnixNano(); mt > newest {
			newest = mt
		}
		total += fi.Size()
		count++
		return nil
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d|%d|%d", newest, total, count), nil
}

// configLoop polls the target file or directory and reloads on change. Polling
// rather than a filesystem watcher because config edits are rare, the check is
// one walk, and inotify does not follow into every bind mount.
//
// reload records the outcome so a rejected edit is shown on the board, not only
// written to the log (THREAT_MODEL.md T36). The previous target set stays live
// either way.
func configLoop(ctx context.Context, st *store.Store, ag *agent.Agent, lg *slog.Logger, reload *config.ReloadState) {
	path := os.Getenv("NETWEATHER_CONFIG")
	if path == "" {
		return
	}

	last, _ := configStamp(path)

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stamp, err := configStamp(path)
			if err != nil {
				continue
			}
			if stamp == last {
				continue
			}
			last = stamp

			if err := reloadTargets(ctx, st, ag, lg); err != nil {
				// The old target set stays live: a typo must not stop monitoring.
				lg.Warn("config reload rejected; previous targets stay live",
					"event", "config_reload_failed", "component", "config", "error", err)
				reload.Failed(err, time.Now())
				continue
			}
			reload.Succeeded()
			lg.Info("config reloaded",
				"event", "config_reloaded", "component", "config", "targets", ag.TargetCount())
		}
	}
}
