package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakePC stands in for the incus CLI. git commands run for real.
// With bash set, "incus exec" runs the script locally in that bash, so the
// shell scripts are tested end to end; otherwise exec answers from canned values.
type fakePC struct {
	t       *testing.T
	insts   []instance
	mode    string
	listErr bool
	bash    string
	log     string // canned tail output
	exit    string // canned exit file
	calls   []string
	pushed  int
}

func (f *fakePC) Run(ctx context.Context, c Cmd) (Result, error) {
	if c.Name == "git" {
		return execRunner{}.Run(ctx, c)
	}
	if c.Name != "incus" || len(c.Args) < 3 || c.Args[0] != "--project" || c.Args[1] != "agents" {
		f.t.Fatalf("unexpected command %s %q", c.Name, c.Args)
	}
	args := c.Args[2:]
	f.calls = append(f.calls, args[0])
	switch args[0] {
	case "list":
		if f.listErr {
			return Result{Stderr: "Error: connection refused", Code: 1}, nil
		}
		b, _ := json.Marshal(f.insts)
		return Result{Stdout: string(b)}, nil
	case "project":
		return Result{Stdout: f.mode + "\n"}, nil
	case "launch":
		cfg := map[string]string{}
		for i, a := range args {
			if a == "-c" {
				k, v, _ := strings.Cut(args[i+1], "=")
				cfg[k] = v
			}
		}
		f.insts = append(f.insts, instance{Name: strings.TrimPrefix(args[2], "pc:"), Status: "Running", Config: cfg})
	case "start":
		f.inst(args[1]).Status = "Running"
	case "delete":
		name := strings.TrimPrefix(args[1], "pc:")
		for i := range f.insts {
			if f.insts[i].Name == name {
				f.insts = append(f.insts[:i], f.insts[i+1:]...)
				break
			}
		}
	case "file":
		f.pushed++
		if _, err := os.Stat(args[3]); err != nil {
			f.t.Errorf("pushed bundle missing: %v", err)
		}
		if f.bash != "" {
			_, dst, _ := strings.Cut(args[4], ":")
			dst = dst[strings.Index(dst, workRoot):]
			os.MkdirAll(filepath.Dir(dst), 0o755)
			if err := copyFile(args[3], dst); err != nil {
				f.t.Fatal(err)
			}
		}
	case "exec":
		argv := args[3:] // after "exec pc:box --"
		if f.bash == "" {
			switch {
			case strings.Contains(strings.Join(argv, " "), "tail -n"):
				io.WriteString(c.Stdout, f.log)
			case argv[0] == "cat":
				return Result{Stdout: f.exit}, nil
			}
			return Result{}, nil
		}
		if argv[0] == "bash" {
			argv = argv[1:]
		} else {
			argv = []string{"-c", shellCommand(argv)}
		}
		cmd := Cmd{Name: f.bash, Args: argv, Stdout: c.Stdout, Stderr: c.Stderr, Env: c.Env}
		return execRunner{}.Run(ctx, cmd)
	default:
		f.t.Fatalf("unexpected incus %q", args)
	}
	return Result{}, nil
}

func (f *fakePC) inst(target string) *instance {
	name := strings.TrimPrefix(target, "pc:")
	for i := range f.insts {
		if f.insts[i].Name == name {
			return &f.insts[i]
		}
	}
	f.t.Fatalf("no instance %s", name)
	return nil
}

