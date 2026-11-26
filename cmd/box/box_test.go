package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/jobrun"
	"github.com/a-Fig/agent-box/internal/mode"
)

// fakePC stands in for the incus CLI. git commands run for real.
// With bash set, "incus exec" runs the script locally in that bash, so the
// shell scripts are tested end to end; otherwise exec answers from canned values.
type fakePC struct {
	t            *testing.T
	mu           sync.Mutex
	insts        []instance
	mode         string
	listErr      bool
	bash         string
	log          string          // canned follow output
	exit         string          // canned exit file
	running      string          // canned output of the running-jobs check
	race         map[string]bool // names another run launches just before ours
	block        bool            // follow waits until its context ends
	pushHang     bool            // file push waits until its context ends
	freeze       bool            // follow freezes the box and fails
	freezeStart  bool            // start exec freezes the box and fails as frozen
	freezeDrop   bool            // start exec freezes the box and drops the connection
	launchErr    bool            // the connection drops after creating an instance
	refuse       string          // launch fails with this Incus error, creating nothing
	launches     [][]string      // arguments of each launch
	startErr     bool            // the connection drops while starting the job
	calls        []string
	pushed       int
	project      map[string]string // project config other than the mode
	memory       string            // the default profile's limits.memory
	jobs         map[string]string // canned "# jobs" output by box
	script       string            // canned output of the page, cancel and checkout scripts
	scriptErr    string
	scriptRC     int
	pull         func(src, dst string) // file pull -r src dst
	pullErr      string                // canned stderr when file pull -r fails
	pulls        []string
	stdins       []string          // stdin of each launch ("" when none)
	launched     func(f *fakePC)   // runs after each successful launch, under f.mu
	stopFor      string            // follow stops the box with this user.box.stopped and fails
	stillRunning bool              // with stopFor: incus still lists the box Running
	remotes      string            // output of incus remote list --format json
	files        map[string]string // content of each pushed file, by its target
	fileReadErr  map[string]string // incus errors when reading individual files
	cancelled    bool              // the .cancelled probe finds the mark
	followErr    int               // this many follows fail with a transport error
	kept         map[string]bool   // boxes the kept-jobs check finds kept
	afterKept    func(box string)  // runs after each kept-jobs check, under f.mu
	refuseOnce   bool              // refuse applies to the first launch only
	digest       string            // canned digest.Script output the follow reports
	etagFails    int               // this many config sets fail as if another write came first
	gone         map[string]bool   // boxes exec can't find
}

func isScript(c Cmd, marker string) bool {
	return len(c.Args) > 0 && strings.HasPrefix(c.Args[len(c.Args)-1], marker)
}

