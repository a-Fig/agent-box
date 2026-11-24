package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/a-Fig/agent-box/internal/desktop"
)

// DashURL is the dashboard's address: https://<host>/ of the pc remote. Incus listens on 8443
// there; Tailscale serve puts boxd on 443 of the same name (docs/ui.md). The name comes from the
// remote, never from here: the repo is public.
func (a *App) DashURL(ctx context.Context) (string, error) {
	res, err := a.incus(ctx, Cmd{Args: []string{"remote", "list", "--format", "json"}})
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		return "", fmt.Errorf("incus remote list: %s", strings.TrimSpace(res.Stderr))
	}
	var remotes map[string]struct {
		Addr            string   `json:"Addr"`
		Addrs           []string `json:"Addrs"`
		LastWorkingAddr string   `json:"LastWorkingAddr"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &remotes); err != nil {
		return "", fmt.Errorf("incus remote list: %v", err)
	}
	r, ok := remotes[a.Remote]
	if !ok {
		return "", fmt.Errorf("no incus remote %q (BOX_REMOTE); --url gives the dashboard's address", a.Remote)
	}
	addr := r.Addr
	if len(r.Addrs) > 0 {
		addr = r.Addrs[0]
	}
	if addr == "" {
		addr = r.LastWorkingAddr
	}
	u, err := url.Parse(addr)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return "", fmt.Errorf("incus remote %s is at %q, not an https address; --url gives the dashboard's address", a.Remote, addr)
	}
	host := u.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return "https://" + host + "/", nil
}

// pageURL is --url when given, checked, or else the remote's dashboard.
func (a *App) pageURL(ctx context.Context, given string) (string, error) {
	if given == "" {
		return a.DashURL(ctx)
	}
	u, err := url.Parse(given)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("--url %q: want an http or https address", given)
	}
	return given, nil
}

// browse opens link in the person's browser.
func browse(link string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", link)
	case "darwin":
		cmd = exec.Command("open", link)
	default:
		cmd = exec.Command("xdg-open", link)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// runUI is box ui: the dashboard in the browser.
func runUI(ctx context.Context, a *App, fs *flag.FlagSet, args []string) int {
	given := fs.String("url", "", "the dashboard's address (default: https://<host>/ of the remote)")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		return 2
	}
	page, err := a.pageURL(ctx, *given)
	if err != nil {
		fmt.Fprintln(a.Err, "box:", err)
		return 1
	}
	fmt.Fprintln(a.Out, page)
	if err := a.Browse(page); err != nil {
		fmt.Fprintln(a.Err, "box: couldn't open the browser:", err)
		return 1
	}
	return 0
}

// appStartWait is how long box app watches the window's process before saying it opened: a
// window that can't be made (no web view, no screen) fails within a second or two.
const appStartWait = 2 * time.Second

// runApp is box app: the dashboard in a window of its own. It starts the window as a process of
// its own and returns, so the terminal is free; --wait is that process.
func runApp(ctx context.Context, a *App, fs *flag.FlagSet, args []string) int {
	given := fs.String("url", "", "the dashboard's address (default: https://<host>/ of the remote)")
	wait := fs.Bool("wait", false, "stay until the window closes")
	devtools := fs.Bool("devtools", false, "let the window open the web view's developer tools")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintln(a.Err, "box:", err)
		return 1
	}
	if !desktop.Supported {
		return fail(desktop.Run(ctx, desktop.Options{}))
	}
	page, err := a.pageURL(ctx, *given)
	if err != nil {
		return fail(err)
	}
	if *wait {
		if err := desktop.Run(ctx, desktop.Options{URL: page, Open: a.Browse, Debug: *devtools}); err != nil {
			return fail(err)
		}
		return 0
	}

	exe, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	logPath, err := appLog()
	if err != nil {
		return fail(err)
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fail(err)
	}
	defer logf.Close()
	start, _ := logf.Seek(0, io.SeekEnd)
	child := []string{"app", "--wait", "--url", page}
	if *devtools {
		child = append(child, "--devtools")
	}
	exited, err := desktop.Detach(exe, child, logf)
	if err != nil {
		return fail(err)
	}
	select {
	case err := <-exited:
		return fail(fmt.Errorf("the window closed as it opened (%v): %s", err, lastLine(logPath, start)))
	case <-time.After(appStartWait):
	}
	fmt.Fprintln(a.Out, "Opened", page)
	return 0
}

// appLogMax is the size at which the window's log starts over, keeping one old copy.
const appLogMax = 1 << 20

// appLog is the window's log, in the user's cache directory.
func appLog() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "agent-box")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "app.log")
	if fi, err := os.Stat(path); err == nil && fi.Size() > appLogMax {
		os.Rename(path, path+".old")
	}
	return path, nil
}

// lastLine is the last line written to the log at path after offset from.
func lastLine(path string, from int64) string {
	b, err := os.ReadFile(path)
	if err != nil || int64(len(b)) <= from {
		return "see " + path
	}
	lines := strings.Split(strings.TrimSpace(string(b[from:])), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
