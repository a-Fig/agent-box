package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/skills"
)

func TestCheckEnv(t *testing.T) {
	if err := checkEnv(map[string]string{"CI": "1", "GOFLAGS": "-count=1", "node_env": "test"}); err != nil {
		t.Errorf("plain names refused: %v", err)
	}
	for _, k := range []string{"GITHUB_TOKEN", "aws_secret_access_key", "API_KEY", "DB_PASSWORD", "1BAD", "A-B", "A=B"} {
		if err := checkEnv(map[string]string{k: "x"}); err == nil {
			t.Errorf("%s accepted", k)
		}
	}
	f := &fakePC{t: t}
	a, _ := testApp(f)
	r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, Env: map[string]string{"NPM_TOKEN": "x"}})
	if err == nil || !strings.Contains(err.Error(), "looks like a secret") || r.Code != 2 {
		t.Fatalf("Run = %+v, %v", r, err)
	}
	if c := f.callList(); c != "" {
		t.Errorf("incus calls = %q, want none", c)
	}
}

func TestRunRejectsInvalidOptionsBeforeStarting(t *testing.T) {
	for name, opts := range map[string]RunOpts{
		"conflicting boxes": {Cmd: []string{"true"}, Box: "named", New: true},
		"negative wait":     {Cmd: []string{"true"}, Wait: -time.Second},
		"NUL env value":     {Cmd: []string{"true"}, Env: map[string]string{"CI": "a\x00b"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakePC{t: t}
			a, _ := testApp(f)
			if r, err := a.Run(context.Background(), opts); err == nil || r.Code != 2 || f.callList() != "" {
				t.Errorf("Run = %+v, %v; Incus calls %q", r, err, f.callList())
			}
		})
	}
}

func TestPayload(t *testing.T) {
	got := payload(RunOpts{Cmd: []string{"go test ./..."}, Env: map[string]string{"B": "it's", "A": "1"}, Timeout: 90*time.Second + time.Millisecond})
	if !strings.HasPrefix(got, "export A='1' || exit 2\nexport B='it'\\''s' || exit 2\n") ||
		!strings.Contains(got, "-k 10s 90.001s /bin/bash") || !strings.Contains(got, "go test ./...") {
		t.Errorf("payload =\n%s", got)
	}
	if got := payload(RunOpts{Cmd: []string{"echo", "a b"}}); got != `'echo' 'a b'` {
		t.Errorf("plain payload = %q", got)
	}
}

func TestPayloadPreservesExitCodeOnHardTimeout(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux process-group signals")
	}
	bash := gitBash(t)
	for _, tc := range []struct {
		command string
		code    int
	}{
		{`printf 'stdout'; printf 'stderr' >&2; trap '' TERM; sleep 20`, 124},
		{`printf 'stdout'; printf 'stderr' >&2; exit 137`, 137},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		res, err := (execRunner{}).Run(ctx, Cmd{Name: bash, Args: []string{"-c", payload(RunOpts{Cmd: []string{tc.command}, Timeout: 100 * time.Millisecond})}})
		cancel()
		if err != nil || res.Code != tc.code || res.Stdout != "stdout" || !strings.Contains(res.Stderr, "stderr") {
			t.Errorf("payload %q = %+v, %v", tc.command, res, err)
		}
	}
}

