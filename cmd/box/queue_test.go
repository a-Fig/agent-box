package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/mode"
)

// fakeBoxd answers box's queue requests (App.HTTP).
type fakeBoxd struct {
	t      *testing.T
	mu     sync.Mutex
	calls  []string
	req    dash.QueueRequest
	bundle int // bytes received
	code   int // POST /api/queue's status; 0 is 201
	reply  dash.QueueReply
	views  []dash.Job // GET /api/queue/{job} answers in turn; the last repeats
	gone   bool       // GET answers 404
	cancel int        // POST .../cancel's status; 0 is 204
}

func (b *fakeBoxd) do(req *http.Request) (*http.Response, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, req.Method+" "+req.URL.Host+req.URL.Path)
	rec := httptest.NewRecorder()
	if req.Header.Get(dash.ControlHeader) != "1" {
		b.t.Errorf("%s %s without %s", req.Method, req.URL.Path, dash.ControlHeader)
	}
	switch {
	case req.Method == "POST" && req.URL.Path == "/api/queue":
		mr, err := req.MultipartReader()
		if err != nil {
			b.t.Fatal(err)
		}
		p, _ := mr.NextPart()
		if p.FormName() != "request" || json.NewDecoder(p).Decode(&b.req) != nil {
			b.t.Fatalf("first part %q", p.FormName())
		}
		p, _ = mr.NextPart()
		n, _ := io.Copy(io.Discard, p)
		b.bundle = int(n)
		if b.code != 0 {
			rec.WriteHeader(b.code)
			json.NewEncoder(rec).Encode(dash.ErrorBody{Error: "the queue is full", Code: dash.CodeQueueFull})
			break
		}
		rec.WriteHeader(201)
		json.NewEncoder(rec).Encode(b.reply)
	case req.Method == "GET" && strings.HasPrefix(req.URL.Path, "/api/queue/"):
		if b.gone || len(b.views) == 0 {
			rec.WriteHeader(404)
			break
		}
		v := b.views[0]
		if len(b.views) > 1 {
			b.views = b.views[1:]
		}
		json.NewEncoder(rec).Encode(v)
	case req.Method == "POST" && strings.HasSuffix(req.URL.Path, "/cancel"):
		if b.cancel == 0 {
			b.cancel = 204
		}
		rec.WriteHeader(b.cancel)
	default:
		rec.WriteHeader(404)
	}
	return rec.Result(), nil
}

func (b *fakeBoxd) callList() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.calls, "\n")
}

const queuedJobName = "my-repo-ab12.20261123-120000-cd34"

func newQueueEnv(t *testing.T, f *fakePC) (pushEnv, *fakeBoxd) {
	f.t, f.remotes = t, testRemotes
	e := newPushEnv(t, f, "thread-1")
	b := &fakeBoxd{t: t, reply: dash.QueueReply{Job: queuedJobName, Position: 2, Why: dash.RefusedFull}}
	e.a.HTTP = b.do
	return e, b
}

func TestRunWithNoRoomIsQueued(t *testing.T) {
	e, b := newQueueEnv(t, &fakePC{refuse: memoryRefusal})
	text, st := e.call(t, "box_run", e.runArgs(map[string]any{"new": true, "timeout": "90s", "env": map[string]string{"CI": "1"}}, "go test ./..."))
	if st["state"] != "queued" || st["job"] != queuedJobName || st["position"] != 2.0 || st["why"] != "full" ||
		st["push"] != true || st["exitCode"] != nil {
		t.Fatalf("structured = %v\n%s", st, text)
	}
	if !strings.HasPrefix(text, "job "+queuedJobName+" on box my-repo-ab12: queued, 2nd in line, waiting for room, push: true") {
		t.Errorf("text = %q", text)
	}
	if tail, _ := st["tail"].(string); !strings.Contains(tail, "it starts on a new box, my-repo-ab12, when it can") {
		t.Errorf("the tail doesn't say which box the job will be on (#89): %q", tail)
	}
	r := b.req
	if !r.New || r.Box != "my-repo" || r.Checkout != "my-repo" || r.Size != "medium" || r.Timeout != 90 ||
		r.Env["CI"] != "1" || len(r.Command) != 1 || r.Command[0] != "go test ./..." || r.Ref == "" || b.bundle == 0 {
		t.Errorf("request = %+v, bundle %d bytes", r, b.bundle)
	}
	var meta jobMeta
	if json.Unmarshal(r.Meta, &meta) != nil || meta.Reason == "" || meta.Command != "go test ./..." {
		t.Errorf("meta = %s", r.Meta)
	}
	if !strings.Contains(b.callList(), "POST agent-box.example.ts.net/api/queue") {
		t.Errorf("calls %s", b.callList())
	}
	var rec jobRecord
	if len(*e.spawns) != 1 || readJSON(e.a.statePath(queuedJobName, ".json"), &rec) != nil || !rec.Queued || !rec.New {
		t.Errorf("watcher %v, record %+v", *e.spawns, rec)
	}
}