func testApp(f *fakePC) (*App, *bytes.Buffer) {
	var out bytes.Buffer
	n := 0
	return &App{
		R: f, Remote: "pc", Project: "agents", Image: "agent-base",
		Out: &out, Err: &out,
		Now:  func() time.Time { return time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC) },
		Rand: func() string { n++; return fmt.Sprintf("%04x", n) },
	}, &out
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRepo makes a repo with one commit, one change to a tracked file, one
// untracked file and one ignored file.
func newRepo(t *testing.T) string {
	dir := filepath.Join(t.TempDir(), "My_Repo")
	os.Mkdir(dir, 0o755)
	gitT(t, dir, "init", "-q")
	write(t, filepath.Join(dir, ".gitignore"), "ignored.txt\n")
	write(t, filepath.Join(dir, "a.txt"), "one\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "init")
	write(t, filepath.Join(dir, "a.txt"), "two\n")
	write(t, filepath.Join(dir, "untracked.txt"), "new\n")
	write(t, filepath.Join(dir, "ignored.txt"), "secret\n")
	return dir
}

func TestBoxName(t *testing.T) {
	for in, want := range map[string]string{
		"agent-box":             "agent-box",
		"My_Repo":               "my-repo",
		"3d-stuff":              "box-3d-stuff",
		"":                      "box",
		strings.Repeat("a", 70): strings.Repeat("a", 50),
	} {
		if got := boxName(in); got != want {
			t.Errorf("boxName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOpenRepoUsesOriginName(t *testing.T) {
	dir := newRepo(t)
	rp, err := openRepo(context.Background(), execRunner{}, dir)
	if err != nil || rp.Name != "my-repo" {
		t.Fatalf("openRepo = %+v, %v", rp, err)
	}
	gitT(t, dir, "remote", "add", "origin", "git@github.com:a-Fig/agent-box.git")
	if rp, _ := openRepo(context.Background(), execRunner{}, dir); rp.Name != "agent-box" {
		t.Fatalf("name = %q, want agent-box", rp.Name)
	}
}

func TestSnapshot(t *testing.T) {
	dir := newRepo(t)
	ctx := context.Background()
	head := gitT(t, dir, "rev-parse", "HEAD")
	before := gitT(t, dir, "status", "--porcelain")

	bundle, err := snapshot(ctx, execRunner{}, dir, "my-repo.20261123-120000-0001")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(bundle))

	if after := gitT(t, dir, "status", "--porcelain"); after != before {
		t.Errorf("user's index changed:\n%s\nwant:\n%s", after, before)
	}
	if refs := gitT(t, dir, "for-each-ref", "refs/agent-box"); refs != "" {
		t.Errorf("temporary ref left behind: %s", refs)
	}

	// Unpack the bundle elsewhere and look at the snapshot.
	clone := t.TempDir()
	gitT(t, clone, "init", "-q")
	gitT(t, clone, "fetch", "-q", bundle, "refs/agent-box/my-repo.20261123-120000-0001")
	if p := gitT(t, clone, "rev-parse", "FETCH_HEAD^"); p != head {
		t.Errorf("parent = %s, want HEAD %s", p, head)
	}
	files := gitT(t, clone, "ls-tree", "--name-only", "FETCH_HEAD")
	if files != ".gitignore\na.txt\nuntracked.txt" {
		t.Errorf("snapshot files = %q", files)
	}
	if a := gitT(t, clone, "show", "FETCH_HEAD:a.txt"); a != "two" {
		t.Errorf("a.txt = %q, want the uncommitted change", a)
	}
}

func TestSnapshotWithoutCommits(t *testing.T) {
	dir := t.TempDir()
	gitT(t, dir, "init", "-q")
	write(t, filepath.Join(dir, "x"), "x\n")
	bundle, err := snapshot(context.Background(), execRunner{}, dir, "x.20261123-120000-0001")
	if err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Dir(bundle))
	if s := gitT(t, dir, "status", "--porcelain"); s != "?? x" {
		t.Errorf("status = %q", s)
	}
}

func TestRunReusesRepoBox(t *testing.T) {
	f := &fakePC{t: t, insts: []instance{{Name: "my-repo", Status: "Stopped"}}, log: "ok\n", exit: "3\n"}
	a, out := testApp(f)
	code, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"go", "test"}})
	if err != nil || code != 3 {
		t.Fatalf("Run = %d, %v\n%s", code, err, out)
	}
	if got := strings.Join(f.calls, " "); got != "list project start file exec exec exec" {
		t.Errorf("incus calls = %s", got)
	}
	if !strings.Contains(out.String(), "ok\n") || !strings.Contains(out.String(), "job my-repo.20261123-120000-0001") {
		t.Errorf("output = %q", out)
	}
}

func TestRunNewBoxIsThrowaway(t *testing.T) {
	f := &fakePC{t: t, exit: "0"}
	a, out := testApp(f)
	code, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, New: true, TTL: 2 * time.Hour})
	if err != nil || code != 0 {
		t.Fatalf("Run = %d, %v\n%s", code, err, out)
	}
	if got := strings.Join(f.calls, " "); got != "list project launch file exec exec exec delete" {
		t.Errorf("incus calls = %s", got)
	}
	if len(f.insts) != 0 {
		t.Errorf("throwaway box left behind: %+v", f.insts)
	}
}