func TestStatus(t *testing.T) {
	limits := map[string]string{"limits.instances": "3"}
	for name, tc := range map[string]struct {
		f      *fakePC
		advice string
		ok     bool
	}{
		"unreachable": {&fakePC{listErr: true}, "unavailable: can't reach pc (incus list: Error: connection refused): run locally", false},
		"my pc":       {&fakePC{mode: "mypc", project: limits}, "unavailable: PC is in My PC mode: run locally", false},
		"repo box frozen": {&fakePC{mode: "shared", project: limits, insts: []instance{{Name: "my-repo", Status: "Frozen"}}},
			"unavailable: this repo's box my-repo is frozen: run locally", false},
		"yielding": {&fakePC{mode: "shared", project: map[string]string{"limits.instances": "3", "user.agent-box.reason": "Steam game running"}},
			"unavailable: PC is yielding to a game (Steam game running): run locally", false},
		"free slots": {&fakePC{mode: "shared", project: limits, memory: "4GiB", insts: []instance{{Name: "other", Status: "Running"}}},
			"available: 2 slot(s) free, 2 CPUs/4GiB per box", true},
		"full slots": {&fakePC{mode: "full", project: limits, insts: []instance{{Name: "a"}, {Name: "b"}, {Name: "c"}}},
			"busy: all 3 box slots are taken: run locally, or reuse a box with box_run box", false},
		"repo box": {&fakePC{mode: "full", project: limits, memory: "4GiB",
			insts: []instance{{Name: "my-repo", Status: "Running", Config: map[string]string{expiresKey: "2026-11-23T14:00:00Z"}}, {Name: "b"}, {Name: "c"}},
			jobs:  map[string]string{"my-repo": "my-repo.20261123-110000-beef running - 10\nmy-repo.20261123-100000-0001 done 0 5\n"}},
			"available: this repo's box my-repo is running, 1 job(s) running there (runs take turns); 4 CPUs/4GiB per box", true},
		"no mode": {&fakePC{project: map[string]string{}}, "available: 2 CPUs/4GiB per box (mode unknown: the tray hasn't set it)", true},
	} {
		t.Run(name, func(t *testing.T) {
			tc.f.t = t
			a, _ := testApp(tc.f)
			s := a.Status(context.Background(), newRepo(t))
			if s.Advice != tc.advice || s.Available != tc.ok {
				t.Errorf("advice = %q, %v\nwant %q, %v", s.Advice, s.Available, tc.advice, tc.ok)
			}
			if name == "repo box" {
				if s.RepoBox == nil || len(s.RepoBox.Running) != 1 || s.RepoBox.Running[0].Job != "my-repo.20261123-110000-beef" || s.SlotsFree != 0 || s.BoxCPUs != 4 {
					t.Errorf("report = %+v, repo box %+v", s, s.RepoBox)
				}
				var b bytes.Buffer
				writeStatus(&b, s)
				if !strings.Contains(b.String(), "mode: full\nboxes: 3 of 3 (0 free)\nper box: 4 CPUs, 4GiB RAM\nrepo box: my-repo, Running") {
					t.Errorf("text =\n%s", b.String())
				}
			}
		})
	}
}

func TestStatusCLIExitCode(t *testing.T) {
	a, out := testApp(&fakePC{t: t, mode: "mypc"})
	if code := run(context.Background(), a, []string{"status"}, nil); code != exitPaused || !strings.HasPrefix(out.String(), "unavailable") {
		t.Errorf("box status = %d\n%s", code, out)
	}
}

