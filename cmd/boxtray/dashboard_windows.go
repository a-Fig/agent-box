//go:build windows

package main

import (
	"context"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/a-Fig/agent-box/internal/desktop"
)

// dashLookupTimeout bounds a click's lookup: two wsl.exe calls, each a second or so when the
// distro runs.
const dashLookupTimeout = 30 * time.Second

// runDashboard is `boxtray.exe --dashboard --url <page>`: the dashboard's window, until it is
// closed. It takes no mutex and writes no mode, so it never stands in for the tray or the
// enforcer. What goes wrong goes to boxtray.log.
func runDashboard(page string) {
	lg = openLog(logPath(), maxLogBytes)
	log.SetFlags(0)
	log.SetOutput(logWriter{}) // internal/desktop reports through the standard log
	if !desktop.Supported {
		lg.Printf("dashboard: this build has no window; opening %s in the browser", page)
	} else if err := desktop.Run(context.Background(), desktop.Options{URL: page, Open: browse}); err != nil {
		lg.Printf("dashboard: the window failed (%v); opening %s in the browser", err, page)
	} else {
		return
	}
	if err := browse(page); err != nil {
		lg.Printf("dashboard: couldn't open the browser: %v", err)
	}
}

type logWriter struct{}

func (logWriter) Write(p []byte) (int, error) {
	lg.Printf("dashboard: %s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// browse opens link in the default browser.
func browse(link string) error {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", link)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// dashOpener is the tray's side of Open dashboard. Both methods return at once; the work is on
// a goroutine of its own, never the UI thread.
type dashOpener struct {
	res     *dashResolver
	warming atomic.Bool
	next    atomic.Int64 // UnixNano: no warm lookup before this
}

// open finds the address and starts the window's process.
func (o *dashOpener) open() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), dashLookupTimeout)
		defer cancel()
		page, err := o.res.resolve(ctx)
		if err != nil {
			lg.Printf("Open dashboard: %v", err)
			return
		}
		exe, err := os.Executable()
		if err != nil {
			lg.Printf("Open dashboard: %v", err)
			return
		}
		// The window logs to boxtray.log itself; its stdout and stderr go nowhere.
		null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			lg.Printf("Open dashboard: %v", err)
			return
		}
		defer null.Close()
		if _, err := desktop.Detach(exe, []string{"--dashboard", "--url", page}, null); err != nil {
			lg.Printf("Open dashboard: couldn't start the window (%v); opening %s in the browser", err, page)
			if err := browse(page); err != nil {
				lg.Printf("Open dashboard: couldn't open the browser: %v", err)
			}
			return
		}
		lg.Printf("Open dashboard: %s", page)
	}()
}

// warmEvery spaces out warm's lookups while they fail (Tailscale not up yet in the distro).
const warmEvery = time.Minute

// warm looks the address up while the distro runs, so the first click is quick and a click after
// Release memory still has one. Called on every status report; it runs one lookup at a time, at
// most every warmEvery, until one has succeeded.
func (o *dashOpener) warm() {
	if !o.res.needsWarm() || time.Now().UnixNano() < o.next.Load() || !o.warming.CompareAndSwap(false, true) {
		return
	}
	o.next.Store(time.Now().Add(warmEvery).UnixNano())
	go func() {
		defer o.warming.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), dashLookupTimeout)
		defer cancel()
		if _, err := o.res.refresh(ctx); err != nil {
			lg.Printf("dashboard address not found yet: %v", err)
		}
	}()
}
