package boxd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/controller"
	"github.com/a-Fig/agent-box/internal/dash"
)

type fakePublishClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
	waiting chan time.Duration
}

type fakeWaiter struct {
	until time.Time
	ch    chan time.Time
}

func newFakePublishClock(start time.Time) *fakePublishClock {
	return &fakePublishClock{
		now:     start,
		waiting: make(chan time.Duration, 16),
	}
}

func (c *fakePublishClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakePublishClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	target := c.now.Add(d)
	if !target.After(c.now) {
		ch <- c.now
		c.mu.Unlock()
		return ch
	}
	c.waiters = append(c.waiters, fakeWaiter{until: target, ch: ch})
	c.mu.Unlock()
	select {
	case c.waiting <- d:
	default:
	}
	return ch
}

func (c *fakePublishClock) WaitForSleep(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-c.waiting:
		return d
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for publisher sleep")
		return 0
	}
}

func (c *fakePublishClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.until.After(now) {
			w.ch <- now
		} else {
			kept = append(kept, w)
		}
	}
	c.waiters = kept
	c.mu.Unlock()
}

type recordedPush struct {
	req controller.PutStateRequest
}

type recordingController struct {
	mu     sync.Mutex
	mem    controller.MemServer
	fail   bool
	pushes []recordedPush
	pushed chan recordedPush
	failed chan struct{}
}

func newRecordingController(now func() time.Time) *recordingController {
	return &recordingController{
		mem: controller.MemServer{
			HostToken:   "host-secret",
			ClientToken: "client-secret",
			Now:         now,
		},
		pushed: make(chan recordedPush, 32),
		failed: make(chan struct{}, 32),
	}
}

func (rc *recordingController) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc.mu.Lock()
	fail := rc.fail
	rc.mu.Unlock()

	if fail && r.Method == http.MethodPut {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":"unavailable","message":"controller down"}`)
		select {
		case rc.failed <- struct{}{}:
		default:
		}
		return
	}

	if r.Method == http.MethodPut {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		var req controller.PutStateRequest
		if json.Unmarshal(body, &req) == nil {
			rc.mem.ServeHTTP(w, r)
			rc.mu.Lock()
			rc.pushes = append(rc.pushes, recordedPush{req: req})
			rc.mu.Unlock()
			select {
			case rc.pushed <- recordedPush{req: req}:
			default:
			}
			return
		}
	}
	rc.mem.ServeHTTP(w, r)
}

func (rc *recordingController) waitPush(t *testing.T) recordedPush {
	t.Helper()
	select {
	case p := <-rc.pushed:
		return p
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for push")
		return recordedPush{}
	}
}

func (rc *recordingController) waitFail(t *testing.T) {
	t.Helper()
	select {
	case <-rc.failed:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for failed push")
	}
}

func newPublishTestWatcher(t *testing.T, now func() time.Time) (*Watcher, *fakeIncus) {
	t.Helper()
	hist, err := OpenHistory(t.TempDir(), now)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIncus{cfg: map[string]string{"user.agent-box.mode": "shared"}}
	w := &Watcher{
		Incus:       f,
		Hist:        hist,
		Now:         now,
		MachineID:   "pc",
		MachineName: "Gaming PC",
	}
	return w, f
}

