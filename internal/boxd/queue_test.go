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

	// It ends: boxd keeps the throwaway it launched for half an hour.
	r.f.set(func() { r.f.jobs[box][0].state, r.f.jobs[box][0].exit = "done", "0" })
	r.c.add(3 * time.Second)
	r.poll()
	time.Sleep(50 * time.Millisecond)
	r.f.mu.Lock()
	sets := r.f.expirySets
	r.f.mu.Unlock()
	if want := box + " " + r.c.now().UTC().Add(30*time.Minute).Format(time.RFC3339); len(sets) != 1 || sets[0] != want {
		t.Fatalf("throwaway's expiry: %v, want %s", sets, want)
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
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "mypc", "limits.instances": "10"})
	gpu := small("g", "s")
	gpu.GPU = true
	if code, _, eb := r.enqueue(gpu, "G"); code != 409 || eb.Code != dash.CodeFrozen {
		t.Fatalf("gpu in My PC: %d %+v", code, eb)
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
		r.f.cfg["user.agent-box.mode"] = "full"
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

// From the review of 820d791: a request with no meta, and Incus refusing
// a launch at the instance cap, which must leave the job waiting.
func TestQueueNoMetaAndInstanceCap(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "full", "limits.memory": "24GiB"})
	r.f.set(func() {
		r.f.cliErr = func(args []string) error {
			if args[0] == "launch" {
				return errors.New(`incus launch: Error: Failed instance creation: Reached maximum number of instances in project "agents"`)
			}
			return nil
		}
	})
	req := small("w", "s")
	req.Meta = nil
	code, rep, eb := r.enqueue(req, "B")
	if code != 201 {
		t.Fatalf("no meta: %d %+v", code, eb)
	}
	r.poll()
	if q := r.w.Queued(); len(q) != 1 || q[0].Box+"."+q[0].ID != rep.Job {
		t.Fatalf("a launch refused at the cap dropped the job: %+v", q)
	}
	r.f.set(func() { r.f.cliErr = nil })
	r.c.add(noRoomBackoff + time.Second)
	r.poll()
	if q := r.w.Queued(); len(q) != 0 {
		t.Fatalf("not started after the backoff: %+v", q)
	}
	if j, ok := r.w.QueuedView(rep.Job); !ok || j.State != dash.JobRunning {
		t.Fatalf("GET /api/queue/{job} right after the start: %+v %v", j, ok)
	}
}

