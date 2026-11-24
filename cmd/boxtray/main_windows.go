//go:build windows

// Command boxtray is the Windows tray app: it picks how much of the PC agents
// get (My PC / Shared / Full), pauses agents while a game runs, and keeps the
// PC awake while agent instances run. Build it with -ldflags "-H=windowsgui".
package main

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/a-Fig/agent-box/internal/mode"
	"golang.org/x/sys/windows"
)

func main() {
	runtime.LockOSThread() // the tray window must be created and pumped on one thread

	// One tray per login session.
	name, _ := windows.UTF16PtrFromString(`Local\agent-box-boxtray`)
	if _, err := windows.CreateMutex(nil, false, name); err == windows.ERROR_ALREADY_EXISTS {
		return
	}

	cfg, err := configPath()
	if err != nil {
		os.Exit(1)
	}
	mgr := mode.New(mode.CLI{Run: wslIncus}, loadMode(cfg))
	wake := make(chan struct{}, 1)
	poke := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}

	t, err := newTray(mgr.Chosen, trayActions{
		pick: func(m mode.Mode) {
			mgr.SetChosen(m)
			saveMode(cfg, m)
			poke()
		},
		release: func() {
			go func() {
				hidden(exec.Command("wsl.exe", "--terminate", distro)).Run()
				poke()
			}()
		},
	})
	if err != nil {
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go run(ctx, mgr, &winSensors{}, 5*time.Second, wake, t.setStatus)
	t.loop()
	cancel()
}
