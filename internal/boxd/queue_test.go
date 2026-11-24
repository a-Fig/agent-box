package boxd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
)

func queueMeta(session string) json.RawMessage {
	return json.RawMessage(`{"v":1,"reason":"Race suite while the PC is busy; queue it.","command":"go test -race ./...",` +
		`"repo":"https://github.com/acme/widget.git","branch":"main","commit":"abc","agent":{"client":"claude-code","display":"` + session +
		`","worktree":"C:\\src\\widget"},"size":"small","gpu":false,"envNames":[]}`)
}

type queueRig struct {
	t   *testing.T
	f   *fakeIncus
	c   *clock
	w   *Watcher
	srv *httptest.Server
	dir string
}

func newQueueRig(t *testing.T, cfg map[string]string) *queueRig {
	dir := t.TempDir()
	q, err := OpenQueue(filepath.Join(dir, "queue"))
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: t0}
	f := &fakeIncus{cfg: cfg, jobs: map[string][]fakeJob{}}
	w := newWatcher(t, filepath.Join(dir, "history"), f, c)
	w.Queue = q
	rig := &queueRig{t: t, f: f, c: c, w: w, srv: httptest.NewServer((&Server{W: w}).Handler()), dir: dir}
	t.Cleanup(rig.srv.Close)
	w.Poll(context.Background())
	return rig
}

// enqueue posts a request and a bundle; it returns the status and reply.
func (r *queueRig) enqueue(req dash.QueueRequest, bundle string) (int, dash.QueueReply, dash.ErrorBody) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	pw, _ := mw.CreateFormField("request")
	json.NewEncoder(pw).Encode(req)
	fw, _ := mw.CreateFormFile("bundle", "snapshot.bundle")
	fw.Write([]byte(bundle))
	mw.Close()
	hr, _ := http.NewRequest("POST", r.srv.URL+"/api/queue", &body)
	hr.Header.Set("Content-Type", mw.FormDataContentType())
	hr.Header.Set(dash.ControlHeader, "1")
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var rep dash.QueueReply
	var eb dash.ErrorBody
	json.Unmarshal(b, &rep)
	json.Unmarshal(b, &eb)
	return resp.StatusCode, rep, eb
}

