package boxd

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/jobrun"
	"github.com/a-Fig/agent-box/internal/mode"
)

// PollEvery is how often the watcher lists Incus (docs/ui.md, "Watching
// Incus"); SampleEvery how often it writes samples.
const (
	PollEvery   = 3 * time.Second
	SampleEvery = time.Minute
	execTimeout = 3 * time.Second
)

// Watcher polls Incus, keeps the dashboard's state, turns changes into SSE
// events and records them in the history.
type Watcher struct {
	Incus       Incus
	Hist        *History
	Now         func() time.Time
	MachineID   string
	MachineName string
	WSLMemory   func() int64  // bytes the distro's VM holds; nil reads 0
	Version     *dash.Version // this boxd's build, shown as Machine.Boxd; nil if unknown
	Logf        func(format string, args ...any)
	// Queue is the waiting jobs (docs/queue.md); nil runs without one.
	Queue *Queue
	// Whois names the device at a tailnet address, for the history; nil uses
	// the login.
	Whois func(ctx context.Context, ip string) string

	mu        sync.Mutex
	polled    bool
	machine   dash.Machine
	boxes     map[string]dash.Box
	known     map[string]boxRec // box id -> what was recorded as created
	live      map[string]*dash.Job
	repoOf    map[string]string    // box -> the repo of its latest job
	died      map[string]time.Time // job key -> when a scan first saw it died
	qview     map[string]dash.Job  // queued job -> as last announced
	starting  string               // the queued job being started, one at a time
	startDone time.Time            // when the last start finished
	noRoom    map[string]time.Time // queued job -> not before, after Incus refused its launch
	started   map[string]dash.Job  // queued jobs started, until a scan sees them
	lastIns   []Instance
	lastProj  Project
	listed    bool // lastIns is from a poll that listed the boxes
	cpu       map[string]cpuMark
	lastMode  *modeRec
	lastReq   string
	sampled   time.Time
	subs      map[chan dash.Event]struct{}
}

type cpuMark struct {
	at    time.Time
	usage int64
}

func (w *Watcher) init() {
	if w.boxes != nil {
		return
	}
	w.boxes, w.known, w.live = map[string]dash.Box{}, map[string]boxRec{}, map[string]*dash.Job{}
	w.repoOf, w.cpu, w.subs = map[string]string{}, map[string]cpuMark{}, map[chan dash.Event]struct{}{}
	w.died, w.qview = map[string]time.Time{}, map[string]dash.Job{}
	w.noRoom, w.started = map[string]time.Time{}, map[string]dash.Job{}
	w.machine = dash.Machine{ID: w.MachineID, Name: w.MachineName, Boxd: w.Version}
	jobs, boxes := w.Hist.Open()
	for _, j := range jobs {
		w.live[j.Key] = &j
	}
	for _, b := range boxes {
		w.known[b.id()] = b
	}
	w.lastMode = w.Hist.LastMode()
}

func (w *Watcher) logf(format string, args ...any) {
	if w.Logf != nil {
		w.Logf(format, args...)
	}
}

// Run polls until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(PollEvery)
	defer t.Stop()
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	if err := w.Hist.Prune(); err != nil {
		w.logf("boxd: %v", err)
	}
	for {
		w.Poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-prune.C:
			if err := w.Hist.Prune(); err != nil {
				w.logf("boxd: %v", err)
			}
		}
	}
}

// Poll reads Incus once and applies what changed.
func (w *Watcher) Poll(ctx context.Context) {
	w.mu.Lock()
	w.init()
	w.mu.Unlock()
	now := w.Now().UTC()
	ins, err := w.Incus.Instances(ctx)
	var proj Project
	if err == nil {
		proj, err = w.Incus.Project(ctx)
	}
	if err != nil {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.logf("boxd: incus: %v", err)
		if w.machine.Reachable || !w.polled {
			w.polled = true
			w.machine.Reachable = false
			w.emit(dash.Event{Name: dash.EventMachine, Data: w.machine})
		}
		return
	}
	scans := w.scanAll(ctx, ins)

	w.mu.Lock()
	defer w.mu.Unlock()
	w.polled = true
	boxes := map[string]dash.Box{}
	present := map[string]Instance{}
	for _, in := range ins {
		present[in.Name] = in
	}
	// Jobs first, so a box's repo is known before the box is built.
	w.applyJobs(now, ins, scans, "")
	for _, in := range ins {
		boxes[in.Name] = w.box(now, in)
	}
	w.applyBoxes(now, ins, boxes)
	w.applyMachine(now, ins, proj, boxes)
	if now.Sub(w.sampled) >= SampleEvery {
		w.sampled = now
		w.sample(now, ins, boxes)
	}
	w.lastIns, w.lastProj, w.listed = ins, proj, true
	w.schedule(now, ins)
}

