//go:build windows

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/a-Fig/agent-box/internal/mode"
	"golang.org/x/sys/windows"
)

// Mutexes in the Global namespace, which every logon session (and the
// session-less scheduled task) can see; Local\ ones are per session.
const (
	// trayActiveMutex is held by an interactive tray for as long as it runs.
	trayActiveMutex = `Global\agent-box-tray-active`
	// headlessMutex is held by the headless enforcer, so a second one exits.
	headlessMutex = `Global\agent-box-enforcer`
)

// holdTrayActive takes trayActiveMutex for the life of the process.
func holdTrayActive() (windows.Handle, error) { return createMutex(trayActiveMutex) }

func createMutex(name string) (windows.Handle, error) {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	// With ERROR_ALREADY_EXISTS the handle is still valid and still counts as a
	// holder, so the caller keeps it for the life of the process.
	return windows.CreateMutex(nil, false, n)
}

// trayRunning reports whether an interactive tray holds trayActiveMutex. An
// access error still means it exists. Any other failure counts as "no tray",
// so a broken check can leave two enforcers (harmless: they agree except
// during a hand-over) but never none.
func trayRunning() bool {
	n, err := windows.UTF16PtrFromString(trayActiveMutex)
	if err != nil {
		return false
	}
	h, err := windows.OpenMutex(windows.SYNCHRONIZE, false, n)
	if err == nil {
		windows.CloseHandle(h)
		return true
	}
	return errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

// headlessLogPath is %LOCALAPPDATA%\agent-box\boxtray-headless.log: its own
// file, because the tray may be trimming boxtray.log at the same moment.
func headlessLogPath() string { return filepath.Join(filepath.Dir(logPath()), "boxtray-headless.log") }

// runHeadless is `boxtray.exe --headless`, run by the agent-box-enforcer
// scheduled task from startup, with or without anyone logged on. It runs the
// same mode loop as the tray, with Full as the default until a pick is
// stored in the project (a tray click or a dashboard request), and it stands aside, writing nothing, while the interactive tray
// runs. It returns only if another headless enforcer already runs.
func runHeadless() {
	lg = openLog(headlessLogPath(), maxLogBytes)
	lg.Printf("boxtray --headless starting (pid %d)", os.Getpid())
	defer func() {
		if r := recover(); r != nil {
			lg.Printf("exit: panic: %v", r)
			os.Exit(2)
		}
	}()
	if _, err := createMutex(headlessMutex); errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		lg.Printf("exit: another headless enforcer is already running")
		return
	}

	inc := mode.CLI{Run: wslIncus}
	hb := newHeartbeat(inc, mode.EnforcerHeadless, lg.Printf)
	cfg := loopCfg{
		Manager: func() *mode.Manager {
			hb.last = time.Time{} // say so at once after standing aside
			// Full is only the default: the project's chosen mode (a tray
			// pick or a dashboard request) replaces it on the first tick.
			m := mode.New(inc, mode.Full)
			m.Handshake, m.Logf = true, lg.Printf
			m.Reclaim = mode.CgroupReclaim{Run: wslRoot} // swap limits are part of the job
			return m
		},
		Idle: trayRunning,
		Beat: hb.beat,
		Logf: lg.Printf,
	}
	sl := &statusLog{logf: lg.Printf}
	ctx := context.Background()
	supervise(ctx, func() {
		run(ctx, cfg, &winSensors{}, 5*time.Second, nil, sl.observe)
	})
}
