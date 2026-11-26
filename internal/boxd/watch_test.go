package boxd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/jobrun"
)

// fakeIncus is a project whose boxes and job files a test sets.
type fakeIncus struct {
	mu     sync.Mutex
	ins    []Instance
	cfg    map[string]string
	jobs   map[string][]fakeJob // box -> its job files
	err    error
	execs  int
	cli    []string
	cliErr func(args []string) error
	cancel string // the last cancel script
	// SetExpiryIf: each write made, as "<box> <expiry>", and a write that
	// comes between a read and its PATCH (making it stale) when set.
	expirySets []string
	between    func(cfg map[string]string)
	running    map[string][]string // jobs the running-jobs script sees that no scan has yet
	runChecks  int                 // running-jobs scripts run
}

type fakeJob struct {
	name, state string
	exit        string
	log         string
	meta        string
	endedAt     int64
}

// SetExpiryIf decides on a copy, without the lock (decide runs Exec), as
// the real one decides between its GET and its PATCH. Writes replace the
// config map, so a Poll reading an earlier copy never races them.
func (f *fakeIncus) SetExpiryIf(ctx context.Context, name string, decide func(Instance) (time.Time, bool)) (bool, error) {
	for try := 0; try < 5; try++ {
		read, ok := f.instance(name)
		if !ok {
			return false, nil
		}
		until, ok := decide(read)
		if !ok {
			return false, nil
		}
		f.mu.Lock()
		i := slices.IndexFunc(f.ins, func(in Instance) bool { return in.Name == name })
		switch {
		case i < 0:
			f.mu.Unlock()
			return false, nil
		case f.between != nil: // another write lands first: the ETag no longer matches
			c := maps.Clone(f.ins[i].Config)
			f.between(c)
			f.ins[i].Config, f.between = c, nil
			f.mu.Unlock()
			continue
		}
		c := maps.Clone(f.ins[i].Config)
		c[jobrun.ExpiresKey] = until.UTC().Format(time.RFC3339)
		f.ins[i].Config = c
		f.expirySets = append(f.expirySets, name+" "+c[jobrun.ExpiresKey])
		f.mu.Unlock()
		return true, nil
	}
	return false, errStale
}

// instance is a copy of one instance, its config too.
func (f *fakeIncus) instance(name string) (Instance, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, in := range f.ins {
		if in.Name == name {
			in.Config = maps.Clone(in.Config)
			return in, true
		}
	}
	return Instance{}, false
}

func (f *fakeIncus) Instances(context.Context) ([]Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ins := append([]Instance(nil), f.ins...)
	for i := range ins {
		ins[i].Config = maps.Clone(ins[i].Config)
	}
	return ins, f.err
}

func (f *fakeIncus) Project(context.Context) (Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := Project{Config: f.cfg}
	p.Resources = map[string]struct {
		Limit int64 `json:"limit"`
		Usage int64 `json:"usage"`
	}{"disk": {Limit: 200 << 30, Usage: 50 << 30}}
	return p, f.err
}

