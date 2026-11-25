package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hookJob writes the state files of a watched job: its record, and its end
// and notified marker when given.
func hookJob(t *testing.T, a *App, job, thread string, end *jobEnd, notified bool) {
	t.Helper()
	if err := writeJSON(a.statePath(job, ".json"), jobRecord{Job: job, Thread: thread, Reason: "Race suite for the scheduler.", Command: "go test ./...", Worktree: `C:\src\widget`}); err != nil {
		t.Fatal(err)
	}
	if end != nil {
		if err := writeJSON(a.statePath(job, ".ended"), *end); err != nil {
			t.Fatal(err)
		}
	}
	if notified {
		write(t, a.statePath(job, ".notified"), "box_job")
	}
}

// The hook tells the agent of its own session's ended jobs, once, and of
// nothing else: not another session's, not one already told, not a running one.
func TestHookTellsThisSessionsEndedJobsOnce(t *testing.T) {
	e := newPushEnv(t, &fakePC{t: t}, "thread-a")
	a := e.a
	var out bytes.Buffer
	a.Out = &out
	hookJob(t, a, "widget.20261123-115000-aaaa", "thread-a", &jobEnd{Code: 1, Took: 260, Tail: "FAIL TestX"}, false)
	hookJob(t, a, "widget.20261123-115100-bbbb", "thread-b", &jobEnd{Code: 0, Took: 10}, false)
	hookJob(t, a, "widget.20261123-115200-cccc", "thread-a", &jobEnd{Code: 0, Took: 10}, true)
	hookJob(t, a, "widget.20261123-115300-dddd", "thread-a", nil, false)
	hookJob(t, a, "widget.20261123-115400-eeee", "thread-a", &jobEnd{Code: exitPaused, Lost: true, Took: 30}, false)

	if code := run(context.Background(), a, []string{"hook"}, strings.NewReader(`{"hook_event_name":"PostToolUse"}`)); code != 0 {
		t.Fatalf("box hook = %d", code)
	}
	var got struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output %q: %v", out.String(), err)
	}
	ctx := got.HookSpecificOutput.AdditionalContext
	if got.HookSpecificOutput.HookEventName != "PostToolUse" ||
		!strings.Contains(ctx, "box: job widget.20261123-115000-aaaa ended, exit 1, after 4m20s") || !strings.Contains(ctx, "FAIL TestX") ||
		!strings.Contains(ctx, "box: job widget.20261123-115400-eeee lost, exit 75") {
		t.Fatalf("context:\n%s", ctx)
	}
	for _, other := range []string{"bbbb", "cccc", "dddd"} {
		if strings.Contains(ctx, other) {
			t.Errorf("told of %s:\n%s", other, ctx)
		}
	}
	if b, _ := os.ReadFile(a.statePath("widget.20261123-115000-aaaa", ".notified")); string(b) != "hook" {
		t.Errorf("aaaa .notified = %q", b)
	}
	if a.exists("widget.20261123-115100-bbbb", ".notified") {
		t.Error("marked another session's job notified")
	}
	if !a.hookActive("thread-a") || a.hookActive("thread-b") {
		t.Error("hookseen not recorded for thread-a alone")
	}

	out.Reset()
	run(context.Background(), a, []string{"hook"}, strings.NewReader("{}"))
	if out.Len() != 0 {
		t.Fatalf("second run said %q", out.String())
	}
}

// Without a T3 thread, or before box has pushed anything, the hook says
// nothing and writes nothing.
func TestHookIsSilentWithoutThreadOrState(t *testing.T) {
	e := newPushEnv(t, &fakePC{t: t}, "")
	var out bytes.Buffer
	e.a.Hook(strings.NewReader("{}"), &out)
	if ents, _ := os.ReadDir(e.a.StateDir); out.Len() != 0 || len(ents) != 0 {
		t.Fatalf("no thread: %q, %d files", out.String(), len(ents))
	}
	e = newPushEnv(t, &fakePC{t: t}, "thread-a")
	e.a.StateDir = filepath.Join(e.a.StateDir, "missing")
	e.a.Hook(nil, &out)
	if _, err := os.Stat(e.a.StateDir); out.Len() != 0 || !os.IsNotExist(err) {
		t.Fatalf("no state: %q, %v", out.String(), err)
	}
}