func (f *fakePC) Run(ctx context.Context, c Cmd) (Result, error) {
	if c.Name == "git" {
		return execRunner{}.Run(ctx, c)
	}
	if c.Name != "incus" || len(c.Args) < 3 || c.Args[0] != "--project" || c.Args[1] != "agents" {
		f.t.Fatalf("unexpected command %s %q", c.Name, c.Args)
	}
	args := c.Args[2:]
	f.mu.Lock()
	f.calls = append(f.calls, args[0])
	hang := (f.block && isScript(c, "# follow")) || (f.pushHang && args[0] == "file")
	f.mu.Unlock()
	if hang {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch args[0] {
	case "list":
		if f.listErr {
			return Result{Stderr: "Error: connection refused", Code: 1}, nil
		}
		b, _ := json.Marshal(f.insts)
		return Result{Stdout: string(b)}, nil
	case "project":
		if args[1] == "info" {
			return Result{Stdout: f.project["info"]}, nil
		}
		if key := args[len(args)-1]; key != "user.agent-box.mode" {
			return Result{Stdout: f.project[key] + "\n"}, nil
		}
		return Result{Stdout: f.mode + "\n"}, nil
	case "profile":
		return Result{Stdout: f.memory + "\n"}, nil
	case "remote":
		if f.remotes == "" {
			return Result{Stderr: "Error: no remotes", Code: 1}, nil
		}
		return Result{Stdout: f.remotes}, nil
	case "launch":
		f.launches = append(f.launches, args)
		in := ""
		if c.Stdin != nil {
			b, _ := io.ReadAll(c.Stdin)
			in = string(b)
		}
		f.stdins = append(f.stdins, in)
		if f.refuse != "" {
			msg := f.refuse
			if f.refuseOnce {
				f.refuse = ""
			}
			return Result{Stderr: msg, Code: 1}, nil
		}
		name := strings.TrimPrefix(args[2], "pc:")
		if f.race[name] {
			f.insts = append(f.insts, instance{Name: name, Status: "Running"})
		}
		for _, in := range f.insts {
			if in.Name == name {
				return Result{Stderr: `Error: Failed creating instance record: Instance "` + name + `" already exists`, Code: 1}, nil
			}
		}
		cfg := map[string]string{}
		for i, a := range args {
			if a == "-c" {
				k, v, _ := strings.Cut(args[i+1], "=")
				cfg[k] = v
			}
		}
		f.insts = append(f.insts, instance{Name: name, Status: "Running", Config: cfg})
		if f.launched != nil {
			f.launched(f)
		}
		if f.launchErr {
			return Result{}, errors.New("connection reset")
		}
	case "start":
		f.inst(args[1]).Status = "Running"
	case "config":
		inst := f.inst(args[2])
		if args[1] == "get" {
			return Result{Stdout: inst.Config[args[3]] + "\n"}, nil
		}
		if args[1] != "set" {
			f.t.Fatalf("unexpected incus config arguments: %q", args)
		}
		if f.etagFails > 0 {
			f.etagFails--
			return Result{Code: 1, Stderr: "Error: ETag doesn't match: 1 vs 2"}, nil
		}
		if inst.Config == nil {
			inst.Config = make(map[string]string)
		}
		for _, arg := range args[3:] {
			k, v, ok := strings.Cut(arg, "=")
			if !ok {
				f.t.Fatalf("unexpected incus config argument: %q", arg)
			}
			inst.Config[k] = v
		}
	case "delete":
		name := strings.TrimPrefix(args[1], "pc:")
		for i := range f.insts {
			if f.insts[i].Name == name {
				f.insts = append(f.insts[:i], f.insts[i+1:]...)
				break
			}
		}
	case "file":
		if args[1] == "pull" && args[3] == "-" { // one file to stdout
			f.pulls = append(f.pulls, args[2])
			if msg := f.fileReadErr[args[2]]; msg != "" {
				return Result{Code: 1, Stderr: msg}, nil
			}
			if s, ok := f.files[args[2]]; ok {
				return Result{Stdout: s}, nil
			}
			return Result{Code: 1, Stderr: "Error: not found"}, nil
		}
		if args[1] == "pull" {
			f.pulls = append(f.pulls, args[3])
			if f.pullErr != "" {
				return Result{Code: 1, Stderr: f.pullErr}, nil
			}
			if f.pull != nil {
				f.pull(args[3], args[4])
			}
			return Result{}, nil
		}
		f.pushed++
		if b, err := os.ReadFile(args[3]); err != nil {
			f.t.Errorf("pushed file missing: %v", err)
		} else if strings.HasSuffix(args[4], ".meta") {
			if f.files == nil {
				f.files = map[string]string{}
			}
			f.files[args[4]] = string(b)
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
		if f.gone[strings.TrimPrefix(args[1], "pc:")] {
			return Result{Stderr: `Error: Failed to fetch instance "x" in project "agents": Instance not found`, Code: 1}, nil
		}
		for _, in := range f.insts {
			if "pc:"+in.Name == args[1] && in.Status == "Frozen" && isScript(c, "# checkout") {
				return Result{Stderr: "Error: Instance is frozen", Code: 1}, nil
			}
			if "pc:"+in.Name == args[1] && in.Status == "Stopped" {
				if c.Stderr != nil { // incus streams it there, as the real runner does
					io.WriteString(c.Stderr, "Error: Instance is not running\n")
					return Result{Code: 1}, nil
				}
				return Result{Stderr: "Error: Instance is not running", Code: 1}, nil
			}
		}
		if f.bash == "" {
			switch {
			case f.freezeStart && len(c.Args) > 0 && strings.Contains(c.Args[len(c.Args)-1], "setsid nohup"):
				f.inst(args[1]).Status = "Frozen"
				return Result{Stderr: "Error: Instance is frozen", Code: 1}, nil
			case f.freezeDrop && len(c.Args) > 0 && strings.Contains(c.Args[len(c.Args)-1], "setsid nohup"):
				f.inst(args[1]).Status = "Frozen"
				return Result{}, errors.New("connection reset")
			case isScript(c, "# follow"):
				if f.followErr > 0 {
					f.followErr--
					io.WriteString(c.Stdout, f.log) // what it printed before the connection went
					return Result{}, errors.New("connection reset")
				}
				if f.stopFor != "" {
					in := f.inst(args[1])
					in.Status = "Stopped"
					in.Config[mode.StoppedKey] = f.stopFor
					if f.stillRunning { // the mark, before incus lists the stop
						in.Status = "Running"
					}
					if c.Stderr != nil {
						io.WriteString(c.Stderr, "Error: Instance is not running\n") // incus writes it to the follow's stderr
					}
					return Result{Code: 1}, nil
				}
				if f.freeze {
					f.inst(args[1]).Status = "Frozen"
					return Result{Code: 1}, nil
				}
				io.WriteString(c.Stdout, f.log)
				if f.exit != "" {
					io.WriteString(c.Stderr, exitMarker+strings.TrimSpace(f.exit)+"\n")
				}
				if f.exit != "" && f.digest != "" {
					io.WriteString(c.Stderr, digestMarker+base64.StdEncoding.EncodeToString([]byte(f.digest))+"\n")
				}
			case isScript(c, "# cancelled"):
				if f.cancelled {
					return Result{Stdout: "yes\n"}, nil
				}
				return Result{}, nil
			case isScript(c, "# kept jobs"):
				box := strings.TrimPrefix(args[1], "pc:")
				kept := f.kept[box]
				if f.afterKept != nil {
					f.afterKept(box)
				}
				if kept {
					return Result{Stdout: "kept\n"}, nil
				}
				return Result{}, nil
			case isScript(c, "# running jobs"):
				return Result{Stdout: f.running}, nil
			case isScript(c, "# jobs"):
				return Result{Stdout: f.jobs[strings.TrimPrefix(args[1], "pc:")]}, nil
			case isScript(c, "# page"), isScript(c, "# cancel"), isScript(c, "# checkout"):
				return Result{Stdout: f.script, Stderr: f.scriptErr, Code: f.scriptRC}, nil
			case argv[0] == "cat":
				return Result{Stdout: f.exit}, nil
			case f.startErr:
				return Result{}, errors.New("connection reset")
			}
			return Result{}, nil
		}
		if argv[0] == "bash" {
			argv = argv[1:]
		} else {
			argv = []string{"-c", jobrun.ShellCommand(argv)}
		}
		cmd := Cmd{Name: f.bash, Args: argv, Stdout: c.Stdout, Stderr: c.Stderr, Env: c.Env}
		f.mu.Unlock()
		defer f.mu.Lock()
		return execRunner{}.Run(ctx, cmd)
	default:
		f.t.Fatalf("unexpected incus %q", args)
	}
	return Result{}, nil
}

func (f *fakePC) callList() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, " ")
}