func TestJobsListsNewestFirst(t *testing.T) {
	f := &fakePC{t: t,
		insts: []instance{{Name: "a", Status: "Running"}, {Name: "b", Status: "Running"}, {Name: "c", Status: "Frozen"}},
		jobs: map[string]string{
			"a": "a.20261123-100000-0001 done 3 12\na.20261123-120000-0002 running - 4\nnot-a-job x y z\n",
			"b": "b.20261123-110000-0003 died - 0\n",
		}}
	a, out := testApp(f)
	jobs, err := a.Jobs(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, j := range jobs {
		ids = append(ids, j.Job+"="+j.stateText())
	}
	if got := strings.Join(ids, " "); got != "a.20261123-120000-0002=running b.20261123-110000-0003=died a.20261123-100000-0001=done (exit 3)" {
		t.Errorf("jobs = %s", got)
	}
	if !jobs[0].Started.Equal(time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("started = %v", jobs[0].Started)
	}
	if !strings.Contains(out.String(), "c is Frozen") {
		t.Errorf("no warning for the frozen box: %q", out)
	}
	if _, err := a.Jobs(context.Background(), "nope"); err == nil {
		t.Error("unknown box accepted")
	}
	if _, err := a.Jobs(context.Background(), "Bad;box"); err == nil {
		t.Error("bad box name accepted")
	}
}

func TestCancel(t *testing.T) {
	f := &fakePC{t: t, insts: []instance{{Name: "my-repo", Status: "Running"}}, script: "cancelled 130\n"}
	a, _ := testApp(f)
	ok, code, err := a.Cancel(context.Background(), "my-repo.20261123-120000-0001")
	if err != nil || !ok || code != 130 {
		t.Fatalf("Cancel = %v, %d, %v", ok, code, err)
	}
	if got := f.inst("my-repo").Config[expiresKey]; got != "2026-11-23T14:00:00Z" {
		t.Errorf("expiry after cancel = %q, want idle", got)
	}
	f.script = "finished 0\n"
	if ok, code, err := a.Cancel(context.Background(), "my-repo.20261123-120000-0001"); err != nil || ok || code != 0 {
		t.Errorf("Cancel of a finished job = %v, %d, %v", ok, code, err)
	}
	f.script, f.scriptRC = "", 3
	if _, _, err := a.Cancel(context.Background(), "my-repo.20261123-120000-0001"); err == nil || !strings.Contains(err.Error(), "no job") {
		t.Errorf("unknown job: %v", err)
	}
	for _, id := range []string{"x", "my-repo.1; reboot", "../x.20261123-120000-01"} {
		if _, _, err := a.Cancel(context.Background(), id); err == nil {
			t.Errorf("Cancel(%q) accepted", id)
		}
	}
}

func TestJobScriptsRequireMatchingJob(t *testing.T) {
	bash := gitBash(t)
	old := workRoot
	workRoot = filepath.ToSlash(t.TempDir())
	defer func() { workRoot = old }()
	if err := os.MkdirAll(filepath.Join(workRoot, ".box"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workRoot, "checkout"), 0o755); err != nil {
		t.Fatal(err)
	}
	job := "sample.20261123-120000-0001"
	ctx := context.Background()
	res, err := (execRunner{}).Run(ctx, Cmd{Name: bash, Args: []string{"-c", checkoutScript(job)}})
	if err != nil || res.Code != 3 || !strings.Contains(res.Stderr, "no job") {
		t.Errorf("lookup of nonexistent job = %+v, %v", res, err)
	}
	file := workRoot + "/.box/" + job
	write(t, file+".log", "starting\n")
	// Delay PID publication, then model a job finishing before it can be cancelled.
	setup := "sleep() { echo 0 > " + shq(file+".exit") + "; }\n"
	res, err = (execRunner{}).Run(ctx, Cmd{Name: bash, Args: []string{"-c", setup + cancelScript(job)}})
	if err != nil || res.Code != 0 || strings.TrimSpace(res.Stdout) != "finished 0" {
		t.Errorf("cancel during startup = %+v, %v", res, err)
	}
}

func TestLogPage(t *testing.T) {
	f := &fakePC{t: t, script: "running - 100\nabcdefghij"}
	a, _ := testApp(f)
	p, err := a.LogPage(context.Background(), "my-repo.20261123-120000-0001", 40, 10)
	if err != nil {
		t.Fatal(err)
	}
	if p.Text != "abcdefghij" || p.NextOffset != 50 || p.LogBytes != 100 || !p.More || p.State != jobRunning {
		t.Errorf("page = %+v", p)
	}
	if h := p.pageHead(); h != "job my-repo.20261123-120000-0001 on box my-repo: running, log bytes 40-50 of 100; next offset 50" {
		t.Errorf("head = %q", h)
	}
	f.script = "done 0 5\nhello"
	if p, _ := a.LogPage(context.Background(), "my-repo.20261123-120000-0001", 0, 0); p.More || p.Code == nil || *p.Code != 0 {
		t.Errorf("last page = %+v", p)
	}
	if _, err := a.LogPage(context.Background(), "my-repo.20261123-120000-0001", -1, 0); err == nil {
		t.Error("negative offset accepted")
	}
	if _, err := a.LogPage(context.Background(), "my-repo.20261123-120000-0001", 1<<63-1, 0); err == nil {
		t.Error("overflowing offset accepted")
	}
	f.script = "done 0 5\n"
	if p, err := a.LogPage(context.Background(), "my-repo.20261123-120000-0001", 10, 0); err != nil || p.More || p.LogBytes != 5 || p.NextOffset != 10 {
		t.Errorf("page past end = %+v, %v", p, err)
	}
}

func TestMCPLogPagePreservesSplitUTF8(t *testing.T) {
	p := Page{Text: "\xc3", NextOffset: 1}
	got := pageResult(p)["structuredContent"].(map[string]any)
	if got["logBase64"] != "ww==" || got["nextOffset"] != int64(1) {
		t.Errorf("structured page = %v", got)
	}
}

func TestPull(t *testing.T) {
	f := &fakePC{t: t, script: "/work/my-repo\n"}
	f.pull = func(src, dst string) {
		dir := filepath.Join(dst, "coverage")
		os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
		write(t, filepath.Join(dir, "sub", "a.txt"), "covered\n")
		os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(dir, "escape")) // may fail on Windows; fine
	}
	a, out := testApp(f)
	parent := t.TempDir()
	local := filepath.Join(parent, "out")
	dest, err := a.Pull(context.Background(), "my-repo.20261123-120000-0001", "coverage", local)
	if err != nil {
		t.Fatalf("Pull: %v\n%s", err, out)
	}
	if dest != filepath.Join(local, "coverage") || f.pulls[0] != "pc:my-repo/work/my-repo/coverage" {
		t.Errorf("dest = %s, pulled %v", dest, f.pulls)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "sub", "a.txt")); string(b) != "covered\n" {
		t.Errorf("a.txt = %q", b)
	}
	if _, err := os.Lstat(filepath.Join(dest, "escape")); err == nil {
		t.Error("symlink kept")
	}
	entries, _ := os.ReadDir(local)
	if len(entries) != 1 {
		t.Errorf("local holds %d entries, want only coverage (staging left behind?)", len(entries))
	}
	if entries, _ := os.ReadDir(parent); len(entries) != 1 {
		t.Errorf("wrote next to the local directory: %v", entries)
	}

	// A second pull never replaces what the first one wrote.
	if _, err := a.Pull(context.Background(), "my-repo.20261123-120000-0001", "coverage", local); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second pull: %v", err)
	}
	// Absolute paths skip the checkout lookup; a box name works as the source.
	if _, err := a.Pull(context.Background(), "my-repo", "/tmp/coverage", t.TempDir()); err != nil || f.pulls[1] != "pc:my-repo/tmp/coverage" {
		t.Errorf("absolute pull: %v, %v", err, f.pulls)
	}
	for _, bad := range [][2]string{{"Bad;box", "x"}, {"my-repo", "/"}, {"my-repo", ""}} {
		if _, err := a.Pull(context.Background(), bad[0], bad[1], t.TempDir()); err == nil {
			t.Errorf("Pull(%q, %q) accepted", bad[0], bad[1])
		}
	}
}

