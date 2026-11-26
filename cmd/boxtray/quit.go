package main

import (
	"context"
	"sync"
	"time"

	"github.com/a-Fig/agent-box/internal/mode"
)

// quitTimeout bounds what the tray does on Quit before it exits.
const quitTimeout = 30 * time.Second

// releaseStoreTimeout bounds Release memory's write of a pick not yet stored.
const releaseStoreTimeout = 30 * time.Second

// releaseMemory is the tray's Release memory. One release runs at a time;
// while it runs, no tick, reclaim or swap call starts a wsl.exe that would
// bring the distro back. It first stores the latest pick not yet in the
// project, so after Quit the headless enforcer applies it, not an older one.
// It ends the release before poke, so the tick that wakes starts the distro
// again, without the boxes' memory (#204).
func releaseMemory(mgr *mode.Manager, terminate func() error, poke func(), logf func(string, ...any)) {
	ctx, cancel := context.WithTimeout(context.Background(), releaseStoreTimeout)
	ok := mgr.BeginRelease(ctx)
	cancel()
	if !ok {
		logf("release memory: already running")
		return
	}
	if err := terminate(); err != nil {
		logf("release memory: wsl --terminate: %v", err)
	}
	mgr.EndRelease()
	poke()
}

// beforeQuit runs when the tray is about to exit. Within limit, it waits for
// a Release memory in flight (releases) to finish, then stores a pick not yet
// in the project, so the headless enforcer that takes over applies the
// owner's last pick (#204). With neither, it returns at once.
func beforeQuit(releases *sync.WaitGroup, mgr *mode.Manager, limit time.Duration, logf func(string, ...any)) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	done := make(chan struct{})
	go func() { releases.Wait(); close(done) }()
	select {
	case <-done:
	default:
		logf("quit: waiting for Release memory to finish")
		select {
		case <-done:
			logf("quit: Release memory finished")
		case <-ctx.Done():
			logf("quit: gave up waiting for Release memory after %s; the pick may not be stored", limit)
			return
		}
	}
	stored, err := mgr.StorePending(ctx)
	switch {
	case err != nil:
		logf("quit: gave up storing the chosen mode: %v", err)
	case stored:
		logf("quit: stored the chosen mode %s", mgr.Chosen())
	}
}
