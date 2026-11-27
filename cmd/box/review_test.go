package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/mode"
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
	if err == nil || r.Box == "" || !strings.Contains(out.String(), "box: launching "+r.Box+" failed") {
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

	snap, err := snapshot(context.Background(), execRunner{}, dir, "id-1")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(snap.bundle))
	clone := fetchSnapshot(t, snap.bundle, "id-1")
	requireSourceRevision(t, snap.source, gitT(t, dir, "rev-parse", "HEAD"), true, gitT(t, clone, "rev-parse", "FETCH_HEAD"))
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

	snap, err := snapshot(context.Background(), execRunner{}, dir, "id-1")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(snap.bundle))
	clone := fetchSnapshot(t, snap.bundle, "id-1")
	requireSourceRevision(t, snap.source, gitT(t, dir, "rev-parse", "HEAD"), true, gitT(t, clone, "rev-parse", "FETCH_HEAD"))
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

	snap, err := snapshot(context.Background(), execRunner{}, dir, "id-1")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(snap.bundle))
	clone := fetchSnapshot(t, snap.bundle, "id-1")
	requireSourceRevision(t, snap.source, gitT(t, dir, "rev-parse", "HEAD"), true, gitT(t, clone, "rev-parse", "FETCH_HEAD"))
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

// pausesOnFollow pauses the PC shortly after the job's follow starts.
type pausesOnFollow struct {
	*fakePC
	pause func(f *fakePC)
}

func (p *pausesOnFollow) Run(ctx context.Context, c Cmd) (Result, error) {
	if c.Name == "incus" && isScript(c, "# follow") {
		go func() {
			time.Sleep(30 * time.Millisecond)
			p.mu.Lock()
			defer p.mu.Unlock()
			p.pause(p.fakePC)
		}()
	}
	return p.fakePC.Run(ctx, c)
}

const pauseTestJob = "my-repo.20261123-120000-0001"

func freezeFirst(f *fakePC) { f.insts[0].Status = "Frozen" }

// hangsAfterFollow lets the job's follow finish, then freezes the box: every
// later exec waits for its context to end.
type hangsAfterFollow struct {
	*fakePC
	frozen bool
	// followErr is what a follow that was cancelled while it drained returns.
	followErr error
}

func (h *hangsAfterFollow) Run(ctx context.Context, c Cmd) (Result, error) {
	h.mu.Lock()
	hang := h.frozen && c.Name == "incus" && len(c.Args) > 2 && c.Args[2] == "exec"
	h.mu.Unlock()
	if hang {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}
	res, err := h.fakePC.Run(ctx, c)
	if c.Name == "incus" && isScript(c, "# follow") {
		h.mu.Lock()
		h.frozen = true
		h.mu.Unlock()
		if h.followErr != nil {
			return res, h.followErr
		}
	}
	return res, err
}

// stallsFirstList makes the first list call wait for its context to end.
type stallsFirstList struct {
	Runner
	stalled atomic.Bool
}

func (s *stallsFirstList) Run(ctx context.Context, c Cmd) (Result, error) {
	if c.Name == "incus" && len(c.Args) > 2 && c.Args[2] == "list" && s.stalled.CompareAndSwap(false, true) {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}
	return s.Runner.Run(ctx, c)
}

func TestAttachMyPCAloneIsNotAPause(t *testing.T) {
	// The tray writes My PC before it freezes, and a freeze can fail.
	f := &fakePC{t: t, block: true, mode: "mypc", insts: []instance{{Name: "my-repo", Status: "Running"}}}
	a, out := testApp(f)
	a.PausePoll = time.Millisecond
	r, err := a.Job(context.Background(), pauseTestJob, 60*time.Millisecond)
	if err != nil || r.State != stateDetached || r.Code != exitDetached {
		t.Fatalf("Job = %+v, %v\n%s", r, err, out)
	}
}

