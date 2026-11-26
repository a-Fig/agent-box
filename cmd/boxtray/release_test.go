package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
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
		run: func(context.Context) error { h.ran <- struct{}{}; return err },
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

func TestCacheDropFailureIsLoggedAndNotRetried(t *testing.T) {
	h := newDropHarness(errors.New("wsl gone"))
	h.d.arm()
	h.d.observe(mode.Status{Chosen: mode.MyPC})
	h.wantRun(t)
	h.wantLine(t, "wsl gone")
	h.d.observe(mode.Status{Chosen: mode.MyPC})
	h.wantNoRun(t)
}