func TestRunsThatAreNotQueued(t *testing.T) {
	for name, c := range map[string]struct {
		f     *fakePC
		extra map[string]any
		code  int
	}{
		"queue false":  {&fakePC{refuse: memoryRefusal}, map[string]any{"queue": false}, 0},
		"push false":   {&fakePC{refuse: memoryRefusal}, map[string]any{"push": false}, 0},
		"gpu in mypc":  {&fakePC{mode: "mypc"}, map[string]any{"gpu": true}, 0},
		"unreachable":  {&fakePC{listErr: true}, nil, 0},
		"queue full":   {&fakePC{refuse: memoryRefusal}, nil, 507},
		"boxd refuses": {&fakePC{mode: "mypc"}, map[string]any{"gpu": true}, 409},
	} {
		t.Run(name, func(t *testing.T) {
			e, b := newQueueEnv(t, c.f)
			b.code = c.code
			text, st := e.call(t, "box_run", e.runArgs(c.extra, "true"))
			if st["state"] != "refused" || st["exitCode"] != 75.0 || len(*e.spawns) != 0 {
				t.Fatalf("structured = %v\n%s", st, text)
			}
			posted := strings.Contains(b.callList(), "/api/queue")
			if posted != (c.code != 0 && name != "boxd refuses") {
				t.Errorf("queue posts: %s", b.callList())
			}
			if c.code == 507 && !strings.Contains(text, "not queued: boxd didn't queue the run: the queue is full") {
				t.Errorf("text = %q", text)
			}
		})
	}
}

// A run turned away by My PC waits for a throwaway: the repo's box wasn't
// checked, and the tray may stop it while it is frozen.
func TestQueuedRunInMyPCIsQueued(t *testing.T) {
	e, b := newQueueEnv(t, &fakePC{mode: "mypc"})
	b.reply.Why = dash.RefusedMyPC
	_, st := e.call(t, "box_run", e.runArgs(map[string]any{"size": "large"}, "true"))
	if st["state"] != "queued" || st["why"] != "mypc" || !b.req.New || b.req.Box != "my-repo" || b.req.Size != "large" {
		t.Fatalf("structured = %v, request %+v", st, b.req)
	}
}

// A job boxd had queued that leaves the queue with no box (cancelled from
// the page) never ran, and is pushed as such.
func TestWatchReportsAJobTakenOffTheQueue(t *testing.T) {
	e, b := newQueueEnv(t, &fakePC{scriptRC: 3})
	queued := time.Date(2026, 11, 23, 11, 55, 0, 0, time.UTC)
	b.views = []dash.Job{{State: dash.JobQueued, QueuedAt: &queued, StartedAt: queued, Position: 1, Why: dash.RefusedFull}}
	writeJSON(e.a.statePath(queuedJobName, ".json"), jobRecord{Job: queuedJobName, Thread: "thread-1", Queued: true})
	go func() {
		time.Sleep(50 * time.Millisecond)
		b.mu.Lock()
		b.gone = true
		b.mu.Unlock()
	}()
	if err := e.a.Watch(context.Background(), queuedJobName); err != nil {
		t.Fatal(err)
	}
	if e.push.count() != 1 || !strings.Contains(e.push.sent[0].msg, "left the queue without running") {
		t.Fatalf("pushed %+v", e.push.sent)
	}
}

