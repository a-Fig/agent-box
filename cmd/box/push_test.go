package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type sentMsg struct{ thread, msg string }

// fakePusher stands in for the t3 CLI.
type fakePusher struct {
	mu   sync.Mutex
	sent []sentMsg
	err  error
}

func (p *fakePusher) Send(ctx context.Context, thread, msg string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.sent = append(p.sent, sentMsg{thread, msg})
	return nil
}

func (p *fakePusher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sent)
}

func (p *fakePusher) setErr(err error) {
	p.mu.Lock()
	p.err = err
	p.mu.Unlock()
}

type pushEnv struct {
	f      *fakePC
	a      *App
	push   *fakePusher
	spawns *[]string
	repo   string
}

// newPushEnv is an App in a T3 thread with a state directory, a fake t3 and a
// fake watcher launcher.
func newPushEnv(t *testing.T, f *fakePC, thread string) pushEnv {
	t.Helper()
	a, _ := testApp(f)
	a.StateDir = t.TempDir()
	a.Push = &fakePusher{}
	var spawns []string
	a.Spawn = func(dir, job string) error { spawns = append(spawns, job); return nil }
	a.InlineWait = 20 * time.Millisecond
	a.WatchPoll = time.Millisecond
	a.Getenv = func(k string) string {
		if k == "T3CODE_THREAD_ID" {
			return thread
		}
		return ""
	}
	return pushEnv{f, a, a.Push.(*fakePusher), &spawns, newRepo(t)}
}

// call runs one MCP tool and returns its text and structuredContent.
func (e pushEnv) call(t *testing.T, name string, args map[string]any) (string, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
	res := callTool(context.Background(), e.a, b, nil)
	text := res["content"].([]map[string]any)[0]["text"].(string)
	var st map[string]any
	if sc, ok := res["structuredContent"]; ok {
		raw, _ := json.Marshal(sc)
		json.Unmarshal(raw, &st)
	}
	return text, st
}

