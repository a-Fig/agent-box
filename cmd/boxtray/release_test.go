package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/a-Fig/agent-box/internal/mode"
)

type dropHarness struct {
	d     *cacheDrop
	ran   chan struct{}
	lines chan string
}

func newDropHarness(err error) *dropHarness {
	h := &dropHarness{ran: make(chan struct{}, 4), lines: make(chan string, 8)}
	h.d = &cacheDrop{
		hostUp: func(context.Context) bool { return true },
		run:    func(context.Context) error { h.ran <- struct{}{}; return err },
		logf: func(f string, a ...any) {
			h.lines <- fmt.Sprintf(f, a...)
		},
	}
	return h
}

func (h *dropHarness) wantRun(t *testing.T) {
	t.Helper()
	select {
	case <-h.ran:
	case <-time.After(5 * time.Second):
		t.Fatal("no drop ran")
	}
}

func (h *dropHarness) wantNoRun(t *testing.T) {
	t.Helper()
	select {
	case <-h.ran:
		t.Fatal("drop ran when it should not")
	case <-time.After(100 * time.Millisecond):
	}
}

func (h *dropHarness) wantLine(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-h.lines:
		if !strings.Contains(got, want) {
			t.Fatalf("log line %q lacks %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no log line containing %q", want)
	}
}

func TestCacheDropRunsOnceWhenDistroReturnsAfterRelease(t *testing.T) {
	h := newDropHarness(nil)
	down := mode.Status{Chosen: mode.MyPC, Down: true}
	up := mode.Status{Chosen: mode.MyPC}

	// Not armed: a distro that comes up on its own drops nothing.
	h.d.observe(up)
	h.wantNoRun(t)
	h.d.arm()
	// Still down: nothing yet, and still armed.
	h.d.observe(down)
	h.wantNoRun(t)
	h.d.observe(up)
	h.wantRun(t)
	h.wantLine(t, "dropped")
	// Once only: later ticks after the same release do not repeat it,
	// not even if the distro goes down and returns again.
	h.d.observe(up)
	h.d.observe(down)
	h.d.observe(up)
	h.wantNoRun(t)
}

func TestCacheDropScriptDropsThenCompacts(t *testing.T) {
	if !strings.Contains(dropCachesScript, "drop_caches && echo 1 > /proc/sys/vm/compact_memory") ||
		!strings.HasPrefix(dropCachesScript, "sync &&") {
		t.Errorf("script = %q, want sync && drop_caches && compact_memory", dropCachesScript)
	}
}

func TestCacheDropWaitsOutAnOverlappingRelease(t *testing.T) {
	h := newDropHarness(nil)
	releasing := true
	h.d.busy = func() bool { return releasing }
	h.d.arm()
	// A release is running: observe spends nothing, and the arm survives.
	h.d.observe(mode.Status{Chosen: mode.MyPC})
	h.wantNoRun(t)
	releasing = false
	h.d.observe(mode.Status{Chosen: mode.MyPC})
	h.wantRun(t)
	h.wantLine(t, "dropped")
}

func TestCacheDropStopsForTheNextRelease(t *testing.T) {
	started := make(chan struct{}, 1)
	mgr := mode.New(&noIncus{}, mode.MyPC)
	d := &cacheDrop{
		hostUp: func(context.Context) bool { return true },
		busy:   mgr.Releasing,
		run: func(ctx context.Context) error {
			started <- struct{}{}
			<-ctx.Done() // the wsl.exe call, killed by its context
			return ctx.Err()
		},
		logf: func(string, ...any) {},
	}
	d.arm()
	d.observe(mode.Status{Chosen: mode.MyPC})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("drop never started")
	}
	// The next Release memory cancels the in-flight drop and waits for
	// it, so the drop cannot restart the distro during its terminate.
	done := make(chan struct{})
	go func() {
		releaseMemory(mgr, d, func() error {
			d.mu.Lock()
			defer d.mu.Unlock()
			if d.done != nil {
				t.Error("terminate ran before the previous drop exited")
			}
			return nil
		}, func() {}, t.Logf)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not end the in-flight drop")
	}
}

func TestReleaseMemoryDropsCacheAfterAutomaticRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &fakeSensors{up: true}
		mgr := mode.New(&noIncus{}, mode.MyPC)
		h := newDropHarness(nil)
		h.d.busy, h.d.hostUp = mgr.Releasing, s.HostUp
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wake := make(chan struct{}, 1)
		poke := func() { wake <- struct{}{} }
		go run(ctx, loopCfg{Manager: func() *mode.Manager { return mgr }}, s, time.Hour, wake, h.d.observe)
		synctest.Wait()
		h.wantNoRun(t)

		releaseMemory(mgr, h.d, func() error { return errors.New("terminate failed") }, poke, t.Logf)
		synctest.Wait()
		h.wantNoRun(t)
		if len(h.lines) != 0 {
			t.Fatal("failed terminate triggered a cache drop")
		}
		releaseMemory(mgr, h.d, func() error {
			s.mu.Lock()
			s.up = false
			s.mu.Unlock()
			return nil
		}, func() {
			if mgr.Releasing() {
				t.Error("release still active at wake")
			}
			// A delayed pre-release report must not start the distro itself.
			h.d.observe(mode.Status{Chosen: mode.MyPC})
			synctest.Wait()
			h.wantNoRun(t)
			poke()
		}, t.Logf)
		synctest.Wait()
		if n := s.started(); n != 1 {
			t.Fatalf("distro start attempts = %d, want 1", n)
		}
		h.wantNoRun(t)
		s.mu.Lock()
		s.up = true
		s.mu.Unlock()
		poke()
		h.wantRun(t)
		h.wantLine(t, "dropped")
		poke()
		synctest.Wait()
		h.wantNoRun(t)
		if len(h.lines) != 0 {
			t.Fatal("cache drop logged more than once")
		}
	})
}

func TestCacheDropFailureIsLoggedAndNotRetried(t *testing.T) {
	h := newDropHarness(errors.New("wsl gone"))
	h.d.arm()
	h.d.observe(mode.Status{Chosen: mode.MyPC})
	h.wantRun(t)
	h.wantLine(t, "wsl gone")
	h.d.observe(mode.Status{Chosen: mode.MyPC})
	h.wantNoRun(t)
}