func (f *fakeIncus) Exec(_ context.Context, box, script string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs++
	skip := map[string]bool{}
	for _, a := range args {
		skip[a] = true
	}
	if strings.HasPrefix(script, "# cancel") {
		f.cancel = script
		for i, j := range f.jobs[box] {
			if strings.Contains(script, "\nj='"+j.name+"'\n") {
				f.jobs[box][i].state, f.jobs[box][i].exit = "done", "130"
				return []byte("cancelled 130\n"), nil
			}
		}
		return nil, &ExitError{Code: 3}
	}
	if strings.HasPrefix(script, "# boxd scan") {
		var b strings.Builder
		for _, j := range f.jobs[box] {
			meta, tail := "-", "-"
			if !skip[j.name] {
				if j.meta != "" {
					meta = base64.StdEncoding.EncodeToString([]byte(j.meta))
				}
				if j.log != "" {
					t := j.log
					if len(t) > dash.TailBytes {
						t = t[len(t)-dash.TailBytes:]
					}
					tail = base64.StdEncoding.EncodeToString([]byte(t))
				}
			}
			exit := j.exit
			if exit == "" {
				exit = "-"
			}
			fmt.Fprintf(&b, "J %s %s %s %d %d %s %s\n", j.name, j.state, exit, len(j.log), j.endedAt, meta, tail)
		}
		return []byte(b.String()), nil
	}
	if strings.HasPrefix(script, "# running jobs") {
		f.runChecks++
		var b strings.Builder
		for _, j := range f.jobs[box] {
			if j.state == "running" {
				fmt.Fprintln(&b, j.name)
			}
		}
		for _, name := range f.running[box] {
			fmt.Fprintln(&b, name)
		}
		return []byte(b.String()), nil
	}
	if strings.HasPrefix(script, "test -e ") { // startedInBox, and startJob's own check
		for _, j := range f.jobs[box] {
			if strings.Contains(script, "/"+j.name+".log") {
				return nil, nil
			}
		}
		return nil, &ExitError{Code: 1}
	}
	for _, j := range f.jobs[box] {
		if strings.Contains(script, j.name+".log") {
			var off int
			fmt.Sscanf(script[strings.Index(script, "tail -c +")+len("tail -c +"):], "%d", &off)
			data := j.log[min(off-1, len(j.log)):]
			data = data[:min(len(data), dash.MaxLogChunk)]
			return []byte(fmt.Sprintf("%d\n%s", len(j.log), data)), nil
		}
	}
	return nil, &ExitError{Code: 3}
}

// CLI records the commands a queued job's start runs; a launch adds the box.
func (f *fakeIncus) CLI(_ context.Context, stdin string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cli = append(f.cli, strings.Join(args, " "))
	if f.cliErr != nil {
		return nil, f.cliErr(args)
	}
	switch args[0] {
	case "launch":
		size := "medium"
		for i, a := range args {
			if strings.HasPrefix(a, "user.box.size=") && i > 0 {
				size = strings.TrimPrefix(a, "user.box.size=")
			}
		}
		in := inst(args[2], "Running", size, t0)
		in.CreatedAt = time.Now().UTC()
		f.ins = append(f.ins, in)
	case "delete":
		for i, in := range f.ins {
			if in.Name == args[1] {
				f.ins = append(f.ins[:i], f.ins[i+1:]...)
				break
			}
		}
	}
	return nil, nil
}

func (f *fakeIncus) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

var t0 = time.Date(2026, 11, 24, 18, 0, 0, 0, time.UTC)

func inst(name, status, size string, created time.Time) Instance {
	in := Instance{Name: name, Status: status, CreatedAt: created,
		Config: map[string]string{"user.box.size": size, "limits.cpu": "2", "limits.memory": "4GiB", "user.expires": "2026-11-25T00:00:00Z"}}
	in.State = &struct {
		Pid    int64 `json:"pid"`
		Memory struct {
			Usage int64 `json:"usage"`
		} `json:"memory"`
		CPU struct {
			Usage int64 `json:"usage"`
		} `json:"cpu"`
	}{Pid: 1}
	in.State.Memory.Usage = 1 << 30
	return in
}

const metaJSON = `{"v":1,"reason":"Full race suite; it takes minutes on every core.","command":"go test -race ./...",
"repo":"https://github.com/acme/widget.git","branch":"feat/x","commit":"abc","agent":{"client":"claude-code",
"session":"t3-thread","display":"quiet-otter","host":"laptop","worktree":"C:\\src\\widget"},"size":"medium","gpu":false}`

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newWatcher(t *testing.T, dir string, f *fakeIncus, c *clock) *Watcher {
	t.Helper()
	h, err := OpenHistory(dir, c.now)
	if err != nil {
		t.Fatal(err)
	}
	return &Watcher{Incus: f, Hist: h, Now: c.now, MachineID: "pc", MachineName: "PC", Logf: t.Logf}
}

func readEvents(t *testing.T, dir string) []record {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "events-*.jsonl"))
	var out []record
	for _, f := range files {
		eachLine(f, func(b []byte) {
			var r record
			if err := json.Unmarshal(b, &r); err != nil {
				t.Fatalf("%s: %v", b, err)
			}
			out = append(out, r)
		})
	}
	return out
}

func types(rs []record) string {
	var s []string
	for _, r := range rs {
		s = append(s, r.Type)
	}
	return strings.Join(s, ",")
}

