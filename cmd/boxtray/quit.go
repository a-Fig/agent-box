package main

import (
	"context"
	"sync"
	"time"

	"github.com/a-Fig/agent-box/internal/mode"
)

// quitTimeout bounds each of Quit's waits: finishing a release and storing the pick.
const quitTimeout = 30 * time.Second

// releaseStoreTimeout bounds Release memory's write of a pick not yet stored.
const releaseStoreTimeout = 30 * time.Second

// releaseMemory is the tray's Release memory. One release runs at a time;
// while it runs, no tick, reclaim or swap call starts a wsl.exe that would
// bring the distro back. It first stores the latest pick not yet in the
// project, so after Quit the headless enforcer applies it, not an older one.
// It ends the release before poke, so the tick that wakes starts the distro
// again, without the boxes' memory (#204).
func releaseMemory(mgr *mode.Manager, drop *cacheDrop, terminate func() error, poke func(), logf func(string, ...any)) {
	ctx, cancel := context.WithTimeout(context.Background(), releaseStoreTimeout)
	ok := mgr.BeginRelease(ctx)
	cancel()
	if !ok {
		logf("release memory: already running")
		return
	}
	if drop != nil {
		drop.stop()
	}
	if err := terminate(); err != nil {
		logf("release memory: wsl --terminate: %v", err)
	} else if drop != nil {
		drop.arm()
	}
	mgr.EndRelease()
	poke()
}

// beforeQuit waits for Release memory to finish, then gives the pending pick
// its own deadline so Incus has time to start and retry the write (#204).
// The tray menu and mode loop must have stopped taking actions. With no
// release or pending pick, it returns at once.
func beforeQuit(releases *sync.WaitGroup, mgr *mode.Manager, limit time.Duration, logf func(string, ...any)) {
	done := make(chan struct{})
	go func() { releases.Wait(); close(done) }()
	select {
	case <-done:
	default:
		logf("quit: waiting for Release memory to finish")
		timer := time.NewTimer(limit)
		defer timer.Stop()
		select {
		case <-done:
			logf("quit: Release memory finished")
		case <-timer.C:
			logf("quit: gave up waiting for Release memory after %s; the pick may not be stored", limit)
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	stored, err := mgr.StorePending(ctx)
	switch {
	case err != nil:
		logf("quit: gave up storing the chosen mode: %v", err)
	case stored:
		logf("quit: stored the chosen mode %s", mgr.Chosen())
	}
}

// finishUpdate starts the replacement after the slow work. Even older
// binaries (v0.1.7) give the outgoing tray only 20 s to free its mutex.
func finishUpdate(releases *sync.WaitGroup, mgr *mode.Manager, limit time.Duration, logf func(string, ...any), start func() bool) bool {
	beforeQuit(releases, mgr, limit, logf)
	return start()
}
