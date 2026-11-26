//go:build windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/a-Fig/agent-box/internal/update"
	"github.com/a-Fig/agent-box/internal/version"
	"golang.org/x/sys/windows"
)

// afterUpdate is the flag a restart passes to the new boxtray: its mutexes may still be held by
// the process it replaces for a moment, so it waits for them instead of exiting.
const afterUpdate = "--after-update"

// autoUpdater is the updater the mode loop runs, or nil when AGENT_BOX_NO_UPDATE is set.
func autoUpdater(args []string, logf func(string, ...any), quit func()) *trayUpdater {
	if os.Getenv("AGENT_BOX_NO_UPDATE") != "" {
		logf("update: off (AGENT_BOX_NO_UPDATE)")
		return nil
	}
	return newTrayUpdater(args, logf, quit)
}

// newTrayUpdater is this process's updater. args are the process's own (without the program),
// passed on to the new boxtray; quit ends this process.
func newTrayUpdater(args []string, logf func(string, ...any), quit func()) *trayUpdater {
	exe, err := os.Executable()
	if err != nil {
		logf("update: off (%v)", err)
		return nil
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	args = slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == afterUpdate })
	return &trayUpdater{Exe: exe, Pin: filepath.Join(filepath.Dir(logPath()), "boxtray-update.json"), Tag: version.Tag, GitHub: update.Client{}, Run: runHidden, Now: time.Now, Logf: logf,
		Restart: func() {
			cmd := exec.Command(exe, append(args, afterUpdate)...)
			// Its own process group, and out of the scheduled task's job object when the job
			// allows it, so it outlives this process.
			cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_BREAKAWAY_FROM_JOB}
			err := cmd.Start()
			if errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
				cmd = exec.Command(exe, append(args, afterUpdate)...)
				cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
				err = cmd.Start()
			}
			if err != nil {
				logf("update: can't start %s (%v): carrying on with this one", exe, err)
				return
			}
			cmd.Process.Release()
			logf("exit: replaced by the updated boxtray (pid %d)", cmd.Process.Pid)
			quit()
		}}
}

// installByHand is `boxtray.exe --update <tag>`. A windowsgui exe has no console, so it says what
// it did in boxtray.log and its exit code.
func installByHand(tag string) int {
	lg = openLog(logPath(), maxLogBytes)
	u := newTrayUpdater(nil, lg.Printf, func() {}) // AGENT_BOX_NO_UPDATE stops the hourly check, not this
	if u == nil {
		return 1
	}
	if err := u.Install(context.Background(), tag); err != nil {
		lg.Printf("--update %s: %v", tag, err)
		return 1
	}
	lg.Printf("--update: installed %s (the old one is %s)", tag, update.OldPath(u.Exe))
	return 0
}

func runHidden(ctx context.Context, exe string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := hidden(exec.CommandContext(ctx, exe, args...)).Output()
	return string(out), err
}

// takeMutex creates the named mutex and reports false only if another process already holds it.
// Any other failure lets this process go on, as before. After an update it waits for the process
// it replaces to let go: up to quitTimeout, which that process may spend on Quit, and 20 s more.
func takeMutex(name string) bool {
	wait := slices.Contains(os.Args[1:], afterUpdate)
	deadline := time.Now().Add(quitTimeout + 20*time.Second)
	for {
		h, err := createMutex(name) // held for the life of the process
		held := errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
		if !held {
			return true
		}
		if h != 0 {
			windows.CloseHandle(h)
		}
		if !wait || time.Now().After(deadline) {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
}