func TestWatcherFollowsAJobFromStartToEnd(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	job := "widget.20261124-180100-ab12"
	f := &fakeIncus{
		ins:  []Instance{inst("widget", "Running", "medium", t0.Add(-time.Hour))},
		cfg:  map[string]string{"user.agent-box.mode": "full", "limits.memory": "24GiB", "limits.cpu": "24", "limits.instances": "10", "limits.disk": "210GiB"},
		jobs: map[string][]fakeJob{"widget": {{name: job, state: "running", log: "ok 1\nok 2\n", meta: metaJSON}}},
	}
	w := newWatcher(t, dir, f, c)
	_, ch, cancel := w.Subscribe()
	defer cancel()
	c.add(2 * time.Minute)
	w.Poll(context.Background())

	st := w.State()
	if len(st.Jobs) != 1 || len(st.Boxes) != 1 {
		t.Fatalf("state: %+v", st)
	}
	j := st.Jobs[0]
	if j.State != dash.JobRunning || j.Kind != dash.KindGoTest || j.Agent.Session != "quiet-otter" || j.Tail != "ok 1\nok 2\n" ||
		j.Key != "widget/20261124-180100-ab12/2026-11-24T18:01:00Z" || j.Repo != "https://github.com/acme/widget.git" {
		t.Fatalf("job: %+v", j)
	}
	if st.Boxes[0].Repo != j.Repo || st.Machines[0].Mode != "full" || !st.Machines[0].Reachable {
		t.Fatalf("box or machine: %+v %+v", st.Boxes[0], st.Machines[0])
	}
	m := st.Machines[0]
	// 24 CPUs less the medium box's 4 Full CPUs: 2 large by CPU; memory 20GiB free: 2 large.
	if m.Allocated.CPU != 4 || m.Room.Large != 2 || m.Room.Medium != 5 || m.Room.GPU != 1 {
		t.Fatalf("room: %+v %+v", m.Allocated, m.Room)
	}
	got := map[string]int{}
	for len(ch) > 0 {
		got[(<-ch).Name]++
	}
	if got[dash.EventJob] != 1 || got[dash.EventBox] != 1 || got[dash.EventMachine] != 1 {
		t.Fatalf("events: %v", got)
	}

	// Nothing changes: no events, and the scan still runs.
	c.add(3 * time.Second)
	w.Poll(context.Background())
	if len(ch) != 0 {
		t.Fatalf("%d events on a quiet poll", len(ch))
	}

	f.set(func() {
		f.jobs["widget"][0].state, f.jobs["widget"][0].exit = "done", "1"
		f.jobs["widget"][0].endedAt = t0.Add(4 * time.Minute).Unix()
		f.jobs["widget"][0].log += "FAIL\n"
	})
	c.add(3 * time.Second)
	w.Poll(context.Background())
	st = w.State()
	if len(st.Jobs) != 1 || st.Jobs[0].Result() != dash.ResultFail || !strings.HasSuffix(st.Jobs[0].Tail, "FAIL\n") ||
		!st.Jobs[0].EndedAt.Equal(t0.Add(4*time.Minute)) {
		t.Fatalf("ended: %+v", st.Jobs)
	}
	rs := readEvents(t, dir)
	var ends int
	for _, r := range rs {
		if r.Type == recJobEnd {
			ends++
			if r.Seconds != 180 {
				t.Errorf("seconds %v", r.Seconds)
			}
		}
	}
	if ends != 1 {
		t.Fatalf("%d job-end records: %s", ends, types(rs))
	}

	// A restart doesn't record the job again, and the scan skips its details.
	w2 := newWatcher(t, dir, f, c)
	w2.Poll(context.Background())
	if n := len(readEvents(t, dir)); n != len(rs) {
		t.Fatalf("restart wrote %d more records: %s", n-len(rs), types(readEvents(t, dir)))
	}
	if st := w2.State(); len(st.Jobs) != 1 || st.Jobs[0].State != dash.JobDone {
		t.Fatalf("after restart: %+v", st.Jobs)
	}
}