func TestNewSetsExpiry(t *testing.T) {
	f := &fakePC{t: t}
	a, _ := testApp(f)
	name, err := a.New(context.Background(), "", 8*time.Hour)
	if err != nil || name != "box-0001" {
		t.Fatalf("New = %q, %v", name, err)
	}
	if exp := f.insts[0].Config["user.expires"]; exp != "2026-11-23T20:00:00Z" {
		t.Errorf("user.expires = %q", exp)
	}
	if _, err := a.New(context.Background(), "Bad;name", 0); err == nil {
		t.Error("bad name accepted")
	}
}

func TestRunPaused(t *testing.T) {
	for name, f := range map[string]*fakePC{
		"frozen box":  {insts: []instance{{Name: "my-repo", Status: "Frozen"}}},
		"my pc mode":  {mode: "mypc"},
		"unreachable": {listErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			f.t = t
			a, out := testApp(f)
			code, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}})
			if err != nil || code != exitPaused {
				t.Fatalf("Run = %d, %v\n%s", code, err, out)
			}
			if f.pushed != 0 {
				t.Error("pushed to a paused PC")
			}
		})
	}
}

func TestJobRejectsBadID(t *testing.T) {
	a, _ := testApp(&fakePC{t: t})
	for _, id := range []string{"", "x", "box.1; rm -rf /", "../x.20261123-120000-01"} {
		if _, err := a.Job(context.Background(), id, 0); err == nil {
			t.Errorf("Job(%q) accepted", id)
		}
	}
}

func TestShellCommand(t *testing.T) {
	if got := shellCommand([]string{"go test ./... && echo hi"}); got != "go test ./... && echo hi" {
		t.Errorf("one arg = %q", got)
	}
	if got := shellCommand([]string{"echo", "it's", "a b"}); got != `'echo' 'it'\''s' 'a b'` {
		t.Errorf("argv = %q", got)
	}
}

// gitBash finds a bash that isn't WSL's launcher, and shims flock and setsid
// where they're missing (Git for Windows). It skips the test if there's none.
func gitBash(t *testing.T) string {
	bash, err := exec.LookPath("bash")
	if err != nil || strings.Contains(strings.ToLower(bash), `\system32\`) {
		t.Skip("no usable bash")
	}
	shims := t.TempDir()
	for name, body := range map[string]string{"flock": "exit 0", "setsid": `exec "$@"`} {
		if _, err := exec.LookPath(name); err != nil {
			write(t, filepath.Join(shims, name), "#!/bin/sh\n"+body+"\n")
		}
	}
	t.Setenv("PATH", shims+string(os.PathListSeparator)+os.Getenv("PATH"))
	return bash
}

// TestRunEndToEnd runs the real start and follow scripts in a local bash.
func TestRunEndToEnd(t *testing.T) {
	bash := gitBash(t)
	root := filepath.ToSlash(t.TempDir()) // C:/... works in Git Bash too
	old := workRoot
	workRoot = root
	defer func() { workRoot = old }()

	dir := newRepo(t)
	f := &fakePC{t: t, bash: bash}
	a, out := testApp(f)
	ctx := context.Background()

	code, err := a.Run(ctx, RunOpts{Dir: dir, Cmd: []string{"cat a.txt untracked.txt; test ! -e ignored.txt || exit 9; exit 7"}})
	if err != nil || code != 7 {
		t.Fatalf("first run = %d, %v\n%s", code, err, out)
	}
	lf := func() string { return strings.ReplaceAll(out.String(), "\r", "") } // Windows git checks out CRLF
	if !strings.Contains(lf(), "two\nnew\n") {
		t.Errorf("output = %q", out)
	}

	// Second run fetches into the existing checkout and drops stale files.
	os.Remove(filepath.Join(dir, "untracked.txt"))
	out.Reset()
	code, err = a.Run(ctx, RunOpts{Dir: dir, Cmd: []string{"test", "!", "-e", "untracked.txt"}})
	if err != nil || code != 0 {
		t.Fatalf("second run = %d, %v\n%s", code, err, out)
	}
	if n := strings.Count(strings.Join(f.calls, " "), "launch"); n != 1 {
		t.Errorf("launched %d times, want 1", n)
	}

	// Reattaching to a finished job replays its log and exit code.
	out.Reset()
	code, err = a.Job(ctx, "my-repo.20261123-120000-0001", 0)
	if err != nil || code != 7 || !strings.Contains(lf(), "two\nnew\n") {
		t.Fatalf("job = %d, %v\n%s", code, err, out)
	}
	if _, err := a.Job(ctx, "my-repo.20261123-120000-ffff", 0); err == nil {
		t.Error("unknown job didn't fail")
	}
}
