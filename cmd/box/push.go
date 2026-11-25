package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/a-Fig/agent-box/internal/jobrun"
	"github.com/a-Fig/agent-box/internal/mask"
	"github.com/a-Fig/agent-box/internal/mode"
)

// Push: a job that outlives box_run's inline wait is followed by a detached
// "box watch" process on the laptop. When the job ends, the watcher sends the
// result into the agent's T3 thread, so the agent can end its turn instead of
// waiting. docs/DESIGN.md ("Pushing results to the agent") has the why.
//
// The rule behind all of it: a duplicate notice is fine, a lost one is not.
//
// The laptop keeps a few small files per job in App.StateDir:
//
//	<job>.json       the record: thread, reason, command, worktree
//	<job>.watch      the watcher's lease: "<token> <expiry>", renewed while it lives
//	<job>.ended      the job's end (exit code or lost, took, tail), written
//	                 before anything is sent
//	<job>.sending    a lease held while a send is under way
//	<job>.notified   the end reached the agent: made after t3 accepted the
//	                 push, by box_job or box job when it gives the session its
//	                 end, or by
//	                 box_cancel. Until it exists the end is still owed.
//	<job>.paused     the watcher already pushed the paused message
//
//	<job>.result     the exit code and output tail box read, pushed or not
//	                 (recordEnd), so box_job can give them after the box has gone

// inlineWait is how long box_run follows a job, counted from the command's
// start, before it returns "running".
const inlineWait = 30 * time.Second

const (
	leaseBeat  = 20 * time.Second
	watchLease = 90 * time.Second
	sendLease  = 2 * time.Minute
	sendTry    = 60 * time.Second
	recordKeep = 7 * 24 * time.Hour
	tempKeep   = time.Hour
	tailLines  = 20
	endedKeep  = jobrun.EndedKeep
)

// Pusher sends a message into a T3 thread as its next turn.
type Pusher interface {
	Send(ctx context.Context, thread, message string) error
}

// t3CLI sends with `t3 thread send <thread> -`, the message on stdin.
type t3CLI struct{ path string }

func (c t3CLI) Send(ctx context.Context, thread, message string) error {
	ctx, cancel := context.WithTimeout(ctx, sendTry)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.path, "thread", "send", thread, "-")
	cmd.Stdin = strings.NewReader(message)
	hideWindow(cmd)
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

// spawnWatcher starts `box watch` detached, so it outlives this process. On
// Windows it first tries to break away from this process's job object, which
// would kill the watcher with the MCP server. Codex starts MCP servers in a
// job that forbids that (access denied, seen 2026-11-25), so the watcher then
// starts inside it: it lives as long as the MCP server, which Codex keeps for
// its session, and if it dies the safety net restarts it (safetyNet).
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
	return startDetached(func(breakaway bool) error {
		cmd := exec.Command(exe, "watch", "--state", stateDir, job)
		cmd.Stdout, cmd.Stderr = logf, logf
		detach(cmd, breakaway)
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	})
}

// errWatcherInJob is the success of a watcher started inside the MCP
// client's job object: it dies with the MCP server, so the push is only as
// good as the server's life, and the agent is told so.
var errWatcherInJob = errors.New("the watcher runs inside the MCP client's job object")

// startDetached starts a process with breakaway, and again without it when
// the job object refuses breakaway (then errWatcherInJob).
func startDetached(start func(breakaway bool) error) error {
	err := start(true)
	if err != nil && breakawayDenied(err) {
		if err = start(false); err == nil {
			return errWatcherInJob
		}
	}
	if err != nil {
		return fmt.Errorf("can't start a detached watcher: %w", err)
	}
	return nil
}

