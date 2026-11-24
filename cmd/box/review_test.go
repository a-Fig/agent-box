package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunNewNeverAdoptsABox(t *testing.T) {
	// Rand gives 0001 to the job, then 0002, 0003, ... to box names.
	f := &fakePC{t: t, exit: "0",
		insts: []instance{{Name: "my-repo-0002", Status: "Running"}},
		race:  map[string]bool{"my-repo-0003": true}}
	a, out := testApp(f)
	r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, New: true})
	if err != nil || r.Code != 0 || r.Box != "my-repo-0004" {
		t.Fatalf("Run = %+v, %v\n%s", r, err, out)
	}
	if n := f.names(); n != "my-repo-0002 my-repo-0003" {
		t.Errorf("boxes = %q, want the others' boxes untouched and ours gone", n)
	}
}

func TestRunRejectsReuseOfThrowawayBox(t *testing.T) {
	f := &fakePC{t: t, insts: []instance{{
		Name: "temporary", Status: "Running", Config: map[string]string{throwawayKey: "true"},
	}}}
	a, _ := testApp(f)
	r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Box: "temporary", Cmd: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "throwaway and cannot be reused") || r.Box != "temporary" {
		t.Fatalf("Run = %+v, %v", r, err)
	}
	if calls := f.callList(); calls != "list project" {
		t.Errorf("incus calls = %q, want only list and project", calls)
	}
}

func TestInterruptedThrowawayLaunchReportsName(t *testing.T) {
	f := &fakePC{t: t, launchErr: true}
	a, out := testApp(f)
	r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), New: true, Cmd: []string{"true"}})
	if err == nil || r.Box == "" || !strings.Contains(out.String(), "box: launching "+r.Box) {
		t.Fatalf("Run = %+v, %v\n%s", r, err, out)
	}
	if got := f.inst(r.Box).Config[throwawayKey]; got != "true" {
		t.Errorf("box %s marker = %q", r.Box, got)
	}
}

func TestRunNewKeepsBoxWhenPaused(t *testing.T) {
	// Frozen while following: the job isn't known to be done, so the box stays.
	f := &fakePC{t: t, freeze: true}
	a, out := testApp(f)
	r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, New: true})
	if err != nil || r.Code != exitPaused || r.State != statePaused {
		t.Fatalf("Run = %+v, %v\n%s", r, err, out)
	}
	if n := f.names(); n != "my-repo-0002" {
		t.Errorf("boxes = %q, want the throwaway box kept", n)
	}
	if c := f.inst("my-repo-0002").Config[throwawayKey]; c != "true" {
		t.Errorf("%s = %q", throwawayKey, c)
	}
}

type startsDuringIdleCheck struct {
	*fakePC
	job string
}

func (f *startsDuringIdleCheck) Run(ctx context.Context, c Cmd) (Result, error) {
	result, err := f.fakePC.Run(ctx, c)
	if c.Name == "incus" && isScript(c, "# running jobs") && f.job != "" {
		// A second caller starts its job and publishes its expiry after the scan.
		f.running, f.job = f.job, ""
	}
	return result, err
}

func TestReattachKeepsExpiryWhenAnotherJobStartsAfterScan(t *testing.T) {
	const box = "my-repo"
	const deadline = "2026-11-24T12:00:00Z"
	f := &fakePC{t: t, exit: "0", insts: []instance{{
		Name: box, Status: "Running", Config: map[string]string{expiresKey: deadline},
	}}}
	a, _ := testApp(f)
	a.R = &startsDuringIdleCheck{fakePC: f, job: box + ".20261123-120000-0002"}
	r, err := a.Job(context.Background(), box+".20261123-120000-0001", 0)
	if err != nil || r.State != stateDone {
		t.Fatalf("Job = %+v, %v", r, err)
	}
	if got := f.inst(box).Config[expiresKey]; got != deadline {
		t.Errorf("active expiry = %q, want %q", got, deadline)
	}
}