func TestNewPublisherFromConfig(t *testing.T) {
	w, _ := newPublishTestWatcher(t, time.Now)

	// Unconfigured controller returns nil without logs.
	var logs []string
	logf := func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	if pub := NewPublisherFromConfig(Config{}, w, logf); pub != nil || len(logs) != 0 {
		t.Fatalf("unconfigured = %v, logs=%v", pub, logs)
	}

	// Missing token_file warns and returns nil so boxd still boots.
	missing := filepath.Join(t.TempDir(), "missing.token")
	pub := NewPublisherFromConfig(Config{
		MachineID:   "Gaming_PC",
		MachineName: "Gaming PC",
		Controller: &ControllerConfig{
			URL:       "https://ctrl.example.com",
			TokenFile: missing,
		},
	}, w, logf)
	if pub != nil || len(logs) != 1 || !strings.Contains(logs[0], "WARNING") {
		t.Fatalf("missing token_file = %v, logs=%v", pub, logs)
	}

	// Empty token_file warns and returns nil.
	empty := filepath.Join(t.TempDir(), "empty.token")
	os.WriteFile(empty, []byte("  \n"), 0o600)
	logs = nil
	pub = NewPublisherFromConfig(Config{
		MachineID:   "Gaming_PC",
		MachineName: "Gaming PC",
		Controller: &ControllerConfig{
			URL:       "https://ctrl.example.com",
			TokenFile: empty,
		},
	}, w, logf)
	if pub != nil || len(logs) != 1 || !strings.Contains(logs[0], "empty") {
		t.Fatalf("empty token_file = %v, logs=%v", pub, logs)
	}

	// Valid token_file returns configured Publisher with sanitized host ID.
	valid := filepath.Join(t.TempDir(), "host.token")
	os.WriteFile(valid, []byte("secret-token\n"), 0o600)
	logs = nil
	pub = NewPublisherFromConfig(Config{
		MachineID:   "Gaming_PC",
		MachineName: "Gaming PC",
		Controller: &ControllerConfig{
			URL:       "https://ctrl.example.com",
			TokenFile: valid,
		},
	}, w, logf)
	if pub == nil || pub.HostID != "gaming-pc" || pub.Client.Token != "secret-token" || len(logs) != 0 {
		t.Fatalf("valid config = %+v, logs=%v", pub, logs)
	}
}

func TestPublisherCoalescingHeartbeatAndShutdown(t *testing.T) {
	clk := newFakePublishClock(time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC))
	w, f := newPublishTestWatcher(t, clk.Now)
	rc := newRecordingController(clk.Now)
	srv := httptest.NewServer(rc)
	defer srv.Close()

	pub := &Publisher{
		Watcher:  w,
		Client:   &controller.Client{URL: srv.URL, Token: "host-secret"},
		HostID:   "pc",
		HostName: "Gaming PC",
		Now:      clk.Now,
		After:    clk.After,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pub.Run(ctx)
		close(done)
	}()

	// Initial push at t=0.
	p1 := rc.waitPush(t)
	if p1.req.Seq != 1 || p1.req.ID != "pc" {
		t.Fatalf("initial push = %+v", p1.req)
	}
	if d := clk.WaitForSleep(t); d != 60*time.Second {
		t.Fatalf("idle wait = %s, want 60s", d)
	}

	// Burst of changes within MinGap (15s) coalesces to one push at t=15s.
	clk.Advance(2 * time.Second)
	f.set(func() {
		f.cfg["user.agent-box.mode"] = "full"
		f.ins = []Instance{inst("b1", "Running", "small", clk.Now())}
	})
	w.Poll(ctx)
	if d := clk.WaitForSleep(t); d != 13*time.Second {
		t.Fatalf("coalesce wait = %s, want 13s", d)
	}

	clk.Advance(3 * time.Second)
	f.set(func() {
		f.ins = append(f.ins, inst("b2", "Running", "medium", clk.Now()))
	})
	w.Poll(ctx)
	if d := clk.WaitForSleep(t); d != 10*time.Second {
		t.Fatalf("second coalesce wait = %s, want 10s", d)
	}

	// Still only 1 push before 15s has elapsed.
	select {
	case extra := <-rc.pushed:
		t.Fatalf("unexpected push before MinGap: %+v", extra.req)
	default:
	}

	// Advance remaining 10s -> coalesced push seq=2 includes both boxes.
	clk.Advance(10 * time.Second)
	p2 := rc.waitPush(t)
	if p2.req.Seq != 2 || len(p2.req.State.Boxes) != 2 || p2.req.State.Machines[0].Mode != "full" {
		t.Fatalf("coalesced push = seq:%d boxes:%d mode:%s", p2.req.Seq, len(p2.req.State.Boxes), p2.req.State.Machines[0].Mode)
	}

	// Quiet period -> heartbeat fires at +60s.
	if d := clk.WaitForSleep(t); d != 60*time.Second {
		t.Fatalf("heartbeat wait = %s, want 60s", d)
	}
	clk.Advance(60 * time.Second)
	p3 := rc.waitPush(t)
	if p3.req.Seq != 3 {
		t.Fatalf("heartbeat seq = %d, want 3", p3.req.Seq)
	}
	clk.WaitForSleep(t)

	// Cancel context -> clean shutdown.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher did not stop on context cancel")
	}
}