// jobRecord is <job>.json.
type jobRecord struct {
	Job    string `json:"job"`
	Thread string `json:"thread"` // the session the result is for
	// Session is Claude Code's session ID, when box_run ran under Claude
	// Code: box hook tells only that session, not a claude started inside it.
	Session  string `json:"session,omitempty"`
	Reason   string `json:"reason"`
	Command  string `json:"command"`
	Worktree string `json:"worktree"`
	GPU      bool   `json:"gpu"`
	New      bool   `json:"new"` // a throwaway box, kept endedKeep after the job
	// Queued is a job box_run handed to boxd's queue: the watcher follows it
	// through the queue first (waitQueued).
	Queued bool `json:"queued,omitempty"`
}

// jobEnd is <job>.ended: everything needed to send the end again.
type jobEnd struct {
	Code int  `json:"code"`
	Lost bool `json:"lost,omitempty"`
	// Dropped is a queued job that left the queue without running.
	Dropped bool   `json:"dropped,omitempty"`
	Took    int64  `json:"tookSeconds"`
	Tail    string `json:"tail,omitempty"`
	Digest  string `json:"digest,omitempty"` // a test run's failure digest
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

// writeFile replaces path whole: a uniquely named temporary file, then a rename.
func writeFile(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFile(path, b)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// claimBy creates <job><ext> exclusively, with who inside. It reports whether
// this call made it; an error is a filesystem failure, not "already there".
// The file is written whole under a unique temporary name and hard-linked into
// place, so it is never seen empty and only one link wins.
func (a *App) claimBy(job, ext, who string) (bool, error) {
	if a.StateDir == "" {
		return false, errors.New("no state directory")
	}
	if err := os.MkdirAll(a.StateDir, 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(a.StateDir, ".claim-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.WriteString(who)
	if err := errors.Join(werr, tmp.Close()); err != nil {
		return false, err
	}
	switch err := os.Link(tmp.Name(), a.statePath(job, ext)); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrExist):
		return false, nil
	default:
		return false, err
	}
}

func (a *App) exists(job, ext string) bool {
	_, err := os.Stat(a.statePath(job, ext))
	return err == nil
}

// markRead marks a pushed job notified once box_job or box job has given
// its end to the session the push was for: the agent has the full result,
// so the watcher needn't push it, and box job deletes a throwaway at once,
// so a later push would say a box is kept that is gone (#76). Read from
// anywhere else (a person's terminal, another session), the push still goes.
// If the watcher pushed it in the same moment, the agent gets it twice.
func (a *App) markRead(job string, r JobResult, err error, who string) {
	var rec jobRecord
	if err == nil && r.State == stateDone && readJSON(a.statePath(job, ".json"), &rec) == nil && rec.Thread == a.thread() {
		a.markNotified(job, who)
	}
}

// markNotified says the end reached the agent. A failure to write it only
// risks a duplicate notice, so it is a warning.
func (a *App) markNotified(job, who string) {
	if a.StateDir == "" {
		return
	}
	if _, err := a.claimBy(job, ".notified", who); err != nil {
		a.warn("box: warning: recording that " + job + " was delivered: " + err.Error())
	}
}

// A lease is a file "<token> <expiry unix nanos>" made exclusively. A lease
// past its expiry may be taken over. Its holder renews it with its token.
var errLeaseHeld = errors.New("lease held")

func (a *App) leaseText(token string, ttl time.Duration) string {
	return token + " " + strconv.FormatInt(a.Now().Add(ttl).UnixNano(), 10)
}

func (a *App) readLease(job, ext string) (token string, expiry time.Time, ok bool) {
	b, err := os.ReadFile(a.statePath(job, ext))
	if err != nil {
		return "", time.Time{}, false
	}
	t, e, found := strings.Cut(string(b), " ")
	n, err := strconv.ParseInt(strings.TrimSpace(e), 10, 64)
	if !found || err != nil {
		return "", time.Time{}, true // a lease nobody can read: treat as expired
	}
	return t, time.Unix(0, n), true
}

func (a *App) leaseHeld(job, ext string) bool {
	_, exp, ok := a.readLease(job, ext)
	return ok && exp.After(a.Now())
}

func (a *App) acquireLease(job, ext string, ttl time.Duration) (string, error) {
	token := strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	for try := 0; try < 2; try++ {
		won, err := a.claimBy(job, ext, a.leaseText(token, ttl))
		if err != nil {
			return "", err
		}
		if won {
			return token, nil
		}
		if a.leaseHeld(job, ext) {
			return "", errLeaseHeld
		}
		os.Remove(a.statePath(job, ext)) // expired: take it over
	}
	return "", errLeaseHeld
}

// renewLease extends a lease this token holds; false means it was lost.
func (a *App) renewLease(job, ext, token string, ttl time.Duration) bool {
	if t, _, ok := a.readLease(job, ext); !ok || t != token {
		return false
	}
	return writeFile(a.statePath(job, ext), []byte(a.leaseText(token, ttl))) == nil
}

func (a *App) releaseLease(job, ext, token string) {
	if t, _, ok := a.readLease(job, ext); ok && t == token {
		os.Remove(a.statePath(job, ext))
	}
}

// startWatch records a running or queued job and starts its watcher. It
// says whether the result will be pushed.
func (a *App) startWatch(info jobMeta, job string, gpu, throwaway, queued bool) bool {
	if !a.canPush() {
		return false
	}
	if err := os.MkdirAll(a.StateDir, 0o755); err != nil {
		a.warn("box: warning: no push for " + job + ": " + err.Error())
		return false
	}
	rec := jobRecord{Job: job, Thread: a.thread(), Session: a.getenv("CLAUDE_CODE_SESSION_ID"), Reason: info.Reason, Command: info.Command, Worktree: info.Agent.Worktree, GPU: gpu, New: throwaway, Queued: queued}
	if err := writeJSON(a.statePath(job, ".json"), rec); err != nil {
		a.warn("box: warning: no push for " + job + ": " + err.Error())
		return false
	}
	err := a.Spawn(a.StateDir, job)
	if errors.Is(err, errWatcherInJob) {
		a.warn("box: notice: this push lasts only as long as box's MCP server, which can't outlive your client here; if your session restarts before it arrives, call box_job " + job + ".")
		err = nil
	}
	if err != nil {
		os.Remove(a.statePath(job, ".json"))
		a.warn("box: warning: no push for " + job + ": " + err.Error())
		return false
	}
	return true
}

// safetyNet runs at the start of every box_* call. It restarts watchers that
// are gone (a laptop sleep or reboot kills them) and returns a line about
// jobs of this session that have ended and not reached the agent yet, or "".
// A job stays in the line until its .notified exists. skip is a job the call
// is about to report itself.
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
		name := e.Name()
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(name, ".claim-") || strings.HasPrefix(name, ".tmp-") {
			if info, err := e.Info(); err == nil && a.Now().Sub(info.ModTime()) > tempKeep {
				os.Remove(filepath.Join(a.StateDir, name)) // left by a crashed writer
			}
			continue
		}
		if strings.HasSuffix(name, ".hookseen") {
			if info, err := e.Info(); err == nil && a.Now().Sub(info.ModTime()) > recordKeep {
				os.Remove(filepath.Join(a.StateDir, name)) // a thread long gone
			}
			continue
		}
		job, ok := strings.CutSuffix(name, ".json")
		if !ok {
			continue
		}
		var rec jobRecord
		if readJSON(a.statePath(job, ".json"), &rec) != nil {
			continue
		}
		if info, err := e.Info(); err == nil && a.Now().Sub(info.ModTime()) > recordKeep {
			for _, ext := range []string{".json", ".watch", ".ended", ".sending", ".notified", ".paused", ".nudged", ".watchlog", ".result"} {
				os.Remove(a.statePath(job, ext))
			}
			continue
		}
		if rec.Thread != thread || job == skip || a.exists(job, ".notified") {
			continue
		}
		var end jobEnd
		if readJSON(a.statePath(job, ".ended"), &end) == nil {
			if end.Lost {
				lines = append(lines, fmt.Sprintf("%s lost (exit 75)", job))
			} else {
				lines = append(lines, fmt.Sprintf("%s exit %d", job, end.Code))
			}
			if a.leaseHeld(job, ".sending") || a.leaseHeld(job, ".watch") {
				continue
			}
		} else if a.leaseHeld(job, ".watch") {
			continue
		}
		// Running with no watcher, or ended with nobody sending it: start one.
		if a.Spawn != nil {
			if err := a.Spawn(a.StateDir, job); err != nil && !errors.Is(err, errWatcherInJob) {
				a.warn("box: warning: restarting the watcher for " + job + ": " + err.Error())
			}
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "finished since you last looked: " + strings.Join(lines, ", ") + "\n"
}

// keptTail is how much of a job's output its recorded end keeps: as much as
// box_run and box_job return.
const keptTail = 32 << 10

// keepTail is the last keptTail bytes of s, from a whole character.
func keepTail(s string) string {
	if len(s) > keptTail {
		s = s[len(s)-keptTail:]
		for len(s) > 0 && !utf8.RuneStart(s[0]) {
			s = s[1:]
		}
	}
	return s
}

// recordEnd keeps, in <job>.result, the end of a job whose exit box has
// read. It is its own file because .ended is the watcher's decision that an
// end is owed to the agent, which a cancelled job's exit is not. Writing one
// also prunes results past recordKeep, since the safety net runs only in T3.
// A failure only costs the record, so it is dropped.
func (a *App) recordEnd(job string, end jobEnd) {
	if a.StateDir == "" || os.MkdirAll(a.StateDir, 0o755) != nil {
		return
	}
	writeJSON(a.statePath(job, ".result"), end)
	ents, _ := os.ReadDir(a.StateDir)
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".result") {
			continue
		}
		if info, err := e.Info(); err == nil && a.Now().Sub(info.ModTime()) > recordKeep {
			os.Remove(filepath.Join(a.StateDir, e.Name()))
		}
	}
}