func TestPullKeepsFileCreatedDuringTransfer(t *testing.T) {
	local := t.TempDir()
	f := &fakePC{t: t, pull: func(_, staging string) {
		write(t, filepath.Join(staging, "result.txt"), "remote")
		write(t, filepath.Join(local, "result.txt"), "local")
	}}
	a, _ := testApp(f)
	if _, err := a.Pull(context.Background(), "my-repo", "/tmp/result.txt", local); err == nil {
		t.Fatal("pull replaced a file created during transfer")
	}
	if got, err := os.ReadFile(filepath.Join(local, "result.txt")); err != nil || string(got) != "local" {
		t.Errorf("local result = %q, %v", got, err)
	}
}

func TestPullRejectsNonLocalWindowsName(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path rules")
	}
	f := &fakePC{t: t}
	a, _ := testApp(f)
	for _, p := range []string{`/tmp/parent\nested.txt`, `/tmp/CON.txt`} {
		if _, err := a.Pull(context.Background(), "my-repo", p, t.TempDir()); err == nil || !strings.Contains(err.Error(), "local file name") {
			t.Errorf("Pull(%q) = %v", p, err)
		}
	}
	if c := f.callList(); c != "" {
		t.Errorf("unexpected Incus calls: %s", c)
	}
}

func TestJobsReturnsFailureForExplicitBox(t *testing.T) {
	a, _ := testApp(&fakePC{t: t})
	a.R = runnerFunc(func(c Cmd) Result {
		if c.Args[2] == "list" {
			return Result{Stdout: `[{"name":"my-repo","status":"Running"}]`}
		}
		return Result{Code: 1, Stderr: "connection reset"}
	})
	if _, err := a.Jobs(context.Background(), "my-repo"); err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("Jobs = %v", err)
	}
}

// fakeAgents records the agent CLI calls box join makes.
type fakeAgents struct {
	calls      []string
	registered map[string]bool
}

func (f *fakeAgents) Run(_ context.Context, c Cmd) (Result, error) {
	name := strings.TrimSuffix(filepath.Base(c.Name), ".exe")
	f.calls = append(f.calls, name+" "+strings.Join(c.Args, " "))
	switch c.Args[1] {
	case "add":
		f.registered[name] = true
	case "remove":
		if !f.registered[name] {
			return Result{Code: 1, Stderr: "No MCP server named box"}, nil
		}
		delete(f.registered, name)
	}
	return Result{}, nil
}

