package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
)

type reconcilePusher func(context.Context, string, string) error

func (p reconcilePusher) Send(ctx context.Context, thread, msg string) error {
	return p(ctx, thread, msg)
}

func reconciliationApp(t *testing.T, profile string) *App {
	t.Helper()
	a := newApp()
	a.StateDir, a.Remote, a.Project = t.TempDir(), "original", "agents"
	a.Getenv = func(key string) string {
		if key == "T3CODE_HOME" {
			return profile
		}
		return ""
	}
	a.Out, a.Err = io.Discard, io.Discard
	a.WatchPoll = time.Millisecond
	return a
}

func saveEndedPush(t *testing.T, a *App, job, thread, profile string) {
	t.Helper()
	if err := writeJSON(a.statePath(job, ".json"), jobRecord{Job: job, Thread: thread, T3BaseDir: profile}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(a.statePath(job, ".ended"), jobEnd{Code: 7, Tail: "saved completion"}); err != nil {
		t.Fatal(err)
	}
}

func waitForReconciledWatchRelease(t *testing.T, ctx context.Context, a *App, job string) {
	t.Helper()
	for a.exists(job, ".watch") {
		select {
		case <-time.After(time.Millisecond):
		case <-ctx.Done():
			t.Fatal("the recovery watcher did not release its lease")
		}
	}
	// The lease can disappear just before its owner closes the lock handle.
	// Wait for that close too so Windows can remove the temporary directory.
	lock, err := a.lockLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileRetriesForSavedProfileAfterLeasesExpire(t *testing.T) {
	profile, otherProfile := t.TempDir(), t.TempDir()
	a := reconciliationApp(t, profile)
	job := testBareJobID
	const otherJob = "repo.20261126-223849-0001"
	const legacy = "repo.20261126-223849-0002"
	saveEndedPush(t, a, job, "original-thread", profile)
	saveEndedPush(t, a, otherJob, "original-thread", otherProfile)
	saveEndedPush(t, a, legacy, "legacy-thread", "")
	start := time.Now()
	var advanced atomic.Int64
	a.Now = func() time.Time { return start.Add(time.Duration(advanced.Load())) }
	for _, lease := range []struct {
		ext string
		ttl time.Duration
	}{{".watch", watchLease}, {".sending", sendLease}} {
		if _, err := a.acquireLease(context.Background(), job, lease.ext, lease.ttl); err != nil {
			t.Fatal(err)
		}
	}
	var accepts atomic.Int32
	var available atomic.Bool
	attempts := make(chan sentMsg, 16)
	a.Push = reconcilePusher(func(_ context.Context, thread, msg string) error {
		ok := available.Load()
		select {
		case attempts <- sentMsg{thread, msg}:
		default:
		}
		if !ok {
			return errors.New("T3 unavailable")
		}
		accepts.Add(1)
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	a.reconcileProfile(ctx, profile)
	advanced.Store(int64(watchLease + time.Second))
	a.reconcileProfile(ctx, profile)
	if len(attempts) != 0 || !a.leaseHeld(job, ".sending") {
		t.Fatal("live leases did not protect the pending notice")
	}
	advanced.Store(int64(sendLease + time.Second))
	reconcileCtx, stopReconcile := context.WithCancel(ctx)
	defer stopReconcile()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		a.reconcilePushes(reconcileCtx, 5*time.Millisecond)
	}()
	for range 4 {
		select {
		case attempt := <-attempts:
			if attempt.thread != "original-thread" || !strings.Contains(attempt.msg, job) {
				t.Fatalf("attempt = %+v", attempt)
			}
		case <-ctx.Done():
			t.Fatal("startup did not retry the expired watcher")
		}
	}
	if a.exists(job, ".notified") || !a.exists(job, ".ended") {
		t.Fatal("a failed send consumed the stored completion")
	}
	available.Store(true)
	for !a.exists(job, ".notified") {
		select {
		case <-time.After(time.Millisecond):
		case <-ctx.Done():
			t.Fatal("the next scan did not deliver the stored result")
		}
	}
	stopReconcile()
	<-stopped
	waitForReconciledWatchRelease(t, ctx, a, job)
	if accepts.Load() != 1 || a.exists(otherJob, ".notified") || a.exists(legacy, ".notified") {
		t.Fatalf("accepted sends = %d, or recovery crossed profile boundaries", accepts.Load())
	}
}

func TestReconcileKeepsLegacyAndAcknowledgementsThreadScoped(t *testing.T) {
	profile := t.TempDir()
	a := reconciliationApp(t, profile)
	a.Thread = "owner"
	saveEndedPush(t, a, testBareJobID, a.Thread, profile)
	const legacy = "repo.20261126-223849-0002"
	read := JobResult{State: stateDone}
	other := *a
	other.Thread = "other-thread"
	other.markRead(testBareJobID, read, nil, "box_job")
	if a.exists(testBareJobID, ".notified") {
		t.Fatal("a different thread consumed the pending result")
	}
	other = *a
	otherProfile := t.TempDir()
	other.Getenv = func(key string) string {
		if key == "T3CODE_HOME" {
			return otherProfile
		}
		return ""
	}
	other.markRead(testBareJobID, read, nil, "box_job")
	var hook bytes.Buffer
	other.Hook(strings.NewReader(`{"session_id":"s"}`), &hook)
	if a.exists(testBareJobID, ".notified") || strings.Contains(hook.String(), testBareJobID) || strings.Contains(other.safetyNet(""), testBareJobID) {
		t.Fatal("a different profile consumed or displayed the pending result")
	}
	a.markRead(testBareJobID, read, nil, "box_job")
	if a.notifiedBy(testBareJobID) != "box_job" {
		t.Fatal("the originating thread could not acknowledge its result")
	}
	saveEndedPush(t, a, legacy, a.Thread, "")
	var spawns []string
	a.Spawn = func(_, job string) error { spawns = append(spawns, job); return nil }
	if got := a.safetyNet(""); !strings.Contains(got, legacy) || strings.Contains(got, testBareJobID) || !slices.Equal(spawns, []string{legacy}) {
		t.Fatalf("per-call recovery = %q, spawns %v", got, spawns)
	}
}

func TestMCPStartupRecoveryDoesNotBlockAndStopsAtEOF(t *testing.T) {
	t.Setenv("AGENT_BOX_NO_UPDATE", "1")
	profile := t.TempDir()
	a := reconciliationApp(t, profile)
	saveEndedPush(t, a, testBareJobID, "origin", profile)
	started, stopped := make(chan struct{}), make(chan struct{})
	a.Push = reconcilePusher(func(ctx context.Context, thread, msg string) error {
		close(started)
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	})
	in, writer := io.Pipe()
	defer writer.Close()
	out, output := io.Pipe()
	defer out.Close()
	a.Out = output
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan int, 1)
	go func() {
		defer output.Close()
		done <- run(ctx, a, []string{"mcp"}, in)
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("MCP startup did not begin pending delivery before any call")
	}
	response := make(chan error, 1)
	go func() {
		if _, err := fmt.Fprintln(writer, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`); err != nil {
			response <- err
			return
		}
		var message rpcMsg
		if err := json.NewDecoder(out).Decode(&message); err != nil || string(message.ID) != "1" || message.Result == nil {
			response <- fmt.Errorf("MCP initialization = %+v, %v", message, err)
			return
		}
		response <- nil
	}()
	select {
	case err := <-response:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("pending delivery blocked MCP initialization")
	}
	writer.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("MCP server exited %d", code)
		}
	case <-ctx.Done():
		t.Fatal("MCP did not exit at EOF")
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("MCP EOF did not stop the recovery watcher")
	}
	waitForReconciledWatchRelease(t, ctx, a, testBareJobID)
}

func TestWatchRecordsProfileAndKeepsAnOldUndeliveredResult(t *testing.T) {
	profile := t.TempDir()
	a := reconciliationApp(t, profile)
	a.Thread, a.Push = "origin", &fakePusher{}
	a.Spawn = func(_, _ string) error { return nil }
	if !a.startWatch(jobMeta{}, testBareJobID, false, false, false) {
		t.Fatal("could not save a bare job")
	}
	rec, err := a.readWatchRecord(testBareJobID)
	if err != nil || rec.T3BaseDir != profile || rec.Host == nil || rec.Host.Remote != "original" || rec.Host.Project != "agents" || rec.Thread != "origin" {
		t.Fatalf("saved record = %+v, %v", rec, err)
	}
	cli := t3CLI{baseDir: rec.T3BaseDir}
	if got := cli.sendArgs(rec.Thread); !slices.Equal(got, []string{"thread", "send", "--base-dir", profile, "origin", "-"}) {
		t.Fatalf("send args = %q", got)
	}
	a.recordEnd(testBareJobID, jobEnd{Code: 7})
	old := time.Now().Add(-recordKeep - time.Hour)
	for _, ext := range []string{".json", ".result"} {
		if err := os.Chtimes(a.statePath(testBareJobID, ext), old, old); err != nil {
			t.Fatal(err)
		}
	}
	a.safetyNet("")
	a.recordEnd("repo.20261126-223849-0003", jobEnd{})
	if !a.exists(testBareJobID, ".json") || !a.exists(testBareJobID, ".result") {
		t.Fatal("an undelivered result expired")
	}
	a.markRead(testBareJobID, JobResult{State: stateDone}, nil, "box_job")
	a.safetyNet("")
	if a.exists(testBareJobID, ".json") || a.exists(testBareJobID, ".result") {
		t.Fatal("the acknowledged old record was kept")
	}
}

func TestRelativeT3ProfileDoesNotEnableStartupRecovery(t *testing.T) {
	a := reconciliationApp(t, filepath.Join("relative", "profile"))
	if got := a.t3BaseDir(); got != "" {
		t.Fatalf("relative T3 profile was treated as stable: %q", got)
	}
}

func TestProfiledBareWatchUsesSavedIncusTarget(t *testing.T) {
	a := reconciliationApp(t, t.TempDir())
	a.Thread = "origin"
	a.Spawn = func(_, _ string) error { return nil }
	a.Push = &fakePusher{}
	if !a.startWatch(jobMeta{}, testBareJobID, false, false, false) {
		t.Fatal("could not save a bare job")
	}
	a.Remote, a.Project = "replacement", "other"
	a.Post = nil
	a.PausePoll = time.Hour
	a.R = lookupRunner(func(_ context.Context, cmd Cmd) (Result, error) {
		if cmd.Args[1] != "agents" {
			t.Fatalf("recovery used the current project: %q", cmd.Args)
		}
		if cmd.Args[2] == "config" && cmd.Args[4] == "original:repo" {
			return Result{Code: 1}, nil
		}
		if len(cmd.Args) != 8 || cmd.Args[2] != "exec" || cmd.Args[3] != "original:repo" {
			t.Fatalf("recovery used the current remote: %q", cmd.Args)
		}
		if cmd.Args[7] == followScript(testBareJobID) {
			fmt.Fprintln(cmd.Stderr, exitMarker+"7")
			return Result{}, nil
		}
		if !strings.HasPrefix(cmd.Args[7], "# cancelled\n") {
			t.Fatalf("unexpected recovery script: %q", cmd.Args[7])
		}
		return Result{}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.Watch(ctx, testBareJobID); err != nil || a.notifiedBy(testBareJobID) != "watcher" {
		t.Fatalf("watching the saved target: %v", err)
	}
}

func TestBareCompletionNoticeUsesSavedWatchTarget(t *testing.T) {
	for _, tc := range []struct {
		name string
		old  *jobLocator
	}{
		{name: "another remote", old: &jobLocator{Remote: "original", Project: "work"}},
		{name: "another project", old: &jobLocator{Remote: "replacement", Project: "agents"}},
		{name: "unscoped legacy end"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := reconciliationApp(t, t.TempDir())
			pusher := &fakePusher{}
			a.Thread, a.Push = "origin", pusher
			a.Spawn = func(_, _ string) error { return nil }
			a.R = lookupRunner(func(_ context.Context, cmd Cmd) (Result, error) {
				t.Fatalf("watch with a matching saved result contacted Incus: %q", cmd.Args)
				return Result{}, nil
			})
			job := testBareJobID
			target := &jobLocator{Remote: "replacement", Project: "work"}
			dirty := true
			source := &dash.SourceRevision{Commit: strings.Repeat("c", 40), Dirty: &dirty, SnapshotCommit: strings.Repeat("d", 40)}
			foreign := &dash.SourceRevision{Commit: strings.Repeat("a", 40), Dirty: &dirty, SnapshotCommit: strings.Repeat("b", 40)}
			rec := jobRecord{Job: job, Thread: a.Thread, T3BaseDir: a.t3BaseDir(), Host: target, Source: source}
			if err := writeJSON(a.statePath(job, ".json"), rec); err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(a.statePath(job, ".ended"), jobEnd{Code: 3, Tail: "foreign output", Source: foreign, Locator: tc.old}); err != nil {
				t.Fatal(err)
			}
			a.recordEnd(job, jobEnd{Code: 7, Tail: "current output", Source: source, Locator: target, Completion: completionExited})

			var out bytes.Buffer
			a.Hook(nil, &out)
			if out.Len() != 0 || a.exists(job, ".notified") || a.safetyNet("") != "" {
				t.Fatalf("stale completion was offered: %q", out.String())
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := a.Watch(ctx, job); err != nil || pusher.count() != 1 {
				t.Fatalf("Watch = %v, notices = %d", err, pusher.count())
			}
			msg := pusher.sent[0].msg
			if !strings.Contains(msg, "exit 7") || !strings.Contains(msg, "current output") || !strings.Contains(msg, sourceLine(source)) ||
				strings.Contains(msg, "foreign output") || strings.Contains(msg, sourceLine(foreign)) {
				t.Fatalf("wrong completion notice: %q", msg)
			}
		})
	}
}

func TestLeaseTakeoverProtectsTheNewOwner(t *testing.T) {
	for _, ext := range []string{".watch", ".sending"} {
		t.Run(ext, func(t *testing.T) {
			a := reconciliationApp(t, t.TempDir())
			job := testBareJobID
			now := time.Now()
			a.Now = func() time.Time { return now }
			oldToken, err := a.acquireLease(context.Background(), job, ext, -time.Second)
			if err != nil {
				t.Fatal(err)
			}
			first := *a
			atExpiryCheck, continueTakeover := make(chan struct{}), make(chan struct{})
			var clockReads int
			first.Now = func() time.Time {
				clockReads++
				if clockReads == 1 {
					close(atExpiryCheck)
					<-continueTakeover
				}
				return now
			}
			type attempt struct {
				token string
				err   error
			}
			winner, contender := make(chan attempt, 1), make(chan attempt, 1)
			go func() {
				token, err := first.acquireLease(context.Background(), job, ext, time.Minute)
				winner <- attempt{token, err}
			}()
			select {
			case <-atExpiryCheck:
			case result := <-winner:
				t.Fatalf("first contender did not inspect the expired lease: %+v", result)
			case <-time.After(3 * time.Second):
				close(continueTakeover)
				t.Fatal("first contender did not reach the expiry check")
			}
			contending := make(chan struct{})
			go func() {
				close(contending)
				token, err := a.acquireLease(context.Background(), job, ext, time.Minute)
				contender <- attempt{token, err}
			}()
			<-contending
			select {
			case result := <-contender:
				close(continueTakeover)
				<-winner
				t.Fatalf("second contender passed the locked expiry check: %+v", result)
			case <-time.After(25 * time.Millisecond):
				close(continueTakeover)
			}
			won, lost := <-winner, <-contender
			if won.err != nil || !errors.Is(lost.err, errLeaseHeld) {
				t.Fatalf("takeover outcomes: first %+v, second %+v", won, lost)
			}
			if a.renewLease(job, ext, oldToken, time.Minute) {
				t.Fatal("the old owner renewed the new lease")
			}
			a.releaseLease(job, ext, oldToken)
			if token, _, ok := a.readLease(job, ext); !ok || token != won.token {
				t.Fatalf("lease kept token %q, want %q", token, won.token)
			}
			a.releaseLease(job, ext, won.token)
		})
	}
}

func TestLeaseAcquisitionCancelsUnderContention(t *testing.T) {
	a := reconciliationApp(t, t.TempDir())
	held, err := a.lockLeases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	awaiting, result := make(chan struct{}), make(chan error, 1)
	go func() {
		close(awaiting)
		_, err := a.acquireLease(ctx, testBareJobID, ".watch", watchLease)
		result <- err
	}()
	<-awaiting
	select {
	case err := <-result:
		t.Fatalf("acquisition returned while another owner holds the lock: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("acquisition returned %v, want cancellation", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelling the waiter did not interrupt the contended acquisition")
	}
	if a.exists(testBareJobID, ".watch") {
		t.Fatal("the cancelled waiter left a lease")
	}
}

func TestOfflineWatchUsesSavedTerminalDisposition(t *testing.T) {
	bash := gitBash(t)
	for _, completion := range []string{completionExited, completionCancelled} {
		t.Run(completion, func(t *testing.T) {
			oldRoot := workRoot
			workRoot = filepath.ToSlash(t.TempDir())
			t.Cleanup(func() { workRoot = oldRoot })
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			job := testBareJobID
			a := reconciliationApp(t, t.TempDir())
			pusher := &fakePusher{}
			a.Thread, a.Push, a.Post = "origin", pusher, nil
			a.Spawn = func(_, _ string) error { return nil }
			a.PausePoll = time.Hour
			if !a.startWatch(jobMeta{}, job, false, false, false) {
				t.Fatal("could not save the job record")
			}
			runner := execRunner{}
			cmdEnd, err := runner.Run(ctx, Cmd{Name: bash, Args: []string{"-c", "echo 'fixture output'; exit 130"}})
			if err != nil || cmdEnd.Code != 130 {
				t.Fatalf("fixture command = %+v, %v", cmdEnd, err)
			}
			prefix := filepath.Join(workRoot, ".box", job)
			if err := os.MkdirAll(filepath.Dir(prefix), 0o755); err != nil {
				t.Fatal(err)
			}
			write(t, prefix+".log", cmdEnd.Stdout)
			write(t, prefix+".exit", fmt.Sprint(cmdEnd.Code))
			if completion == completionCancelled {
				write(t, prefix+".cancelled", "")
			}
			a.R = lookupRunner(func(ctx context.Context, cmd Cmd) (Result, error) {
				if cmd.Args[2] == "config" {
					return Result{Code: 1}, nil
				}
				if len(cmd.Args) != 8 || cmd.Args[7] != followScript(job) {
					return Result{}, fmt.Errorf("unexpected Incus call: %q", cmd.Args)
				}
				return runner.Run(ctx, Cmd{Name: bash, Args: []string{"-c", cmd.Args[7]}, Stdout: cmd.Stdout, Stderr: cmd.Stderr})
			})
			if res, err := a.Job(ctx, job, 0); err != nil || res.State != stateDone || res.Code != 130 {
				t.Fatalf("attach = %+v, %v", res, err)
			}
			if saved, ok := a.recordedEnd(job); !ok || saved.Completion != completion || !strings.Contains(saved.Tail, "fixture output") {
				t.Fatalf("terminal evidence = %+v, present %v", saved, ok)
			}
			a.R = lookupRunner(func(_ context.Context, cmd Cmd) (Result, error) {
				t.Errorf("offline recovery contacted Incus: %q", cmd.Args)
				return Result{}, errors.New("Incus unavailable")
			})
			a.HTTP = func(req *http.Request) (*http.Response, error) {
				t.Errorf("offline recovery contacted boxd: %s", req.URL)
				return nil, errors.New("boxd unavailable")
			}
			if err := a.Watch(ctx, job); err != nil {
				t.Fatal(err)
			}
			if completion == completionCancelled {
				if pusher.count() != 0 || a.notifiedBy(job) != "cancel" || a.exists(job, ".ended") {
					t.Fatal("a confirmed cancellation was delivered")
				}
			} else if pusher.count() != 1 || pusher.sent[0].thread != "origin" || !strings.Contains(pusher.sent[0].msg, "exit 130") ||
				!strings.Contains(pusher.sent[0].msg, "fixture output") || a.notifiedBy(job) != "watcher" {
				t.Fatalf("offline result was not delivered to the origin: %+v", pusher.sent)
			}
		})
	}
}
