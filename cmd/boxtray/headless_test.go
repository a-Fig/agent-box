package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/mode"
)

// recIncus records the project keys written to it.
type recIncus struct {
	noIncus
	mu   sync.Mutex
	sets []string // "key=value", in order
}

func (r *recIncus) SetProjectConfig(_ context.Context, k, v string) error {
	r.mu.Lock()
	r.sets = append(r.sets, k+"="+v)
	r.mu.Unlock()
	return nil
}

func (r *recIncus) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sets...)
}

func (r *recIncus) count(prefix string) int {
	n := 0
	for _, s := range r.snapshot() {
		if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

func TestHeadlessRequested(t *testing.T) {
	for args, want := range map[string]bool{"": false, "--headless": true, "-headless": true, "--HEADLESS": true, "--other": false} {
		var a []string
		if args != "" {
			a = []string{args}
		}
		if got := headlessRequested(a); got != want {
			t.Errorf("%q: got %v want %v", args, got, want)
		}
	}
}

func TestNoGamesNeverYields(t *testing.T) {
	s := noGames{&fakeSensors{up: true, gaming: true}}
	if g, _ := s.Gaming(); g {
		t.Fatal("headless saw a game")
	}
	if !s.HostUp(context.Background()) {
		t.Fatal("HostUp should pass through")
	}
}

func TestHeartbeatThrottlesAndNamesTheEnforcer(t *testing.T) {
	inc := &recIncus{}
	hb := newHeartbeat(inc, mode.EnforcerHeadless, t.Logf)
	t0 := time.Date(2026, 11, 25, 3, 0, 0, 0, time.UTC)
	hb.beat(context.Background(), t0)
	hb.beat(context.Background(), t0.Add(29*time.Second))
	hb.beat(context.Background(), t0.Add(30*time.Second))
	want := []string{
		"user.agent-box.enforcer=headless", "user.agent-box.tray-seen=2026-11-25T03:00:00Z",
		"user.agent-box.enforcer=headless", "user.agent-box.tray-seen=2026-11-25T03:00:30Z",
	}
	got := inc.snapshot()
	if len(got) != len(want) {
		t.Fatalf("writes = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("writes = %v", got)
		}
	}
}

// The headless loop enforces Full until the interactive tray appears, writes
// nothing while it runs, and starts clean when the tray has gone again.
//
// The test scripts what the "interactive tray present" check answers, one
// answer per tick, so no tick runs on a stale answer. A wake is accepted only
// once the loop is back at its select, which makes it a barrier for the tick
// before it.
func TestHeadlessStandsAsideForTheTrayAndResumes(t *testing.T) {
	inc := &recIncus{}
	s := &fakeSensors{up: true, gaming: true} // headless wraps it in noGames
	managers := 0
	hb := newHeartbeat(inc, mode.EnforcerHeadless, t.Logf)
	trayPresent := make(chan bool)
	cfg := loopCfg{
		Manager: func() *mode.Manager {
			managers++
			hb.last = time.Time{}
			return mode.New(inc, mode.Full)
		},
		Idle: func() bool { return <-trayPresent },
		Beat: hb.beat,
		Logf: t.Logf,
	}

	ctx, cancel := context.WithCancel(context.Background())
	wake := make(chan struct{})
	reports := make(chan mode.Status, 32)
	done := make(chan struct{})
	go func() {
		run(ctx, cfg, noGames{s}, time.Hour, wake, func(st mode.Status) { reports <- st })
		close(done)
	}()
	idle := func() { // one tick that sees the tray
		wake <- struct{}{}
		trayPresent <- true
	}

	trayPresent <- true // the loop's first tick: a tray is already running
	idle()
	wake <- struct{}{} // barrier; the tick it starts waits for its answer
	if len(inc.snapshot()) != 0 || inc.calls != 0 || managers != 0 || len(reports) != 0 {
		t.Fatalf("idle wrote: sets=%v calls=%d managers=%d", inc.snapshot(), inc.calls, managers)
	}

	trayPresent <- false // the tray is gone
	st := <-reports
	if managers != 1 || inc.count("user.agent-box.mode=full") != 1 || inc.count("user.agent-box.enforcer=headless") != 1 || inc.count("user.agent-box.tray-seen=") != 1 {
		t.Fatalf("enforcing: managers=%d sets=%v", managers, inc.snapshot())
	}
	if st.Effective != mode.Full || st.Yielding {
		t.Fatalf("headless status = %+v (a game must not make it yield)", st)
	}

	before := len(inc.snapshot()) // the tray takes over
	callsBefore := inc.calls
	idle()
	wake <- struct{}{} // barrier
	if len(inc.snapshot()) != before || inc.calls != callsBefore {
		t.Fatalf("wrote while the tray ran: %v", inc.snapshot()[before:])
	}
	s.mu.Lock()
	lastAwake := s.awake[len(s.awake)-1]
	s.mu.Unlock()
	if lastAwake {
		t.Fatal("still holding the PC awake while the tray is the enforcer")
	}

	trayPresent <- false // the tray is gone again; a fresh manager rewrites everything
	<-reports
	if managers != 2 || inc.count("user.agent-box.mode=full") != 2 || inc.count("user.agent-box.enforcer=headless") != 2 {
		t.Fatalf("resume: managers=%d sets=%v", managers, inc.snapshot())
	}
	cancel()
	<-done
}