func (e pushEnv) runArgs(extra map[string]any, command string) map[string]any {
	args := map[string]any{"dir": e.repo, "command": command, "reason": "Full suite for the push change; too heavy for the laptop."}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

func (e pushEnv) run(t *testing.T, command string) (string, map[string]any) {
	return e.call(t, "box_run", e.runArgs(nil, command))
}

// finish makes the fake's follow end with the given exit code and log.
func (f *fakePC) finish(code, log string) {
	f.mu.Lock()
	f.block, f.log, f.exit = false, log, code+"\n"
	f.mu.Unlock()
}

var pullArgs = map[string]any{"from": "x", "remotePath": "y", "localDir": "relative"}

func TestRunInlineResultWithinWindow(t *testing.T) {
	e := newPushEnv(t, &fakePC{t: t, log: "ok\n", exit: "0\n"}, "thread-1")
	text, st := e.run(t, "go test ./...")
	if st["state"] != "done" || st["exitCode"] != 0.0 || st["push"] != nil {
		t.Fatalf("structured = %v\n%s", st, text)
	}
	if len(*e.spawns) != 0 {
		t.Errorf("spawned a watcher for a finished run: %v", *e.spawns)
	}
	// Only the recorded end, for a box_job after the box has gone.
	if ents, _ := os.ReadDir(e.a.StateDir); len(ents) != 1 || !strings.HasSuffix(ents[0].Name(), ".result") {
		t.Errorf("state files for an inline result: %v", ents)
	}
}

// A job whose exit was read keeps its result after its box goes: box_job
// gives the recorded exit code and tail, never "lost".
func TestJobAfterBoxGoneGivesRecordedEnd(t *testing.T) {
	e := newPushEnv(t, &fakePC{t: t, log: "hello\n", exit: "3\n"}, "thread-1")
	_, st := e.run(t, "go test ./...")
	job := st["job"].(string)
	if st["exitCode"] != 3.0 {
		t.Fatalf("run = %v", st)
	}
	e.f.mu.Lock()
	e.f.insts, e.f.log, e.f.exit = nil, "", ""
	e.f.mu.Unlock()
	text, st := e.call(t, "box_job", map[string]any{"id": job})
	if st["state"] != "done" || st["exitCode"] != 3.0 || !strings.Contains(st["tail"].(string), "hello") || !strings.Contains(text, "recorded when it ended") {
		t.Errorf("box_job after the box went = %v\n%s", st, text)
	}
	if _, st := e.call(t, "box_job", map[string]any{"id": job, "offset": 0}); st["exitCode"] != 3.0 || !strings.Contains(st["tail"].(string), "hello") {
		t.Errorf("box_job page after the box went = %v", st)
	}
	if text, _ := e.call(t, "box_job", map[string]any{"id": job, "offset": -1}); !strings.Contains(text, "can't be negative") {
		t.Errorf("a bad offset after the box went = %q", text)
	}
	// A lost guess from a watcher that lost the race doesn't hide the real end.
	writeJSON(e.a.statePath(job, ".ended"), jobEnd{Code: exitPaused, Lost: true})
	if end, ok := e.a.recordedEnd(job); !ok || end.Code != 3 {
		t.Errorf("recorded end under a lost .ended = %+v, %v", end, ok)
	}
	if _, ok := e.a.recordedEnd("../" + job); ok {
		t.Error("recordedEnd read a path outside the state directory")
	}
}

func TestRunRunningPushesWhenThreadKnown(t *testing.T) {
	e := newPushEnv(t, &fakePC{t: t, block: true}, "thread-1")
	text, st := e.run(t, "sleep 600")
	if st["state"] != "running" || st["push"] != true {
		t.Fatalf("structured = %v\n%s", st, text)
	}
	job := st["job"].(string)
	if len(*e.spawns) != 1 || (*e.spawns)[0] != job {
		t.Fatalf("spawns = %v, want [%s]", *e.spawns, job)
	}
	var rec jobRecord
	if err := readJSON(e.a.statePath(job, ".json"), &rec); err != nil || rec.Thread != "thread-1" || rec.Command != "sleep 600" || !strings.Contains(rec.Reason, "push change") || rec.Worktree != e.repo {
		t.Errorf("record = %+v, %v", rec, err)
	}
	if !strings.Contains(text, "running") || !strings.Contains(text, "push: true") {
		t.Errorf("text = %q", text)
	}
}

func TestRunRunningWithoutThreadHasNoPush(t *testing.T) {
	e := newPushEnv(t, &fakePC{t: t, block: true}, "")
	_, st := e.run(t, "sleep 600")
	if st["state"] != "running" || st["push"] != false || len(*e.spawns) != 0 {
		t.Errorf("structured = %v, spawns %v", st, *e.spawns)
	}
}

func TestRunPushFalseSkipsTheWatcher(t *testing.T) {
	e := newPushEnv(t, &fakePC{t: t, block: true}, "thread-1")
	_, st := e.call(t, "box_run", e.runArgs(map[string]any{"push": false}, "sleep 600"))
	job := st["job"].(string)
	if st["state"] != "running" || st["push"] != false || len(*e.spawns) != 0 || e.a.exists(job, ".json") {
		t.Errorf("structured = %v, spawns %v", st, *e.spawns)
	}
	if text, _ := e.call(t, "box_pull", pullArgs); strings.Contains(text, "finished since") {
		t.Errorf("a subagent's job showed up in the safety net: %q", text)
	}
}

// A watcher that had to start inside the client's job object still pushes,
// and the result says how long the push lasts.
func TestRunWatcherInJobPushesWithNotice(t *testing.T) {
	e := newPushEnv(t, &fakePC{t: t, block: true}, "thread-1")
	e.a.Spawn = func(dir, job string) error { return errWatcherInJob }
	text, st := e.run(t, "sleep 600")
	if st["push"] != true || !strings.Contains(text, "this push lasts only as long as box's MCP server") || !e.a.exists(st["job"].(string), ".json") {
		t.Errorf("structured = %v\n%s", st, text)
	}
}

func TestRunSpawnFailureReportsNoPush(t *testing.T) {
	e := newPushEnv(t, &fakePC{t: t, block: true}, "thread-1")
	e.a.Spawn = func(dir, job string) error { return errors.New("access denied: no breakaway from the job object") }
	text, st := e.run(t, "sleep 600")
	job := st["job"].(string)
	if st["push"] != false || e.a.exists(job, ".json") {
		t.Errorf("structured = %v\n%s", st, text)
	}
}

func TestRunRefusedHasNoPush(t *testing.T) {
	e := newPushEnv(t, &fakePC{t: t, mode: "mypc"}, "thread-1")
	_, st := e.run(t, "true")
	if st["state"] != "refused" || st["exitCode"] != 75.0 || st["push"] != nil || len(*e.spawns) != 0 {
		t.Errorf("structured = %v, spawns %v", st, *e.spawns)
	}
}

// A session holding an older tool list still sends these (#34): the run goes
// ahead, with a notice.
func TestRunIgnoresObsoleteParameters(t *testing.T) {
	e := newPushEnv(t, &fakePC{t: t}, "thread-1")
	for _, extra := range []map[string]any{{"box": "mine"}, {"wait": "10m"}} {
		text, st := e.call(t, "box_run", e.runArgs(extra, "true"))
		if !strings.Contains(text, "no longer takes") || !strings.Contains(text, "ignored") || st["job"] == nil {
			t.Errorf("%v = %q", extra, text)
		}
	}
}

func TestWatchPushesTheEnd(t *testing.T) {
	f := &fakePC{t: t, block: true}
	e := newPushEnv(t, f, "thread-1")
	var lines []string
	for i := 1; i <= 30; i++ {
		lines = append(lines, "FAIL line "+string(rune('a'+i%26))+strings.Repeat("x", i))
	}
	_, st := e.call(t, "box_run", e.runArgs(nil, "GITHUB_TOKEN=ghp_abcdef123456 go test ./..."))
	job := st["job"].(string)
	f.finish("1", strings.Join(lines, "\n")+"\n")
	if err := e.a.Watch(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if e.push.count() != 1 {
		t.Fatalf("pushed %d messages", e.push.count())
	}
	got := e.push.sent[0]
	if got.thread != "thread-1" {
		t.Errorf("thread = %q", got.thread)
	}
	for _, want := range []string{"box: job " + job + " ended, exit 1, after ", "From box, not your user.", "Full suite for the push change", "GITHUB_TOKEN=*** go test ./...  in  " + e.repo, lines[29], "Full log: box_job id " + job + " offset 0. Artifacts: box_pull, before this repo's next run."} {
		if !strings.Contains(got.msg, want) {
			t.Errorf("message lacks %q:\n%s", want, got.msg)
		}
	}
	if strings.Contains(got.msg, "ghp_abcdef123456") || strings.Contains(got.msg, lines[9]) || !strings.Contains(got.msg, lines[10]) {
		t.Errorf("secret kept, or the tail isn't the last 20 lines:\n%s", got.msg)
	}
	if !e.a.exists(job, ".notified") || !e.a.exists(job, ".ended") {
		t.Error("watcher left no .notified or .ended")
	}
	// A second watcher finds it delivered.
	if err := e.a.Watch(context.Background(), job); err != nil || e.push.count() != 1 {
		t.Errorf("second watch pushed again: %v, %d", err, e.push.count())
	}
}

func TestExitCode75IsSaidToBeTheCommands(t *testing.T) {
	r := jobRecord{Job: "my-repo.20261123-120000-0001", Reason: "Why.", Command: "make", Worktree: "w"}
	if got := endedMessage(r, jobEnd{Code: 75}); !strings.Contains(got, "exited 75") || !strings.Contains(got, "not a refusal") {
		t.Errorf("message = %q", got)
	}
}

func TestBoxJobAlwaysReturnsTheFullResult(t *testing.T) {
	f := &fakePC{t: t, block: true}
	e := newPushEnv(t, f, "thread-1")
	_, st := e.run(t, "make test")
	job := st["job"].(string)
	f.finish("0", "output tail\n")
	// The watcher pushed first; box_job still returns everything.
	if err := e.a.Watch(context.Background(), job); err != nil || e.push.count() != 1 {
		t.Fatalf("watch: %v, %d pushes", err, e.push.count())
	}
	text, got := e.call(t, "box_job", map[string]any{"id": job})
	if got["state"] != "done" || got["pushed"] != nil || !strings.Contains(text, "output tail") {
		t.Errorf("box_job after the push = %v\n%s", got, text)
	}
}

func TestBoxJobFirstMakesTheWatcherQuiet(t *testing.T) {
	f := &fakePC{t: t, block: true}
	e := newPushEnv(t, f, "thread-1")
	_, st := e.run(t, "sleep 60")
	job := st["job"].(string)
	f.finish("0", "tail\n")
	text, got := e.call(t, "box_job", map[string]any{"id": job})
	if got["state"] != "done" || !strings.Contains(text, "tail") || !e.a.exists(job, ".notified") {
		t.Fatalf("box_job = %v\n%s", got, text)
	}
	if err := e.a.Watch(context.Background(), job); err != nil || e.push.count() != 0 {
		t.Errorf("watch pushed after box_job returned the result: %v, %d", err, e.push.count())
	}
}

// The CLI's box job, which agents run from a shell, counts as the agent
// reading the end too: no push follows it (#76).
func TestCLIBoxJobMakesTheWatcherQuiet(t *testing.T) {
	f := &fakePC{t: t, block: true}
	e := newPushEnv(t, f, "thread-1")
	_, st := e.run(t, "sleep 60")
	job := st["job"].(string)
	f.finish("0", "tail\n")
	if c := run(context.Background(), e.a, []string{"job", job}, nil); c != 0 || !e.a.exists(job, ".notified") {
		t.Fatalf("box job: exit %d, notified %v", c, e.a.exists(job, ".notified"))
	}
	if err := e.a.Watch(context.Background(), job); err != nil || e.push.count() != 0 {
		t.Errorf("watch pushed after box job printed the result: %v, %d", err, e.push.count())
	}

	// Read from a terminal outside the session, it is still pushed.
	_, st = e.run(t, "sleep 60")
	job = st["job"].(string)
	f.finish("0", "tail\n")
	getenv := e.a.Getenv
	e.a.Getenv = func(k string) string {
		if k == "T3CODE_THREAD_ID" {
			return ""
		}
		return getenv(k)
	}
	if c := run(context.Background(), e.a, []string{"job", job}, nil); c != 0 || e.a.exists(job, ".notified") {
		t.Fatalf("box job outside the session: exit %d, notified %v", c, e.a.exists(job, ".notified"))
	}
}

// Whatever the interleaving, the end reaches the agent at least once.
func TestBoxJobAndWatcherRaceNeverLoseTheEnd(t *testing.T) {
	f := &fakePC{t: t, block: true}
	e := newPushEnv(t, f, "thread-1")
	_, st := e.run(t, "make test")
	job := st["job"].(string)
	f.finish("0", "tail line\n")
	var wg sync.WaitGroup
	var text string
	wg.Add(2)
	go func() { defer wg.Done(); e.a.Watch(context.Background(), job) }()
	go func() { defer wg.Done(); text, _ = e.call(t, "box_job", map[string]any{"id": job}) }()
	wg.Wait()
	if e.push.count() == 0 && !strings.Contains(text, "tail line") {
		t.Errorf("the end was lost: no push, and box_job said %q", text)
	}
}

func TestCancelledJobIsNotPushed(t *testing.T) {
	f := &fakePC{t: t, block: true, script: "cancelled 130\n"}
	e := newPushEnv(t, f, "thread-1")
	_, st := e.run(t, "sleep 600")
	job := st["job"].(string)
	if _, got := e.call(t, "box_cancel", map[string]any{"id": job}); got["cancelled"] != true {
		t.Fatalf("box_cancel = %v", got)
	}
	f.finish("130", "")
	if err := e.a.Watch(context.Background(), job); err != nil || e.push.count() != 0 {
		t.Errorf("watch pushed a cancelled job: %v, %d", err, e.push.count())
	}
}

func TestJobCancelledElsewhereIsNotPushed(t *testing.T) {
	f := &fakePC{t: t, block: true}
	e := newPushEnv(t, f, "thread-1")
	_, st := e.run(t, "sleep 600")
	job := st["job"].(string)
	f.finish("130", "")
	f.cancelled = true // the box has the .cancelled mark box cancel writes
	if err := e.a.Watch(context.Background(), job); err != nil || e.push.count() != 0 || e.a.exists(job, ".ended") {
		t.Errorf("watch pushed a job cancelled on the box: %v, %d", err, e.push.count())
	}
	if text, _ := e.call(t, "box_pull", pullArgs); strings.Contains(text, "finished since") {
		t.Errorf("safety net offered a cancelled job: %q", text)
	}
}

func TestCancelScriptMarksTheJobBeforeItsExitFile(t *testing.T) {
	s := cancelScript("my-repo.20261123-120000-0001")
	m, x := strings.Index(s, `"$j.cancelled"`), strings.Index(s, `ln "$j.cancel.$$"`)
	if m < 0 || x < 0 || m > x {
		t.Errorf("cancelScript marks .cancelled at %d, links the exit file at %d", m, x)
	}
}

func TestSafetyNetKeepsOfferingUntilNotified(t *testing.T) {
	f := &fakePC{t: t, block: true}
	e := newPushEnv(t, f, "thread-1")
	_, st := e.run(t, "make test")
	job := st["job"].(string)
	// Another session's job isn't mentioned.
	other := jobRecord{Job: "other-box.20261123-120000-0009", Thread: "thread-2"}
	writeJSON(e.a.statePath(other.Job, ".json"), other)
	writeJSON(e.a.statePath(other.Job, ".ended"), jobEnd{Code: 9})

	f.finish("2", "")
	e.push.setErr(errors.New("t3 is not running"))
	if err := e.a.Watch(context.Background(), job); err == nil {
		t.Fatal("Watch hid a failed send")
	}
	for i := 0; i < 2; i++ { // it is offered again and again
		text, _ := e.call(t, "box_pull", pullArgs)
		if !strings.HasPrefix(text, "finished since you last looked: "+job+" exit 2\n") || strings.Contains(text, "other-box") {
			t.Errorf("result %d = %q", i, text)
		}
	}
	// The send works again: a restarted watcher retries it from .ended.
	e.push.setErr(nil)
	if err := e.a.Watch(context.Background(), job); err != nil || e.push.count() != 1 {
		t.Fatalf("retry: %v, %d pushes", err, e.push.count())
	}
	if text, _ := e.call(t, "box_pull", pullArgs); strings.Contains(text, "finished since") {
		t.Errorf("still offered after the push: %q", text)
	}
}

func TestSendingLeaseKeepsRestartsFromSendingAlongside(t *testing.T) {
	f := &fakePC{t: t, block: true}
	e := newPushEnv(t, f, "thread-1")
	_, st := e.run(t, "make")
	job := st["job"].(string)
	f.finish("0", "")
	if _, err := e.a.acquireLease(job, ".sending", sendLease); err != nil { // another watcher is mid-send
		t.Fatal(err)
	}
	if err := e.a.Watch(context.Background(), job); err != nil || e.push.count() != 0 {
		t.Fatalf("sent alongside a live send: %v, %d", err, e.push.count())
	}
	// Its lease runs out: the send is retried.
	now := e.a.Now()
	e.a.Now = func() time.Time { return now.Add(sendLease + time.Second) }
	if err := e.a.Watch(context.Background(), job); err != nil || e.push.count() != 1 {
		t.Errorf("no retry after the lease expired: %v, %d", err, e.push.count())
	}
}

func TestSafetyNetRestartsLostWatchers(t *testing.T) {
	e := newPushEnv(t, &fakePC{t: t, block: true}, "thread-1")
	_, st := e.run(t, "sleep 600")
	job := st["job"].(string)
	if len(*e.spawns) != 1 {
		t.Fatalf("spawns after box_run = %v", *e.spawns)
	}
	// No lease: the watcher died with a sleep or reboot.
	e.call(t, "box_pull", pullArgs)
	if len(*e.spawns) != 2 || (*e.spawns)[1] != job {
		t.Fatalf("spawns after a box_* call = %v", *e.spawns)
	}
	// A live lease means it lives.
	token, err := e.a.acquireLease(job, ".watch", watchLease)
	if err != nil {
		t.Fatal(err)
	}
	e.call(t, "box_pull", pullArgs)
	if len(*e.spawns) != 2 {
		t.Errorf("restarted a live watcher: %v", *e.spawns)
	}
	// An expired one doesn't.
	now := e.a.Now()
	e.a.Now = func() time.Time { return now.Add(time.Hour) }
	e.call(t, "box_pull", pullArgs)
	if len(*e.spawns) != 3 {
		t.Errorf("didn't restart a watcher whose lease ran out: %v", *e.spawns)
	}
	e.a.releaseLease(job, ".watch", token)
	// Nothing to restart once the job was delivered.
	e.a.markNotified(job, "box_job")
	e.call(t, "box_pull", pullArgs)
	if len(*e.spawns) != 3 {
		t.Errorf("restarted a delivered job's watcher: %v", *e.spawns)
	}
}

// A follow after a broken one (or a pause) prints the log from its start
// again; the pushed tail has each line once. Seen live 2026-11-25 after a
// pause: "pause-smoke-start" twice.
func TestWatchTailAfterRefollowIsNotDoubled(t *testing.T) {
	f := &fakePC{t: t, block: true}
	e := newPushEnv(t, f, "thread-1")
	_, st := e.call(t, "box_run", e.runArgs(nil, "go test ./..."))
	job := st["job"].(string)
	f.finish("0", "only-once\n")
	f.mu.Lock()
	f.followErr = 1
	f.mu.Unlock()
	if err := e.a.Watch(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if e.push.count() != 1 || strings.Count(e.push.sent[0].msg, "only-once") != 1 {
		t.Errorf("pushed:\n%v", e.push.sent)
	}
}

func TestWatcherLeaseIsExclusive(t *testing.T) {
	f := &fakePC{t: t, block: true}
	e := newPushEnv(t, f, "thread-1")
	_, st := e.run(t, "make")
	job := st["job"].(string)
	token, err := e.a.acquireLease(job, ".watch", watchLease)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.acquireLease(job, ".watch", watchLease); !errors.Is(err, errLeaseHeld) {
		t.Fatalf("second acquire = %v", err)
	}
	f.finish("0", "")
	if err := e.a.Watch(context.Background(), job); err != nil || e.push.count() != 0 {
		t.Errorf("a second watcher ran alongside the first: %v, %d", err, e.push.count())
	}
	if !e.a.renewLease(job, ".watch", token, watchLease) || e.a.renewLease(job, ".watch", "someone-else", watchLease) {
		t.Error("renew accepted the wrong token, or refused the right one")
	}
}

func TestFollowRetriesTransportErrors(t *testing.T) {
	f := &fakePC{t: t, block: true, script: "running - 7\n"}
	e := newPushEnv(t, f, "thread-1")
	_, st := e.run(t, "make test")
	job := st["job"].(string)
	f.finish("0", "all good\n")
	f.followErr = 2 // the connection drops twice, but the job runs
	if err := e.a.Watch(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if e.push.count() != 1 || !strings.Contains(e.push.sent[0].msg, "ended, exit 0") || strings.Contains(e.push.sent[0].msg, "lost") {
		t.Errorf("pushes = %+v", e.push.sent)
	}
}

func TestLostOnlyWhenConfirmed(t *testing.T) {
	// The box is gone.
	f := &fakePC{t: t, block: true}
	e := newPushEnv(t, f, "thread-1")
	_, st := e.run(t, "make test")
	job := st["job"].(string)
	f.mu.Lock()
	f.block, f.followErr, f.insts = false, 1, nil
	f.mu.Unlock()
	if err := e.a.Watch(context.Background(), job); err != nil || e.push.count() != 1 || !strings.Contains(e.push.sent[0].msg, "lost, exit 75: run it locally") {
		t.Errorf("deleted box: %v, %+v", err, e.push.sent)
	}

	// The job's process is dead with no exit file.
	f = &fakePC{t: t, block: true, script: "died - 7\n"}
	e = newPushEnv(t, f, "thread-1")
	_, st = e.run(t, "make test")
	job = st["job"].(string)
	f.mu.Lock()
	f.block, f.followErr = false, 1
	f.mu.Unlock()
	if err := e.a.Watch(context.Background(), job); err != nil || e.push.count() != 1 || !strings.Contains(e.push.sent[0].msg, "lost") {
		t.Errorf("dead job: %v, %+v", err, e.push.sent)
	}

	// The PC can't be reached: not lost, and no push yet.
	f = &fakePC{t: t, block: true}
	e = newPushEnv(t, f, "thread-1")
	_, st = e.run(t, "make test")
	job = st["job"].(string)
	f.mu.Lock()
	f.block, f.followErr, f.listErr = false, 1000, true
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	e.a.Watch(ctx, job)
	if e.push.count() != 0 || e.a.exists(job, ".ended") {
		t.Errorf("an unreachable PC was called lost: %+v", e.push.sent)
	}
}

func TestPauseProbeNoticesADeletedBox(t *testing.T) {
	f := &fakePC{t: t, insts: []instance{{Name: "my-repo", Status: "Running"}}, block: true}
	a, _ := testApp(f)
	a.PausePoll = 5 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		_, err := a.Job(context.Background(), "my-repo.20261123-120000-0001", 0)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	f.mu.Lock()
	f.insts = nil
	f.mu.Unlock()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Job on a deleted box returned no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the follow of a deleted box never ended")
	}
}

func TestThrowawayKeptAfterItsPushedJob(t *testing.T) {
	f := &fakePC{t: t, block: true}
	e := newPushEnv(t, f, "thread-1")
	_, st := e.call(t, "box_run", e.runArgs(map[string]any{"new": true}, "make test"))
	job, box := st["job"].(string), st["box"].(string)
	f.finish("0", "built\n")
	if err := e.a.Watch(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.names(), box) {
		t.Fatalf("the throwaway %s was deleted before the agent could pull from it", box)
	}
	want := strings.TrimPrefix(e.a.expiryConfig(endedKeep), expiresKey+"=")
	if got := f.inst(box).Config[expiresKey]; got != want {
		t.Errorf("expiry = %q, want %q", got, want)
	}
	if !strings.Contains(e.push.sent[0].msg, "Box and log kept up to 30 minutes") {
		t.Errorf("message = %q", e.push.sent[0].msg)
	}
}

func TestClaimSeparatesFilesystemErrorsFromAlreadyClaimed(t *testing.T) {
	a := &App{StateDir: t.TempDir()}
	if won, err := a.claimBy("j", ".notified", "watcher"); !won || err != nil {
		t.Fatalf("first claim = %v, %v", won, err)
	}
	if won, err := a.claimBy("j", ".notified", "box_job"); won || err != nil {
		t.Errorf("second claim = %v, %v; want false, nil", won, err)
	}
	file := filepath.Join(t.TempDir(), "afile")
	os.WriteFile(file, nil, 0o644)
	b := &App{StateDir: filepath.Join(file, "sub")} // a directory that can't be made
	if won, err := b.claimBy("j", ".notified", "x"); won || err == nil {
		t.Errorf("claim in an unusable directory = %v, %v; want an error", won, err)
	}
}

func TestSafetyNetClearsOrphanedTempFiles(t *testing.T) {
	e := newPushEnv(t, &fakePC{t: t}, "thread-1")
	old, fresh := filepath.Join(e.a.StateDir, ".claim-old"), filepath.Join(e.a.StateDir, ".tmp-fresh")
	os.WriteFile(old, nil, 0o644)
	os.WriteFile(fresh, nil, 0o644)
	e.a.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	os.Chtimes(fresh, time.Now().Add(90*time.Minute), time.Now().Add(90*time.Minute))
	e.call(t, "box_pull", pullArgs)
	if _, err := os.Stat(old); err == nil {
		t.Error("an hour-old .claim file was kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a recent temp file was removed")
	}
}

func TestMessages(t *testing.T) {
	rec := jobRecord{Job: "my-repo.20261123-120000-0001", Reason: "Why.", Command: "make", Worktree: `C:\w`}
	if got := pausedMessage(rec); !strings.Contains(got, "paused: the PC's owner took the PC back") || !strings.Contains(got, frozenWindow) || !strings.Contains(got, "box_cancel this job") || !strings.Contains(got, "If box_job still says paused") || !strings.Contains(got, "From box, not your user.") {
		t.Errorf("paused = %q", got)
	}
	if got := lostMessage(rec, jobEnd{}); !strings.Contains(got, "lost, exit 75: run it locally.") {
		t.Errorf("lost = %q", got)
	}
	for d, want := range map[time.Duration]string{45 * time.Second: "45s", 6*time.Minute + 12*time.Second: "6m12s", time.Hour + 2*time.Minute + 3*time.Second: "1h02m03s"} {
		if got := elapsed(d); got != want {
			t.Errorf("elapsed(%v) = %q, want %q", d, got, want)
		}
	}
	var tail lineTail
	for i := 0; i < 100; i++ {
		tail.Write([]byte("line " + string(rune('a'+i%26)) + "\n"))
	}
	if n := len(strings.Split(tail.lastLines(tailLines), "\n")); n != tailLines || tailLines != 20 {
		t.Errorf("lastLines gave %d lines", n)
	}
}

func TestFindT3(t *testing.T) {
	cli := filepath.Join(t.TempDir(), "t3.cmd")
	os.WriteFile(cli, nil, 0o644)
	if got := findT3(func(k string) string {
		if k == "T3CODE_CLI_PATH" {
			return cli
		}
		return ""
	}); got != cli {
		t.Errorf("findT3 = %q, want %q", got, cli)
	}
}

func TestRunningHasNoExitCode(t *testing.T) {
	f := &fakePC{t: t, block: true}
	e := newPushEnv(t, f, "thread-1")
	text, st := e.run(t, "sleep 600")
	if v, ok := st["exitCode"]; !ok || v != nil {
		t.Errorf("exitCode = %v (present %v), want null", v, ok)
	}
	if strings.Contains(text, "exit code") || strings.Contains(text, "130") {
		t.Errorf("text gives a running job an exit code: %q", text)
	}
	job := st["job"].(string)
	_, got := e.call(t, "box_job", map[string]any{"id": job, "wait": "10ms"})
	if got["state"] != "running" || got["exitCode"] != nil {
		t.Errorf("box_job on a running job = %v", got)
	}
	f.mu.Lock()
	f.script = "running - 7\nhello\n"
	f.mu.Unlock()
	_, page := e.call(t, "box_job", map[string]any{"id": job, "offset": 0})
	if page["state"] != "running" || page["exitCode"] != nil {
		t.Errorf("log page of a running job = %v", page)
	}
	for _, tl := range tools {
		if tl.Name == "box_job" {
			d := tl.OutputSchema["properties"].(map[string]any)["exitCode"].(map[string]any)["description"].(string)
			if !strings.Contains(d, "null while the job is still running") {
				t.Errorf("exitCode schema = %q", d)
			}
		}
	}
}

func TestMCPToolSurface(t *testing.T) {
	var names []string
	for _, tl := range tools {
		names = append(names, tl.Name)
		props, _ := tl.InputSchema["properties"].(map[string]any)
		if tl.Name == "box_run" {
			for _, gone := range []string{"wait", "box"} {
				if _, ok := props[gone]; ok {
					t.Errorf("box_run still has %q", gone)
				}
			}
			if _, ok := props["push"]; !ok {
				t.Error("box_run has no push parameter")
			}
			if !strings.Contains(tl.Description, "push") || !strings.Contains(tl.Description, "of the command starting") {
				t.Errorf("box_run description doesn't explain push and the 30 s: %q", tl.Description)
			}
		}
	}
	if strings.Join(names, ",") != "box_status,box_run,box_job,box_cancel,box_pull" {
		t.Errorf("tools = %v", names)
	}
}

// reset drops the last follow's output only once the next one prints: a
// follow that prints nothing (a frozen box, a gone one) keeps it, for the
// tail of a lost push.
func TestLineTailResetIsLazy(t *testing.T) {
	var l lineTail
	l.Write([]byte("last-words\n"))
	l.reset()
	if got := l.lastLines(5); got != "last-words" {
		t.Errorf("after a reset with no output = %q", got)
	}
	l.Write([]byte("again\n"))
	if got := l.lastLines(5); got != "again" {
		t.Errorf("after the next follow printed = %q", got)
	}
}

// A watcher that dies with its MCP server (Codex keeps it inside its job
// object) leaves leases that expire: the safety net restarts it once they
// have, within sendLease of a death mid-send, and the end is pushed once.
func TestSafetyNetRecoversAWatcherKilledMidSend(t *testing.T) {
	f := &fakePC{t: t, block: true}
	e := newPushEnv(t, f, "thread-1")
	_, st := e.run(t, "go test ./...")
	job := st["job"].(string)
	now := e.a.Now()
	e.a.Now = func() time.Time { return now }
	// Killed while sending: its .ended written, its leases left behind.
	if err := writeJSON(e.a.statePath(job, ".ended"), jobEnd{Code: 1, Tail: "FAIL x"}); err != nil {
		t.Fatal(err)
	}
	for ext, ttl := range map[string]time.Duration{".sending": sendLease, ".watch": watchLease} {
		if won, err := e.a.claimBy(job, ext, e.a.leaseText("dead", ttl)); !won || err != nil {
			t.Fatal(ext, won, err)
		}
	}
	*e.spawns = nil
	if line := e.a.safetyNet(""); !strings.Contains(line, job+" exit 1") || len(*e.spawns) != 0 {
		t.Errorf("while the leases hold: %q, spawns %v", line, *e.spawns)
	}
	now = now.Add(watchLease + time.Second) // .watch gone, .sending still held
	if e.a.safetyNet(""); len(*e.spawns) != 0 {
		t.Errorf("restarted while .sending holds: %v", *e.spawns)
	}
	now = now.Add(sendLease - watchLease)
	if e.a.safetyNet(""); len(*e.spawns) != 1 {
		t.Fatalf("after the leases expired, spawns = %v", *e.spawns)
	}
	if err := e.a.Watch(context.Background(), job); err != nil || e.push.count() != 1 || !strings.Contains(e.push.sent[0].msg, "ended, exit 1") {
		t.Errorf("restarted watcher: %v, %+v", err, e.push.sent)
	}
	if line := e.a.safetyNet(""); line != "" {
		t.Errorf("after delivery: %q", line)
	}
}

// A Go test run's result leads with its digest, read from the box after
// the job: inline, in structuredContent, and in the pushed message.
func TestDigestLeadsTheResult(t *testing.T) {
	report := "#box-digest go\n" +
		`{"Action":"fail","Package":"p","Test":"TestQueue"}` + "\n" +
		`{"Action":"pass","Package":"p","Test":"TestOther"}` + "\n" +
		`{"Action":"output","Package":"p","Test":"TestQueue","Output":"    queue_test.go:88: expected 2 got 3\n"}` + "\n"
	want := "FAIL: 1 of 2 tests failed (go test)\np\n  TestQueue  queue_test.go:88: expected 2 got 3\n"

	e := newPushEnv(t, &fakePC{t: t, log: "FAIL\tp\t0.1s\n", exit: "1\n", digest: report}, "thread-1")
	text, st := e.run(t, "go test ./...")
	if st["exitCode"] != 1.0 || st["digest"] != want {
		t.Fatalf("structured = %v", st)
	}
	if _, rest, _ := strings.Cut(text, "\n"); !strings.HasPrefix(rest, want) || !strings.HasSuffix(rest, "FAIL\tp\t0.1s\n") {
		t.Fatalf("text doesn't lead with the digest:\n%s", text)
	}

	f := &fakePC{t: t, block: true, digest: report}
	e = newPushEnv(t, f, "thread-1")
	_, st = e.run(t, "go test ./...")
	job := st["job"].(string)
	f.finish("1", "FAIL\tp\t0.1s\n")
	if err := e.a.Watch(context.Background(), job); err != nil || e.push.count() != 1 {
		t.Fatalf("watch: %v, pushed %d", err, e.push.count())
	}
	if msg := e.push.sent[0].msg; !strings.Contains(msg, "\n"+want+"Output tail:\nFAIL\tp\t0.1s\n") {
		t.Fatalf("pushed message:\n%s", msg)
	}
}