// A job that ran and ended while nobody watched gets boxd's result, though
// its kept box is gone.
func TestWatchTakesAQueuedJobsEndFromBoxd(t *testing.T) {
	e, b := newQueueEnv(t, &fakePC{})
	queued := time.Date(2026, 11, 23, 11, 0, 0, 0, time.UTC)
	started, ended, exit := queued.Add(10*time.Minute), queued.Add(25*time.Minute), 3
	b.views = []dash.Job{{State: dash.JobDone, QueuedAt: &queued, StartedAt: started, EndedAt: &ended, Exit: &exit, Tail: "a\nFAIL b\n"}}
	writeJSON(e.a.statePath(queuedJobName, ".json"), jobRecord{Job: queuedJobName, Thread: "thread-1", Queued: true})
	if err := e.a.Watch(context.Background(), queuedJobName); err != nil {
		t.Fatal(err)
	}
	if e.push.count() != 1 || !strings.Contains(e.push.sent[0].msg, "ended, exit 3, after 15m00s") || !strings.Contains(e.push.sent[0].msg, "FAIL b") {
		t.Fatalf("pushed %+v", e.push.sent)
	}
	if strings.Contains(e.f.callList(), "exec") {
		t.Errorf("looked in the box: %s", e.f.callList())
	}
}

// The watcher follows a queued job through the queue: one nudge at 20
// minutes, then the box once it starts.
func TestWatchFollowsAQueuedJob(t *testing.T) {
	f := &fakePC{}
	e, b := newQueueEnv(t, f)
	queued := time.Date(2026, 11, 23, 11, 30, 0, 0, time.UTC) // 30 minutes before the test clock
	started := queued.Add(31 * time.Minute)
	b.views = []dash.Job{
		{Box: "my-repo-ab12", State: dash.JobQueued, QueuedAt: &queued, StartedAt: queued, Position: 1, Why: dash.RefusedFull},
		{Box: "my-repo-ab12", State: dash.JobQueued, QueuedAt: &queued, StartedAt: queued, Position: 1, Why: dash.RefusedFull},
		{Box: "my-repo-ab12", State: dash.JobRunning, QueuedAt: &queued, StartedAt: started},
	}
	writeJSON(e.a.statePath(queuedJobName, ".json"), jobRecord{Job: queuedJobName, Thread: "thread-1", Reason: "r", Command: "true", Queued: true, New: true})
	f.insts = []instance{{Name: "my-repo-ab12", Status: "Running", Config: map[string]string{throwawayKey: "true"}}}
	f.log, f.exit = "ok\n", "0\n"
	if err := e.a.Watch(context.Background(), queuedJobName); err != nil {
		t.Fatal(err)
	}
	if e.push.count() != 2 {
		t.Fatalf("pushed %d: %+v", e.push.count(), e.push.sent)
	}
	nudge, end := e.push.sent[0].msg, e.push.sent[1].msg
	if !strings.Contains(nudge, "box: job "+queuedJobName+" still queued after 30m00s (waiting for room). Can't wait? Run it locally and box_cancel this job.") {
		t.Errorf("nudge = %q", nudge)
	}
	if !strings.Contains(end, "box: job "+queuedJobName+" ended, exit 0") {
		t.Errorf("end = %q", end)
	}
}

func TestWatchReportsADroppedJob(t *testing.T) {
	e, b := newQueueEnv(t, &fakePC{})
	queued := time.Date(2026, 11, 22, 11, 0, 0, 0, time.UTC)
	ended := queued.Add(24 * time.Hour)
	b.views = []dash.Job{{State: dash.JobLost, QueuedAt: &queued, StartedAt: queued, EndedAt: &ended}}
	writeJSON(e.a.statePath(queuedJobName, ".json"), jobRecord{Job: queuedJobName, Thread: "thread-1", Reason: "r", Command: "true", Queued: true})
	if err := e.a.Watch(context.Background(), queuedJobName); err != nil {
		t.Fatal(err)
	}
	if e.push.count() != 1 || !strings.Contains(e.push.sent[0].msg, "left the queue without running") {
		t.Fatalf("pushed %+v", e.push.sent)
	}
}