// recordedEnd is a job's recorded end: the exit box read, else the end the
// watcher recorded if it ran to an exit code.
func (a *App) recordedEnd(job string) (jobEnd, bool) {
	if a.StateDir == "" || !validJob.MatchString(job) {
		return jobEnd{}, false
	}
	for _, ext := range []string{".result", ".ended"} {
		var end jobEnd
		if readJSON(a.statePath(job, ext), &end) == nil && !end.Lost && !end.Dropped {
			return end, true
		}
	}
	return jobEnd{}, false
}

// lineTail keeps the end of what it is written, for the last lines of a log.
type lineTail struct {
	mu    sync.Mutex
	buf   []byte
	stale bool // reset was called: the next write replaces buf
}

// reset is called before a follow that prints the log again from its start:
// what was written goes at that follow's first write, so a follow that
// prints nothing (a frozen box, a box gone) keeps the last one's tail for a
// lost push.
func (l *lineTail) reset() {
	l.mu.Lock()
	l.stale = true
	l.mu.Unlock()
}

func (l *lineTail) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stale && len(p) > 0 {
		l.buf, l.stale = l.buf[:0], false
	}
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

// context is the reason and command, masked like boxd's history masks them,
// since T3 keeps what is sent.
func (r jobRecord) context() string {
	return fmt.Sprintf("%s\n%s  in  %s", mask.Mask(r.Reason), mask.Mask(r.Command), r.Worktree)
}