func (f *fakePC) names() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n []string
	for _, in := range f.insts {
		n = append(n, in.Name)
	}
	return strings.Join(n, " ")
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
	r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"go", "test"}})
	if err != nil || r.Code != 3 {
		t.Fatalf("Run = %d, %v\n%s", r.Code, err, out)
	}
	if got := f.callList(); got != "list project start file file config config exec config config exec config exec config config exec" {
		t.Errorf("incus calls = %s", got)
	}
	if got := f.inst("my-repo").Config[expiresKey]; got != "2026-11-23T14:00:00Z" {
		t.Errorf("idle expiry = %q", got)
	}
	if !strings.Contains(out.String(), "ok\n") || !strings.Contains(out.String(), "job my-repo.20261123-120000-0001") {
		t.Errorf("output = %q", out)
	}
}

// A box a person extended (mode.HoldKey) keeps that time through a job's
// start and end: box writes the later of its own deadline and the hold (#198).
func TestRunKeepsTheHold(t *testing.T) {
	for hold, want := range map[string]string{
		"2026-11-23T18:00:00Z": "2026-11-23T18:00:00Z", // later than the 2h idle expiry
		"2026-11-23T13:00:00Z": "2026-11-23T14:00:00Z", // passed by then
	} {
		f := &fakePC{t: t, insts: []instance{{Name: "my-repo", Status: "Running", Config: map[string]string{mode.HoldKey: hold}}}, log: "ok\n", exit: "0\n"}
		a, out := testApp(f)
		if r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}}); err != nil || r.Code != 0 {
			t.Fatalf("Run = %d, %v\n%s", r.Code, err, out)
		}
		if got := f.inst("my-repo").Config[expiresKey]; got != want {
			t.Errorf("held until %s: expiry after the job = %q, want %q", hold, got, want)
		}
	}
}