// isolateHome points every home and config variable at a temp folder, so
// nothing in the test can touch the real ones.
func isolateHome(t *testing.T) string {
	home := t.TempDir()
	for _, k := range []string{"HOME", "USERPROFILE", "CLAUDE_CONFIG_DIR", "CODEX_HOME"} {
		t.Setenv(k, filepath.Join(home, "unused-"+strings.ToLower(k)))
	}
	return home
}

func TestJoinAndLeave(t *testing.T) {
	home := isolateHome(t)
	configDir := filepath.Join(home, ".claude_personal")
	agents := &fakeAgents{registered: map[string]bool{}}
	var out bytes.Buffer
	exe := filepath.Join(home, "bin", "box.exe")
	c := joinConfig{
		R: agents,
		LookPath: func(name string) (string, error) {
			if name == "claude" {
				return filepath.Join(home, "bin", "claude.exe"), nil
			}
			return "", errors.New("not found")
		},
		Home: home, ClaudeConfigDir: configDir, Exe: exe, Out: &out,
	}
	a, _ := testApp(&fakePC{t: t})
	a.JoinConfig = func(io.Writer) (joinConfig, error) { return c, nil }
	if code := run(context.Background(), a, []string{"join"}, nil); code != 0 {
		t.Fatalf("box join = %d\n%s", code, out.String())
	}
	// Joining again is fine: it removes before adding.
	if err := join(context.Background(), c, false); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"claude mcp remove --scope user box",
		"claude mcp add --scope user box -- " + exe + " mcp",
		"claude mcp remove --scope user box",
		"claude mcp add --scope user box -- " + exe + " mcp",
	}
	if fmt.Sprint(agents.calls) != fmt.Sprint(want) {
		t.Errorf("calls =\n%q\nwant\n%q", agents.calls, want)
	}
	skillsAt := []string{filepath.Join(home, ".claude", "skills", "box", "SKILL.md"), filepath.Join(configDir, "skills", "box", "SKILL.md"),
		filepath.Join(home, ".codex", "skills", "box", "SKILL.md")}
	for _, p := range skillsAt {
		if b, err := os.ReadFile(p); err != nil || string(b) != skills.Box {
			t.Errorf("skill at %s: %v", p, err)
		}
	}
	if s := out.String(); !strings.Contains(s, "codex: not installed, skipped") || !strings.Contains(s, "claude: registered MCP server box (user scope)") {
		t.Errorf("report =\n%s", s)
	}

	out.Reset()
	if err := join(context.Background(), c, true); err != nil {
		t.Fatal(err)
	}
	if agents.registered["claude"] {
		t.Error("still registered after leave")
	}
	for _, p := range skillsAt {
		if _, err := os.Stat(filepath.Dir(p)); !os.IsNotExist(err) {
			t.Errorf("%s still there after leave", filepath.Dir(p))
		}
	}
	if s := out.String(); !strings.Contains(s, "claude: removed MCP server box") {
		t.Errorf("leave report =\n%s", s)
	}
}