func TestWatcherPausesAndLosesJobs(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0.Add(5 * time.Minute)}
	f := &fakeIncus{
		ins: []Instance{inst("a", "Running", "small", t0), inst("b", "Running", "small", t0)},
		cfg: map[string]string{"user.agent-box.mode": "shared"},
		jobs: map[string][]fakeJob{
			"a": {{name: "a.20261124-180100-0001", state: "running", log: "x", meta: metaJSON}},
			"b": {{name: "b.20261124-180100-0002", state: "running", log: "y", meta: metaJSON}},
		},
	}
	w := newWatcher(t, dir, f, c)
	w.Poll(context.Background())

	f.set(func() {
		f.ins[0].Status = "Frozen"
		f.cfg["user.agent-box.mode"] = "mypc"
	})
	w.Poll(context.Background())
	byBox := func() map[string]dash.Job {
		out := map[string]dash.Job{}
		for _, j := range w.State().Jobs {
			out[j.Box] = j
		}
		return out
	}
	if j := byBox()["a"]; j.State != dash.JobPaused {
		t.Fatalf("frozen box's job: %+v", j)
	}
	if m := w.State().Machines[0]; m.Mode != "mypc" {
		t.Fatalf("machine: %+v", m)
	}

	// The tray deletes b mid-run, and a thaws.
	f.set(func() {
		f.ins = f.ins[:1]
		f.ins[0].Status = "Running"
	})
	c.add(time.Minute)
	w.Poll(context.Background())
	jobs := byBox()
	if jobs["a"].State != dash.JobRunning || jobs["b"].State != dash.JobLost || jobs["b"].Result() != dash.ResultLost {
		t.Fatalf("jobs: %+v", jobs)
	}
	if len(w.State().Boxes) != 1 {
		t.Fatal("b still listed")
	}
	rs := readEvents(t, dir)
	if !strings.Contains(types(rs), recBoxDelete) {
		t.Fatalf("no box-delete: %s", types(rs))
	}

	// boxd restarts while a's box is replaced by a new one of the same name:
	// the open job is reconciled as lost.
	f.set(func() { f.ins[0].CreatedAt = c.now(); f.jobs["a"] = nil })
	c.add(time.Minute)
	w2 := newWatcher(t, dir, f, c)
	w2.Poll(context.Background())
	for _, j := range w2.State().Jobs {
		if j.State != dash.JobLost {
			t.Fatalf("not reconciled: %+v", j)
		}
	}
	var segs []string
	for _, s := range w2.Hist.Range(dash.RangeDay, w2.Live()).Segments {
		segs = append(segs, s.Mode)
	}
	if strings.Join(segs, ",") != "shared,mypc" {
		t.Fatalf("segments %v", segs)
	}
}

func TestWatcherMarksIncusDown(t *testing.T) {
	c := &clock{t: t0}
	f := &fakeIncus{err: fmt.Errorf("socket gone")}
	w := newWatcher(t, t.TempDir(), f, c)
	w.Poll(context.Background())
	if w.State().Machines[0].Reachable {
		t.Fatal("reachable with Incus down")
	}
}