// The tray setting a new box's CPU limits makes a config set lose its ETag
// race; box tries again rather than failing the run or warning.
func TestRunRetriesExpiryOnETagRace(t *testing.T) {
	f := &fakePC{t: t, insts: []instance{{Name: "my-repo", Status: "Running"}}, log: "ok\n", exit: "0\n", etagFails: 2}
	a, out := testApp(f)
	r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}})
	if err != nil || r.Code != 0 || strings.Contains(out.String(), "warning") {
		t.Fatalf("Run = %d, %v\n%s", r.Code, err, out)
	}
	if got := strings.Count(f.callList(), "config"); got != 11 { // pool lookup plus five hold/read and set attempts
		t.Errorf("config calls = %d: %s", got, f.callList())
	}
}

func TestRunNewBoxIsThrowaway(t *testing.T) {
	f := &fakePC{t: t, exit: "0"}
	a, out := testApp(f)
	r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, New: true, TTL: 2 * time.Hour})
	if err != nil || r.Code != 0 {
		t.Fatalf("Run = %d, %v\n%s", r.Code, err, out)
	}
	if got := f.callList(); got != "list project launch file file config config exec config config exec config exec config config exec exec delete" {
		t.Errorf("incus calls = %s", got)
	}
	if len(f.insts) != 0 {
		t.Errorf("throwaway box left behind: %+v", f.insts)
	}
}

func TestLaunchRetriesOnlyImageLookupEOF(t *testing.T) {
	const lookupEOF = `Error: Get "https://example.test:8443/1.0/images/abc?project=agents": EOF`
	for _, tt := range []struct {
		name, stderr string
		once         bool
		launches     int
		wantErr      bool
	}{
		{"transient image lookup", lookupEOF, true, 2, false},
		{"persistent image lookup", lookupEOF, false, 2, true},
		{"ambiguous create failure", `Error: Post "https://example.test:8443/1.0/instances?project=agents": EOF`, true, 1, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakePC{t: t, refuse: tt.stderr, refuseOnce: tt.once}
			a, _ := testApp(f)
			err := a.launch(context.Background(), "scratch", time.Hour, true, mustSize(t, mode.GPUSize))
			if (err != nil) != tt.wantErr || len(f.launches) != tt.launches {
				t.Fatalf("launch = %v, attempts = %d; want error = %v, attempts = %d", err, len(f.launches), tt.wantErr, tt.launches)
			}
			if tt.launches == 2 && (f.launches[0][2] != f.launches[1][2] || f.stdins[0] == "" || f.stdins[0] != f.stdins[1]) {
				t.Fatal("retry changed the instance name or GPU configuration")
			}
		})
	}
}

