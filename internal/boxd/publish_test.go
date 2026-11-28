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
	"testing/synctest"
	"time"

	"github.com/a-Fig/agent-box/internal/controller"
	"github.com/a-Fig/agent-box/internal/dash"
)

type handlerTransport struct {
	h http.Handler
}

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, r)
	res := rec.Result()
	res.Request = r
	return res, nil
}

type recordedPush struct {
	req controller.PutStateRequest
}

type recordingController struct {
	mu       sync.Mutex
	mem      controller.MemServer
	fail     bool
	pushes   []recordedPush
	failures int
	pushed   chan recordedPush
}

func newRecordingController(now func() time.Time) *recordingController {
	return &recordingController{
		mem: controller.MemServer{
			HostToken:   "host-secret",
			ClientToken: "client-secret",
			Now:         now,
		},
		pushed: make(chan recordedPush, 32),
	}
}

func (rc *recordingController) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc.mu.Lock()
	fail := rc.fail
	rc.mu.Unlock()

	if fail && r.Method == http.MethodPut {
		rc.mu.Lock()
		rc.failures++
		rc.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":"unavailable","message":"controller down"}`)
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

func (rc *recordingController) snapshot() ([]recordedPush, int) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	out := make([]recordedPush, len(rc.pushes))
	copy(out, rc.pushes)
	return out, rc.failures
}

func (rc *recordingController) setFail(v bool) {
	rc.mu.Lock()
	rc.fail = v
	rc.mu.Unlock()
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
	synctest.Test(t, func(t *testing.T) {
		w, f := newPublishTestWatcher(t, time.Now)
		rc := newRecordingController(time.Now)
		pub := &Publisher{
			Watcher: w,
			Client: &controller.Client{
				URL:   "https://ctrl.test",
				Token: "host-secret",
				HTTP:  &http.Client{Transport: handlerTransport{rc}},
			},
			HostID:   "pc",
			HostName: "Gaming PC",
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			pub.Run(ctx)
			close(done)
		}()

		// Initial push at t=0.
		synctest.Wait()
		pushes, _ := rc.snapshot()
		if len(pushes) != 1 || pushes[0].req.Seq != 1 || pushes[0].req.ID != "pc" {
			t.Fatalf("initial pushes = %+v", pushes)
		}

		// Burst of changes within MinGap (15s) coalesces to one push at t=15s.
		time.Sleep(2 * time.Second)
		f.set(func() {
			f.cfg["user.agent-box.mode"] = "full"
			f.ins = []Instance{inst("b1", "Running", "small", time.Now())}
		})
		w.Poll(ctx)
		synctest.Wait()
		if pushes, _ = rc.snapshot(); len(pushes) != 1 {
			t.Fatalf("pushed too early at +2s: %d", len(pushes))
		}

		time.Sleep(3 * time.Second)
		f.set(func() {
			f.ins = append(f.ins, inst("b2", "Running", "medium", time.Now()))
		})
		w.Poll(ctx)
		synctest.Wait()
		if pushes, _ = rc.snapshot(); len(pushes) != 1 {
			t.Fatalf("pushed too early at +5s: %d", len(pushes))
		}

		// Still only 1 push right before 15s has elapsed.
		time.Sleep(9 * time.Second)
		synctest.Wait()
		if pushes, _ = rc.snapshot(); len(pushes) != 1 {
			t.Fatalf("pushed too early at +14s: %d", len(pushes))
		}

		// At +15s, coalesced push seq=2 includes both boxes and updated mode.
		time.Sleep(1 * time.Second)
		synctest.Wait()
		pushes, _ = rc.snapshot()
		if len(pushes) != 2 {
			t.Fatalf("pushes at +15s = %d, want 2", len(pushes))
		}
		p2 := pushes[1]
		if p2.req.Seq != 2 || len(p2.req.State.Boxes) != 2 || p2.req.State.Machines[0].Mode != "full" {
			t.Fatalf("coalesced push = seq:%d boxes:%d mode:%s", p2.req.Seq, len(p2.req.State.Boxes), p2.req.State.Machines[0].Mode)
		}

		// Quiet period -> no push before 60s, heartbeat fires at +60s.
		time.Sleep(59 * time.Second)
		synctest.Wait()
		if pushes, _ = rc.snapshot(); len(pushes) != 2 {
			t.Fatalf("heartbeat fired early at +59s: %d", len(pushes))
		}
		time.Sleep(1 * time.Second)
		synctest.Wait()
		pushes, _ = rc.snapshot()
		if len(pushes) != 3 || pushes[2].req.Seq != 3 {
			t.Fatalf("heartbeat pushes = %+v", pushes)
		}

		// Cancel context -> clean shutdown.
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("publisher did not stop on context cancel")
		}
	})
}