// A throwaway kept after its job ended gives way to a queued job that needs
// its room; one whose job hasn't ended, or that isn't kept, doesn't.
func TestQueueDeletesKeptThrowawayForRoom(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "full", "limits.memory": "24GiB", "limits.instances": "2"})
	r.f.set(func() {
		repo := inst("widget", "Running", "small", t0)
		fresh := inst("fresh-0001", "Running", "small", t0)
		fresh.Config["user.box.throwaway"] = "true" // box run's, with its 2h launch expiry
		r.f.ins = append(r.f.ins, repo, fresh)
		r.f.jobs["fresh-0001"] = []fakeJob{{name: "fresh-0001.20261124-180000-bbbb", state: "running", log: "ok\n", meta: metaJSON}}
	})
	r.poll()
	code, rep, _ := r.enqueue(small("job", "quiet-otter"), "B")
	if code != 201 || rep.Why != dash.RefusedFull {
		t.Fatalf("enqueue: %d %+v", code, rep)
	}
	r.poll()
	if len(r.f.cli) != 0 {
		t.Fatalf("made room from a box not kept: %v", r.f.cli)
	}

	// A short expiry alone isn't enough: its job still runs.
	r.f.set(func() {
		r.f.ins[1].Config["user.expires"] = r.c.now().Add(30 * time.Minute).Format(time.RFC3339)
	})
	r.c.add(3 * time.Second)
	r.poll()
	if len(r.f.cli) != 0 {
		t.Fatalf("made room from a box with a job running: %v", r.f.cli)
	}

	// fresh's job ended and nobody read it: boxd gives it 30 minutes, as box
	// would (its result stays in boxd's history, #90), so its room is spare.
	r.f.set(func() {
		r.f.jobs["fresh-0001"][0].state, r.f.jobs["fresh-0001"][0].exit = "done", "0"
		r.f.ins[1].Config["user.expires"] = r.c.now().Add(jobTTL).Format(time.RFC3339)
	})
	r.c.add(3 * time.Second)
	r.poll()
	for i := 0; i < 50; i++ {
		r.f.mu.Lock()
		n := len(r.f.expirySets)
		r.f.mu.Unlock()
		if n > 0 {
			break
		}
		if i == 49 {
			t.Fatal("boxd didn't shorten the unread throwaway's expiry")
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.c.add(3 * time.Second)
	r.poll()
	if len(r.f.cli) < 2 || r.f.cli[0] != "delete fresh-0001 --force" || !strings.HasPrefix(r.f.cli[1], "launch agent-base job-") {
		t.Fatalf("cli: %v", r.f.cli)
	}
	if q := r.w.Queued(); len(q) != 0 {
		t.Fatalf("still queued: %+v", q)
	}
}

// A start cut short by a boxd restart doesn't run the job twice: boxd looks
// in the box first, and starts it only if its files aren't there.
func TestQueueStartCutShortIsNotRepeated(t *testing.T) {
	for _, inBox := range []bool{true, false} {
		r := newQueueRig(t, map[string]string{"user.agent-box.mode": "mypc", "limits.memory": "24GiB", "limits.instances": "10"})
		_, rep, _ := r.enqueue(small("widget", "quiet-otter"), "B")
		box, _, _ := strings.Cut(rep.Job, ".")
		it, _ := r.w.Queue.get(rep.Job)
		began := r.c.now().UTC()
		if err := r.w.Queue.markStarting(it, began); err != nil {
			t.Fatal(err)
		}
		// The restart: the store reloads it marked, and the box it launched is there.
		q, err := OpenQueue(filepath.Join(r.dir, "queue"))
		if err != nil {
			t.Fatal(err)
		}
		if re, ok := q.get(rep.Job); !ok || re.Starting == nil || !re.Starting.Equal(began) {
			t.Fatalf("reloaded: %+v", re)
		}
		r.f.set(func() {
			in := inst(box, "Running", "small", r.c.now())
			in.Config["user.box.throwaway"] = "true"
			r.f.ins = append(r.f.ins, in)
			if inBox {
				r.f.jobs[box] = []fakeJob{{name: rep.Job, state: "running", log: "x"}}
			}
			r.f.cfg["user.agent-box.mode"] = "full"
		})
		r.c.add(3 * time.Second)
		r.poll()
		cli := strings.Join(r.f.cli, "\n")
		if strings.Contains(cli, "launch ") || strings.Contains(cli, "exec "+box+" -- bash -lc ") == inBox {
			t.Fatalf("in box %v: %s", inBox, cli)
		}
		if q := r.w.Queued(); len(q) != 0 {
			t.Fatalf("in box %v: still queued: %+v", inBox, q)
		}
	}
}

// A second attempt at a start moves the mark to its own time, the start
// time its .meta holds, on disk too: a third boxd that finds the job in the
// box gives it the key the scan does.
func TestQueueStartMarkIsTheLatestAttempt(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "mypc", "limits.memory": "24GiB", "limits.instances": "10"})
	_, rep, _ := r.enqueue(small("widget", "quiet-otter"), "B")
	it, _ := r.w.Queue.get(rep.Job)
	first, second := t0, t0.Add(time.Minute)
	r.w.Queue.markStarting(it, first)
	r.w.Queue.markStarting(it, second)
	q, err := OpenQueue(filepath.Join(r.dir, "queue"))
	if err != nil {
		t.Fatal(err)
	}
	re, ok := q.get(rep.Job)
	if !ok || re.Starting == nil || !re.Starting.Equal(second) || !q.wasRestored(re) {
		t.Fatalf("reloaded: %+v", re)
	}
}

// restart is a boxd started again on the same queue, history and Incus. It
// hasn't polled yet.
func (r *queueRig) restart() *Watcher {
	r.t.Helper()
	q, err := OpenQueue(filepath.Join(r.dir, "queue"))
	if err != nil {
		r.t.Fatal(err)
	}
	w := newWatcher(r.t, filepath.Join(r.dir, "history"), r.f, r.c)
	w.Queue = q
	return w
}