func TestJobRemovesThrowawayBoxOnceDone(t *testing.T) {
	f := &fakePC{t: t, block: true}
	a, out := testApp(f)
	r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"sleep 100"}, New: true, Wait: 10 * time.Millisecond})
	if err != nil || r.State != stateDetached || r.Job != "my-repo-0002.20261123-120000-0001" {
		t.Fatalf("Run = %+v, %v\n%s", r, err, out)
	}
	if f.names() != "my-repo-0002" {
		t.Fatal("detached run removed its box")
	}

	// Another job still runs there: keep the box.
	f.block, f.exit, f.running = false, "130\n", "my-repo-0002.20261123-120000-0009\n"
	if r, err := a.Job(context.Background(), r.Job, 0); err != nil || r.Code != 130 {
		t.Fatalf("Job = %+v, %v", r, err)
	}
	if f.names() != "my-repo-0002" {
		t.Error("removed a box with a running job")
	}

	// The payload's own 130 is an exit code, not a detach.
	f.running = ""
	r, err = a.Job(context.Background(), r.Job, 0)
	if err != nil || r.Code != 130 || r.State != stateDone {
		t.Fatalf("Job = %+v, %v", r, err)
	}
	if n := f.names(); n != "" {
		t.Errorf("boxes = %q, want the throwaway box gone", n)
	}

	// A box that box run --new didn't make stays.
	f.insts = []instance{{Name: "mine", Status: "Running"}}
	if _, err := a.Job(context.Background(), "mine.20261123-120000-0001", 0); err != nil {
		t.Fatal(err)
	}
	if f.names() != "mine" {
		t.Error("removed a box that isn't a throwaway")
	}
}

func TestRunNewExit130RemovesBox(t *testing.T) {
	f := &fakePC{t: t, exit: "130"}
	a, out := testApp(f)
	r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"exit 130"}, New: true})
	if err != nil || r.Code != 130 || r.State != stateDone {
		t.Fatalf("Run = %+v, %v\n%s", r, err, out)
	}
	if n := f.names(); n != "" {
		t.Errorf("boxes = %q, want the throwaway box gone", n)
	}
}

func TestRunNewRemovesBoxWhenSetupStops(t *testing.T) {
	f := &fakePC{t: t, pushHang: true}
	a, out := testApp(f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { // stop the run once it is pushing to the box it launched
		for !strings.Contains(f.callList(), "file") {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	if _, err := a.Run(ctx, RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, New: true}); err == nil {
		t.Fatalf("Run didn't fail\n%s", out)
	}
	if n := f.names(); n != "" {
		t.Errorf("boxes = %q, want the throwaway box gone", n)
	}
}

func TestRunPrintsJobBeforeStarting(t *testing.T) {
	f := &fakePC{t: t, insts: []instance{{Name: "my-repo", Status: "Running"}}, startErr: true}
	a, out := testApp(f)
	r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}})
	if err == nil {
		t.Fatal("Run didn't fail")
	}
	if r.Job != "my-repo.20261123-120000-0001" || !strings.Contains(out.String(), "job my-repo.20261123-120000-0001") {
		t.Errorf("job ID lost: %+v\n%s", r, out)
	}
}

func TestRunConcurrentLaunchOfRepoBox(t *testing.T) {
	f := &fakePC{t: t, exit: "0", race: map[string]bool{"my-repo": true}}
	a, out := testApp(f)
	r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}})
	if err != nil || r.Code != 0 || r.Box != "my-repo" {
		t.Fatalf("Run = %+v, %v\n%s", r, err, out)
	}
	if n := f.names(); n != "my-repo" {
		t.Errorf("boxes = %q", n)
	}
}

func TestRunWarnsAboutSubmodules(t *testing.T) {
	sub := newRepo(t)
	dir := newRepo(t)
	gitT(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "lib")
	f := &fakePC{t: t, insts: []instance{{Name: "my-repo", Status: "Running"}}, exit: "0"}
	a, out := testApp(f)
	if _, err := a.Run(context.Background(), RunOpts{Dir: dir, Cmd: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "submodules aren't synced") || !strings.Contains(out.String(), "lib") {
		t.Errorf("no submodule warning:\n%s", out)
	}

	out.Reset()
	if _, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "submodule") {
		t.Errorf("warned without submodules:\n%s", out)
	}
}