func TestLaunchThrowawayRetriesOnceOnDisallowedMetadata(t *testing.T) {
	const metaErr = "Error: Failed creating instance from image: Instance metadata contained disallowed entries: [/var/lib/incus/storage-pools/default/containers/agents_my-repo-0001/abc/rootfs/bin]"

	t.Run("recovers on fresh name", func(t *testing.T) {
		f := &fakePC{t: t, exit: "0", refuse: metaErr, refuseOnce: true}
		a, out := testApp(f)
		r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, New: true, TTL: 2 * time.Hour})
		if err != nil || r.Code != 0 {
			t.Fatalf("Run = %+v, %v\n%s", r, err, out)
		}
		if len(f.launches) != 2 {
			t.Fatalf("launches = %d, want 2", len(f.launches))
		}
		if first, second := f.launches[0][2], f.launches[1][2]; first == second {
			t.Errorf("retried with same name %q; want a fresh name", second)
		}
		if r.Box != strings.TrimPrefix(f.launches[1][2], "pc:") {
			t.Errorf("result box = %q, want %q", r.Box, f.launches[1][2])
		}
		if !strings.Contains(out.String(), "retrying once with a fresh box") {
			t.Errorf("missing retry notice in output:\n%s", out)
		}
	})

	t.Run("final try returns real metadata error", func(t *testing.T) {
		// 7 colliding names (0002..0008; 0001 is consumed by the job ID) leave
		// only the 8th try (try == 7); a metadata error there must return the
		// real Incus error instead of "no free name".
		insts := make([]instance, 7)
		for i := range insts {
			insts[i] = instance{Name: fmt.Sprintf("my-repo-%04x", i+2), Status: "Running"}
		}
		f := &fakePC{t: t, insts: insts, refuse: metaErr, refuseOnce: true}
		a, _ := testApp(f)
		_, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, New: true, TTL: 2 * time.Hour})
		if err == nil || !strings.Contains(err.Error(), "Instance metadata contained disallowed entries") {
			t.Fatalf("err = %v, want real disallowed metadata error", err)
		}
		if len(f.launches) != 1 || f.launches[0][2] != "pc:my-repo-0009" {
			t.Errorf("launches = %v, want single launch of pc:my-repo-0009", f.launches)
		}
	})

	t.Run("stops after one retry", func(t *testing.T) {
		f := &fakePC{t: t, refuse: metaErr}
		a, _ := testApp(f)
		_, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, New: true, TTL: 2 * time.Hour})
		if err == nil || !strings.Contains(err.Error(), "Instance metadata contained disallowed entries") {
			t.Fatalf("err = %v, want disallowed metadata error", err)
		}
		if len(f.launches) != 2 {
			t.Errorf("launches = %d, want 2 (1 initial + 1 retry)", len(f.launches))
		}
	})

	t.Run("does not retry unrelated launch errors", func(t *testing.T) {
		f := &fakePC{t: t, refuse: "Error: Failed creating instance: disk broken", refuseOnce: true}
		a, _ := testApp(f)
		_, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, New: true, TTL: 2 * time.Hour})
		if err == nil || !strings.Contains(err.Error(), "disk broken") {
			t.Fatalf("err = %v, want disk broken", err)
		}
		if len(f.launches) != 1 {
			t.Errorf("launches = %d, want 1", len(f.launches))
		}
	})

	t.Run("does not retry reusable repo box", func(t *testing.T) {
		f := &fakePC{t: t, refuse: metaErr, refuseOnce: true}
		a, _ := testApp(f)
		_, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}})
		if err == nil || !strings.Contains(err.Error(), "Instance metadata contained disallowed entries") {
			t.Fatalf("err = %v, want disallowed metadata error", err)
		}
		if !strings.Contains(err.Error(), "/var/lib/incus/storage-pools/default/containers/agents_my-repo") || !strings.Contains(err.Error(), "new: true") {
			t.Errorf("err missing recovery hint: %v", err)
		}
		if len(f.launches) != 1 {
			t.Errorf("launches = %d, want 1", len(f.launches))
		}
	})
}

func TestNewSetsExpiry(t *testing.T) {
	f := &fakePC{t: t}
	a, _ := testApp(f)
	name, err := a.New(context.Background(), "", 8*time.Hour, "", "")
	if err != nil || name != "box-0001" {
		t.Fatalf("New = %q, %v", name, err)
	}
	if exp := f.insts[0].Config["user.expires"]; exp != "2026-11-23T20:00:00Z" {
		t.Errorf("user.expires = %q", exp)
	}
	if hold := f.insts[0].Config[mode.HoldKey]; hold != "2026-11-23T20:00:00Z" {
		t.Errorf("box new's TTL doesn't hold the box: %s = %q", mode.HoldKey, hold)
	}
	if _, err := a.New(context.Background(), "Bad;name", 0, "", ""); err == nil {
		t.Error("bad name accepted")
	}
	if code := run(context.Background(), a, []string{"new", "idle"}, nil); code != 0 {
		t.Fatalf("box new returned %d", code)
	}
	if got := f.inst("idle").Config[expiresKey]; got != "2026-11-23T14:00:00Z" {
		t.Errorf("default expiry = %q", got)
	}
}

