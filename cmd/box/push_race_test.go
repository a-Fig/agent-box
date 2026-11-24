package main

import (
	"context"
	"strings"
	"sync"
	"testing"
)

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

func TestWatcherFirstMakesBoxJobQuiet(t *testing.T) {
	f := &fakePC{t: t, block: true}
	e := newPushEnv(t, f, "thread-1")
	_, st := e.run(t, "make test")
	job := st["job"].(string)
	f.mu.Lock()
	f.block, f.log, f.exit = false, "output tail\n", "0\n"
	f.mu.Unlock()
	if err := e.a.Watch(context.Background(), job); err != nil || e.push.count() != 1 {
		t.Fatalf("watch: %v, %d pushes", err, e.push.count())
	}
	text, got := e.call(t, "box_job", map[string]any{"id": job})
	if got["pushed"] != true || got["state"] != "done" || strings.Contains(text, "output tail") || !strings.Contains(text, "already sent to your thread") {
		t.Errorf("box_job after the push = %v\n%s", got, text)
	}
}

func TestCancelledJobStillDeliversThroughBoxJob(t *testing.T) {
	f := &fakePC{t: t, block: true, script: "cancelled 130\n"}
	e := newPushEnv(t, f, "thread-1")
	_, st := e.run(t, "sleep 600")
	job := st["job"].(string)
	e.call(t, "box_cancel", map[string]any{"id": job})
	f.mu.Lock()
	f.block, f.log, f.exit = false, "partial\n", "130\n"
	f.mu.Unlock()
	text, got := e.call(t, "box_job", map[string]any{"id": job})
	if got["pushed"] != nil || got["exitCode"] != 130.0 || !strings.Contains(text, "partial") {
		t.Errorf("box_job after cancel = %v\n%s", got, text)
	}
}

// Whatever the interleaving, the end reaches the agent once: either the
// watcher's push or box_job's full result.
func TestBoxJobAndWatcherRaceDeliversOnce(t *testing.T) {
	f := &fakePC{t: t, block: true}
	e := newPushEnv(t, f, "thread-1")
	_, st := e.run(t, "make test")
	job := st["job"].(string)
	f.mu.Lock()
	f.block, f.log, f.exit = false, "tail line\n", "0\n"
	f.mu.Unlock()
	var wg sync.WaitGroup
	var text string
	wg.Add(2)
	go func() { defer wg.Done(); e.a.Watch(context.Background(), job) }()
	go func() { defer wg.Done(); text, _ = e.call(t, "box_job", map[string]any{"id": job}) }()
	wg.Wait()
	inline := 0
	if strings.Contains(text, "tail line") {
		inline = 1
	}
	if pushed := e.push.count(); pushed+inline != 1 {
		t.Errorf("delivered %d pushes and %d inline results; want exactly one delivery\n%s", pushed, inline, text)
	}
}

func TestClaimIsExclusiveAndNamesItsOwner(t *testing.T) {
	a := &App{StateDir: t.TempDir()}
	if !a.claimBy("j", ".notified", "watcher") || a.claimBy("j", ".notified", "box_job") {
		t.Fatal("second claim won")
	}
	if got := a.owner("j", ".notified"); got != "watcher" {
		t.Errorf("owner = %q", got)
	}
}