func TestCancelAQueuedJob(t *testing.T) {
	e, b := newQueueEnv(t, &fakePC{insts: []instance{{Name: "my-repo-ab12", Status: "Running", Config: map[string]string{throwawayKey: "true"}}}})
	writeJSON(e.a.statePath(queuedJobName, ".json"), jobRecord{Job: queuedJobName, Thread: "thread-1", Queued: true})
	text, st := e.call(t, "box_cancel", map[string]any{"id": queuedJobName})
	if !strings.Contains(text, "removed "+queuedJobName+" from the queue") || st["cancelled"] != true ||
		!e.a.exists(queuedJobName, ".notified") || strings.Contains(e.f.callList(), "exec") {
		t.Fatalf("%q %v, incus calls %q", text, st, e.f.callList())
	}
	if !strings.Contains(b.callList(), "POST agent-box.example.ts.net/api/queue/"+queuedJobName+"/cancel") {
		t.Errorf("calls %s", b.callList())
	}

	// Once it has started, box_cancel stops it in its box.
	b.cancel = 409
	e.f.script = "cancelled 130\n"
	text, _ = e.call(t, "box_cancel", map[string]any{"id": queuedJobName})
	if !strings.Contains(text, "cancelled "+queuedJobName+" (exit 130)") || !strings.Contains(e.f.callList(), "exec") {
		t.Errorf("%q, calls %q", text, e.f.callList())
	}
}

func TestBoxJobOnAQueuedJob(t *testing.T) {
	e, b := newQueueEnv(t, &fakePC{})
	queued := time.Date(2026, 11, 23, 11, 50, 0, 0, time.UTC)
	b.views = []dash.Job{{Box: "my-repo-ab12", State: dash.JobQueued, QueuedAt: &queued, StartedAt: queued, Position: 3, Why: dash.RefusedGPUBusy}}
	writeJSON(e.a.statePath(queuedJobName, ".json"), jobRecord{Job: queuedJobName, Thread: "thread-1", Queued: true})
	text, st := e.call(t, "box_job", map[string]any{"id": queuedJobName})
	if st["state"] != "queued" || st["position"] != 3.0 || !strings.Contains(text, "queued, 3rd in line, waiting for the GPU") {
		t.Fatalf("%q %v", text, st)
	}
}

func TestKeptThrowawaysGiveWayToALaunch(t *testing.T) {
	soon := "2026-11-23T12:20:00Z" // within the keep window of the test clock's 12:00
	f := &fakePC{t: t, refuse: memoryRefusal, kept: map[string]bool{"old-0001": true, "fresh-0002": false},
		insts: []instance{
			{Name: "old-0001", Status: "Running", Config: map[string]string{throwawayKey: "true", expiresKey: soon}},
			{Name: "fresh-0002", Status: "Running", Config: map[string]string{throwawayKey: "true", expiresKey: soon}},
			{Name: "far-0003", Status: "Running", Config: map[string]string{throwawayKey: "true", expiresKey: "2026-11-23T14:00:00Z"}},
		}}
	f.refuseOnce = true
	a, out := testApp(f)
	if err := a.launch(context.Background(), "new-1", time.Hour, true, mustSize(t, "small")); err != nil {
		t.Fatalf("launch: %v\n%s", err, out)
	}
	if names := f.names(); strings.Contains(names, "old-0001") || !strings.Contains(names, "fresh-0002") || !strings.Contains(names, "far-0003") {
		t.Errorf("boxes left: %s (calls %s)", names, f.callList())
	}
	if calls := f.callList(); calls != "launch list list exec exec project project project project project delete launch" { // far-0003 isn't even checked; the projects are the room check (#87)
		t.Errorf("calls: %s", calls)
	}
	if !strings.Contains(out.String(), "box: deleting old-0001, kept after its job, for room") {
		t.Errorf("output %q", out)
	}
}

// A launch that deleting every kept throwaway still couldn't fit leaves them
// alone, for their own box pull (#87).
func TestKeptThrowawaysStayWhenTheyCantMakeRoom(t *testing.T) {
	soon := "2026-11-23T12:20:00Z"
	f := &fakePC{t: t, refuse: memoryRefusal, kept: map[string]bool{"old-0001": true},
		project: map[string]string{"limits.memory": "24GiB"},
		insts: []instance{
			{Name: "old-0001", Status: "Running", Config: map[string]string{throwawayKey: "true", expiresKey: soon, "user.box.size": "small", "limits.memory": "2GiB"}},
			{Name: "busy-0002", Status: "Running", Config: map[string]string{"user.box.size": "large", "limits.memory": "20GiB"}},
		}}
	a, out := testApp(f)
	if err := a.launch(context.Background(), "new-1", time.Hour, true, mustSize(t, "large")); !isNoRoom(err) {
		t.Fatalf("launch: %v\n%s", err, out)
	}
	if !strings.Contains(f.names(), "old-0001") || !strings.Contains(out.String(), "wouldn't make room for a large box; leaving them") {
		t.Errorf("boxes %s, output %q", f.names(), out)
	}
}

