package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
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

func TestHeartbeatThrottlesAndNamesTheEnforcer(t *testing.T) {
	inc := &recIncus{}
	hb := newHeartbeat(inc, mode.EnforcerHeadless, t.Logf)
	t0 := time.Date(2026, 11, 25, 3, 0, 0, 0, time.UTC)
	hb.beat(context.Background(), t0)
	hb.beat(context.Background(), t0.Add(29*time.Second))
	hb.beat(context.Background(), t0.Add(30*time.Second))
	want := []string{
		"user.agent-box.tray-version=" + hb.build,
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
	s := &fakeSensors{up: true}
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
		run(ctx, cfg, s, time.Hour, wake, func(st mode.Status) { reports <- st })
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
	if st.Chosen != mode.Full {
		t.Fatalf("headless status = %+v", st)
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
	if managers != 2 || inc.count("user.agent-box.mode=full") != 2 || inc.count("user.agent-box.enforcer=headless") != 2 || inc.count("user.agent-box.tray-version=") != 2 {
		t.Fatalf("resume: managers=%d sets=%v", managers, inc.snapshot())
	}
	cancel()
	<-done
}

// My PC, then Release memory, then Quit at once: Quit waits for the release
// goroutine (which has not even begun) and the project ends with my-pc (#204).
func TestQuitRightAfterReleaseStoresThePick(t *testing.T) {
	inc := &recIncus{}
	mgr := mode.New(inc, mode.Full)
	drop := newDropHarness(nil)
	drop.d.busy = mgr.Releasing
	mgr.Handshake = true
	mgr.SetChosen(mode.MyPC)
	var releases sync.WaitGroup
	start := make(chan struct{})
	terminated := false
	releases.Add(1)
	go func() {
		defer releases.Done()
		<-start // the goroutine is slow to begin
		releaseMemory(mgr, drop.d, func() error { terminated = true; return nil }, func() {
			drop.d.observe(mode.Status{Chosen: mode.MyPC})
		}, t.Logf)
	}()
	go func() { time.Sleep(50 * time.Millisecond); close(start) }()
	drop.d.setPaused(true)
	beforeQuit(&releases, mgr, 5*time.Second, t.Logf)
	drop.wantNoRun(t)
	if !terminated {
		t.Fatal("Quit returned before the release terminated the distro")
	}
	if inc.count(mode.ProjectChosenKey+`={"mode":"`+string(mode.MyPC)+`"`) != 1 {
		t.Fatalf("writes %q, want my-pc stored once", inc.snapshot())
	}
}

// A pick made while the release runs, after its write, is stored by Quit.
func TestQuitStoresAPickMadeDuringTheRelease(t *testing.T) {
	inc := &recIncus{}
	mgr := mode.New(inc, mode.Full)
	mgr.Handshake = true
	mgr.SetChosen(mode.MyPC)
	var releases sync.WaitGroup
	releases.Add(1)
	go func() {
		defer releases.Done()
		releaseMemory(mgr, nil, func() error { mgr.SetChosen(mode.Shared); return nil }, func() {}, t.Logf)
	}()
	beforeQuit(&releases, mgr, 5*time.Second, t.Logf)
	got := inc.snapshot()
	if len(got) == 0 || !strings.HasPrefix(got[len(got)-1], mode.ProjectChosenKey+`={"mode":"shared"`) {
		t.Fatalf("writes %q, want shared last", got)
	}
}

func TestQuitWithNothingPendingExitsAtOnce(t *testing.T) {
	inc := &noIncus{}
	mgr := mode.New(inc, mode.Full)
	mgr.Handshake = true
	var releases sync.WaitGroup
	start := time.Now()
	beforeQuit(&releases, mgr, 5*time.Second, t.Logf)
	if d := time.Since(start); d > time.Second || inc.calls != 0 {
		t.Fatalf("took %s with %d Incus calls, want at once with none", d, inc.calls)
	}
}

// failIncus fails its first n project writes, as Incus does while the distro boots.
type failIncus struct {
	recIncus
	n int
}

func (f *failIncus) SetProjectConfig(ctx context.Context, k, v string) error {
	f.mu.Lock()
	if f.n > 0 {
		f.n--
		f.mu.Unlock()
		return errors.New("incus is starting")
	}
	f.mu.Unlock()
	return f.recIncus.SetProjectConfig(ctx, k, v)
}

// Quit right after a release, while Incus is still coming up: the pick is
// retried until it is stored, within Quit's limit.
func TestQuitRetriesThePickWhileIncusStarts(t *testing.T) {
	inc := &failIncus{n: 2}
	mgr := mode.New(inc, mode.Full)
	mgr.Handshake = true
	mgr.SetChosen(mode.MyPC)
	var releases sync.WaitGroup
	beforeQuit(&releases, mgr, 10*time.Second, t.Logf)
	if inc.count(mode.ProjectChosenKey+`={"mode":"`+string(mode.MyPC)+`"`) != 1 {
		t.Fatalf("writes %q, want my-pc stored once", inc.snapshot())
	}
}

func TestQuitRetryGetsItsOwnDeadlineAfterSlowRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inc := &failIncus{n: 2} // fail before termination and on the first attempt after it
		mgr := mode.New(inc, mode.Full)
		mgr.Handshake = true
		mgr.SetChosen(mode.MyPC)
		var releases sync.WaitGroup
		releases.Add(1)
		go func() {
			defer releases.Done()
			releaseMemory(mgr, nil, func() error {
				time.Sleep(quitTimeout - 500*time.Millisecond)
				return nil
			}, func() {}, t.Logf)
		}()
		beforeQuit(&releases, mgr, quitTimeout, t.Logf)
		if inc.count(mode.ProjectChosenKey+`={"mode":"mypc"`) != 1 {
			t.Fatalf("writes %q, want My PC stored on retry", inc.snapshot())
		}
	})
}

func TestUpdateStartsReplacementAfterQuit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inc := &recIncus{}
		mgr := mode.New(inc, mode.Full)
		mgr.Handshake = true
		var releases sync.WaitGroup
		completed := make(chan struct{})
		releases.Add(1)
		go func() {
			defer releases.Done()
			releaseMemory(mgr, nil, func() error {
				time.Sleep(25 * time.Second) // longer than v0.1.7's mutex wait
				mgr.SetChosen(mode.MyPC)
				return nil
			}, func() {}, t.Logf)
			close(completed)
		}()
		launched := finishUpdate(&releases, mgr, quitTimeout, t.Logf, func() bool {
			select {
			case <-completed:
			default:
				t.Error("started the replacement while Release memory was running")
			}
			if inc.count(mode.ProjectChosenKey+`={"mode":"mypc"`) != 1 {
				t.Errorf("started the replacement before My PC was stored: %q", inc.snapshot())
			}
			return true
		})
		if !launched {
			t.Fatal("replacement was not launched")
		}
	})
}