func TestJoinUsesAlternateCodexHome(t *testing.T) {
	home := isolateHome(t)
	custom := filepath.Join(home, "custom-codex")
	t.Setenv("CODEX_HOME", custom)
	c := joinConfig{
		Home: home, CodexHome: os.Getenv("CODEX_HOME"), Exe: filepath.Join(home, "box.exe"), Out: io.Discard,
		LookPath: func(string) (string, error) { return "", os.ErrNotExist },
	}
	ctx := context.Background()
	if err := join(ctx, c, false); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(custom, "skills", "box", "SKILL.md")
	if b, err := os.ReadFile(p); err != nil || string(b) != skills.Box {
		t.Fatalf("custom skill = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); !os.IsNotExist(err) {
		t.Errorf("default Codex directory was used: %v", err)
	}
	if err := join(ctx, c, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("custom skill still there: %v", err)
	}
}

func TestJoinAndLeaveKeepCustomSkill(t *testing.T) {
	home := isolateHome(t)
	c := joinConfig{
		Home: home, Exe: filepath.Join(home, "box.exe"), Out: io.Discard,
		LookPath: func(string) (string, error) { return "", os.ErrNotExist },
	}
	p := filepath.Join(home, ".codex", "skills", "box", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, p, "local skill\n")
	for _, leave := range []bool{false, true} {
		if err := join(context.Background(), c, leave); err == nil || !strings.Contains(err.Error(), "existing skill") {
			t.Errorf("join(leave=%t) = %v", leave, err)
		}
		if got, err := os.ReadFile(p); err != nil || string(got) != "local skill\n" {
			t.Errorf("local skill after join(leave=%t) = %q, %v", leave, got, err)
		}
	}
}

func TestJoinReportsFailedAdd(t *testing.T) {
	home := isolateHome(t)
	c := joinConfig{
		R: runnerFunc(func(c Cmd) Result {
			if c.Args[1] == "add" {
				return Result{Code: 1, Stderr: "boom"}
			}
			return Result{}
		}),
		LookPath: func(name string) (string, error) { return filepath.Join(home, name), nil },
		Home:     home, Exe: filepath.Join(home, "box.exe"), Out: &bytes.Buffer{},
	}
	err := join(context.Background(), c, false)
	if err == nil || !strings.Contains(err.Error(), "claude: claude mcp add: boom") || !strings.Contains(err.Error(), "codex: codex mcp add: boom") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "box", "SKILL.md")); err != nil {
		t.Errorf("skill not installed after a failed add: %v", err)
	}
}

type runnerFunc func(Cmd) Result

func (f runnerFunc) Run(_ context.Context, c Cmd) (Result, error) { return f(c), nil }

func TestSkill(t *testing.T) {
	if !strings.HasPrefix(skills.Box, "---\nname: box\n") && !strings.HasPrefix(skills.Box, "---\r\nname: box\r\n") {
		t.Errorf("skill frontmatter: %.40q", skills.Box)
	}
	_, body, _ := strings.Cut(strings.TrimPrefix(skills.Box, "---"), "---")
	if n := len(strings.Fields(body)); n > 300 {
		t.Errorf("skill body is %d words, want at most 300", n)
	}
}

func TestMCPAgentTools(t *testing.T) {
	f := &fakePC{t: t, mode: "shared", project: map[string]string{"limits.instances": "3"}, memory: "4GiB",
		insts:  []instance{{Name: "my-repo", Status: "Running"}},
		jobs:   map[string]string{"my-repo": "my-repo.20261123-120000-0001 running - 7\n"},
		script: "running - 7\nhello\n"}
	a, _ := testApp(f)
	dir, _ := json.Marshal(newRepo(t))
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"box_status","arguments":{"dir":` + string(dir) + `}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"box_jobs","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"box_job","arguments":{"id":"my-repo.20261123-120000-0001","offset":0}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"box_run","arguments":{"dir":` + string(dir) + `,"command":"true","env":{"GH_TOKEN":"x"}}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"box_pull","arguments":{"from":"my-repo","remotePath":"x","localDir":"relative"}}}`,
	}, "\n")
	var out bytes.Buffer
	if err := serveMCP(context.Background(), a, strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	type reply struct {
		ID     int
		Result struct {
			Content    []struct{ Text string }
			IsError    bool
			Structured map[string]any `json:"structuredContent"`
		}
	}
	got := map[int]reply{}
	dec := json.NewDecoder(&out)
	for dec.More() {
		var r reply
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		got[r.ID] = r
	}
	first := func(id int) string { l, _, _ := strings.Cut(got[id].Result.Content[0].Text, "\n"); return l }
	if l := first(1); l != "available: this repo's box my-repo is running, 1 job(s) running there (runs take turns); 2 CPUs/4GiB per box" {
		t.Errorf("box_status first line = %q", l)
	}
	if s := got[1].Result.Structured; s["slotsFree"] != 2.0 || s["mode"] != "shared" {
		t.Errorf("box_status structured = %v", s)
	}
	if l := first(2); l != "1 job(s), 1 running" {
		t.Errorf("box_jobs first line = %q", l)
	}
	if r := got[3].Result; r.IsError || r.Structured["nextOffset"] != 6.0 || r.Structured["exitCode"] != 130.0 || !strings.HasSuffix(r.Content[0].Text, "\nhello\n") {
		t.Errorf("box_job page = %+v", r)
	}
	if r := got[4].Result; !r.IsError || !strings.Contains(r.Content[0].Text, "GH_TOKEN looks like a secret") {
		t.Errorf("box_run with a secret = %+v", r)
	}
	if r := got[5].Result; !r.IsError || !strings.Contains(r.Content[0].Text, "absolute") {
		t.Errorf("box_pull relative = %+v", r)
	}
}