func TestPublisherBackoffWithoutLogSpam(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w, _ := newPublishTestWatcher(t, time.Now)
		rc := newRecordingController(time.Now)
		rc.setFail(true)

		var logsMu sync.Mutex
		var logs []string
		pub := &Publisher{
			Watcher: w,
			Client: &controller.Client{
				URL:   "https://ctrl.test",
				Token: "host-secret",
				HTTP:  &http.Client{Transport: handlerTransport{rc}},
			},
			HostID:   "pc",
			HostName: "Gaming PC",
			Logf: func(format string, args ...any) {
				logsMu.Lock()
				logs = append(logs, fmt.Sprintf(format, args...))
				logsMu.Unlock()
			},
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			pub.Run(ctx)
			close(done)
		}()

		// Failure 1 -> 60s backoff, 1 log.
		synctest.Wait()
		if _, fails := rc.snapshot(); fails != 1 {
			t.Fatalf("failures = %d, want 1", fails)
		}

		// No retry before 60s.
		time.Sleep(59 * time.Second)
		synctest.Wait()
		if _, fails := rc.snapshot(); fails != 1 {
			t.Fatalf("retried early at 59s: failures=%d", fails)
		}

		// Failure 2 at +60s -> 120s backoff, same error so no log spam.
		time.Sleep(1 * time.Second)
		synctest.Wait()
		if _, fails := rc.snapshot(); fails != 2 {
			t.Fatalf("failures = %d, want 2", fails)
		}

		// Failure 3 at +120s -> 240s backoff.
		time.Sleep(120 * time.Second)
		synctest.Wait()
		if _, fails := rc.snapshot(); fails != 3 {
			t.Fatalf("failures = %d, want 3", fails)
		}

		// Failure 4 at +240s -> capped at 300s (5m).
		time.Sleep(240 * time.Second)
		synctest.Wait()
		if _, fails := rc.snapshot(); fails != 4 {
			t.Fatalf("failures = %d, want 4", fails)
		}

		logsMu.Lock()
		if len(logs) != 1 {
			t.Fatalf("expected 1 failure log during identical outage, got %d: %v", len(logs), logs)
		}
		logsMu.Unlock()

		// Still waiting at 299s.
		time.Sleep(299 * time.Second)
		synctest.Wait()
		if _, fails := rc.snapshot(); fails != 4 {
			t.Fatalf("retried before 300s cap: failures=%d", fails)
		}

		// Controller recovers -> next retry at +300s succeeds and logs recovery.
		rc.setFail(false)
		time.Sleep(1 * time.Second)
		synctest.Wait()
		pushes, _ := rc.snapshot()
		if len(pushes) != 1 || pushes[0].req.Seq != 5 {
			t.Fatalf("recovery pushes = %+v, want seq 5", pushes)
		}

		logsMu.Lock()
		if len(logs) != 2 || !strings.Contains(logs[1], "recovered after 4 failure(s)") {
			t.Fatalf("logs after recovery = %v", logs)
		}
		logsMu.Unlock()

		cancel()
		synctest.Wait()
		<-done
	})
}

func TestPublisherEndToEndHTTP(t *testing.T) {
	w, _ := newPublishTestWatcher(t, time.Now)
	rc := newRecordingController(time.Now)
	srv := httptest.NewServer(rc)
	defer srv.Close()

	pub := &Publisher{
		Watcher:  w,
		Client:   &controller.Client{URL: srv.URL, Token: "host-secret"},
		HostID:   "pc",
		HostName: "Gaming PC",
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pub.Run(ctx)
		close(done)
	}()

	select {
	case p := <-rc.pushed:
		if p.req.Seq != 1 || p.req.ID != "pc" {
			t.Fatalf("unexpected push = %+v", p.req)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for HTTP push")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publisher did not stop on context cancel")
	}
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