// settle polls until no start is in flight.
func (r *queueRig) poll() {
	r.w.Poll(context.Background())
	for i := 0; i < 200; i++ {
		r.w.mu.Lock()
		busy := r.w.starting != ""
		r.w.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.t.Fatal("a start never finished")
}

func small(box, session string) dash.QueueRequest {
	return dash.QueueRequest{Box: box, New: true, Size: "small", Checkout: "widget", Ref: "20261124-180000-aaaa",
		Command: []string{"go test -race ./..."}, Meta: queueMeta(session)}
}

func TestQueueWaitsForRoomThenStarts(t *testing.T) {
	// A 1GiB memory cap: no small box (2GiB) fits.
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "full", "limits.memory": "1GiB", "limits.instances": "10"})
	code, rep, eb := r.enqueue(small("widget", "quiet-otter"), "BUNDLE")
	if code != 201 || rep.Position != 1 || rep.Why != dash.RefusedFull || !strings.HasPrefix(rep.Job, "widget-") {
		t.Fatalf("enqueue: %d %+v %+v", code, rep, eb)
	}
	st := r.w.State()
	if len(st.Jobs) != 1 || st.Jobs[0].State != dash.JobQueued || st.Jobs[0].Agent.Session != "quiet-otter" ||
		st.Jobs[0].Reason == "" || st.Jobs[0].QueuedAt == nil {
		t.Fatalf("state: %+v", st.Jobs)
	}
	bundle := r.w.Queue.BundlePath(rep.Job)
	if b, _ := os.ReadFile(bundle); string(b) != "BUNDLE" {
		t.Fatalf("bundle %q", b)
	}
	r.poll()
	if len(r.f.cli) != 0 {
		t.Fatalf("started with no room: %v", r.f.cli)
	}

	// Room appears 30 minutes later.
	r.f.set(func() { r.f.cfg["limits.memory"] = "24GiB" })
	r.c.add(30 * time.Minute)
	r.poll() // the poll that sees the room starts it
	cli := strings.Join(r.f.cli, "\n")
	box, _, _ := strings.Cut(rep.Job, ".")
	for _, want := range []string{"launch agent-base " + box + " -c user.expires=", "user.box.size=small", "user.box.throwaway=true",
		"file push --create-dirs " + bundle + " " + box + "/work/.box/" + rep.Job + ".bundle",
		".meta.tmp " + box + "/work/.box/" + rep.Job + ".meta", "config set " + box + " user.expires=", "exec " + box + " -- bash -lc "} {
		if !strings.Contains(cli, want) {
			t.Errorf("no %q in\n%s", want, cli)
		}
	}
	if _, err := os.Stat(bundle); !os.IsNotExist(err) {
		t.Error("the snapshot outlived the start")
	}
	if q := r.w.Queued(); len(q) != 0 {
		t.Fatalf("still queued: %+v", q)
	}

	// The started job: its .meta says when it was queued and started, and
	// its key is the start, so the box launched 30 minutes after the ID's time
	// is still its box.
	started := r.c.now().UTC().Format(time.RFC3339)
	m := strings.TrimSuffix(string(queueMeta("quiet-otter")), "}") + `,"queuedAt":"2026-11-24T18:00:00Z","startedAt":"` + started + `"}`
	r.f.set(func() {
		for i := range r.f.ins {
			r.f.ins[i].CreatedAt = r.c.now()
			r.f.ins[i].Config["user.box.throwaway"] = "true"
		}
		r.f.jobs[box] = []fakeJob{{name: rep.Job, state: "running", log: "ok\n", meta: m}}
	})
	r.c.add(3 * time.Second)
	r.poll()
	st = r.w.State()
	if len(st.Jobs) != 1 || st.Jobs[0].State != dash.JobRunning || st.Jobs[0].QueuedAt == nil ||
		st.Jobs[0].StartedAt.Format(time.RFC3339) != started {
		t.Fatalf("running: %+v", st.Jobs)
	}

	// It ends: boxd deletes the throwaway it launched.
	r.f.set(func() { r.f.jobs[box][0].state, r.f.jobs[box][0].exit = "done", "0" })
	r.c.add(3 * time.Second)
	r.poll()
	time.Sleep(50 * time.Millisecond)
	r.f.mu.Lock()
	last := r.f.cli[len(r.f.cli)-1]
	r.f.mu.Unlock()
	if last != "delete "+box+" --force" {
		t.Fatalf("throwaway not deleted: %s", last)
	}
	var h dash.History
	resp, _ := http.Get(r.srv.URL + "/api/history?range=day")
	json.NewDecoder(resp.Body).Decode(&h)
	resp.Body.Close()
	if b := h.Buckets[23]; b.Queued.Jobs != 1 || b.Queued.MedianWaitSeconds != 1800 {
		t.Fatalf("history: %+v", b.Queued)
	}
}

func TestQueueIsFairAcrossSessions(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "mypc", "limits.memory": "24GiB", "limits.instances": "10"})
	// quiet-otter already runs two jobs.
	r.f.set(func() {
		r.f.ins = []Instance{inst("busy", "Running", "small", t0)}
		r.f.jobs["busy"] = []fakeJob{
			{name: "busy.20261124-180000-0001", state: "running", log: "x", meta: string(queueMeta("quiet-otter"))},
			{name: "busy.20261124-180000-0002", state: "running", log: "x", meta: string(queueMeta("quiet-otter"))},
		}
	})
	r.c.add(time.Minute)
	r.poll()
	_, first, _ := r.enqueue(small("a", "quiet-otter"), "A")
	_, second, _ := r.enqueue(small("b", "brisk-heron"), "B")
	if first.Why != dash.RefusedMyPC || second.Position != 2 {
		t.Fatalf("%+v %+v", first, second)
	}
	r.f.set(func() { r.f.cfg["user.agent-box.mode"] = "full" })
	r.poll()
	if len(r.f.cli) == 0 || !strings.Contains(r.f.cli[0], "launch agent-base b-") {
		t.Fatalf("brisk-heron's job should start first: %v", r.f.cli)
	}

	// Front, then cancel, the one left.
	req, _ := http.NewRequest("POST", r.srv.URL+"/api/queue/"+first.Job+"/front", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(dash.ControlHeader, "1")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("front: %d", resp.StatusCode)
	}
	bundle := r.w.Queue.BundlePath(first.Job)
	r.f.set(func() { r.f.cfg["user.agent-box.mode"] = "mypc" })
	r.poll()
	req, _ = http.NewRequest("POST", r.srv.URL+"/api/queue/"+first.Job+"/cancel", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(dash.ControlHeader, "1")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("cancel: %d", resp.StatusCode)
	}
	if _, err := os.Stat(bundle); !os.IsNotExist(err) || len(r.w.Queued()) != 0 {
		t.Fatal("cancel left the job or its snapshot")
	}
}