// TestAgentToolsEndToEnd runs the real job scripts in a local bash: env,
// timeout, listing, log paging and cancelling.
func TestAgentToolsEndToEnd(t *testing.T) {
	bash := gitBash(t)
	if _, err := os.Stat(filepath.Join(filepath.Dir(bash), "timeout")); err != nil {
		if _, err := os.Stat(filepath.Join(filepath.Dir(bash), "timeout.exe")); err != nil {
			t.Skip("no coreutils timeout next to bash")
		}
	}
	// ps, pkill and pgrep stand-ins: the wrapper's PID is its session ID.
	shims := t.TempDir()
	write(t, filepath.Join(shims, "ps"), "#!/bin/sh\necho \"$4\"\n")
	write(t, filepath.Join(shims, "pkill"), "#!/bin/sh\nkill \"$1\" \"$3\" 2>/dev/null; exit 0\n")
	write(t, filepath.Join(shims, "pgrep"), "#!/bin/sh\nkill -0 \"$2\" 2>/dev/null\n")
	t.Setenv("PATH", shims+string(os.PathListSeparator)+os.Getenv("PATH"))

	root := filepath.ToSlash(t.TempDir())
	old := workRoot
	workRoot = root
	defer func() { workRoot = old }()

	dir := newRepo(t)
	f := &fakePC{t: t, bash: bash}
	a, out := testApp(f)
	ctx := context.Background()

	r, err := a.Run(ctx, RunOpts{Dir: dir, Cmd: []string{`echo "mode=$MODE quote=$Q"`}, Env: map[string]string{"MODE": "ci", "Q": "it's"}})
	if err != nil || r.Code != 0 || !strings.Contains(out.String(), "mode=ci quote=it's") {
		t.Fatalf("env run = %+v, %v\n%s", r, err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(root, ".box", r.Job+".repo")); strings.TrimSpace(string(b)) != "my-repo" {
		t.Errorf(".repo = %q", b)
	}

	out.Reset()
	r, err = a.Run(ctx, RunOpts{Dir: dir, Cmd: []string{"sleep 5"}, Timeout: time.Second})
	if err != nil || r.Code != exitTimeout {
		t.Fatalf("timeout run = %+v, %v\n%s", r, err, out)
	}

	// A job left running, then cancelled.
	out.Reset()
	r, err = a.Run(ctx, RunOpts{Dir: dir, Cmd: []string{"echo started; sleep 20"}, Wait: 2 * time.Second})
	if err != nil || r.State != stateDetached {
		t.Fatalf("long run = %+v, %v\n%s", r, err, out)
	}
	jobs, err := a.Jobs(ctx, "my-repo")
	if err != nil || len(jobs) != 3 {
		t.Fatalf("jobs = %+v, %v", jobs, err)
	}
	if jobs[0].Job != r.Job || jobs[0].State != jobRunning || jobs[1].Code == nil || *jobs[1].Code != exitTimeout || jobs[2].Code == nil || *jobs[2].Code != 0 {
		t.Errorf("jobs = %+v", jobs)
	}
	p, err := a.LogPage(ctx, r.Job, 2, 3)
	if err != nil || strings.ReplaceAll(p.Text, "\r", "") != "art" || p.NextOffset != 5 || !p.More {
		t.Errorf("page = %+v, %v", p, err)
	}
	cancelled, code, err := a.Cancel(ctx, r.Job)
	if err != nil || !cancelled || code != exitDetached {
		t.Fatalf("Cancel = %v, %d, %v", cancelled, code, err)
	}
	if j, _ := a.Jobs(ctx, "my-repo"); j[0].State != jobDone || *j[0].Code != exitDetached {
		t.Errorf("after cancel = %+v", j[0])
	}
	// Followers stop once cancel has written the exit code.
	if fin, err := a.Job(ctx, r.Job, 5*time.Second); err != nil || fin.State != stateDone || fin.Code != exitDetached {
		t.Errorf("Job after cancel = %+v, %v", fin, err)
	}
}