func TestPublisherBackoffWithoutLogSpam(t *testing.T) {
	clk := newFakePublishClock(time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC))
	w, _ := newPublishTestWatcher(t, clk.Now)
	rc := newRecordingController(clk.Now)
	rc.fail = true
	srv := httptest.NewServer(rc)
	defer srv.Close()

	var logsMu sync.Mutex
	var logs []string
	pub := &Publisher{
		Watcher:  w,
		Client:   &controller.Client{URL: srv.URL, Token: "host-secret"},
		HostID:   "pc",
		HostName: "Gaming PC",
		Now:      clk.Now,
		After:    clk.After,
		Logf: func(format string, args ...any) {
			logsMu.Lock()
			logs = append(logs, fmt.Sprintf(format, args...))
			logsMu.Unlock()
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		pub.Run(ctx)
		close(done)
	}()

	// Failure 1 -> 60s backoff, 1 log.
	rc.waitFail(t)
	if d := clk.WaitForSleep(t); d != 60*time.Second {
		t.Fatalf("backoff 1 = %s, want 60s", d)
	}

	// Failure 2 -> 120s backoff, same error so no log spam.
	clk.Advance(60 * time.Second)
	rc.waitFail(t)
	if d := clk.WaitForSleep(t); d != 120*time.Second {
		t.Fatalf("backoff 2 = %s, want 120s", d)
	}

	// Failure 3 -> 240s backoff.
	clk.Advance(120 * time.Second)
	rc.waitFail(t)
	if d := clk.WaitForSleep(t); d != 240*time.Second {
		t.Fatalf("backoff 3 = %s, want 240s", d)
	}

	// Failure 4 -> capped at 300s (5m).
	clk.Advance(240 * time.Second)
	rc.waitFail(t)
	if d := clk.WaitForSleep(t); d != 300*time.Second {
		t.Fatalf("backoff 4 = %s, want 300s", d)
	}

	logsMu.Lock()
	if len(logs) != 1 {
		t.Fatalf("expected 1 failure log during identical outage, got %d: %v", len(logs), logs)
	}
	logsMu.Unlock()

	// Controller recovers -> next retry succeeds and logs recovery.
	rc.mu.Lock()
	rc.fail = false
	rc.mu.Unlock()
	clk.Advance(300 * time.Second)
	p := rc.waitPush(t)
	if p.req.Seq != 5 {
		t.Fatalf("recovery seq = %d, want 5", p.req.Seq)
	}
	clk.WaitForSleep(t)

	logsMu.Lock()
	if len(logs) != 2 || !strings.Contains(logs[1], "recovered after 4 failure(s)") {
		t.Fatalf("logs after recovery = %v", logs)
	}
	logsMu.Unlock()

	cancel()
	<-done
}

func TestPublisherTrimsAndEnforcesBodyCap(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	var jobs []dash.Job
	for i := 0; i < 30; i++ {
		end := now.Add(time.Duration(i) * time.Minute)
		jobs = append(jobs, dash.Job{
			Key:       fmt.Sprintf("b/j%02d/t", i),
			ID:        fmt.Sprintf("j%02d", i),
			State:     dash.JobDone,
			StartedAt: end.Add(-time.Second),
			EndedAt:   &end,
			Tail:      strings.Repeat("x", 2048),
		})
	}
	// Normal trim reduces 30 finished jobs to 20 and stays well under 256 KiB.
	req, ok := fitPutStateRequest("pc", "PC", 1, now, controller.TrimState(dash.State{Jobs: jobs, Now: now}))
	if !ok || len(req.State.Jobs) != controller.MaxFinishedJobs {
		t.Fatalf("normal trim = ok:%v jobs:%d", ok, len(req.State.Jobs))
	}

	// Giant active job tails force progressive trimming under MaxBodyBytes.
	var huge []dash.Job
	for i := 0; i < 160; i++ {
		huge = append(huge, dash.Job{
			Key:   fmt.Sprintf("b/run%03d/t", i),
			ID:    fmt.Sprintf("run%03d", i),
			State: dash.JobRunning,
			Tail:  strings.Repeat("y", 2048),
		})
	}
	req, ok = fitPutStateRequest("pc", "PC", 2, now, controller.TrimState(dash.State{Jobs: huge, Now: now}))
	if !ok {
		t.Fatal("fitPutStateRequest failed to shrink giant state")
	}
	b, err := json.Marshal(req)
	if err != nil || len(b) > controller.MaxBodyBytes {
		t.Fatalf("shrunk payload = %d bytes, err=%v", len(b), err)
	}
}