// scanAll scans the running boxes' job files, in parallel. A box missing from
// the result couldn't be scanned.
func (w *Watcher) scanAll(ctx context.Context, ins []Instance) map[string][]scanned {
	var mu sync.Mutex
	var wg sync.WaitGroup
	out := map[string][]scanned{}
	for _, in := range ins {
		if in.Status != "Running" {
			continue
		}
		skip := w.Hist.EndedOn(in.Name, in.CreatedAt)
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, execTimeout)
			defer cancel()
			b, err := w.Incus.Exec(c, name, scanScript, slices.Collect(mapKeys(skip))...)
			if err != nil {
				w.logf("boxd: scan %s: %v", name, err)
				return
			}
			mu.Lock()
			out[name] = parseScan(b, skip)
			mu.Unlock()
		}(in.Name)
	}
	wg.Wait()
	return out
}

// holds says whether a box created at created can be the box a job started at
// started ran on: names are reused, and box run --new makes the job's ID
// before it launches the box (dash.JobBoxGrace). Whether the job's files are
// on the box decides the rest.
func holds(created, started time.Time) bool {
	return !created.After(started.Add(dash.JobBoxGrace))
}

// EndJob applies box's report of how a job ended (POST /api/jobs/ended). It
// takes it only for a job boxd knows is running or paused, and says whether
// it did.
//
// box posts the report before it deletes a throwaway, so when the job isn't
// live yet (it ran between two polls, or a poll's scan is still in flight)
// boxd scans that box now and then applies the report.
func (w *Watcher) EndJob(ctx context.Context, b dash.JobEndedBody) bool {
	if w.endJob(b) {
		return true
	}
	ins, err := w.Incus.Instances(ctx)
	if err != nil {
		return false
	}
	for _, in := range ins {
		if in.Name != b.Box || in.Status != "Running" {
			continue
		}
		scans := w.scanAll(ctx, []Instance{in})
		w.mu.Lock()
		w.applyJobs(w.Now().UTC(), []Instance{in}, scans, in.Name)
		w.mu.Unlock()
	}
	return w.endJob(b)
}

func (w *Watcher) endJob(b dash.JobEndedBody) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.init()
	for key, j := range w.live {
		if j.Box != b.Box || j.ID != b.ID {
			continue
		}
		before := *j
		at, now := b.EndedAt.UTC(), w.Now().UTC()
		if at.IsZero() || at.Before(j.StartedAt) || at.After(now) {
			at = now // the laptop's clock may be off
		}
		exit := b.Exit
		j.State, j.Exit, j.EndedAt = dash.JobDone, &exit, &at
		if t := tailOf([]byte(b.Tail)); t != "" {
			j.Tail = t
		}
		delete(w.died, key)
		w.settle(key, j, before)
		return true
	}
	return false
}

func mapKeys(m map[string]bool) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// diedAfter is how long a job may look died (its wrapper gone, no .exit)
// before it counts as lost: box cancel kills the wrapper first and writes
// .exit up to 5s later.
const diedAfter = 10 * time.Second