func TestQueueRefusalsAndLimits(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "mypc", "user.agent-box.reason": "gaming: x", "limits.instances": "10"})
	gpu := small("g", "s")
	gpu.GPU = true
	if code, _, eb := r.enqueue(gpu, "G"); code != 409 || eb.Code != dash.CodeYielding {
		t.Fatalf("gpu while yielding: %d %+v", code, eb)
	}
	bad := small("w", "s")
	bad.Checkout = "../etc"
	if code, _, _ := r.enqueue(bad, "X"); code != 400 {
		t.Fatalf("bad checkout: %d", code)
	}
	bad = small("w", "s")
	bad.Env = map[string]string{"GH_TOKEN": "x"}
	if code, _, _ := r.enqueue(bad, "X"); code != 400 {
		t.Fatalf("secret env: %d", code)
	}
	r.w.Queue.max = 10
	if code, _, eb := r.enqueue(small("w", "s"), "0123456789AB"); code != 507 || eb.Code != dash.CodeQueueFull {
		t.Fatalf("over the cap: %d %+v", code, eb)
	}
	r.w.Queue.max = dash.QueueBytes
	_, rep, _ := r.enqueue(small("w", "s"), "OK")

	// A restart reloads the queue and drops a stray bundle.
	stray := filepath.Join(r.dir, "queue", "stray.20261124-180000-ffff.bundle")
	os.WriteFile(stray, []byte("x"), 0o600)
	q2, err := OpenQueue(filepath.Join(r.dir, "queue"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := q2.get(rep.Job); !ok {
		t.Fatal("not reloaded")
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatal("stray bundle kept")
	}

	// A start that fails drops the job as lost and removes its throwaway.
	r.f.set(func() {
		r.f.cfg["user.agent-box.mode"], r.f.cfg["user.agent-box.reason"] = "full", ""
		r.f.cliErr = func(args []string) error {
			if args[0] == "exec" {
				return errors.New("exec failed")
			}
			return nil
		}
	})
	r.poll()
	time.Sleep(20 * time.Millisecond)
	if len(r.w.Queued()) != 0 {
		t.Fatal("a failed start stayed queued")
	}
	var jobs []dash.Job
	for _, j := range r.w.State().Jobs {
		jobs = append(jobs, j)
	}
	if len(jobs) != 1 || jobs[0].State != dash.JobLost {
		t.Fatalf("after a failed start: %+v", jobs)
	}
	if last := r.f.cli[len(r.f.cli)-1]; !strings.HasPrefix(last, "delete ") {
		t.Fatalf("throwaway kept: %s", fmt.Sprint(r.f.cli))
	}

	// Over 24 hours waiting: dropped.
	r.f.set(func() { r.f.cliErr = nil; r.f.cfg["user.agent-box.mode"] = "mypc" })
	_, rep, _ = r.enqueue(small("w", "s"), "OK")
	r.c.add(25 * time.Hour)
	r.poll()
	if len(r.w.Queued()) != 0 {
		t.Fatal("kept past 24 hours")
	}
}
