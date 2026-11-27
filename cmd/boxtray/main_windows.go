//go:build windows

// Command boxtray is the Windows tray app: it picks how much of the PC agents
// get (My PC / Shared / Full) and keeps the PC awake while agent instances
// run. With --headless it is instead the
// enforcer for when nobody is logged on (headless_windows.go). Build it with
// -ldflags "-H=windowsgui".
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"github.com/a-Fig/agent-box/internal/mode"
	"github.com/a-Fig/agent-box/internal/version"
	"golang.org/x/sys/windows"
)

// lg is the log at %LOCALAPPDATA%\agent-box\boxtray.log. A nil *fileLog is a
// no-op, so it is safe to use before main sets it.
var lg *fileLog

func main() {
	runtime.LockOSThread() // the tray window must be created and pumped on one thread

	if len(os.Args) > 1 && os.Args[1] == "--version" {
		// A windowsgui exe has no console: this reaches only a caller that
		// captures stdout, as an updater's probe does.
		fmt.Println(version.Text("boxtray", version.Get()))
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "--update" {
		if len(os.Args) != 3 {
			os.Exit(2) // usage: boxtray --update <tag>; without one this would start a tray
		}
		os.Exit(installByHand(os.Args[2]))
	}
	if headlessRequested(os.Args[1:]) {
		runHeadless()
		return
	}

	lg = openLog(logPath(), maxLogBytes)
	lg.Printf("boxtray starting (pid %d)", os.Getpid())
	defer func() {
		if r := recover(); r != nil {
			lg.Printf("exit: panic: %v\n%s", r, debug.Stack())
			os.Exit(2)
		}
	}()

	// One tray per login session.
	if !takeMutex(`Local\agent-box-boxtray`) {
		lg.Printf("exit: another boxtray is already running in this session")
		return
	}

	// The interactive tray is the enforcer while it runs; the headless one
	// sees this and stands aside. Taken first so the gap at logon is short.
	// Held until the process ends, which is also what frees it at logoff.
	// ALREADY_EXISTS is not a reason to exit: the headless enforcer opens this
	// name for an instant every tick, so a tray that starts in that instant
	// sees it, and exiting (code 0, no restart) would leave Full enforced all
	// session. One tray per session is the Local\ mutex above.
	if _, err := holdTrayActive(); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		// Not fatal: without it the headless enforcer would run beside us.
		lg.Printf("could not create %s (%v): the headless enforcer will not see this tray", trayActiveMutex, err)
	}

	// A missing config folder costs only the saved mode, never the tray.
	cfg, err := configPath()
	if err != nil {
		lg.Printf("no config path (%v): the chosen mode will not be saved", err)
		cfg = ""
	}
	mgr := mode.New(mode.CLI{Run: wslIncus}, loadMode(cfg))
	mgr.Reclaim = mode.CgroupReclaim{Run: wslRoot}
	// The project's chosen mode is the truth (a headless enforcer or a
	// dashboard request may have changed it since this tray last ran);
	// boxtray.json is the local copy, and the start-up pick if the project has none.
	mgr.Handshake, mgr.SeedChosen, mgr.Logf = true, true, lg.Printf
	mgr.Persist = func(m mode.Mode) {
		if cfg == "" {
			return
		}
		if err := saveMode(cfg, m); err != nil {
			lg.Printf("could not save the mode: %v", err)
		}
	}
	lg.Printf("mode at start: %s", mgr.Chosen())
	wake := make(chan struct{}, 1)
	poke := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}

	sensors := &winSensors{}
	drop := &cacheDrop{
		hostUp: sensors.HostUp,
		run: func(ctx context.Context) error {
			_, err := wslRoot(ctx, "sh", "-c", dropCachesScript)
			return err
		},
		logf: lg.Printf,
		busy: mgr.Releasing,
	}
	var releases sync.WaitGroup // Release memory runs in flight
	t, err := newTray(mgr.Chosen, trayActions{
		pick: func(m mode.Mode) {
			mgr.SetChosen(m)
			if cfg != "" {
				if err := saveMode(cfg, m); err != nil {
					lg.Printf("could not save the mode: %v", err)
				}
			}
			poke()
		},
		release: func() {
			// Counted before it starts, so a Quit right after it waits for it.
			releases.Add(1)
			go func() {
				defer releases.Done()
				releaseMemory(mgr, drop, func() error {
					ctx, cancel := context.WithTimeout(context.Background(), wslTerminateTimeout)
					defer cancel()
					return hidden(exec.CommandContext(ctx, "wsl.exe", "--terminate", distro)).Run()
				}, poke, lg.Printf)
			}()
		},
	})
	sl := &statusLog{logf: lg.Printf}
	report := func(st mode.Status) {
		sl.observe(st)
		drop.observe(st)
		if t != nil {
			t.setStatus(st)
		}
	}
	if err != nil {
		// No window means no icon and no menu, but enforcing the mode
		// matters more, so keep going.
		t = nil
		lg.Printf("tray window failed (%v): running the mode loop without an icon", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hb := newHeartbeat(mgr.Incus, mode.EnforcerTray, lg.Printf)
	restarts := make(chan func() bool, 1)
	up := autoUpdater(os.Args[1:], lg.Printf, func(start func() bool) {
		finish := func() bool {
			drop.setPaused(true)
			if finishUpdate(&releases, mgr, quitTimeout, lg.Printf, start) {
				cancel()
				return true
			}
			drop.setPaused(false)
			return false
		}
		if t == nil {
			if finish() {
				os.Exit(0)
			}
			return
		}
		// The mode loop stays here until the menu has stopped and the handoff
		// has finished. On a launch failure, both loops can resume.
		done := make(chan struct{})
		restarts <- func() bool {
			defer close(done)
			return finish()
		}
		if !t.requestRestart() {
			<-restarts
			return
		}
		select {
		case <-done:
		case <-ctx.Done():
		}
	})
	lc := loopCfg{Manager: func() *mode.Manager { return mgr }, Beat: hb.beat, Logf: lg.Printf, Maintain: up.maintain}
	go supervise(ctx, func() { run(ctx, lc, sensors, 5*time.Second, wake, report) })
	lg.Printf("mode loop started")

	if t == nil {
		select {} // nothing to pump; only a kill ends this
	}
	for t.loop() {
		if (<-restarts)() {
			return
		}
		t.restartIcon()
	}
	lg.Printf("exit: message loop ended")
	// Stop ticking, then let a release in flight finish and store the last
	// pick, so the headless enforcer that takes over applies it (#204).
	cancel()
	drop.setPaused(true)
	beforeQuit(&releases, mgr, quitTimeout, lg.Printf)
	lg.Printf("exit")
}

// supervise reruns f if it panics, so a bug in one tick can't leave the tray
// up with nothing enforcing the modes.
func supervise(ctx context.Context, f func()) {
	for ctx.Err() == nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					lg.Printf("mode loop panicked, restarting in 5s: %v\n%s", r, debug.Stack())
					time.Sleep(5 * time.Second)
				}
			}()
			f()
		}()
	}
}
