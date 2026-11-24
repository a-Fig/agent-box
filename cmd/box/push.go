package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/a-Fig/agent-box/internal/mode"
)

// Push: a job that outlives box_run's inline wait is followed by a detached
// "box watch" process on the laptop. When the job ends, the watcher sends the
// result into the agent's T3 thread, so the agent can end its turn instead of
// waiting. docs/DESIGN.md ("Pushing results to the agent") has the why.
//
// The laptop keeps a few small files per job in App.StateDir:
//
//	<job>.json       the record: thread, reason, command, worktree
//	<job>.watch      the watcher's heartbeat (its mtime)
//	<job>.ended      the job ended: its exit code, or lost
//	<job>.notified   created exclusively by whoever delivers the end first
//	                 (the watcher before it sends, box_job when it returns a
//	                 terminal state, box_cancel), so it is delivered once
//	<job>.surfaced   a box_* result already said "finished since you last looked"
//	<job>.paused     the watcher already pushed the paused message

// inlineWait is how long box_run follows a job before it returns "running".
const inlineWait = 30 * time.Second

const (
	heartbeatEvery = 20 * time.Second
	heartbeatFresh = 75 * time.Second
	recordKeep     = 7 * 24 * time.Hour
	tailLines      = 60
)

// Pusher sends a message into a T3 thread as its next turn.
type Pusher interface {
	Send(ctx context.Context, thread, message string) error
}

// t3CLI sends with `t3 thread send <thread> -`, the message on stdin.
type t3CLI struct{ path string }

func (c t3CLI) Send(ctx context.Context, thread, message string) error {
	cmd := exec.CommandContext(ctx, c.path, "thread", "send", thread, "-")
	cmd.Stdin = strings.NewReader(message)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("t3 thread send: %w: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

// findT3 locates T3 Code's CLI: the path T3 exports, else the launcher it
// writes under ~/.t3/bin (Codex starts MCP servers with a bare environment).
func findT3(getenv func(string) string) string {
	cands := []string{getenv("T3CODE_CLI_PATH")}
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands, filepath.Join(home, ".t3", "bin", "t3.cmd"), filepath.Join(home, ".t3", "bin", "t3"))
	}
	for _, c := range cands {
		if c == "" {
			continue
		}
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

// defaultStateDir is %LOCALAPPDATA%\agent-box\jobs on Windows.
func defaultStateDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "agent-box", "jobs")
}