// A job whose start a restart cut short may be running in its box: until a
// start looks there, it isn't cancelled or dropped as if it only waited, nor
// before boxd has listed the boxes. Once its box is gone it is only waiting.
func TestQueueRestoredJobWithBoxIsNotCancelled(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "mypc", "limits.memory": "24GiB", "limits.instances": "10"})
	_, rep, _ := r.enqueue(small("widget", "quiet-otter"), "B")
	box, _, _ := strings.Cut(rep.Job, ".")
	it, _ := r.w.Queue.get(rep.Job)
	r.w.Queue.markStarting(it, r.c.now().UTC())
	r.f.set(func() { r.f.ins = append(r.f.ins, inst(box, "Frozen", "small", r.c.now())) })
	w := r.restart()
	if w.CancelQueued(rep.Job, "tyler") {
		t.Fatal("cancelled before boxd listed the boxes")
	}
	w.Poll(context.Background())
	if w.CancelQueued(rep.Job, "tyler") {
		t.Fatal("cancelled a job that may run in its box")
	}
	r.c.add(dash.QueueMaxAge + time.Minute)
	w.Poll(context.Background())
	if q := w.Queued(); len(q) != 1 {
		t.Fatalf("dropped while its box is there: %+v", q)
	}
	r.f.set(func() { r.f.ins = nil }) // the box reaped at its expiry
	w.Poll(context.Background())
	if q := w.Queued(); len(q) != 0 {
		t.Fatalf("still queued with its box gone: %+v", q)
	}
}

// Restored without its box, a job is only waiting, and cancels.
func TestQueueRestoredJobWithoutBoxCancels(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "mypc", "limits.memory": "24GiB", "limits.instances": "10"})
	_, rep, _ := r.enqueue(small("widget", "quiet-otter"), "B")
	it, _ := r.w.Queue.get(rep.Job)
	r.w.Queue.markStarting(it, r.c.now().UTC())
	w := r.restart()
	w.Poll(context.Background())
	if !w.CancelQueued(rep.Job, "tyler") {
		t.Fatal("a restored job with no box didn't cancel")
	}
}

// A start that failed within one boxd (paused by My PC, its repo box still
// there) leaves the mark, but the job is waiting and cancels, as before.
func TestQueuePausedStartStillCancels(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "mypc", "limits.memory": "24GiB", "limits.instances": "10"})
	_, rep, _ := r.enqueue(small("widget", "quiet-otter"), "B")
	box, _, _ := strings.Cut(rep.Job, ".")
	it, _ := r.w.Queue.get(rep.Job)
	r.w.Queue.markStarting(it, r.c.now().UTC())
	r.f.set(func() { r.f.ins = append(r.f.ins, inst(box, "Frozen", "small", r.c.now())) })
	r.poll()
	if !r.w.CancelQueued(rep.Job, "tyler") {
		t.Fatal("a paused start's job didn't cancel")
	}
}

// A start that looks in a restored job's box clears restored, however it
// ends: a later pause leaves the job cancellable again.
func TestQueueStartClearsRestored(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "mypc", "limits.memory": "24GiB", "limits.instances": "10"})
	_, rep, _ := r.enqueue(small("widget", "quiet-otter"), "B")
	box, _, _ := strings.Cut(rep.Job, ".")
	it, _ := r.w.Queue.get(rep.Job)
	r.w.Queue.markStarting(it, r.c.now().UTC())
	r.f.set(func() {
		r.f.ins = append(r.f.ins, inst(box, "Running", "small", r.c.now()))
		r.f.cfg["user.agent-box.mode"] = "full"
		r.f.cliErr = func(args []string) error {
			if args[0] == "file" {
				return errors.New("push failed")
			}
			return nil
		}
	})
	w := r.restart()
	re, _ := w.Queue.get(rep.Job)
	w.Poll(context.Background())
	for i := 0; i < 200; i++ {
		w.mu.Lock()
		busy := w.starting != ""
		w.mu.Unlock()
		if !busy && !w.Queue.wasRestored(re) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("a start that looked left the job restored")
}