func endedMessage(r jobRecord, e jobEnd) string {
	msg := fmt.Sprintf("box: job %s ended, exit %d, after %s. %s\n%s\n", r.Job, e.Code, elapsed(time.Duration(e.Took)*time.Second), fromBox, r.context())
	if e.Digest != "" {
		msg += mask.Mask(e.Digest) + "Output tail:\n"
	}
	msg += mask.Mask(e.Tail) + "\n"
	if e.Code == exitPaused {
		msg += "(The command itself exited 75: this is its result, not a refusal.)\n"
	}
	if r.New {
		msg += fmt.Sprintf("Box and log kept up to %d minutes (a launch short of room may take them sooner). ", int(endedKeep.Minutes()))
	}
	return msg + fmt.Sprintf("Full log: box_job id %s offset 0. Artifacts: box_pull, before this repo's next run.", r.Job)
}

func pausedMessage(r jobRecord) string {
	// T3 may deliver this long after it was sent, when the job has resumed
	// (#96); the end push follows either way.
	return fmt.Sprintf("box: job %s paused: the PC's owner took the PC back. It resumes if they switch back within %s, else it is lost; its end is pushed either way. If box_job still says paused and you can't wait, run it locally and box_cancel this job. %s\n%s",
		r.Job, frozenWindow, fromBox, r.context())
}