// A delete another launch got to first is no failure (#87).
func TestIsGone(t *testing.T) {
	if !isGone(errors.New(`incus delete: Error: Failed deleting instance pc:x in project "agents": Instance not found`)) ||
		!isGone(errors.New("incus delete: Error: Stopping the instance pc:x failed: The instance is already stopped")) ||
		isGone(errors.New("incus delete: Error: Failed to delete the storage volume")) {
		t.Error("isGone is wrong about a delete")
	}
}

func mustSize(t *testing.T, name string) mode.Size {
	t.Helper()
	sz, err := lookupSize(name)
	if err != nil {
		t.Fatal(err)
	}
	return sz
}

// A job boxd is starting can't leave the queue, nor be stopped in a box yet:
// box_cancel waits for the start, then stops it there.
func TestCancelAJobBeingStarted(t *testing.T) {
	e, b := newQueueEnv(t, &fakePC{script: "cancelled 130\n", insts: []instance{{Name: "my-repo-ab12", Status: "Running", Config: map[string]string{throwawayKey: "true"}}}})
	queued := time.Date(2026, 11, 23, 11, 55, 0, 0, time.UTC)
	b.cancel = 409
	b.views = []dash.Job{{State: dash.JobQueued, QueuedAt: &queued, StartedAt: queued}, {State: dash.JobRunning, QueuedAt: &queued, StartedAt: queued.Add(time.Minute)}}
	writeJSON(e.a.statePath(queuedJobName, ".json"), jobRecord{Job: queuedJobName, Thread: "thread-1", Queued: true})
	text, _ := e.call(t, "box_cancel", map[string]any{"id": queuedJobName})
	if !strings.Contains(text, "cancelled "+queuedJobName+" (exit 130)") || strings.Count(b.callList(), "/cancel") != 2 {
		t.Fatalf("%q, boxd calls %s", text, b.callList())
	}
}

// A job nobody here read whose box has gone gives boxd's record of its end,
// not "lost" (#90).
func TestJobAfterBoxGoneAsksBoxd(t *testing.T) {
	e, b := newQueueEnv(t, &fakePC{})
	exit := 3
	b.views = []dash.Job{{Box: "my-repo-ab12", ID: "20261123-120000-cd34", State: dash.JobDone, Exit: &exit, Tail: "boxd's tail"}}
	var out strings.Builder
	e.a.Out = &out
	r, err := e.a.Job(context.Background(), queuedJobName, 0)
	if err != nil || r.State != stateDone || r.Code != 3 || out.String() != "boxd's tail" {
		t.Fatalf("Job = %+v, %v, out %q", r, err, out.String())
	}
	if _, st := e.call(t, "box_job", map[string]any{"id": queuedJobName, "offset": 0}); st["exitCode"] != 3.0 || st["tail"] != "boxd's tail" {
		t.Errorf("box_job page after the box went = %v", st)
	}
	b.gone = true
	if _, err := e.a.Job(context.Background(), queuedJobName, 0); err == nil {
		t.Error("a job neither the box nor boxd knows wasn't an error")
	}
}

// box cancel takes a job queued from anywhere off the queue: with no record
// here, its box not being there yet sends it to boxd (#100).
func TestCLICancelOfAJobQueuedElsewhere(t *testing.T) {
	e, b := newQueueEnv(t, &fakePC{gone: map[string]bool{"my-repo-ab12": true}})
	var out strings.Builder
	e.a.Out = &out
	if c := run(context.Background(), e.a, []string{"cancel", queuedJobName}, nil); c != 0 || !strings.Contains(out.String(), "removed "+queuedJobName+" from the queue") {
		t.Fatalf("box cancel: exit %d, %q", c, out.String())
	}
	if !strings.Contains(b.callList(), "/api/queue/"+queuedJobName+"/cancel") {
		t.Errorf("boxd calls: %s", b.callList())
	}
}