// applyJobs updates the jobs from the scans and the boxes' states. With only
// set, ins and scans hold that box alone, and other boxes' jobs are left be.
func (w *Watcher) applyJobs(now time.Time, ins []Instance, scans map[string][]scanned, only string) {
	byName := map[string]Instance{}
	for _, in := range ins {
		byName[in.Name] = in
	}
	seen := map[string]bool{}
	for box, jobs := range scans {
		in := byName[box]
		for _, s := range jobs {
			name, id, started := jobStart(s.Job)
			if s.Meta != nil && s.Meta.QueuedAt != "" {
				// A job from the queue is named when it is queued and starts
				// later: its start is the .meta's.
				if t, err := time.Parse(time.RFC3339, s.Meta.StartedAt); err == nil {
					started = t.UTC()
				}
			}
			if name != box || started.IsZero() || !holds(in.CreatedAt, started) {
				continue
			}
			key := dash.JobKey(box, id, started)
			seen[key] = true
			if s.Skipped {
				continue
			}
			j, ok := w.live[key]
			if !ok {
				if old, done := w.Hist.Job(key); done && old.State != dash.JobRunning && old.State != dash.JobPaused {
					continue // recorded as ended already
				}
				j = w.newJob(key, id, box, started, in, s.Meta)
				w.live[key] = j
				delete(w.started, s.Job)
				w.record(record{Type: recJobStart, At: started, Job: j})
			} else if s.Meta != nil && j.Command == "" && j.Reason == "" {
				fillMeta(j, s.Meta)
			}
			if j.Repo != "" {
				w.repoOf[box] = j.Repo
			}
			before := *j
			if s.Tail != "" {
				j.Tail = s.Tail
			}
			switch s.State {
			case "done":
				j.State, j.Exit, j.EndedAt = dash.JobDone, s.Exit, s.EndedAt
				if j.EndedAt == nil {
					j.EndedAt = &now
				}
			case "died":
				if first, ok := w.died[key]; !ok {
					w.died[key] = now
				} else if now.Sub(first) >= diedAfter {
					j.State, j.EndedAt = dash.JobLost, &now
				}
			default:
				j.State = dash.JobRunning
			}
			if s.State != "died" || j.State == dash.JobLost {
				delete(w.died, key)
			}
			w.settle(key, j, before)
		}
	}
	for key, j := range w.live {
		if only != "" && j.Box != only {
			continue
		}
		in, ok := byName[j.Box]
		before := *j
		switch {
		case !ok || !holds(in.CreatedAt, j.StartedAt):
			j.State, j.EndedAt = dash.JobLost, &now // the box went (or is a new one by the same name)
		case in.Status == "Frozen":
			j.State = dash.JobPaused
		case in.Status == "Stopped" || in.Status == "Error":
			j.State, j.EndedAt = dash.JobLost, &now // stopping a box kills its jobs
		case in.Status == "Running":
			if _, scanned := scans[j.Box]; scanned && !seen[key] {
				j.State, j.EndedAt = dash.JobLost, &now // its files are gone
			} else if j.State == dash.JobPaused && scanned {
				j.State = dash.JobRunning
			}
		}
		w.settle(key, j, before)
	}
}

// settle records and announces a job that changed, and drops it from the
// live set once it ended.
func (w *Watcher) settle(key string, j *dash.Job, before dash.Job) {
	ended := j.State == dash.JobDone || j.State == dash.JobLost
	if ended {
		r := record{Type: recJobEnd, Job: j}
		if j.EndedAt != nil {
			r.At = *j.EndedAt
			r.Seconds = j.EndedAt.Sub(j.StartedAt).Seconds()
		}
		w.record(r)
		delete(w.live, key)
		w.reapEnded(j)
	}
	if ended || !reflect.DeepEqual(before, *j) {
		w.emit(dash.Event{Name: dash.EventJob, Data: *j})
	}
}

func (w *Watcher) newJob(key, id, box string, started time.Time, in Instance, m *meta) *dash.Job {
	sz := mode.SizeOf(in.Config)
	j := &dash.Job{Key: key, ID: id, Box: box, Machine: w.MachineID, State: dash.JobRunning, StartedAt: started,
		Size: sz.Name, GPU: sz.GPU}
	if m != nil {
		fillMeta(j, m)
		if t, err := time.Parse(time.RFC3339, m.QueuedAt); err == nil {
			t = t.UTC()
			j.QueuedAt = &t
		}
	}
	j.Kind = dash.Classify(j.Command, j.GPU)
	return j
}