// With this thread's hook running, the watcher leaves an ended job to it for
// hookGrace and sends nothing if the hook delivers it; otherwise it sends.
func TestWatcherLeavesTheEndToARunningHook(t *testing.T) {
	end := jobEnd{Code: 0, Took: 50}
	for _, c := range []struct {
		name                string
		hookSeen, hookTakes bool
		sends               int
	}{
		{"no hook", false, false, 1},
		{"hook delivers", true, true, 0},
		{"hook idle", true, false, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newPushEnv(t, &fakePC{t: t}, "thread-a")
			a := e.a
			a.WatchPoll = 50 * time.Millisecond // a grace of 32 steps, about 1.6s: room for the hook on a slow runner
			job := "widget.20261123-115000-aaaa"
			hookJob(t, a, job, "thread-a", &end, false)
			if c.hookSeen {
				p := a.hookSeenPath("thread-a")
				write(t, p, "")
				os.Chtimes(p, a.Now(), a.Now())
			}
			var rec jobRecord
			readJSON(a.statePath(job, ".json"), &rec)
			done := make(chan error)
			go func() { done <- a.deliver(context.Background(), rec, end) }()
			if c.hookTakes {
				time.Sleep(5 * time.Millisecond)
				a.Hook(nil, io.Discard)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if n := e.push.count(); n != c.sends {
				t.Fatalf("sent %d, want %d", n, c.sends)
			}
			if !a.exists(job, ".notified") {
				t.Fatal("not marked notified")
			}
		})
	}
}

// box join adds the hook to Claude Code's settings once, keeps everything
// else as it was, replaces the hook of a moved binary, and leave takes it out.
func TestJoinAddsAndRemovesTheHook(t *testing.T) {
	home := isolateHome(t)
	configDir := filepath.Join(home, ".claude_personal")
	os.MkdirAll(configDir, 0o755)
	settings := filepath.Join(configDir, "settings.json")
	orig := `{
  "model": "opus",
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "[ -f x ] && \"C:/bin/mur.exe\" hook > /dev/null"
          }
        ]
      }
    ],
    "Stop": []
  },
  "env": {
    "A": "1"
  }
}
`
	write(t, settings, orig)
	c := joinConfig{
		R: &fakeAgents{registered: map[string]bool{}},
		LookPath: func(name string) (string, error) {
			if name == "claude" {
				return filepath.Join(home, "bin", "claude.exe"), nil
			}
			return "", os.ErrNotExist
		},
		Home: home, ClaudeConfigDir: configDir, Exe: filepath.Join(home, "bin", "box.exe"), Out: io.Discard,
	}
	boxHooks := func(p string) []string {
		t.Helper()
		var s struct {
			Hooks map[string][]struct {
				Hooks []struct{ Command string }
			}
		}
		b, _ := os.ReadFile(p)
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatalf("%s: %v\n%s", p, err, b)
		}
		var out []string
		for _, g := range s.Hooks["PostToolUse"] {
			for _, h := range g.Hooks {
				if isBoxHook.MatchString(h.Command) {
					out = append(out, h.Command)
				}
			}
		}
		return out
	}
	for range 2 {
		if err := join(context.Background(), c, false); err != nil {
			t.Fatal(err)
		}
	}
	want := `"` + filepath.ToSlash(c.Exe) + `" hook`
	for _, p := range []string{settings, filepath.Join(home, ".claude", "settings.json")} {
		if got := boxHooks(p); len(got) != 1 || got[0] != want {
			t.Errorf("%s: box hooks %q, want one %q", p, got, want)
		}
	}
	b, _ := os.ReadFile(settings)
	if s := string(b); !strings.Contains(s, `[ -f x ] && \"C:/bin/mur.exe\" hook > /dev/null`) ||
		strings.Index(s, `"model"`) > strings.Index(s, `"hooks"`) || strings.Index(s, `"hooks"`) > strings.Index(s, `"env"`) {
		t.Errorf("other settings changed:\n%s", s)
	}

	moved := c
	moved.Exe = filepath.Join(home, "new", "box.exe")
	if err := join(context.Background(), moved, false); err != nil {
		t.Fatal(err)
	}
	if got := boxHooks(settings); len(got) != 1 || !strings.Contains(got[0], "/new/box.exe") {
		t.Errorf("after a move: %q", got)
	}

	if err := join(context.Background(), c, true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(settings); string(b) != orig {
		t.Errorf("after leave:\n%s\nwant\n%s", b, orig)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json")); strings.TrimSpace(string(b)) != "{}" {
		t.Errorf("a settings file join made, after leave: %s", b)
	}
}

