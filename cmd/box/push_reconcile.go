package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const pushReconcileInterval = 30 * time.Second

// t3BaseDir is the profile selected by the local T3 CLI. A relative setting
// cannot identify the same profile from a different working directory.
func (a *App) t3BaseDir() string {
	dir := a.getenv("T3CODE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".t3")
	}
	if !filepath.IsAbs(dir) {
		return ""
	}
	return filepath.Clean(dir)
}

func (rec jobRecord) inProfile(baseDir string) bool {
	return rec.T3BaseDir == "" || rec.T3BaseDir == baseDir
}

// reconcilePushes runs alongside MCP. Retrying after startup lets an expired
// watcher or send lease be recovered without another tool call.
func (a *App) reconcilePushes(ctx context.Context, interval time.Duration) {
	baseDir := a.t3BaseDir()
	if baseDir == "" || a.StateDir == "" || a.Push == nil {
		return
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		a.reconcileProfile(ctx, baseDir)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (a *App) reconcileProfile(ctx context.Context, baseDir string) {
	entries, err := os.ReadDir(a.StateDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		job, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || entry.IsDir() {
			continue
		}
		if _, err := parseJobID(job); err != nil {
			continue
		}
		rec, err := a.readWatchRecord(job)
		if err != nil || rec.Thread == "" || rec.T3BaseDir != baseDir || a.exists(job, ".notified") ||
			a.leaseHeld(job, ".watch") || a.leaseHeld(job, ".sending") {
			continue
		}
		// Watch claims the lease again so a detached watcher or a second MCP
		// process can race this scan safely. Failures remain owed for the next tick.
		go a.Watch(ctx, job)
	}
}