// fetchSnapshot unpacks a bundle into a fresh repo, as a box would.
func fetchSnapshot(t *testing.T, bundle, id string) string {
	clone := t.TempDir()
	gitT(t, clone, "init", "-q")
	gitT(t, clone, "fetch", "-q", bundle, "refs/agent-box/"+id)
	return clone
}

func TestSnapshotShallowClone(t *testing.T) {
	src := newRepo(t)
	gitT(t, src, "commit", "-qam", "second")
	dir := filepath.Join(t.TempDir(), "shallow")
	gitT(t, t.TempDir(), "clone", "-q", "--depth=1", "file://"+filepath.ToSlash(src), dir)
	write(t, filepath.Join(dir, "a.txt"), "three\n")

	bundle, err := snapshot(context.Background(), execRunner{}, dir, "id-1")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(bundle))
	clone := fetchSnapshot(t, bundle, "id-1")
	if a := gitT(t, clone, "show", "FETCH_HEAD:a.txt"); a != "three" {
		t.Errorf("a.txt = %q", a)
	}
}

func TestSnapshotIgnoresIndexFlags(t *testing.T) {
	dir := newRepo(t)
	write(t, filepath.Join(dir, "b.txt"), "b1\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-qm", "two files")
	gitT(t, dir, "update-index", "--assume-unchanged", "a.txt")
	gitT(t, dir, "update-index", "--skip-worktree", "a.txt")
	gitT(t, dir, "update-index", "--skip-worktree", "b.txt")
	write(t, filepath.Join(dir, "a.txt"), "a2\n")
	write(t, filepath.Join(dir, "b.txt"), "b2\n")

	bundle, err := snapshot(context.Background(), execRunner{}, dir, "id-1")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(bundle))
	clone := fetchSnapshot(t, bundle, "id-1")
	for f, want := range map[string]string{"a.txt": "a2", "b.txt": "b2"} {
		if got := gitT(t, clone, "show", "FETCH_HEAD:"+f); got != want {
			t.Errorf("%s = %q, want %q", f, got, want)
		}
	}
	// The user's own flags stay as they were.
	if v := gitT(t, dir, "ls-files", "-v", "a.txt", "b.txt"); v != "s a.txt\nS b.txt" {
		t.Errorf("ls-files -v = %q", v)
	}
}

func TestSnapshotIncludesForceAddedFile(t *testing.T) {
	dir := newRepo(t)
	write(t, filepath.Join(dir, "ignored.txt"), "staged\n")
	gitT(t, dir, "add", "-f", "ignored.txt")
	write(t, filepath.Join(dir, "ignored.txt"), "worktree\n")

	bundle, err := snapshot(context.Background(), execRunner{}, dir, "id-1")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(bundle))
	clone := fetchSnapshot(t, bundle, "id-1")
	if got := gitT(t, clone, "show", "FETCH_HEAD:ignored.txt"); got != "worktree" {
		t.Errorf("ignored.txt = %q, want worktree contents", got)
	}
	if got := gitT(t, dir, "show", ":ignored.txt"); got != "staged" {
		t.Errorf("original index = %q, want staged contents", got)
	}
}

func TestFollowWaitsForLauncherPID(t *testing.T) {
	bash := gitBash(t)
	old := workRoot
	workRoot = filepath.ToSlash(t.TempDir())
	defer func() { workRoot = old }()
	if err := os.Mkdir(filepath.Join(workRoot, ".box"), 0o755); err != nil {
		t.Fatal(err)
	}
	job := "sample.20261123-120000-0001"
	file := workRoot + "/.box/" + job
	write(t, file+".log", "started\n")
	// Publish the files when the follower first waits to model the launch race.
	publish := "sleep() { echo $$ > " + shq(file+".pid") + "; echo 0 > " + shq(file+".exit") + "; }\n"
	res, err := (execRunner{}).Run(context.Background(), Cmd{Name: bash, Args: []string{"-c", publish + followScript(job)}})
	if err != nil || res.Code != 0 || strings.TrimSpace(res.Stdout) != "started" {
		t.Fatalf("follow = %+v, %v", res, err)
	}
}