func TestAPI(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0.Add(5 * time.Minute)}
	job := "w.20261124-180100-ab12"
	f := &fakeIncus{
		ins:  []Instance{inst("w", "Running", "medium", t0)},
		cfg:  map[string]string{"user.agent-box.mode": "full", "limits.memory": "24GiB"},
		jobs: map[string][]fakeJob{"w": {{name: job, state: "running", log: strings.Repeat("é", 40000), meta: metaJSON}}},
	}
	w := newWatcher(t, dir, f, c)
	w.Poll(context.Background())
	srv := httptest.NewServer((&Server{W: w}).Handler())
	defer srv.Close()
	get := func(path string, out any) int {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if out != nil {
			json.NewDecoder(resp.Body).Decode(out)
		}
		return resp.StatusCode
	}
	var st dash.State
	if get("/api/state", &st) != 200 || len(st.Jobs) != 1 {
		t.Fatalf("state %+v", st)
	}
	key := url.PathEscape(st.Jobs[0].Key)
	var chunk dash.LogChunk
	if code := get("/api/jobs/"+key+"/log?offset=0", &chunk); code != 200 || chunk.EOF || len(chunk.Data) > dash.MaxLogChunk ||
		!strings.HasPrefix(chunk.Data, "é") || chunk.Offset != int64(len(chunk.Data)) || chunk.Offset%2 != 0 {
		t.Fatalf("log %d: offset %d eof %v", code, chunk.Offset, chunk.EOF)
	}
	off := chunk.Offset
	if get(fmt.Sprintf("/api/jobs/%s/log?offset=%d", key, off), &chunk); !chunk.EOF || chunk.Offset != 80000 {
		t.Fatalf("second chunk: offset %d eof %v", chunk.Offset, chunk.EOF)
	}
	var eb dash.ErrorBody
	if get("/api/jobs/nope/log", &eb) != 404 || eb.Code != dash.CodeNotFound {
		t.Fatalf("missing job: %+v", eb)
	}
	f.set(func() { f.ins[0].Status = "Frozen" })
	w.Poll(context.Background())
	if get("/api/jobs/"+key+"/log?offset=5", &chunk); !chunk.Paused || chunk.Offset != 5 {
		t.Fatalf("frozen: %+v", chunk)
	}

	post := func(body string, hdr bool) int {
		req, _ := http.NewRequest("POST", srv.URL+"/api/refusals", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if hdr {
			req.Header.Set(dash.ControlHeader, "1")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if post(`{"why":"full","reason":"race suite","size":"large"}`, false) != 403 {
		t.Fatal("refusal without the control header")
	}
	if post(`{"why":"bored"}`, true) != 400 {
		t.Fatal("unknown why accepted")
	}
	if post(`{"why":"mypc","reason":"race suite TOKEN=abcdefgh12345","size":"large","agent":"quiet-otter"}`, true) != 204 {
		t.Fatal("refusal refused")
	}
	var h dash.History
	if get("/api/history?range=day", &h) != 200 || len(h.Buckets) != 24 || h.Buckets[23].Refusals.ByWhy["mypc"] != 1 {
		t.Fatalf("history: %+v", h.Buckets[23])
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "events-2026-11-24.jsonl")); strings.Contains(string(b), "abcdefgh12345") {
		t.Fatal("secret written to the history")
	}
	if get("/api/history?range=year", nil) != 400 || get("/api/jobs?limit=500", nil) != 400 ||
		get("/api/jobs?since=yesterday", nil) != 400 {
		t.Fatal("bad queries accepted")
	}
	var page dash.JobsPage
	if get("/api/jobs", &page) != 200 || page.Jobs == nil {
		t.Fatalf("jobs page: %+v", page)
	}
	var one dash.Job
	if get("/api/jobs/"+key, &one) != 200 || one.Key != st.Jobs[0].Key {
		t.Fatalf("one job: %+v", one)
	}
}

func TestEventsStreamStartsWithState(t *testing.T) {
	c := &clock{t: t0}
	f := &fakeIncus{cfg: map[string]string{"user.agent-box.mode": "full"}}
	w := newWatcher(t, t.TempDir(), f, c)
	w.Poll(context.Background())
	srv := httptest.NewServer((&Server{W: w}).Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if !strings.HasPrefix(string(buf[:n]), "event: state\ndata: {") || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%q", buf[:n])
	}
	f.set(func() { f.ins = []Instance{inst("new", "Running", "small", t0)} })
	w.Poll(context.Background())
	n, _ = resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "event: box\n") {
		t.Fatalf("%q", buf[:n])
	}
}

// A throwaway goes within a second of its exit being read; box's report of
// the end keeps its job from being recorded as lost.
func TestJobEndedBeatsTheBoxGoing(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0.Add(time.Minute)}
	f := &fakeIncus{
		// Launched 7s after the job's ID was made, as box run --new does.
		ins:  []Instance{inst("tmp-1a2b", "Running", "small", t0.Add(67*time.Second))},
		cfg:  map[string]string{"user.agent-box.mode": "full"},
		jobs: map[string][]fakeJob{"tmp-1a2b": {{name: "tmp-1a2b.20261124-180100-ab12", state: "running", log: "x\n", meta: metaJSON}}},
	}
	w := newWatcher(t, dir, f, c)
	c.add(time.Minute)
	w.Poll(context.Background())
	if st := w.State(); len(st.Jobs) != 1 || st.Jobs[0].State != dash.JobRunning {
		t.Fatalf("the throwaway's job isn't seen: %+v", st.Jobs)
	}
	srv := httptest.NewServer((&Server{W: w}).Handler())
	defer srv.Close()
	post := func(body string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/api/jobs/ended", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(dash.ControlHeader, "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	body := `{"box":"tmp-1a2b","id":"20261124-180100-ab12","exit":0,"endedAt":"2026-11-24T18:02:30Z","tail":"x\nPASS\n"}`
	if code := post(body); code != 204 {
		t.Fatalf("ended: %d", code)
	}
	if code := post(body); code != 404 {
		t.Fatalf("a second report: %d", code)
	}
	f.set(func() { f.ins = nil })
	w.Poll(context.Background())
	st := w.State()
	if len(st.Jobs) != 1 || st.Jobs[0].Result() != dash.ResultPass || st.Jobs[0].Tail != "x\nPASS\n" {
		t.Fatalf("after the box went: %+v", st.Jobs)
	}
}