func TestDetachedRunAndReattachRefreshExpiry(t *testing.T) {
	f := &fakePC{t: t, block: true}
	a, out := testApp(f)
	started := a.Now()
	r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, Wait: 10 * time.Millisecond})
	if err != nil || r.State != stateDetached {
		t.Fatalf("Run = %+v, %v\n%s", r, err, out)
	}
	if got := f.inst(r.Box).Config[expiresKey]; got != "2026-11-24T12:00:00Z" {
		t.Errorf("detached job expiry = %q", got)
	}
	a.Now = func() time.Time { return started.Add(3 * time.Hour) }
	f.block, f.exit = false, "0"
	finished, err := a.Job(context.Background(), r.Job, 0)
	if err != nil || finished.State != stateDone {
		t.Fatalf("Job = %+v, %v\n%s", finished, err, out)
	}
	if got := f.inst(r.Box).Config[expiresKey]; got != "2026-11-23T17:00:00Z" {
		t.Errorf("reattached job idle expiry = %q", got)
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
			r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}})
			if err != nil || r.Code != exitPaused {
				t.Fatalf("Run = %d, %v\n%s", r.Code, err, out)
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
	if got := jobrun.ShellCommand([]string{"go test ./... && echo hi"}); got != "go test ./... && echo hi" {
		t.Errorf("one arg = %q", got)
	}
	if got := jobrun.ShellCommand([]string{"echo", "it's", "a b"}); got != `'echo' 'it'\''s' 'a b'` {
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

	r, err := a.Run(ctx, RunOpts{Dir: dir, Cmd: []string{"cat a.txt untracked.txt; test ! -e ignored.txt || exit 9; exit 7"}})
	if err != nil || r.Code != 7 {
		t.Fatalf("first run = %d, %v\n%s", r.Code, err, out)
	}
	lf := func() string { return strings.ReplaceAll(out.String(), "\r", "") } // Windows git checks out CRLF
	if !strings.Contains(lf(), "two\nnew\n") {
		t.Errorf("output = %q", out)
	}

	// Second run fetches into the existing checkout and drops stale files.
	os.Remove(filepath.Join(dir, "untracked.txt"))
	out.Reset()
	r, err = a.Run(ctx, RunOpts{Dir: dir, Cmd: []string{"test", "!", "-e", "untracked.txt"}})
	if err != nil || r.Code != 0 {
		t.Fatalf("second run = %d, %v\n%s", r.Code, err, out)
	}
	if n := strings.Count(f.callList(), "launch"); n != 1 {
		t.Errorf("launched %d times, want 1", n)
	}
	// Run 2 replaced /work/my-repo, so pulling from run 1 refuses clearly (#197).
	if _, err := a.Pull(ctx, "my-repo.20261123-120000-0001", "a.txt", t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "was replaced by job my-repo.20261123-120000-0002") {
		t.Errorf("Pull from replaced job 1 = %v, want replaced error", err)
	}

	// Reattaching to a finished job replays its log and exit code.
	out.Reset()
	r, err = a.Job(ctx, "my-repo.20261123-120000-0001", 0)
	if err != nil || r.Code != 7 || !strings.Contains(lf(), "two\nnew\n") {
		t.Fatalf("job = %d, %v\n%s", r.Code, err, out)
	}
	if _, err := a.Job(ctx, "my-repo.20261123-120000-ffff", 0); err == nil {
		t.Error("unknown job didn't fail")
	}

	// The payload doesn't inherit the checkout lock's FD 9, so a background
	// child it leaves behind can't hold the lock.
	out.Reset()
	r, err = a.Run(ctx, RunOpts{Dir: dir, Cmd: []string{"{ : >&9; } 2>/dev/null && exit 9; exit 0"}})
	if err != nil || r.Code != 0 {
		t.Fatalf("fd 9 run = %d, %v\n%s", r.Code, err, out)
	}

	// A throwaway box goes once its job is done and nothing else runs there.
	out.Reset()
	r, err = a.Run(ctx, RunOpts{Dir: dir, Cmd: []string{"exit 0"}, New: true})
	if err != nil || r.Code != 0 || r.State != stateDone {
		t.Fatalf("--new run = %+v, %v\n%s", r, err, out)
	}
	if n := f.names(); n != "my-repo" {
		t.Errorf("boxes = %q, want the throwaway one gone", n)
	}

	// The CLI keeps it 30 minutes, as box mcp does, for box pull (#78).
	if newApp().KeepEnded != endedKeep {
		t.Error("the CLI doesn't keep throwaways")
	}
	a.KeepEnded = endedKeep
	out.Reset()
	r, err = a.Run(ctx, RunOpts{Dir: dir, Cmd: []string{"exit 0"}, New: true})
	if err != nil || r.Code != 0 || !strings.Contains(f.names(), r.Box) || !strings.Contains(out.String(), "box: kept "+r.Box+" for box pull for up to 30 minutes") {
		t.Fatalf("kept --new run = %+v, %v, boxes %q\n%s", r, err, f.names(), out)
	}
	a.KeepEnded = 0
	a.Rm(ctx, r.Box)

	// Reattaching to a finished job doesn't wait on its PID, which may
	// belong to another process by now.
	job := "my-repo.20261123-120000-0001"
	pidFile := filepath.Join(root, ".box", job+".pid")
	prevPID, _ := os.ReadFile(pidFile)
	other := exec.Command(bash, "-c", `echo $$ > "$1.tmp" && mv "$1.tmp" "$1"; sleep 20`, "-", filepath.ToSlash(pidFile))
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer other.Process.Kill()
	for i := 0; ; i++ {
		if b, _ := os.ReadFile(pidFile); len(b) > 0 && string(b) != string(prevPID) {
			break
		}
		if i > 100 {
			t.Fatal("pid file not replaced")
		}
		time.Sleep(50 * time.Millisecond)
	}
	out.Reset()
	r, err = a.Job(ctx, job, 5*time.Second)
	if err != nil || r.Code != 7 || r.State != stateDone {
		t.Fatalf("job with a reused pid = %+v, %v\n%s", r, err, out)
	}

	// A job whose wrapper died without an exit code is reported, not followed forever.
	lost := filepath.Join(root, ".box", "my-repo.20261123-120000-00ff")
	write(t, lost+".log", "partial\n")
	write(t, lost+".pid", "999999\n")
	out.Reset()
	r, err = a.Job(ctx, "my-repo.20261123-120000-00ff", 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "died") {
		t.Fatalf("lost job = %+v, %v\n%s", r, err, out)
	}
}

func TestRunKeepsJobExpiryWhileAnotherJobRuns(t *testing.T) {
	f := &fakePC{t: t, insts: []instance{{Name: "my-repo", Status: "Running"}}, log: "ok\n", exit: "0\n", running: "my-repo.20261123-110000-beef\n"}
	a, out := testApp(f)
	if _, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out)
	}
	if got := f.inst("my-repo").Config[expiresKey]; got != "2026-11-24T12:00:00Z" {
		t.Errorf("expiry = %q, want the 24h job expiry kept", got)
	}
}

func TestVersion(t *testing.T) {
	a, out := testApp(&fakePC{})
	if c := run(context.Background(), a, []string{"--version"}, nil); c != 0 || !strings.HasPrefix(out.String(), "box ") {
		t.Fatalf("--version: %d %q", c, out)
	}
}

// A follow cut off in the middle of the digest line leaves no base64 in the
// output; the exit it read before still stands.
func TestExitFilterDropsACutMarkerLine(t *testing.T) {
	var out strings.Builder
	ef := &exitFilter{w: &out}
	io.WriteString(ef, "box: a warning\n"+exitMarker+"1\n"+digestMarker+"AAAA")
	ef.flush()
	if code, ok := ef.exit(); !ok || code != "1" || out.String() != "box: a warning\n" || ef.digest() != "" {
		t.Fatalf("exit %q %v, out %q, digest %q", code, ok, out.String(), ef.digest())
	}
}

// copyFile is the fake incus file pull.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