func fillMeta(j *dash.Job, m *meta) {
	j.Reason, j.Command, j.Repo, j.Branch, j.Commit = m.Reason, m.Command, m.Repo, m.Branch, m.Commit
	session := m.Agent.Display
	if session == "" {
		session = lastElem(m.Agent.Worktree)
	}
	j.Agent = dash.Agent{Client: m.Agent.Client, Session: session, Host: m.Agent.Host, Worktree: m.Agent.Worktree}
	if j.Size == "" {
		if m.Size != "" {
			j.Size = m.Size
		}
		j.GPU = j.GPU || m.GPU
	}
	j.Kind = dash.Classify(j.Command, j.GPU)
}

func lastElem(p string) string {
	p = strings.TrimRight(p, `/\`)
	return p[strings.LastIndexAny(p, `/\`)+1:]
}

func (w *Watcher) box(now time.Time, in Instance) dash.Box {
	sz := mode.SizeOf(in.Config)
	cfg := in.ExpandedConfig
	if cfg == nil {
		cfg = in.Config
	}
	repo := w.repoOf[in.Name]
	if repo == "" && w.Hist != nil && !unsetTime(in.CreatedAt) {
		if r := w.Hist.BoxRepo(in.Name, in.CreatedAt.UTC()); r != "" {
			w.repoOf[in.Name] = r
			repo = r
		}
	}
	b := dash.Box{Name: in.Name, Machine: w.MachineID, Size: sz.Name, GPU: sz.GPU, State: strings.ToLower(in.Status),
		Throwaway: in.Config[jobrun.ThrowawayKey] == "true", Repo: repo, CreatedAt: in.CreatedAt.UTC()}
	if t, err := time.Parse(time.RFC3339, in.Config[jobrun.ExpiresKey]); err == nil {
		t = t.UTC()
		b.ExpiresAt = &t
	}
	b.MemLimit, _ = mode.ParseBytes(cfg["limits.memory"])
	b.CPUs, _ = strconv.Atoi(cfg["limits.cpu"])
	if in.State != nil {
		b.MemUsed = in.State.Memory.Usage
		if prev, ok := w.cpu[in.Name]; ok && b.CPUs > 0 && in.Status == "Running" {
			if dt := now.Sub(prev.at); dt > 0 {
				pct := float64(in.State.CPU.Usage-prev.usage) / float64(dt) / float64(b.CPUs) * 100
				b.CPUPct = min(max(round2(pct), 0), 100)
			}
		}
		w.cpu[in.Name] = cpuMark{now, in.State.CPU.Usage}
	}
	return b
}

// applyBoxes records boxes created and deleted, and announces changes.
func (w *Watcher) applyBoxes(now time.Time, ins []Instance, boxes map[string]dash.Box) {
	ids := map[string]bool{}
	for _, in := range ins {
		if unsetTime(in.CreatedAt) {
			continue
		}
		sz := mode.SizeOf(in.Config)
		rec := boxRec{Name: in.Name, Size: sz.Name, GPU: sz.GPU, CreatedAt: in.CreatedAt.UTC()}
		ids[rec.id()] = true
		if _, ok := w.known[rec.id()]; !ok {
			w.known[rec.id()] = rec
			w.record(record{Type: recBoxCreate, At: rec.CreatedAt, Box: &rec})
		}
	}
	for id, rec := range w.known {
		if !ids[id] {
			delete(w.known, id)
			w.record(record{Type: recBoxDelete, At: now, Box: &rec})
		}
	}
	for name, b := range boxes {
		if old, ok := w.boxes[name]; !ok || !reflect.DeepEqual(old, b) {
			w.emit(dash.Event{Name: dash.EventBox, Data: b})
		}
	}
	for name := range w.boxes {
		if _, ok := boxes[name]; !ok {
			w.emit(dash.Event{Name: dash.EventBoxGone, Data: dash.BoxGone{Name: name, Machine: w.MachineID}})
			delete(w.cpu, name)
			delete(w.repoOf, name)
		}
	}
	w.boxes = boxes
}

func (w *Watcher) applyMachine(now time.Time, ins []Instance, proj Project, boxes map[string]dash.Box) {
	cfg := proj.Config
	m := dash.Machine{ID: w.MachineID, Name: w.MachineName, Reachable: true, Mode: cfg[mode.ProjectModeKey],
		Boxd: w.Version}
	if v := cfg[mode.ProjectTrayVersionKey]; v != "" {
		var tv dash.Version
		if json.Unmarshal([]byte(v), &tv) == nil && tv.Commit != "" {
			if tv.Time != nil {
				t := tv.Time.UTC()
				tv.Time = &t
			}
			m.Tray = &tv
		}
	}
	if v := cfg[mode.ProjectModeRequestKey]; v != "" {
		var r dash.ModeRequest
		if json.Unmarshal([]byte(v), &r) == nil && r.Mode != "" {
			m.ModeRequested = &r
		}
	}
	if t, err := time.Parse(time.RFC3339, cfg[mode.ProjectTraySeenKey]); err == nil {
		t = t.UTC()
		m.TrayLastSeen = &t
	}
	// Which enforcer wrote the heartbeat. Values other than boxtray's are unknown.
	switch e := cfg[mode.ProjectEnforcerKey]; e {
	case dash.EnforcerTray, dash.EnforcerHeadless:
		m.Enforcer = e
	}
	m.Caps, m.Allocated, m.Room = allot(cfg, proj, ins)
	if w.WSLMemory != nil {
		m.WSLMemory = w.WSLMemory()
	}

	if cur := (modeRec{m.Mode}); m.Mode != "" && (w.lastMode == nil || *w.lastMode != cur) {
		w.lastMode = &cur
		w.record(record{Type: recMode, At: now, Mode: &cur})
	}
	if v := cfg[mode.ProjectModeRequestKey]; v != w.lastReq {
		w.lastReq = v
		if m.ModeRequested != nil {
			w.record(record{Type: recModeRequest, At: now, Request: m.ModeRequested})
		}
	}
	if !reflect.DeepEqual(w.machine, m) {
		w.machine = m
		w.emit(dash.Event{Name: dash.EventMachine, Data: m})
	}
}

// allot works out the project's caps, what boxes hold of them and how many
// boxes of each size fit, as box status does: a box holds at least its size's
// Full CPUs (the tray raises them in Full), and disk is Incus's own count,
// which includes the image cache. A cap that is unset doesn't limit.
func allot(cfg map[string]string, proj Project, ins []Instance) (caps, held dash.Resources, room dash.Room) {
	caps.CPU, _ = strconv.Atoi(cfg["limits.cpu"])
	caps.Memory, _ = mode.ParseBytes(cfg["limits.memory"])
	caps.Instances, _ = strconv.Atoi(cfg["limits.instances"])
	caps.Disk, _ = mode.ParseBytes(cfg["limits.disk"])
	gpuTaken := false
	for _, in := range ins {
		sz := mode.SizeOf(in.Config)
		ecfg := in.ExpandedConfig
		if ecfg == nil {
			ecfg = in.Config
		}
		cpu, _ := strconv.Atoi(ecfg["limits.cpu"])
		mem, ok := mode.ParseBytes(ecfg["limits.memory"])
		if !ok {
			mem, _ = mode.ParseBytes(sz.Memory)
		}
		held.CPU += max(sz.FullCPUs, cpu)
		held.Memory += mem
		held.Instances++
		gpuTaken = gpuTaken || (sz.GPU && in.Status != "Stopped")
	}
	if d, ok := proj.Resources["disk"]; ok && d.Usage >= 0 {
		held.Disk = d.Usage
	}
	fits := func(name string) int {
		sz, _ := mode.LookupSize(name)
		mem, _ := mode.ParseBytes(sz.Memory)
		disk, _ := mode.ParseBytes(sz.Disk)
		n := 1 << 30
		limit := func(capv, used, each int64) {
			if capv > 0 && each > 0 {
				n = min(n, int(max(capv-used, 0)/each))
			}
		}
		limit(int64(caps.Instances), int64(held.Instances), 1)
		limit(int64(caps.CPU), int64(held.CPU), int64(sz.FullCPUs))
		limit(caps.Memory, held.Memory, mem)
		limit(caps.Disk, held.Disk, disk)
		return n
	}
	room = dash.Room{Small: fits("small"), Medium: fits("medium"), Large: fits("large")}
	if !gpuTaken {
		room.GPU = min(fits(mode.GPUSize), 1)
	}
	return caps, held, room
}

func (w *Watcher) sample(now time.Time, ins []Instance, boxes map[string]dash.Box) {
	ss := []sample{{At: now, Kind: "machine", MemCap: w.machine.Caps.Memory, MemAlloc: w.machine.Allocated.Memory,
		WSL: w.machine.WSLMemory}}
	for _, in := range ins {
		b := boxes[in.Name]
		s := sample{At: now, Kind: "box", Box: b.Name, Size: b.Size, MemUsed: b.MemUsed, MemLimit: b.MemLimit}
		if in.State != nil {
			s.CPUTime = in.State.CPU.Usage
		}
		ss[0].MemUsed += b.MemUsed
		ss = append(ss, s)
	}
	if err := w.Hist.Samples(ss); err != nil {
		w.logf("boxd: history: %v", err)
	}
}

func (w *Watcher) record(r record) {
	if err := w.Hist.Record(r); err != nil {
		w.logf("boxd: history: %v", err)
	}
}

// State is GET /api/state.
func (w *Watcher) State() dash.State {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state()
}

func (w *Watcher) state() dash.State {
	w.init()
	s := dash.State{Machines: []dash.Machine{w.machine}, Boxes: []dash.Box{}, Jobs: []dash.Job{}, Now: w.Now().UTC()}
	for _, b := range w.boxes {
		s.Boxes = append(s.Boxes, b)
	}
	slices.SortFunc(s.Boxes, func(a, b dash.Box) int { return strings.Compare(a.Name, b.Name) })
	for _, j := range w.live {
		s.Jobs = append(s.Jobs, *j)
	}
	slices.SortFunc(s.Jobs, func(a, b dash.Job) int { return b.StartedAt.Compare(a.StartedAt) })
	s.Jobs = append(s.Jobs, w.queuedJobs()...)
	s.Jobs = append(s.Jobs, w.Hist.Finished(Filter{Limit: dash.FinishedInState}).Jobs...)
	return s
}

// Box returns a box as last polled.
func (w *Watcher) Box(name string) (dash.Box, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	b, ok := w.boxes[name]
	return b, ok
}

// Job returns a live job, or a recorded one.
func (w *Watcher) Job(key string) (dash.Job, bool) {
	w.mu.Lock()
	j, ok := w.live[key]
	w.mu.Unlock()
	if ok {
		return *j, true
	}
	return w.Hist.Job(key)
}

// Live is what History.Range needs from now.
func (w *Watcher) Live() Live {
	w.mu.Lock()
	defer w.mu.Unlock()
	return Live{Mode: modeRec{w.machine.Mode}, MemCap: w.machine.Caps.Memory}
}

// Subscribe returns the state now and a channel of the events after it. The
// channel closes when the subscriber falls too far behind; cancel ends it.
func (w *Watcher) Subscribe() (dash.State, <-chan dash.Event, func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.state()
	ch := make(chan dash.Event, 256)
	w.subs[ch] = struct{}{}
	return st, ch, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if _, ok := w.subs[ch]; ok {
			delete(w.subs, ch)
			close(ch)
		}
	}
}

// emit sends e to every subscriber; one whose buffer is full is dropped, and
// its page starts over from state when it reconnects.
func (w *Watcher) emit(e dash.Event) {
	for ch := range w.subs {
		select {
		case ch <- e:
		default:
			delete(w.subs, ch)
			close(ch)
		}
	}
}

// Machine is the PC as last polled.
func (w *Watcher) Machine() dash.Machine {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.machine
}

// JobByID finds the newest job of a box name and ID, live or recorded.
func (w *Watcher) JobByID(box, id string) (dash.Job, bool) {
	w.mu.Lock()
	for _, j := range w.live {
		if j.Box == box && j.ID == id {
			w.mu.Unlock()
			return *j, true
		}
	}
	w.mu.Unlock()
	return w.Hist.JobByID(box, id)
}
