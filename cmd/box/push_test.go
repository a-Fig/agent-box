package main

import (
	"context"
	"encoding/json"
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

func (e pushEnv) run(t *testing.T, command string) (string, map[string]any) {
	return e.call(t, "box_run", map[string]any{"dir": e.repo, "command": command, "reason": "Full suite for the push change; too heavy for the laptop."})
}

func TestRunInlineResultWithinWindow(t *testing.T) {
	e := newPushEnv(t, &fakePC{t: t, log: "ok\n", exit: "0\n"}, "thread-1")
	text, st := e.run(t, "go test ./...")
	if st["state"] != "done" || st["exitCode"] != 0.0 || st["push"] != nil {
		t.Fatalf("structured = %v\n%s", st, text)
	}
	if len(*e.spawns) != 0 {
		t.Errorf("spawned a watcher for a finished run: %v", *e.spawns)
	}
	if ents, _ := os.ReadDir(e.a.StateDir); len(ents) != 0 {
		t.Errorf("state files for an inline result: %v", ents)
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

func TestRunRefusedHasNoPush(t *testing.T) {
	e := newPushEnv(t, &fakePC{t: t, mode: "mypc"}, "thread-1")
	_, st := e.run(t, "true")
	if st["state"] != "refused" || st["exitCode"] != 75.0 || st["push"] != nil || len(*e.spawns) != 0 {
		t.Errorf("structured = %v, spawns %v", st, *e.spawns)
	}
}

func TestWatchPushesTheEnd(t *testing.T) {
	f := &fakePC{t: t, block: true}
	e := newPushEnv(t, f, "thread-1")
	_, st := e.run(t, "go test ./...")
	job := st["job"].(string)
	f.mu.Lock()
	f.block, f.log, f.exit = false, "FAIL one\nFAIL two\n", "1\n"
	f.mu.Unlock()
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
	for _, want := range []string{"box: job " + job + " ended, exit 1, after ", "From box, not your user.", "Full suite for the push change", "go test ./...  in  " + e.repo, "FAIL one\nFAIL two", "Full log: box_job id " + job + " offset 0. Artifacts: box_pull, before this repo's next run."} {
		if !strings.Contains(got.msg, want) {
			t.Errorf("message lacks %q:\n%s", want, got.msg)
		}
	}
	if !e.a.exists(job, ".notified") || !e.a.exists(job, ".ended") {
		t.Error("watcher left no .notified or .ended")
	}
	// A second watcher finds it delivered.
	if err := e.a.Watch(context.Background(), job); err != nil || e.push.count() != 1 {
		t.Errorf("second watch pushed again: %v, %d", err, e.push.count())
	}
}

func TestBoxJobAndWatchDeliverOnce(t *testing.T) {
	f := &fakePC{t: t, block: true}
	e := newPushEnv(t, f, "thread-1")
	_, st := e.run(t, "sleep 60")
	job := st["job"].(string)
	f.mu.Lock()
	f.block, f.exit = false, "0\n"
	f.mu.Unlock()
	_, got := e.call(t, "box_job", map[string]any{"id": job})
	if got["state"] != "done" {
		t.Fatalf("box_job = %v", got)
	}
	if !e.a.exists(job, ".notified") {
		t.Fatal("box_job left no .notified marker")
	}
	if err := e.a.Watch(context.Background(), job); err != nil || e.push.count() != 0 {
		t.Errorf("watch pushed after box_job returned the result: %v, %d", err, e.push.count())
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
	f.mu.Lock()
	f.block, f.exit = false, "130\n"
	f.mu.Unlock()
	if err := e.a.Watch(context.Background(), job); err != nil || e.push.count() != 0 {
		t.Errorf("watch pushed a cancelled job: %v, %d", err, e.push.count())
	}
}

func TestSafetyNetSaysWhatFinished(t *testing.T) {
	f := &fakePC{t: t, block: true}
	e := newPushEnv(t, f, "thread-1")
	_, st := e.run(t, "make test")
	job := st["job"].(string)
	// Another session's job isn't mentioned.
	other := jobRecord{Job: "other-box.20261123-120000-0009", Thread: "thread-2"}
	writeJSON(e.a.statePath(other.Job, ".json"), other)
	writeJSON(e.a.statePath(other.Job, ".ended"), jobEnd{Code: 9})

	f.mu.Lock()
	f.block, f.exit = false, "2\n"
	f.mu.Unlock()
	if err := e.a.Watch(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	pull := map[string]any{"from": "x", "remotePath": "y", "localDir": "relative"}
	text, _ := e.call(t, "box_pull", pull)
	if !strings.HasPrefix(text, "finished since you last looked: "+job+" exit 2\n") || strings.Contains(text, "other-box") {
		t.Errorf("first result = %q", text)
	}
	text, _ = e.call(t, "box_pull", pull)
	if strings.Contains(text, "finished since") {
		t.Errorf("said it twice: %q", text)
	}
}

func TestSafetyNetRestartsLostWatchers(t *testing.T) {
	e := newPushEnv(t, &fakePC{t: t, block: true}, "thread-1")
	_, st := e.run(t, "sleep 600")
	job := st["job"].(string)
	pull := map[string]any{"from": "x", "remotePath": "y", "localDir": "relative"}
	if len(*e.spawns) != 1 {
		t.Fatalf("spawns after box_run = %v", *e.spawns)
	}
	// No heartbeat: the watcher died with a sleep or reboot.
	e.call(t, "box_pull", pull)
	if len(*e.spawns) != 2 || (*e.spawns)[1] != job {
		t.Fatalf("spawns after a box_* call = %v", *e.spawns)
	}
	// A fresh heartbeat means it lives.
	os.WriteFile(e.a.statePath(job, ".watch"), nil, 0o644)
	e.call(t, "box_pull", pull)
	if len(*e.spawns) != 2 {
		t.Errorf("restarted a live watcher: %v", *e.spawns)
	}
	// A stale one doesn't.
	old := time.Now().Add(-time.Hour)
	os.Chtimes(e.a.statePath(job, ".watch"), old, old)
	e.a.Now = func() time.Time { return time.Now() }
	e.call(t, "box_pull", pull)
	if len(*e.spawns) != 3 {
		t.Errorf("didn't restart a stale watcher: %v", *e.spawns)
	}
	// Nothing to restart once the job was delivered.
	e.a.claim(job, ".notified")
	os.Remove(e.a.statePath(job, ".watch"))
	e.call(t, "box_pull", pull)
	if len(*e.spawns) != 3 {
		t.Errorf("restarted a delivered job's watcher: %v", *e.spawns)
	}
}

func TestFailedSendLeavesItToTheSafetyNet(t *testing.T) {
	f := &fakePC{t: t, block: true}
	e := newPushEnv(t, f, "thread-1")
	_, st := e.run(t, "make")
	job := st["job"].(string)
	f.mu.Lock()
	f.block, f.exit = false, "0\n"
	f.mu.Unlock()
	e.push.err = os.ErrDeadlineExceeded
	if err := e.a.Watch(context.Background(), job); err == nil {
		t.Fatal("Watch hid a failed send")
	}
	text, _ := e.call(t, "box_pull", map[string]any{"from": "x", "remotePath": "y", "localDir": "relative"})
	if !strings.HasPrefix(text, "finished since you last looked: "+job+" exit 0") {
		t.Errorf("text = %q", text)
	}
}

func TestMessages(t *testing.T) {
	rec := jobRecord{Job: "my-repo.20261123-120000-0001", Reason: "Why.", Command: "make", Worktree: `C:\w`}
	if got := pausedMessage(rec); !strings.Contains(got, "paused: the PC's owner took the PC back") || !strings.Contains(got, frozenWindow) || !strings.Contains(got, "box_cancel this job") || !strings.Contains(got, "From box, not your user.") {
		t.Errorf("paused = %q", got)
	}
	if got := lostMessage(rec, ""); !strings.Contains(got, "lost, exit 75: run it locally.") {
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
	if n := len(strings.Split(tail.lastLines(60), "\n")); n != 60 {
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
			if !strings.Contains(tl.Description, "push") {
				t.Errorf("box_run description doesn't explain push: %q", tl.Description)
			}
		}
	}
	if strings.Join(names, ",") != "box_status,box_run,box_job,box_cancel,box_pull" {
		t.Errorf("tools = %v", names)
	}
}