// spawnWatcher starts `box watch` detached, so it outlives this process.
func spawnWatcher(stateDir, job string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(stateDir, job+".watchlog"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	var last error
	for _, breakaway := range []bool{true, false} {
		cmd := exec.Command(exe, "watch", "--state", stateDir, job)
		cmd.Stdout, cmd.Stderr = logf, logf
		detach(cmd, breakaway)
		if last = cmd.Start(); last == nil {
			return cmd.Process.Release()
		}
	}
	return last
}

// jobRecord is <job>.json.
type jobRecord struct {
	Job      string `json:"job"`
	Thread   string `json:"thread"`
	Reason   string `json:"reason"`
	Command  string `json:"command"`
	Worktree string `json:"worktree"`
	GPU      bool   `json:"gpu"`
}

// jobEnd is <job>.ended.
type jobEnd struct {
	Code int  `json:"code"`
	Lost bool `json:"lost,omitempty"`
}

// thread is the T3 thread of this session, or "".
func (a *App) thread() string {
	if id, src := a.session(); src == "t3" {
		return id
	}
	return ""
}

// canPush says whether a job's result can be pushed to this session.
func (a *App) canPush() bool {
	return a.thread() != "" && a.StateDir != "" && a.Push != nil && a.Spawn != nil
}

func (a *App) statePath(job, ext string) string { return filepath.Join(a.StateDir, job+ext) }

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// claim creates <job><ext> exclusively and says whether this caller made it.
func (a *App) claim(job, ext string) bool {
	if a.StateDir == "" {
		return false
	}
	os.MkdirAll(a.StateDir, 0o755)
	f, err := os.OpenFile(a.statePath(job, ext), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

func (a *App) exists(job, ext string) bool {
	_, err := os.Stat(a.statePath(job, ext))
	return err == nil
}

// startWatch records a running job and starts its watcher. It says whether
// the result will be pushed.
func (a *App) startWatch(info jobMeta, job string, gpu bool) bool {
	if !a.canPush() {
		return false
	}
	os.MkdirAll(a.StateDir, 0o755)
	rec := jobRecord{Job: job, Thread: a.thread(), Reason: info.Reason, Command: info.Command, Worktree: info.Agent.Worktree, GPU: gpu}
	if err := writeJSON(a.statePath(job, ".json"), rec); err != nil {
		a.warn("box: warning: no push for " + job + ": " + err.Error())
		return false
	}
	if err := a.Spawn(a.StateDir, job); err != nil {
		a.warn("box: warning: no push for " + job + ": starting box watch: " + err.Error())
		return false
	}
	return true
}

// noteDelivered says a result reached the agent directly (box_job, box_cancel
// or an inline box_run), so the watcher must not push it again.
func (a *App) noteDelivered(job string) {
	if a.StateDir == "" {
		return
	}
	a.claim(job, ".notified")
	if a.exists(job, ".json") {
		a.claim(job, ".surfaced")
	}
}

// safetyNet runs at the start of every box_* call. It restarts the watchers
// this session lost (a laptop sleep or reboot kills them) and returns a line
// about jobs of this session that have ended since the agent last looked, or
// "". skip is a job the call is about to report itself.
func (a *App) safetyNet(skip string) string {
	thread := a.thread()
	if thread == "" || a.StateDir == "" {
		return ""
	}
	ents, err := os.ReadDir(a.StateDir)
	if err != nil {
		return ""
	}
	var lines []string
	for _, e := range ents {
		job, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() {
			continue
		}
		var rec jobRecord
		if readJSON(a.statePath(job, ".json"), &rec) != nil {
			continue
		}
		if info, err := e.Info(); err == nil && a.Now().Sub(info.ModTime()) > recordKeep {
			for _, ext := range []string{".json", ".watch", ".ended", ".notified", ".surfaced", ".paused", ".watchlog"} {
				os.Remove(a.statePath(job, ext))
			}
			continue
		}
		if rec.Thread != thread || job == skip {
			continue
		}
		var end jobEnd
		switch {
		case readJSON(a.statePath(job, ".ended"), &end) == nil:
			if a.claim(job, ".surfaced") {
				if end.Lost {
					lines = append(lines, fmt.Sprintf("%s lost (exit 75)", job))
				} else {
					lines = append(lines, fmt.Sprintf("%s exit %d", job, end.Code))
				}
			}
		case !a.exists(job, ".notified") && !a.watcherAlive(job):
			if a.Spawn != nil {
				if err := a.Spawn(a.StateDir, job); err != nil {
					a.warn("box: warning: restarting the watcher for " + job + ": " + err.Error())
				}
			}
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "finished since you last looked: " + strings.Join(lines, ", ") + "\n"
}

func (a *App) watcherAlive(job string) bool {
	st, err := os.Stat(a.statePath(job, ".watch"))
	return err == nil && a.Now().Sub(st.ModTime()) < heartbeatFresh
}

// lineTail keeps the end of what it is written, for the last lines of a log.
type lineTail struct {
	mu  sync.Mutex
	buf []byte
}

func (l *lineTail) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	if len(l.buf) > 64<<10 {
		l.buf = append(l.buf[:0], l.buf[len(l.buf)-64<<10:]...)
	}
	return len(p), nil
}

// lastLines is the last n lines, cut at a whole character.
func (l *lineTail) lastLines(n int) string {
	l.mu.Lock()
	b := l.buf
	l.mu.Unlock()
	for len(b) > 0 && !utf8.RuneStart(b[0]) {
		b = b[1:]
	}
	s := strings.TrimRight(strings.ToValidUTF8(string(b), "�"), "\r\n")
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// elapsed is a duration as "45s", "6m12s" or "1h02m03s".
func elapsed(d time.Duration) string {
	d = d.Round(time.Second)
	if d < 0 {
		d = 0
	}
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

const fromBox = "From box, not your user."

func (r jobRecord) context() string {
	return fmt.Sprintf("%s\n%s  in  %s", r.Reason, r.Command, r.Worktree)
}

func endedMessage(r jobRecord, code int, took time.Duration, tail string) string {
	return fmt.Sprintf("box: job %s ended, exit %d, after %s. %s\n%s\n%s\nFull log: box_job id %s offset 0. Artifacts: box_pull, before this repo's next run.",
		r.Job, code, elapsed(took), fromBox, r.context(), tail, r.Job)
}

func pausedMessage(r jobRecord) string {
	return fmt.Sprintf("box: job %s paused: the PC's owner took the PC back. It resumes if they switch back within %s, else it is lost. Can't wait? Run it locally and box_cancel this job. %s\n%s",
		r.Job, frozenWindow, fromBox, r.context())
}

func lostMessage(r jobRecord, tail string) string {
	msg := fmt.Sprintf("box: job %s lost, exit 75: run it locally. %s\n%s", r.Job, fromBox, r.context())
	if tail != "" {
		msg += "\n" + tail
	}
	return msg
}

// send delivers a message, retrying a few times.
func (a *App) send(ctx context.Context, thread, msg string) error {
	var err error
	for try := 0; try < 4; try++ {
		if try > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(a.watchWait(5 * time.Second)):
			}
		}
		if err = a.Push.Send(ctx, thread, msg); err == nil {
			return nil
		}
	}
	return err
}

// watchWait is d, or a tenth of a millisecond when tests set WatchPoll.
func (a *App) watchWait(d time.Duration) time.Duration {
	if a.WatchPoll > 0 {
		return a.WatchPoll
	}
	return d
}

// Watch follows a job until it ends and pushes the result into its thread.
// It is `box watch`, started detached by box_run.
func (a *App) Watch(ctx context.Context, job string) error {
	if !validJob.MatchString(job) {
		return fmt.Errorf("bad job id %q", job)
	}
	if a.StateDir == "" || a.Push == nil {
		return errors.New("no state directory or T3 CLI to push with")
	}
	var rec jobRecord
	if err := readJSON(a.statePath(job, ".json"), &rec); err != nil {
		return fmt.Errorf("no record of %s: %w", job, err)
	}
	if a.exists(job, ".notified") || a.exists(job, ".ended") {
		return nil
	}
	if a.watcherAlive(job) {
		return nil
	}
	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	beat := func() {
		now := a.Now()
		if f, err := os.OpenFile(a.statePath(job, ".watch"), os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			f.Close()
			os.Chtimes(a.statePath(job, ".watch"), now, now)
		}
	}
	beat()
	go func() {
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-time.After(a.watchWait(heartbeatEvery)):
				beat()
			}
		}
	}()

	w := *a
	tail := &lineTail{}
	w.Out, w.Err = tail, io.Discard
	w.Warn = func(string) {}
	w.Notify = nil
	w.Thread = rec.Thread
	start := jobStart(job)
	deadline := time.Time{}
	for {
		res, err := w.Job(ctx, job, 0)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		switch {
		case err == nil && res.State == stateDone:
			return a.finish(ctx, rec, jobEnd{Code: res.Code}, a.Now().Sub(start), tail.lastLines(tailLines))
		case err == nil && res.State == statePaused && !rec.GPU:
			if a.claim(job, ".paused") {
				a.send(ctx, rec.Thread, pausedMessage(rec))
			}
			if deadline.IsZero() {
				deadline = a.Now().Add(mode.FrozenSwappedStop + 10*time.Minute)
			}
			if a.Now().After(deadline) {
				return a.finish(ctx, rec, jobEnd{Code: exitPaused, Lost: true}, 0, "")
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(a.watchWait(30 * time.Second)):
			}
		default:
			// A stopped GPU box, a job that died without an exit code, or one
			// the box no longer has.
			return a.finish(ctx, rec, jobEnd{Code: exitPaused, Lost: true}, 0, tail.lastLines(tailLines))
		}
	}
}

// finish records the end and pushes it unless someone delivered it already.
func (a *App) finish(ctx context.Context, rec jobRecord, end jobEnd, took time.Duration, tail string) error {
	writeJSON(a.statePath(rec.Job, ".ended"), end)
	if !a.claim(rec.Job, ".notified") {
		return nil // box_job or box_cancel delivered it, or a second watcher did
	}
	msg := endedMessage(rec, end.Code, took, tail)
	if end.Lost {
		msg = lostMessage(rec, tail)
	}
	if err := a.send(ctx, rec.Thread, msg); err != nil {
		os.Remove(a.statePath(rec.Job, ".notified")) // box_* results will say it ended
		return err
	}
	return nil
}