func TestAttachReportsBoxFrozenMidRun(t *testing.T) {
	f := &fakePC{t: t, block: true, insts: []instance{{Name: "my-repo", Status: "Running"}}}
	a, out := testApp(f)
	a.PausePoll = 5 * time.Millisecond
	a.R = &pausesOnFollow{fakePC: f, pause: freezeFirst}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	r, err := a.Job(ctx, pauseTestJob, 0)
	if err != nil || r.State != statePaused || r.Code != exitPaused {
		t.Fatalf("Job = %+v, %v\n%s", r, err, out)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v, want a prompt return", d)
	}
	want := "box: job " + pauseTestJob + " is paused (the PC's owner is using it). It resumes if they switch back within 15 minutes (4 hours once its memory is moved to swap); after that the box is stopped and the job lost. Reattach later with: box job " + pauseTestJob
	if !strings.Contains(out.String(), want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
	if strings.Contains(out.String(), "still running") {
		t.Errorf("reported detached:\n%s", out)
	}
}

func TestRunReportsBoxFrozenMidRun(t *testing.T) {
	f := &fakePC{t: t, block: true, insts: []instance{{Name: "my-repo", Status: "Running"}}}
	a, out := testApp(f)
	a.PausePoll = 5 * time.Millisecond
	a.R = &pausesOnFollow{fakePC: f, pause: freezeFirst}
	r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}})
	if err != nil || r.State != statePaused || r.Code != exitPaused || r.Job == "" {
		t.Fatalf("Run = %+v, %v\n%s", r, err, out)
	}
	if !strings.Contains(out.String(), "box: job "+r.Job+" is paused (the PC's owner") {
		t.Errorf("message lacks the job ID:\n%s", out)
	}
}

func TestRunReportsBoxFrozenAtStart(t *testing.T) {
	// Incus explicitly refused the start exec, so no job ever started:
	// the run is refused like an already-frozen box, with no job ID.
	f := &fakePC{t: t, freezeStart: true, insts: []instance{{Name: "my-repo", Status: "Running"}}}
	a, out := testApp(f)
	r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}})
	if err != nil || r.State != statePaused || r.Code != exitPaused || r.Job != "" {
		t.Fatalf("Run = %+v, %v\n%s", r, err, out)
	}
	if !strings.Contains(out.String(), "my-repo is paused; nothing started") || strings.Contains(out.String(), "Instance is frozen") {
		t.Errorf("output lacks the nothing-started message or leaks incus's error:\n%s", out)
	}
}

func TestRunNewFrozenAtStartRemovesThrowaway(t *testing.T) {
	// The explicit refusal means no job started, so the throwaway the run
	// just launched goes with it instead of holding a slot unused.
	f := &fakePC{t: t, freezeStart: true}
	a, out := testApp(f)
	r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, New: true})
	if err != nil || r.State != statePaused || r.Code != exitPaused || r.Job != "" {
		t.Fatalf("Run = %+v, %v\n%s", r, err, out)
	}
	if len(f.insts) != 0 {
		t.Errorf("throwaway left behind: %s", f.names())
	}
}

func TestRunKeepsJobWhenFrozenStartFailureIsAmbiguous(t *testing.T) {
	// The connection dropped, so the start script may already have run:
	// the job ID stays, for a job that may resume when the box thaws.
	f := &fakePC{t: t, freezeDrop: true, insts: []instance{{Name: "my-repo", Status: "Running"}}}
	a, out := testApp(f)
	r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}})
	if err != nil || r.State != statePaused || r.Code != exitPaused || r.Job == "" {
		t.Fatalf("Run = %+v, %v\n%s", r, err, out)
	}
	if !strings.Contains(out.String(), "is paused (the PC's owner is using it)") {
		t.Errorf("output lacks the paused message:\n%s", out)
	}
}

func TestAttachReportsFrozenGPUBox(t *testing.T) {
	f := &fakePC{t: t, block: true, insts: []instance{{Name: "my-repo", Status: "Running", Config: map[string]string{mode.SizeKey: mode.GPUSize}}}}
	a, out := testApp(f)
	a.PausePoll = 5 * time.Millisecond
	a.R = &pausesOnFollow{fakePC: f, pause: freezeFirst}
	r, err := a.Job(context.Background(), pauseTestJob, 0)
	if err != nil || r.State != statePaused || r.Code != exitPaused {
		t.Fatalf("Job = %+v, %v\n%s", r, err, out)
	}
	if !strings.Contains(out.String(), "The tray may still stop it, which loses the job") {
		t.Errorf("output lacks the GPU wording:\n%s", out)
	}
}

func TestAttachConfirmedStopOfGPUBoxKeepsItsMessage(t *testing.T) {
	f := &fakePC{t: t, stopFor: string(mode.MyPC), insts: []instance{{Name: "my-repo", Status: "Running", Config: map[string]string{mode.SizeKey: mode.GPUSize}}}}
	a, out := testApp(f)
	r, err := a.Job(context.Background(), pauseTestJob, 0)
	if err != nil || r.State != statePaused || !strings.Contains(out.String(), "the GPU box was stopped because the PC's owner switched it to My PC") {
		t.Fatalf("Job = %+v, %v\n%s", r, err, out)
	}
}