func lostMessage(r jobRecord, e jobEnd) string {
	msg := fmt.Sprintf("box: job %s lost, exit 75: run it locally. %s\n%s", r.Job, fromBox, r.context())
	if e.Tail != "" {
		msg += "\n" + mask.Mask(e.Tail)
	}
	return msg
}

// endMessage is what the agent is told of a job's end.
func endMessage(rec jobRecord, end jobEnd) string {
	switch {
	case end.Dropped:
		return droppedMessage(rec)
	case end.Lost:
		return lostMessage(rec, end)
	}
	return endedMessage(rec, end)
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

// watchWait is d, or WatchPoll when tests set it.
func (a *App) watchWait(d time.Duration) time.Duration {
	if a.WatchPoll > 0 {
		return a.WatchPoll
	}
	return d
}

func (a *App) sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(a.watchWait(d)):
		return true
	}
}

// Watch follows a job until it ends and pushes the result into its thread.
// It is `box watch`, started detached by box_run. It holds the job's watch
// lease, so a second watcher exits at once.
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
	if a.exists(job, ".notified") {
		return nil
	}
	token, err := a.acquireLease(job, ".watch", watchLease)
	if err != nil {
		if errors.Is(err, errLeaseHeld) {
			return nil
		}
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	renewed := make(chan struct{})
	defer func() {
		cancel()
		<-renewed // a renewal in flight would write the lease back after we drop it
		a.releaseLease(job, ".watch", token)
	}()
	go func() {
		defer close(renewed)
		for a.sleep(ctx, leaseBeat) {
			if !a.renewLease(job, ".watch", token, watchLease) {
				cancel() // another watcher took the lease over
				return
			}
		}
	}()

	var end jobEnd
	if readJSON(a.statePath(job, ".ended"), &end) != nil {
		var ended bool
		end, ended = a.follow(ctx, rec)
		if !ended {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return nil // cancelled by the agent: nothing to tell it
		}
		if err := writeJSON(a.statePath(job, ".ended"), end); err != nil {
			return fmt.Errorf("recording the end of %s: %w", job, err)
		}
	}
	return a.deliver(ctx, rec, end)
}

