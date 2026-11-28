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

	"github.com/a-Fig/agent-box/internal/dash"
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
//	<job>.json       the record: thread, reason, command, worktree, qualified host locator
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
	// endedLines is how much of the tail a failure with no digest is pushed with.
	endedLines = 10
	// endedLinesAfterDigest is how many of the log's last lines follow a
	// failure digest; reasonChars is how much of the reason a push's first
	// line keeps.
	endedLinesAfterDigest = 3
	reasonChars           = 80
	endedKeep             = jobrun.EndedKeep
)

// Pusher sends a message into a T3 thread as its next turn.
type Pusher interface {
	Send(ctx context.Context, thread, message string) error
}

// t3CLI sends through the local T3 launcher with the message on stdin.
type t3CLI struct{ path, baseDir string }

func (c t3CLI) sendArgs(thread string) []string {
	args := []string{"thread", "send"}
	if c.baseDir != "" {
		args = append(args, "--base-dir", c.baseDir)
	}
	return append(args, thread, "-")
}

func (c t3CLI) Send(ctx context.Context, thread, message string) error {
	ctx, cancel := context.WithTimeout(ctx, sendTry)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.path, c.sendArgs(thread)...)
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
	Job       string               `json:"job"`
	Thread    string               `json:"thread"` // the session the result is for
	T3BaseDir string               `json:"t3BaseDir,omitempty"`
	Host      *jobLocator          `json:"host,omitempty"`
	Source    *dash.SourceRevision `json:"source,omitempty"`
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

type jobLocator struct {
	Name         string `json:"name,omitempty"`
	Remote       string `json:"remote"`
	Project      string `json:"project"`
	DashboardURL string `json:"dashboard_url,omitempty"`
	ControllerID string `json:"controller_id,omitempty"`
}

func locateJobHost(h fleetHost) *jobLocator {
	return &jobLocator{Name: h.Name, Remote: h.Remote, Project: h.Project, DashboardURL: h.DashboardURL, ControllerID: h.ControllerID}
}

func (rec jobRecord) noticeJob() string {
	if rec.Host == nil {
		return rec.Job
	}
	return rec.Job + onHost(rec.Host.Name)
}

func (rec jobRecord) watchHost(id jobID) (fleetHost, error) {
	if rec.Host == nil {
		return fleetHost{}, fmt.Errorf("no saved host for qualified job %s", id.ID)
	}
	host := fleetHost{Name: rec.Host.Name, Remote: rec.Host.Remote, Project: rec.Host.Project, DashboardURL: rec.Host.DashboardURL, ControllerID: rec.Host.ControllerID}
	tag, err := jobHostTag(host)
	if err != nil || (id.hostTag != "" && tag != id.hostTag) {
		return fleetHost{}, fmt.Errorf("saved host does not match job %s", id.ID)
	}
	if host.DashboardURL != "" && !validDashboardURL(host.DashboardURL) {
		return fleetHost{}, fmt.Errorf("invalid saved dashboard URL for job %s", id.ID)
	}
	if rec.Queued && id.hostTag != "" && host.Project != mode.Project {
		return fleetHost{}, fmt.Errorf("boxd cannot follow queued job %s in project %q", id.ID, host.Project)
	}
	return host, nil
}

func (a *App) watchTarget(rec jobRecord, id jobID) (*App, error) {
	if id.hostTag == "" && rec.Host == nil {
		return a, nil
	}
	host, err := rec.watchHost(id)
	if err != nil {
		return nil, err
	}
	return a.jobHostApp(host), nil
}

func (a *App) readWatchRecord(job string) (jobRecord, error) {
	var rec jobRecord
	if err := readJSON(a.statePath(job, ".json"), &rec); err != nil {
		return jobRecord{}, fmt.Errorf("no record of %s: %w", job, err)
	}
	if rec.Job != job {
		return jobRecord{}, fmt.Errorf("record does not match job %s", job)
	}
	return rec, nil
}

// jobEnd is <job>.ended: everything needed to send the end again.
type jobEnd struct {
	Code int  `json:"code"`
	Lost bool `json:"lost,omitempty"`
	// Dropped is a queued job that left the queue without running.
	Dropped    bool                 `json:"dropped,omitempty"`
	Took       int64                `json:"tookSeconds"`
	Tail       string               `json:"tail,omitempty"`
	Digest     string               `json:"digest,omitempty"`     // a test run's failure digest
	Completion string               `json:"completion,omitempty"` // observed along with the exit file
	Source     *dash.SourceRevision `json:"source,omitempty"`
	Host       string               `json:"host,omitempty"`
	Locator    *jobLocator          `json:"hostLocator,omitempty"` // identifies the target for bare job IDs
}