// A settings file box can't read is reported and left alone.
func TestJoinLeavesABadSettingsFileAlone(t *testing.T) {
	home := isolateHome(t)
	p := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(p), 0o755)
	write(t, p, "{ not json")
	c := joinConfig{
		R:        &fakeAgents{registered: map[string]bool{}},
		LookPath: func(name string) (string, error) { return filepath.Join(home, name), nil },
		Home:     home, Exe: filepath.Join(home, "box.exe"), Out: io.Discard,
	}
	if err := join(context.Background(), c, false); err == nil || !strings.Contains(err.Error(), "claude hook") {
		t.Fatalf("join = %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "{ not json" {
		t.Fatalf("file changed: %q", b)
	}
}

// A subagent's tool call, or a claude started inside the session, doesn't
// take the notice: its event says so, by agent_id or another session_id.
func TestHookSkipsSubagentsAndOtherSessions(t *testing.T) {
	e := newPushEnv(t, &fakePC{t: t}, "thread-a")
	a := e.a
	job := "widget.20261123-115000-aaaa"
	writeJSON(a.statePath(job, ".json"), jobRecord{Job: job, Thread: "thread-a", Session: "sess-1", Command: "go test ./..."})
	writeJSON(a.statePath(job, ".ended"), jobEnd{Code: 0, Took: 9})
	for _, ev := range []string{`{"session_id":"sess-1","agent_id":"sub-1"}`, `{"session_id":"sess-2"}`} {
		var out bytes.Buffer
		a.Hook(strings.NewReader(ev), &out)
		if out.Len() != 0 || a.exists(job, ".notified") {
			t.Fatalf("event %s: said %q, notified %v", ev, out.String(), a.exists(job, ".notified"))
		}
	}
	var out bytes.Buffer
	a.Hook(strings.NewReader(`{"session_id":"sess-1"}`), &out)
	if !strings.Contains(out.String(), job) || !a.exists(job, ".notified") {
		t.Fatalf("its own session: %q", out.String())
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// A hook that can't write its output leaves the end owed, for the watcher
// or the next hook.
func TestHookLeavesTheEndOwedWhenItCantWrite(t *testing.T) {
	e := newPushEnv(t, &fakePC{t: t}, "thread-a")
	job := "widget.20261123-115000-aaaa"
	hookJob(t, e.a, job, "thread-a", &jobEnd{Code: 0, Took: 9}, false)
	e.a.Hook(nil, failWriter{})
	if e.a.exists(job, ".notified") {
		t.Fatal("marked notified with nothing written")
	}
}

// A settings file with a BOM keeps it, and a join that finds its hook in
// place leaves the file's bytes alone, however they are laid out.
func TestJoinKeepsABOMAndAnInstalledHook(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	exe := filepath.Join(dir, "box.exe")
	write(t, p, "\xef\xbb\xbf{\r\n    \"model\": \"opus\"\r\n}\r\n")
	if did, err := setHook(p, exe, false); err != nil || !strings.HasPrefix(did, "added") {
		t.Fatalf("setHook = %q, %v", did, err)
	}
	b, _ := os.ReadFile(p)
	if !bytes.HasPrefix(b, []byte("\xef\xbb\xbf{")) || !strings.Contains(string(b), `/box.exe\" hook"`) {
		t.Fatalf("after join:\n%q", b)
	}
	laidOut := strings.Replace(string(b), "\n", "\r\n", -1) // as an editor might save it
	write(t, p, laidOut)
	if did, err := setHook(p, exe, false); err != nil || !strings.HasPrefix(did, "hook already") {
		t.Fatalf("again: %q, %v", did, err)
	}
	if b, _ := os.ReadFile(p); string(b) != laidOut {
		t.Fatal("rewrote a file whose hook was in place")
	}
	if did, err := setHook(p, filepath.Join(dir, "moved", "box.exe"), false); err != nil || !strings.HasPrefix(did, "replaced") {
		t.Fatalf("moved: %q, %v", did, err)
	}
}