// follow follows the job to its end. It retries transport errors, and says
// the job is lost only when that is confirmed. ended is false when ctx ended
// or the agent cancelled the job.
func (a *App) follow(ctx context.Context, rec jobRecord) (end jobEnd, ended bool) {
	job := rec.Job
	w := *a
	tail := &lineTail{}
	w.Out, w.Err = tail, io.Discard
	w.Warn = func(string) {}
	w.Notify = nil
	w.Thread = rec.Thread
	w.KeepEnded = endedKeep
	start := jobStart(job)
	took := func() int64 { return int64(a.Now().Sub(start) / time.Second) }
	lost := func() (jobEnd, bool) {
		return jobEnd{Code: exitPaused, Lost: true, Took: took(), Tail: tail.lastLines(tailLines)}, true
	}
	if rec.Queued {
		out, ok := w.waitQueued(ctx, rec)
		switch {
		case !ok:
			return jobEnd{}, false
		case out.end != nil:
			return *out.end, true
		case !out.started.IsZero():
			start = out.started // took counts the run, not the wait
		}
	}
	var pauseDeadline time.Time
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		// Each follow prints the whole log from its start, so a follow after
		// a pause or a broken connection mustn't add to the last one's.
		tail.reset()
		res, err := w.Job(ctx, job, 0)
		if ctx.Err() != nil {
			return jobEnd{}, false
		}
		switch {
		case err == nil && res.State == stateDone:
			if w.wasCancelled(ctx, job) {
				a.markNotified(job, "cancel")
				return jobEnd{}, false
			}
			return jobEnd{Code: res.Code, Took: took(), Tail: tail.lastLines(tailLines), Digest: res.Digest}, true
		case err == nil && res.State == statePaused && rec.GPU:
			return lost() // a GPU box is stopped in My PC, not frozen
		case err == nil && res.State == statePaused:
			if won, _ := a.claimBy(job, ".paused", "watcher"); won {
				a.send(ctx, rec.Thread, pausedMessage(rec))
			}
			if pauseDeadline.IsZero() {
				pauseDeadline = a.Now().Add(mode.FrozenSwappedStop + 10*time.Minute)
			}
			if a.Now().After(pauseDeadline) && w.probe(ctx, rec) == probeLost {
				return lost()
			}
			a.sleep(ctx, 30*time.Second)
			continue
		}
		// An error: the follow broke. Look before calling it lost.
		switch w.probe(ctx, rec) {
		case probeLost:
			return lost()
		case probeRunning, probeDone:
			backoff = 5 * time.Second
		}
		a.sleep(ctx, backoff)
		backoff = min(backoff*2, 2*time.Minute)
	}
	return jobEnd{}, false
}

// What probe found out about a job whose follow broke.
type probeResult int

const (
	probeUnknown probeResult = iota // can't tell: the PC or network is out of reach
	probeRunning
	probeDone
	probeLost // confirmed: the box is gone or stopped, or the job's process is dead with no exit code
)

func (a *App) probe(ctx context.Context, rec jobRecord) probeResult {
	look, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	box, _, _ := strings.Cut(rec.Job, ".")
	insts, err := a.list(look)
	if err != nil {
		return probeUnknown
	}
	in := find(insts, box)
	switch {
	case in == nil, in.Status == "Stopped":
		return probeLost
	case in.Status != "Running":
		return probeUnknown // frozen: the pause path owns it
	}
	pg, err := a.LogPage(look, rec.Job, 0, 1)
	switch {
	case err != nil && strings.HasPrefix(err.Error(), "no job "):
		return probeLost
	case err != nil:
		return probeUnknown
	case pg.State == jobDone:
		return probeDone
	case pg.State == jobDied:
		return probeLost
	}
	return probeRunning
}

// deliver sends the end unless it already reached the agent. The .notified
// marker is made only after t3 accepted the push; a send under way holds the
// .sending lease so a restarted watcher doesn't send alongside it.
func (a *App) deliver(ctx context.Context, rec jobRecord, end jobEnd) error {
	if a.exists(rec.Job, ".notified") {
		return nil
	}
	token, err := a.acquireLease(rec.Job, ".sending", sendLease)
	if err != nil {
		if errors.Is(err, errLeaseHeld) {
			return nil
		}
		return err
	}
	defer a.releaseLease(rec.Job, ".sending", token)
	if a.exists(rec.Job, ".notified") || a.waitForHook(ctx, rec) {
		return nil // the agent has it, from box_job or mid-turn from the hook
	}
	if err := a.send(ctx, rec.Thread, endMessage(rec, end)); err != nil {
		return err // .ended stays, so box_* results keep offering it
	}
	a.markNotified(rec.Job, "watcher")
	return nil
}