// Early provenance builds stored the locator in the host field.
func (e *jobEnd) UnmarshalJSON(data []byte) error {
	type plainEnd jobEnd
	fields := struct {
		*plainEnd
		Host json.RawMessage `json:"host"`
	}{plainEnd: (*plainEnd)(e)}
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if len(fields.Host) == 0 {
		return nil
	}
	if fields.Host[0] == '{' && e.Locator == nil {
		return json.Unmarshal(fields.Host, &e.Locator)
	}
	return json.Unmarshal(fields.Host, &e.Host)
}

const (
	completionExited    = "exited"
	completionCancelled = "cancelled"
)

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
	if err != nil || r.State != stateDone || a.StateDir == "" {
		return
	}
	rec, err := a.readWatchRecord(job)
	if err == nil && rec.Thread == a.thread() && rec.inProfile(a.t3BaseDir()) {
		a.markNotified(job, who)
	}
}

// markNotified says the end reached the agent. A failure to write it only
// risks a duplicate notice, so it is a warning.
func (a *App) markNotified(job, who string) {
	if a.StateDir == "" {
		return
	}
	if who == "cancel" {
		if tag, err := jobHostTag(fleetHost{Remote: a.Remote, Project: a.Project}); err == nil {
			who += "\n" + tag
		}
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

func (a *App) acquireLease(ctx context.Context, job, ext string, ttl time.Duration) (string, error) {
	lock, err := a.lockLeases(ctx)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if a.leaseHeld(job, ext) {
		return "", errLeaseHeld
	}
	if err := os.Remove(a.statePath(job, ext)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	token := strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	won, err := a.claimBy(job, ext, a.leaseText(token, ttl))
	if err != nil {
		return "", err
	}
	if !won {
		return "", errLeaseHeld
	}
	return token, nil
}

// renewLease extends a lease this token holds; false means it was lost.
func (a *App) renewLease(job, ext, token string, ttl time.Duration) bool {
	lock, err := a.lockLeasesBriefly()
	if err != nil {
		return false
	}
	defer lock.Close()
	if t, _, ok := a.readLease(job, ext); !ok || t != token {
		return false
	}
	return writeFile(a.statePath(job, ext), []byte(a.leaseText(token, ttl))) == nil
}

func (a *App) releaseLease(job, ext, token string) {
	lock, err := a.lockLeasesBriefly()
	if err != nil {
		return
	}
	defer lock.Close()
	if t, _, ok := a.readLease(job, ext); ok && t == token {
		os.Remove(a.statePath(job, ext))
	}
}

// startWatch records a running or queued job and starts its watcher. It
// says whether the result will be pushed.
func (a *App) startWatch(info jobMeta, job string, gpu, throwaway, queued bool) bool {
	return a.startWatchRecord(info, job, nil, gpu, throwaway, queued)
}

func (a *App) startWatchOnHost(info jobMeta, job string, host fleetHost, gpu, throwaway, queued bool) bool {
	locator := locateJobHost(host)
	return a.startWatchRecord(info, job, locator, gpu, throwaway, queued)
}

func (a *App) startWatchRecord(info jobMeta, job string, host *jobLocator, gpu, throwaway, queued bool) bool {
	if !a.canPush() {
		return false
	}
	id, err := parseJobID(job)
	if err == nil && id.hostTag == "" && host != nil {
		err = fmt.Errorf("saved fleet host requires a qualified job ID")
	}
	baseDir := a.t3BaseDir()
	if err == nil && id.hostTag == "" {
		host = &jobLocator{Remote: a.Remote, Project: a.Project, DashboardURL: a.dashboardURL}
	}
	rec := jobRecord{Job: job, Host: host, Source: info.Source, Thread: a.thread(), T3BaseDir: baseDir, Session: a.getenv("CLAUDE_CODE_SESSION_ID"), Reason: info.Reason, Command: info.Command, Worktree: info.Agent.Worktree, GPU: gpu, New: throwaway, Queued: queued}
	if err == nil && (id.hostTag != "" || rec.Host != nil) {
		_, err = rec.watchHost(id)
	}
	if err != nil {
		a.warn("box: warning: no push for " + job + ": " + err.Error())
		return false
	}
	if err := os.MkdirAll(a.StateDir, 0o755); err != nil {
		a.warn("box: warning: no push for " + job + ": " + err.Error())
		return false
	}
	if err := writeJSON(a.statePath(job, ".json"), rec); err != nil {
		a.warn("box: warning: no push for " + job + ": " + err.Error())
		return false
	}
	err = a.Spawn(a.StateDir, job)
	if errors.Is(err, errWatcherInJob) {
		recovery := "Call box_job " + job + " to recover the result after a restart."
		if rec.T3BaseDir != "" {
			recovery = "Box retries when this T3 profile starts Box MCP again. You can also call box_job " + job + "."
		}
		a.warn("box: notice: this watcher stops with Box MCP. " + recovery)
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
	baseDir := a.t3BaseDir()
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
		id, err := parseJobID(job)
		if err != nil {
			continue
		}
		rec, err := a.readWatchRecord(job)
		if err != nil || !rec.inProfile(baseDir) {
			continue
		}
		notified := a.exists(job, ".notified")
		if info, err := e.Info(); err == nil && a.Now().Sub(info.ModTime()) > recordKeep && (rec.T3BaseDir == "" || notified) {
			a.pruneWatchRecord(job)
			continue
		}
		if rec.Thread != thread || job == skip || notified {
			continue
		}
		target, err := a.watchTarget(rec, id)
		if err != nil {
			a.warn("box: warning: cannot restart the watcher: " + err.Error())
			continue
		}
		if end, ok := target.readJobEnd(id, ".ended", rec.Host != nil); ok {
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

func (a *App) pruneWatchRecord(job string) {
	lock, err := a.lockLeasesBriefly()
	if err != nil {
		return
	}
	defer lock.Close()
	if a.leaseHeld(job, ".watch") || a.leaseHeld(job, ".sending") {
		return
	}
	for _, ext := range []string{".json", ".watch", ".ended", ".sending", ".notified", ".paused", ".nudged", ".watchlog", ".result"} {
		os.Remove(a.statePath(job, ext))
	}
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
		job, ok := strings.CutSuffix(e.Name(), ".result")
		if !ok {
			continue
		}
		if info, err := e.Info(); err == nil && a.Now().Sub(info.ModTime()) > recordKeep {
			if rec, err := a.readWatchRecord(job); err == nil && rec.T3BaseDir != "" && !a.exists(job, ".notified") {
				continue
			}
			os.Remove(filepath.Join(a.StateDir, e.Name()))
		}
	}
}

// recordedEnd is a job's recorded end: the exit box read, else the end the
// watcher recorded if it ran to an exit code.
func (a *App) recordedEnd(job string) (jobEnd, bool) {
	if a.StateDir == "" {
		return jobEnd{}, false
	}
	id, err := parseJobID(job)
	if err != nil {
		return jobEnd{}, false
	}
	for _, ext := range []string{".result", ".ended"} {
		if end, ok := a.readJobEnd(id, ext, false); ok && !end.Lost && !end.Dropped {
			return end, true
		}
	}
	return jobEnd{}, false
}

func (a *App) readJobEnd(id jobID, ext string, scopedWatch bool) (end jobEnd, ok bool) {
	if readJSON(a.statePath(id.ID, ext), &end) != nil {
		return jobEnd{}, false
	}
	if end.Locator != nil || id.hostTag != "" || a.Project != mode.Project || scopedWatch {
		return end, a.jobSourceMatchesHost(id, end.Locator)
	}
	// Only a legacy bare job in agents can use a record without a locator.
	rec, err := a.readWatchRecord(id.ID)
	return end, err != nil || rec.Host == nil
}

func (a *App) jobSourceMatchesHost(id jobID, host *jobLocator) bool {
	if host == nil {
		return id.hostTag != ""
	}
	if id.hostTag != "" {
		tag, err := jobHostTag(fleetHost{Remote: host.Remote, Project: host.Project})
		return err == nil && tag == id.hostTag
	}
	return host.Remote == a.Remote && host.Project == a.Project
}

func (a *App) savedJobSource(id jobID) *dash.SourceRevision {
	if a.StateDir == "" {
		return nil
	}
	if end, ok := a.recordedEnd(id.ID); ok && end.Source.Valid() && a.jobSourceMatchesHost(id, end.Locator) {
		return end.Source
	}
	if rec, err := a.readWatchRecord(id.ID); err == nil && rec.Source.Valid() && a.jobSourceMatchesHost(id, rec.Host) {
		return rec.Source
	}
	return nil
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

// endedMessage is the push for a job that ran to an exit code. It is short,
// because the agent may read the result with box_job too (#182): a clean
// pass is one line; a test failure is its digest (#57), which names what the
// agent reads next; any other failure is its last endedLines lines. box_job
// always has the full result, and the message says so.
func endedMessage(r jobRecord, e jobEnd) string {
	head := fmt.Sprintf("box: job %s ended, exit %d, after %s", r.noticeJob(), e.Code, elapsed(time.Duration(e.Took)*time.Second))
	if why := shortReason(r.Reason); why != "" {
		head += " (" + why + ")" // which run this is, after compaction or among parallel runs
	}
	where := fmt.Sprintf("Full result: box_job id %s. Artifacts: box_pull, before this repo's next run.", r.Job)
	if r.New {
		where = fmt.Sprintf("Full result: box_job id %s. Artifacts: box_pull; box and log kept up to %d minutes (a queued job or another launch that needs the room takes them sooner).", r.Job, int(endedKeep.Minutes()))
	}
	digest := strings.TrimRight(mask.Mask(e.Digest), "\n")
	failed := testsFailed(digest)
	// A passing digest goes on the first line. It never stands alone: a digest
	// can miss a failure (a report cut short, a second runner with its own
	// reporters, boxd's copy of the end with none), so the log's last lines
	// always follow.
	msg := head + ". " + fromBox + "\n"
	if e.Code == 0 && !failed && testsPassed(digest) {
		msg = head + ": " + strings.Join(strings.Split(digest, "\n"), "; ") + ". " + fromBox + "\n"
	} else if digest != "" {
		msg += digest + "\n"
	}
	// An exit 0 and a failure digest get only the log's last few lines: they
	// carry what came last, like a cancel from the dashboard, a later step's
	// error or a test summary. Any other failure gets more.
	n := endedLines
	if failed || e.Code == 0 {
		n = endedLinesAfterDigest
	}
	// Trailing blank lines (CRLF ones too) would take the place of the last real ones.
	if tail := lastLines(strings.TrimRight(mask.Mask(e.Tail), " \t\r\n"), n); tail != "" {
		msg += tail + "\n"
	}
	if e.Code == exitPaused {
		msg += "(The command itself exited 75: this is its result, not a refusal.)\n"
	}
	return msg + where
}

// shortReason is the agent's reason for a run, masked, on one line and cut to
// reasonChars, to say which run a push is about.
func shortReason(reason string) string {
	s := strings.Join(strings.Fields(mask.Mask(reason)), " ")
	if r := []rune(s); len(r) > reasonChars {
		s = strings.TrimRight(string(r[:reasonChars-1]), " ") + "…"
	}
	return s
}

// testsPassed says whether a digest reports passing tests and no failures.
func testsPassed(digest string) bool {
	if testsFailed(digest) {
		return false
	}
	for _, l := range strings.Split(digest, "\n") {
		if strings.HasPrefix(l, "PASS:") {
			return true
		}
	}
	return false
}

// testsFailed says whether a digest reports failed tests or packages.
func testsFailed(digest string) bool {
	for _, l := range strings.Split(digest, "\n") {
		if strings.HasPrefix(l, "FAIL:") {
			return true
		}
	}
	return false
}

func pausedMessage(r jobRecord) string {
	// T3 may deliver this long after it was sent, when the job has resumed
	// (#96); the end push follows either way.
	return fmt.Sprintf("box: job %s paused: the PC's owner took the PC back. It resumes if they switch back within %s, else it is lost; its end is pushed either way. Once it resumes, box_cancel this job if you no longer need it. %s\n%s",
		r.noticeJob(), frozenWindow, fromBox, r.context())
}

func lostMessage(r jobRecord, e jobEnd) string {
	msg := fmt.Sprintf("box: job %s lost, exit 75: it stopped before it finished. %s\n%s", r.noticeJob(), fromBox, r.context())
	if e.Tail != "" {
		msg += "\n" + mask.Mask(e.Tail)
	}
	return msg
}

// endMessage is what the agent is told of a job's end.
func endMessage(rec jobRecord, end jobEnd) string {
	var msg string
	switch {
	case end.Dropped:
		msg = droppedMessage(rec)
	case end.Lost:
		msg = lostMessage(rec, end)
	default:
		msg = endedMessage(rec, end)
	}
	return msg + "\n" + sourceLine(end.Source)
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
	if err := ctx.Err(); err != nil {
		return err
	}
	id, err := parseJobID(job)
	if err != nil {
		return err
	}
	if a.StateDir == "" || a.Push == nil {
		return errors.New("no state directory or T3 CLI to push with")
	}
	rec, err := a.readWatchRecord(job)
	if err != nil {
		return err
	}
	if rec.T3BaseDir != "" {
		if !filepath.IsAbs(rec.T3BaseDir) {
			return fmt.Errorf("invalid saved T3 base directory for job %s", job)
		}
		if pusher, ok := a.Push.(t3CLI); ok {
			scoped := *a
			pusher.baseDir = rec.T3BaseDir
			scoped.Push = pusher
			a = &scoped
		}
	}
	target, err := a.watchTarget(rec, id)
	if err != nil {
		return err
	}
	if a.exists(job, ".notified") {
		return nil
	}
	token, err := a.acquireLease(ctx, job, ".watch", watchLease)
	if err != nil {
		if errors.Is(err, errLeaseHeld) {
			return nil
		}
		return err
	}
	if err := ctx.Err(); err != nil {
		a.releaseLease(job, ".watch", token)
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

	end, ok := target.readJobEnd(id, ".ended", rec.Host != nil)
	if !ok {
		if !target.jobSourceMatchesHost(id, rec.Host) {
			rec.Source = nil
		}
		var ended bool
		end, ended = target.follow(ctx, rec, id)
		if !ended {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return nil // cancelled by the agent: nothing to tell it
		}
		if end.Source == nil {
			end.Source = rec.Source
		}
		end.Host = target.jobHostName
		end.Locator = &jobLocator{Remote: target.Remote, Project: target.Project}
		if err := writeJSON(a.statePath(job, ".ended"), end); err != nil {
			return fmt.Errorf("recording the end of %s: %w", job, err)
		}
	}
	return a.deliver(ctx, rec, end)
}

// follow follows the job to its end. It retries transport errors, and says
// the job is lost only when that is confirmed. ended is false when ctx ended
// or the agent cancelled the job.
func (a *App) follow(ctx context.Context, rec jobRecord, id jobID) (end jobEnd, ended bool) {
	job := rec.Job
	if saved, ok := a.readJobEnd(id, ".result", rec.Host != nil); ok && !saved.Lost && !saved.Dropped {
		switch saved.Completion {
		case completionExited:
			return saved, true
		case completionCancelled:
			a.markNotified(job, "cancel")
			return jobEnd{}, false
		}
	}
	w := *a
	tail := &lineTail{}
	w.Out, w.Err = tail, io.Discard
	w.Warn = func(string) {}
	w.Notify = nil
	w.Thread = rec.Thread
	w.KeepEnded = endedKeep
	start := jobStart(id.Bare)
	source := rec.Source
	took := func() int64 { return int64(a.Now().Sub(start) / time.Second) }
	lost := func() (jobEnd, bool) {
		return jobEnd{Code: exitPaused, Lost: true, Took: took(), Tail: tail.lastLines(tailLines), Source: source}, true
	}
	if rec.Queued {
		out, ok := w.waitQueuedJob(ctx, rec, id, false, true)
		source = out.source
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
		res, err := w.job(ctx, id, 0, source)
		if res.Source != nil {
			source = res.Source
		}
		if ctx.Err() != nil {
			return jobEnd{}, false
		}
		switch {
		case err == nil && res.State == stateDone:
			saved, _ := w.recordedEnd(job)
			if saved.Completion == completionCancelled || (saved.Completion != completionExited && w.wasCancelled(ctx, id.Bare)) {
				a.markNotified(job, "cancel")
				return jobEnd{}, false
			}
			return jobEnd{Code: res.Code, Took: took(), Tail: tail.lastLines(tailLines), Digest: res.Digest, Source: source}, true
		case err == nil && res.State == statePaused && rec.GPU:
			return lost() // a GPU box is stopped in My PC, not frozen
		case err == nil && res.State == statePaused:
			if won, _ := a.claimBy(job, ".paused", "watcher"); won {
				a.send(ctx, rec.Thread, pausedMessage(rec))
			}
			if pauseDeadline.IsZero() {
				pauseDeadline = a.Now().Add(mode.FrozenSwappedStop + 10*time.Minute)
			}
			if a.Now().After(pauseDeadline) && w.probe(ctx, id) == probeLost {
				return lost()
			}
			a.sleep(ctx, 30*time.Second)
			continue
		}
		// An error: the follow broke. Look before calling it lost.
		switch w.probe(ctx, id) {
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

func (a *App) probe(ctx context.Context, id jobID) probeResult {
	look, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	insts, err := a.list(look)
	if err != nil {
		return probeUnknown
	}
	in := find(insts, id.Box)
	switch {
	case in == nil, in.Status == "Stopped":
		return probeLost
	case in.Status != "Running":
		return probeUnknown // frozen: the pause path owns it
	}
	pg, err := a.LogPage(look, id.Bare, 0, 1)
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
	token, err := a.acquireLease(ctx, rec.Job, ".sending", sendLease)
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