func TestAttachRetriesAStalledPoll(t *testing.T) {
	f := &fakePC{t: t, block: true, insts: []instance{{Name: "my-repo", Status: "Running"}}}
	a, out := testApp(f)
	a.PausePoll = 5 * time.Millisecond
	a.PollTimeout = 20 * time.Millisecond
	s := &stallsFirstList{Runner: &pausesOnFollow{fakePC: f, pause: freezeFirst}}
	a.R = s
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := a.Job(ctx, pauseTestJob, 0)
	if err != nil || r.State != statePaused {
		t.Fatalf("Job = %+v, %v\n%s", r, err, out)
	}
	if !s.stalled.Load() {
		t.Error("the first poll never stalled")
	}
}

func TestAttachExitCodeSurvivesAFreezeAfterCompletion(t *testing.T) {
	f := &fakePC{t: t, exit: "42\n", insts: []instance{{Name: "my-repo", Status: "Running"}}}
	a, out := testApp(f)
	a.ProbeTimeout = 30 * time.Millisecond
	a.R = &hangsAfterFollow{fakePC: f}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	r, err := a.Job(ctx, pauseTestJob, 0)
	if err != nil || r.State != stateDone || r.Code != 42 {
		t.Fatalf("Job = %+v, %v\n%s", r, err, out)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("a freeze after completion held the result back for %v", d)
	}
	if strings.Contains(out.String(), "paused") || strings.Contains(out.String(), "box-exit") {
		t.Errorf("unexpected output:\n%q", out)
	}
}

func TestAttachCompletionBeatsCancellation(t *testing.T) {
	// The follow reported the exit code, then its output drain was cut off.
	f := &fakePC{t: t, exit: "7\n", insts: []instance{{Name: "my-repo", Status: "Frozen"}}}
	a, out := testApp(f)
	a.ProbeTimeout = 30 * time.Millisecond
	a.R = &hangsAfterFollow{fakePC: f, followErr: context.Canceled}
	r, err := a.Job(context.Background(), pauseTestJob, 0)
	if err != nil || r.State != stateDone || r.Code != 7 {
		t.Fatalf("Job = %+v, %v\n%s", r, err, out)
	}
}

func TestFollowScriptPrintsTheTerminalMarker(t *testing.T) {
	bash := gitBash(t)
	old := workRoot
	workRoot = filepath.ToSlash(t.TempDir())
	defer func() { workRoot = old }()
	if err := os.Mkdir(filepath.Join(workRoot, ".box"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := workRoot + "/.box/" + pauseTestJob
	write(t, file+".log", "hi\n")
	write(t, file+".exit", "42\n")
	head, snapshot := strings.Repeat("a", 40), strings.Repeat("b", 40)
	write(t, file+".meta", `{"source":{"commit":"`+head+`","dirty":false,"snapshotCommit":"`+snapshot+`"}}`)
	var stderr, out bytes.Buffer
	ef := &exitFilter{w: &stderr}
	res, err := (execRunner{}).Run(context.Background(), Cmd{Name: bash, Args: []string{"-c", followScript(pauseTestJob)}, Stdout: &out, Stderr: ef})
	if err != nil || res.Code != 0 || out.String() != "hi\n" {
		t.Fatalf("follow = %+v, %v, %q", res, err, out.String())
	}
	if marker, ok := ef.exit(); !ok || marker != "42 "+completionExited {
		t.Errorf("exit marker = %q, %v", marker, ok)
	}
	requireSourceRevision(t, ef.source(), head, false, snapshot)
	if stderr.Len() != 0 {
		t.Errorf("metadata reached stderr: %q", stderr.String())
	}
	t.Run("nonregular metadata", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("requires a POSIX FIFO")
		}
		if err := os.Remove(file + ".meta"); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		runner := execRunner{}
		fifo, err := runner.Run(ctx, Cmd{Name: bash, Args: []string{"-c", `mkfifo "$1"`, "bash", file + ".meta"}})
		if err != nil || fifo.Code != 0 {
			t.Fatalf("mkfifo = %+v, %v", fifo, err)
		}
		out.Reset()
		ef := &exitFilter{w: &stderr}
		res, err := runner.Run(ctx, Cmd{Name: bash, Args: []string{"-c", followScript(pauseTestJob)}, Stdout: &out, Stderr: ef})
		if marker, ok := ef.exit(); err != nil || res.Code != 0 || out.String() != "hi\n" || !ok || marker != "42 "+completionExited || ef.source() != nil {
			t.Fatalf("follow with FIFO metadata = %+v, %v, log %q, exit marker %q, source %+v", res, err, out.String(), marker, ef.source())
		}
	})
}