// An instance whose CreatedAt isn't set yet (still being created) is not
// recorded as a box-create with a zero timestamp, and a restarted watcher
// recovers each running box's repo from history instead of showing (no repo).
func TestWatcherSkipsUnsetCreatedAtAndRecoversBoxRepoAfterRestart(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	creating := inst("demo", "Stopped", "small", time.Time{})
	f := &fakeIncus{
		ins: []Instance{creating},
		cfg: map[string]string{"user.agent-box.mode": "full"},
	}
	w := newWatcher(t, dir, f, c)
	w.Poll(context.Background())
	if got := w.Hist.Range(dash.RangeDay, w.Live()).Buckets[0].BoxHours.BySize["small"]; got != 0 {
		t.Fatalf("unset CreatedAt added phantom box-hours: %v", got)
	}

	// Creation finishes and a job runs and finishes on the box.
	created := t0.Add(3 * time.Second)
	c.add(3 * time.Second)
	f.set(func() {
		f.ins = []Instance{inst("demo", "Running", "small", created)}
		f.jobs = map[string][]fakeJob{
			"demo": {{name: "demo.20261124-180005-ab12", state: "done", exit: "0", log: "ok\n", meta: metaJSON, endedAt: created.Add(10 * time.Second).Unix()}},
		}
	})
	c.add(15 * time.Second)
	w.Poll(context.Background())
	if st := w.State(); len(st.Boxes) != 1 || st.Boxes[0].Repo != "https://github.com/acme/widget.git" {
		t.Fatalf("before restart: %+v", st.Boxes)
	}

	// Simulate a boxd restart: fresh Watcher on the same history directory.
	w2 := newWatcher(t, dir, f, c)
	w2.Poll(context.Background())
	if st := w2.State(); len(st.Boxes) != 1 || st.Boxes[0].Repo != "https://github.com/acme/widget.git" {
		t.Fatalf("after restart box lost repo: %+v", st.Boxes)
	}
	// The box is small even though metaJSON says "medium" (as an older box
	// binary writes when reusing a small repo box without --size): boxd keeps
	// the box's real size and GPU flag.
	st := w2.State()
	if len(st.Jobs) != 1 {
		t.Fatalf("jobs = %+v, want 1", st.Jobs)
	}
	if st.Jobs[0].Size != "small" || st.Jobs[0].GPU || st.Jobs[0].Kind != dash.KindGoTest {
		t.Fatalf("job size/gpu/kind = %q/%v/%q, want small/false/go-test", st.Jobs[0].Size, st.Jobs[0].GPU, st.Jobs[0].Kind)
	}

	// Stale gpu:true in .meta cannot override a CPU box's real size/GPU/kind.
	gpuMeta := strings.Replace(metaJSON, `"gpu":false`, `"gpu":true`, 1)
	f.set(func() {
		f.jobs["demo"] = append(f.jobs["demo"], fakeJob{name: "demo.20261124-180025-cd34", state: "running", log: "ok\n", meta: gpuMeta})
	})
	c.add(15 * time.Second)
	w2.Poll(context.Background())
	for _, j := range w2.State().Jobs {
		if j.ID == "20261124-180025-cd34" && (j.Size != "small" || j.GPU || j.Kind != dash.KindGoTest) {
			t.Fatalf("stale gpu meta overrode CPU box: %+v", j)
		}
	}
}
