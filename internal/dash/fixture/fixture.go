// Package fixture builds the dashboard's scenarios from the dash types: what
// boxd would answer for an empty PC, a busy one, one in My PC, and so on. The golden test in internal/dash writes them to internal/dash/testdata
// at a fixed time; the devserver builds them at the current time.
//
// Everything here is made up: the repos, agents, hosts and paths are fiction,
// since the repo is public. A scenario is the same for the same now, so the
// golden files only change when the code does.
package fixture

import (
	"fmt"
	"hash/fnv"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/a-Fig/agent-box/internal/boxd"
	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/mode"
)

// Scenario is everything the API answers in one situation.
type Scenario struct {
	Name  string        `json:"name"`
	About string        `json:"about"`
	State dash.State    `json:"state"`
	Jobs  dash.JobsPage `json:"jobs"` // GET /api/jobs with no filter: every finished job
	Day   dash.History  `json:"day"`
	Week  dash.History  `json:"week"`
	Month dash.History  `json:"month"`
	// Logs holds the log so far of each job whose box still exists, by key.
	Logs map[string]string `json:"logs"`
}

// The scenarios, in docs/ui.md's order.
var scenarios = []struct {
	name, about string
	build       func(*world)
}{
	{"empty", "No boxes and no jobs yet: the PC in Shared, the tray alive.", buildEmpty},
	{"busy", "Full mode, three boxes, two jobs running, a day of finished jobs.", buildBusy},
	{"mypc", "My PC is chosen, so every box is frozen, a job paused and two runs queued.", buildMyPC},
	{"full", "The caps are full (a GPU box and a medium box), and runs were turned away today.", buildFull},
	{"tray-down", "The tray hasn't been seen for 14 minutes, so a mode request from a phone isn't being picked up.", buildTrayDown},
	{"headless", "Nobody is logged on to the PC: the headless enforcer holds Full, and a request for Shared from a phone waits for the tray.", buildHeadless},
	{"lost", "boxd restarted and found a job's box gone: the job is lost.", buildLost},
	{"week", "A week of history: jobs, box-hours, modes and refusals.", buildWeek},
}

// Names lists the scenarios.
func Names() []string {
	var out []string
	for _, s := range scenarios {
		out = append(out, s.name)
	}
	return out
}

// Build returns the scenario called name as it would be at now.
func Build(name string, now time.Time) (Scenario, error) {
	for _, s := range scenarios {
		if s.name == name {
			w := newWorld(name, now)
			s.build(w)
			return w.finish(s.name, s.about), nil
		}
	}
	return Scenario{}, fmt.Errorf("no scenario %q; there are %s", name, strings.Join(Names(), ", "))
}

// The fiction.

const (
	machineID   = "pc"
	machineName = "Gaming PC"
	host        = "dev-laptop"
	cacheDisk   = 30 << 30
)

type repo struct {
	url, short string
	branches   []string
	runs       []run
}

type run struct {
	command  string
	reason   string
	size     string
	gpu      bool
	min, max time.Duration
}

var (
	lantern = &repo{"https://github.com/acme/lantern", "lantern",
		[]string{"main", "feat/resumable-sync", "fix/store-lock-order", "feat/metrics-export"},
		[]run{
			{"go test -race ./...", "Full -race suite before opening the PR; it pins every core.", "medium", false, 4 * time.Minute, 9 * time.Minute},
			{"go vet ./... && go test ./...", "Checking the whole module after rebasing on main, while I keep editing.", "medium", false, 2 * time.Minute, 5 * time.Minute},
			{"go build ./cmd/... && GOOS=windows go build ./cmd/...", "Cross-compiling the release binaries for two platforms at once.", "medium", false, time.Minute, 3 * time.Minute},
			{"make release", "Release build with the full test matrix, about 20 minutes.", "large", false, 8 * time.Minute, 16 * time.Minute},
		}}
	tidepool = &repo{"https://github.com/acme/tidepool", "tidepool",
		[]string{"main", "feat/router-cache", "fix/hydration-flash"},
		[]run{
			{"npx vitest run", "Whole vitest suite after the router refactor; it spawns a worker per core.", "medium", false, 3 * time.Minute, 7 * time.Minute},
			{"npm test -- --coverage", "Coverage run for the review; it doubles the suite's memory.", "large", false, 5 * time.Minute, 9 * time.Minute},
			{"npm run build", "Production build to check the bundle size budget.", "small", false, time.Minute, 3 * time.Minute},
		}}
	orbit = &repo{"https://github.com/acme/orbit-android", "orbit-android",
		[]string{"main", "feat/offline-queue", "fix/sync-retry"},
		[]run{
			{"./gradlew :app:testDebugUnitTest", "Robolectric unit tests for the sync change; the full run needs a lot of disk.", "large", false, 6 * time.Minute, 14 * time.Minute},
			{"./gradlew lint assembleRelease", "Lint and a release build, so the PR's CI isn't the first to see them.", "medium", false, 4 * time.Minute, 8 * time.Minute},
		}}
	sparrow = &repo{"https://github.com/acme/sparrow-ml", "sparrow-ml",
		[]string{"main", "exp/lora-rank", "fix/tokenizer-pad"},
		[]run{
			{"uv run pytest -x tests/", "Property tests take 12 minutes here; running them on a box while I write the docs.", "medium", false, 3 * time.Minute, 12 * time.Minute},
			{"uv run python train.py --smoke --steps 200", "Fine-tuning smoke test; it needs CUDA.", "gpu", true, 10 * time.Minute, 35 * time.Minute},
		}}
	repos = []*repo{lantern, tidepool, orbit, sparrow}

	agents = []dash.Agent{
		{Client: "claude-code", Session: "quiet-otter", Host: host, Worktree: `C:\Users\dev\src\lantern\.claude\worktrees\agent-3f1c`},
		{Client: "codex", Session: "brisk-heron", Host: host, Worktree: `C:\Users\dev\src\tidepool`},
		{Client: "claude-code", Session: "amber-fox", Host: host, Worktree: `C:\Users\dev\src\orbit-android\.claude\worktrees\agent-9b20`},
		{Client: "cli", Session: "sparrow-wt-2", Host: host, Worktree: `C:\Users\dev\src\sparrow-wt-2`},
		{Client: "claude-code", Session: "still-pine", Host: host, Worktree: `C:\Users\dev\src\sparrow-ml`},
	}
)

// agentFor picks the agents who work in a repo.
func agentFor(r *repo, rnd *rand.Rand) dash.Agent {
	switch r {
	case lantern:
		return agents[0]
	case tidepool:
		return agents[1]
	case orbit:
		return agents[2]
	}
	return agents[3+rnd.IntN(2)]
}

// world is a scenario being built.
type world struct {
	now      time.Time
	rnd      *rand.Rand
	machine  dash.Machine
	boxes    []dash.Box
	jobs     []dash.Job // every job, newest first once finished
	logs     map[string]string
	segments []dash.Segment
	refusals []refusal
	since    time.Time // how far back history goes
}

type refusal struct {
	at  time.Time
	why string
}

func newWorld(name string, now time.Time) *world {
	h := fnv.New64a()
	h.Write([]byte(name))
	now = now.UTC().Truncate(time.Second)
	seen := now.Add(-12 * time.Second)
	built := now.Add(-26 * time.Hour)
	v := dash.Version{Commit: "e823842a1f0c9d6b7e5a4c3b2d1e0f9a8b7c6d5e", Time: &built}
	return &world{
		now: now,
		rnd: rand.New(rand.NewPCG(h.Sum64(), 0x5eed)),
		machine: dash.Machine{
			ID: machineID, Name: machineName, Reachable: true, Mode: dash.ModeShared,
			TrayLastSeen: &seen, Enforcer: dash.EnforcerTray,
			Caps:      dash.Resources{CPU: 24, Memory: 12 << 30, Instances: 6, Disk: 150 << 30},
			WSLMemory: 3 << 30,
			Boxd:      &v, Tray: &v,
			Config: hostConfig(true),
		},
		logs:  map[string]string{},
		since: now.Add(-24 * time.Hour),
	}
}

// hostConfig is boxd's HostConfig with every cap set and, without a GPU, the device missing.
func hostConfig(gpu bool) dash.Config {
	c := boxd.HostConfig(gpu)
	c.Capped = dash.Capped{CPU: true, Memory: true, Instances: true, Disk: true}
	if !gpu {
		c.GPUWhy = "device"
	}
	return c
}

func sizeOf(name string) mode.Size {
	s, ok := mode.LookupSize(name)
	if !ok {
		panic("fixture: no size " + name)
	}
	return s
}

func bytesOf(v string) int64 {
	n, ok := mode.ParseBytes(v)
	if !ok {
		panic("fixture: bad size " + v)
	}
	return n
}

func ptr[T any](v T) *T { return &v }

// uidOf is a made-up volatile.uuid for the box of name made at created. It is drawn from a hash,
// not w.rnd, so each scenario's other choices stay as they were.
func uidOf(name string, created time.Time) string {
	h := fnv.New128a()
	h.Write([]byte(name + "@" + created.UTC().Format(time.RFC3339Nano)))
	x := fmt.Sprintf("%x", h.Sum(nil))
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
}

// boxUIDAt is the UID of the box of name a job that started at started ran on: the live box of
// that name if it was made by then, else an earlier box of the name, gone now.
func (w *world) boxUIDAt(name string, started time.Time) string {
	for _, b := range w.boxes {
		if b.Name == name && !b.CreatedAt.After(started) {
			return b.UID
		}
	}
	return uidOf(name, started.Truncate(24*time.Hour))
}

func (w *world) hex(n int) string {
	const digits = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = digits[w.rnd.IntN(16)]
	}
	return string(b)
}

// box adds a box made age ago that expires in expires (0: no expiry).
func (w *world) box(name, size string, r *repo, state string, throwaway bool, age, expires time.Duration) *dash.Box {
	sz := sizeOf(size)
	b := dash.Box{
		Name: name, Machine: machineID, Size: size, GPU: sz.GPU, State: state, Throwaway: throwaway,
		Repo: r.url, CreatedAt: w.now.Add(-age), MemLimit: bytesOf(sz.Memory),
	}
	b.UID = uidOf(b.Name, b.CreatedAt)
	if expires != 0 {
		b.ExpiresAt = ptr(w.now.Add(expires))
	}
	w.boxes = append(w.boxes, b)
	return &w.boxes[len(w.boxes)-1]
}

// load sets a box's live use: a share of its memory and CPU in use.
func (w *world) load(b *dash.Box, mem, cpu float64) {
	b.MemUsed = int64(float64(b.MemLimit) * mem)
	b.CPUPct = math.Round(cpu*1000) / 10
}

// running adds a job running on b, started ago.
func (w *world) running(b *dash.Box, r *repo, ri int, branch string, ago time.Duration, state string) *dash.Job {
	rn := r.runs[ri]
	started := w.now.Add(-ago)
	id := started.Format("20060102-150405") + "-" + w.hex(4)
	j := dash.Job{
		Key: dash.JobKey(b.Name, id, started), ID: id, Box: b.Name, BoxUID: b.UID, Machine: machineID, State: state,
		StartedAt: started, Reason: rn.reason, Command: rn.command, Kind: dash.Classify(rn.command, b.GPU),
		Repo: r.url, Branch: branch, Commit: w.hex(40), Agent: agentFor(r, w.rnd), Size: b.Size, GPU: b.GPU,
	}
	log := w.log(j, 60+w.rnd.IntN(40), true)
	w.logs[j.Key] = log
	j.Tail = tail(log)
	w.jobs = append(w.jobs, j)
	return &w.jobs[len(w.jobs)-1]
}

// queued adds a job that has waited in the queue since ago, for why, at the end of the line.
func (w *world) queued(r *repo, ri int, branch, why string, ago time.Duration) *dash.Job {
	rn := r.runs[ri]
	at := w.now.Add(-ago)
	id := at.Format("20060102-150405") + "-" + w.hex(4)
	n := 1
	for _, j := range w.jobs {
		if j.State == dash.JobQueued {
			n++
		}
	}
	// Its box, as box run names it: the repo's own for a medium run, else a throwaway.
	box := r.short
	if rn.size != "medium" || rn.gpu {
		box = r.short + "-" + w.hex(4)
	}
	j := dash.Job{
		Key: dash.JobKey(box, id, at), ID: id, Box: box, Machine: machineID, State: dash.JobQueued, StartedAt: at, QueuedAt: &at, Position: n, Why: why,
		Reason: rn.reason, Command: rn.command, Kind: dash.Classify(rn.command, rn.gpu),
		Repo: r.url, Branch: branch, Commit: w.hex(40), Agent: agentFor(r, w.rnd), Size: rn.size, GPU: rn.gpu,
	}
	w.jobs = append(w.jobs, j)
	return &w.jobs[len(w.jobs)-1]
}

// history adds finished jobs from since to now, about perDay a day, most of
// them in working hours. Boxes named in keep keep their logs.
func (w *world) history(since time.Time, perDay int) {
	w.since = since
	var out []dash.Job
	days := w.now.Sub(since).Hours() / 24
	n := int(days * float64(perDay))
	for range n {
		t := since.Add(time.Duration(w.rnd.Float64() * float64(w.now.Sub(since))))
		// Fewer at night: the laptop's owner sleeps (UTC-8 in this fiction).
		if h := (t.Hour() + 16) % 24; h < 8 && w.rnd.IntN(4) != 0 {
			continue
		}
		r := repos[w.rnd.IntN(len(repos))]
		ri := w.rnd.IntN(len(r.runs))
		rn := r.runs[ri]
		d := rn.min + time.Duration(w.rnd.Float64()*float64(rn.max-rn.min))
		end := t.Add(d)
		if end.After(w.now.Add(-time.Minute)) {
			continue
		}
		boxName := r.short
		if rn.gpu || rn.size != "medium" || w.rnd.IntN(3) == 0 {
			boxName = r.short + "-" + w.hex(4)
		}
		id := t.Format("20060102-150405") + "-" + w.hex(4)
		j := dash.Job{
			Key: dash.JobKey(boxName, id, t), ID: id, Box: boxName, Machine: machineID, State: dash.JobDone,
			StartedAt: t.UTC().Truncate(time.Second), EndedAt: ptr(end.UTC().Truncate(time.Second)),
			Reason: rn.reason, Command: rn.command, Kind: dash.Classify(rn.command, rn.gpu), Repo: r.url,
			Branch: r.branches[w.rnd.IntN(len(r.branches))], Commit: w.hex(40), Agent: agentFor(r, w.rnd),
			Size: rn.size, GPU: rn.gpu,
		}
		j.Key = dash.JobKey(boxName, id, j.StartedAt)
		j.BoxUID = w.boxUIDAt(boxName, j.StartedAt)
		// About one in six waited in the queue first, for up to 25 minutes. It is drawn from
		// the ID, not w.rnd, so the rest of each scenario stays as it was.
		hv := fnv.New32a()
		hv.Write([]byte(id))
		if v := hv.Sum32(); v%6 == 0 {
			j.QueuedAt = ptr(j.StartedAt.Add(-time.Duration(30+v/6%1470) * time.Second))
		}
		switch p := w.rnd.IntN(100); {
		case p < 78:
			j.Exit = ptr(0)
		case p < 94:
			j.Exit = ptr(1 + w.rnd.IntN(2))
		case p < 98:
			j.Exit = ptr(130)
		default:
			j.State, j.Exit = dash.JobLost, nil
		}
		// Some jobs from before box run asked why.
		if w.rnd.IntN(25) == 0 {
			j.Reason = ""
		}
		j.Tail = tail(w.log(j, 8+w.rnd.IntN(10), false))
		out = append(out, j)
	}
	w.jobs = append(w.jobs, out...)
}

// segment adds a stretch of a mode.
func (w *world) segment(from, to time.Time, m string) {
	w.segments = append(w.segments, dash.Segment{Start: from, End: to, Mode: m})
}

// days fills since..now with a working week's modes, in the fiction's local
// time (UTC-8): Full overnight, Shared by day, now and then My PC at lunch,
// and most evenings My PC.
func (w *world) days(since time.Time) {
	cur := since
	add := func(to time.Time, m string) {
		to = earlier(to, w.now)
		if !to.After(cur) {
			return
		}
		w.segment(cur, to, m)
		cur = to
	}
	mins := func(n int) time.Duration { return time.Duration(n) * time.Minute }
	for d := since.Truncate(24 * time.Hour).Add(-16 * time.Hour); d.Before(w.now); d = d.Add(24 * time.Hour) {
		add(d.Add(8*time.Hour+mins(w.rnd.IntN(40))), dash.ModeFull)
		add(d.Add(12*time.Hour), dash.ModeShared)
		if w.rnd.IntN(3) == 0 {
			add(d.Add(13*time.Hour), dash.ModeMyPC)
		}
		add(d.Add(19*time.Hour+mins(w.rnd.IntN(60))), dash.ModeShared)
		if w.rnd.IntN(3) != 0 {
			add(d.Add(21*time.Hour+mins(w.rnd.IntN(90))), dash.ModeMyPC)
		}
		add(d.Add(24*time.Hour), dash.ModeFull)
	}
	w.merge()
}

// merge joins neighbouring segments of the same mode, and ends
// the last at now.
func (w *world) merge() {
	var out []dash.Segment
	for _, s := range w.segments {
		if n := len(out); n > 0 && out[n-1].Mode == s.Mode && !out[n-1].End.Before(s.Start) {
			out[n-1].End = s.End
			continue
		}
		out = append(out, s)
	}
	w.segments = out
}

// refuse adds runs turned away in each My PC stretch.
func (w *world) refuse() {
	for _, s := range w.segments {
		if s.Mode != dash.ModeMyPC {
			continue
		}
		why := dash.RefusedMyPC
		for range int(s.End.Sub(s.Start).Hours()*1.5) + w.rnd.IntN(2) {
			w.refusals = append(w.refusals, refusal{s.Start.Add(time.Duration(w.rnd.Float64() * float64(s.End.Sub(s.Start)))), why})
		}
	}
}

func (w *world) refuseRecent(why string, n int, within time.Duration) {
	for range n {
		w.refusals = append(w.refusals, refusal{w.now.Add(-time.Duration(w.rnd.Float64() * float64(within))), why})
	}
}

// Allot works out what the boxes hold of m's caps and what fits now, as
// box status does, and sets each box's CPUs for m's mode.
func Allot(m *dash.Machine, boxes []dash.Box) {
	m.Allocated = dash.Resources{Disk: cacheDisk}
	gpuTaken := false
	for i := range boxes {
		b := &boxes[i]
		sz := sizeOf(b.Size)
		m.Allocated.CPU += sz.FullCPUs
		m.Allocated.Memory += b.MemLimit
		m.Allocated.Instances++
		m.Allocated.Disk += bytesOf(sz.Disk)
		gpuTaken = gpuTaken || sz.GPU
		if b.State == dash.BoxRunning {
			b.CPUs = sz.CPUs(mode.Mode(m.Mode))
		} else {
			b.CPUs = sz.SharedCPUs
			b.CPUPct = 0
		}
	}
	fits := func(name string) int {
		sz := sizeOf(name)
		free := dash.Resources{
			CPU: m.Caps.CPU - m.Allocated.CPU, Memory: m.Caps.Memory - m.Allocated.Memory,
			Instances: m.Caps.Instances - m.Allocated.Instances, Disk: m.Caps.Disk - m.Allocated.Disk,
		}
		n := min(free.Instances, free.CPU/sz.FullCPUs, int(free.Memory/bytesOf(sz.Memory)), int(free.Disk/bytesOf(sz.Disk)))
		return max(n, 0)
	}
	m.Room = dash.Room{Small: fits("small"), Medium: fits("medium"), Large: fits("large")}
	if !gpuTaken {
		m.Room.GPU = min(fits("gpu"), 1)
	}
}

// finish works out what follows from the boxes and jobs: allocations, room,
// state's job list, the jobs page and the history.
func (w *world) finish(name, about string) Scenario {
	m := &w.machine
	Allot(m, w.boxes)
	var used int64
	for _, b := range w.boxes {
		used += b.MemUsed
	}
	m.WSLMemory = max(m.WSLMemory, used+900<<20)

	var live, queue, done []dash.Job
	for _, j := range w.jobs {
		switch j.State {
		case dash.JobRunning, dash.JobPaused:
			live = append(live, j)
		case dash.JobQueued:
			queue = append(queue, j)
		default:
			done = append(done, j)
		}
	}
	slices.SortFunc(live, func(a, b dash.Job) int { return b.StartedAt.Compare(a.StartedAt) })
	slices.SortFunc(queue, func(a, b dash.Job) int { return a.Position - b.Position })
	live = append(live, queue...)
	slices.SortFunc(done, func(a, b dash.Job) int { return b.StartedAt.Compare(a.StartedAt) })
	// The newest finished job on a box that still exists keeps its whole log.
	for _, j := range done {
		if w.hasBox(j.Box) && w.logs[j.Key] == "" && j.State == dash.JobDone {
			log := w.log(j, 30, false)
			w.logs[j.Key] = log
			for i := range done {
				if done[i].Key == j.Key {
					done[i].Tail = tail(log)
				}
			}
		}
	}
	state := append(slices.Clone(live), done[:min(len(done), dash.FinishedInState)]...)
	if state == nil {
		state = []dash.Job{}
	}
	boxes := w.boxes
	if boxes == nil {
		boxes = []dash.Box{}
	}
	if done == nil {
		done = []dash.Job{}
	}
	return Scenario{
		Name:  name,
		About: about,
		State: dash.State{Machines: []dash.Machine{*m}, Boxes: boxes, Jobs: state, Now: w.now},
		Jobs:  dash.JobsPage{Jobs: done},
		Day:   w.historyFor(dash.RangeDay, done),
		Week:  w.historyFor(dash.RangeWeek, done),
		Month: w.historyFor(dash.RangeMonth, done),
		Logs:  w.logs,
	}
}

func (w *world) hasBox(name string) bool {
	return slices.ContainsFunc(w.boxes, func(b dash.Box) bool { return b.Name == name })
}

// historyFor is a range's history, with the totals of the span before it.
func (w *world) historyFor(rng string, done []dash.Job) dash.History {
	start, step, n, _ := dash.Window(rng, w.now)
	h := w.span(start, step, n, done)
	h.Range = rng
	h.Previous = dash.Total(w.span(start.Add(-time.Duration(n)*step), step, n, done).Buckets)
	return h
}

// span buckets the finished jobs, box lives, refusals and samples.
func (w *world) span(start time.Time, step time.Duration, n int, done []dash.Job) dash.History {
	h := dash.History{Buckets: make([]dash.Bucket, n), Segments: []dash.Segment{}}
	durs := make([]map[string][]float64, n)
	for i := range h.Buckets {
		h.Buckets[i] = dash.Bucket{
			Start: start.Add(time.Duration(i) * step),
			Jobs: dash.JobCounts{ByResult: map[string]int{}, ByKind: map[string]int{}, ByRepo: map[string]int{},
				ByAgent: map[string]int{}},
			BoxHours:      dash.BoxHours{BySize: map[string]float64{}},
			Refusals:      dash.RefusalCounts{ByWhy: map[string]int{}},
			MedianSeconds: dash.Medians{ByRepo: map[string]float64{}},
			MemCap:        w.machine.Caps.Memory,
		}
		durs[i] = map[string][]float64{}
	}
	at := func(t time.Time) int {
		if t.Before(start) {
			return -1
		}
		i := int(t.Sub(start) / step)
		if i >= n {
			return -1
		}
		return i
	}
	// A box lives from a minute before its job to when it ends: a throwaway
	// at once, a repo's box up to 2h later, or at its next job.
	type life struct {
		from, to time.Time
		size     string
		mem      float64
	}
	var lives []life
	byBox := map[string][]dash.Job{}
	for _, j := range done {
		byBox[j.Box] = append(byBox[j.Box], j)
	}
	for _, name := range slices.Sorted(maps.Keys(byBox)) {
		js := byBox[name]
		slices.SortFunc(js, func(a, b dash.Job) int { return a.StartedAt.Compare(b.StartedAt) })
		for i, j := range js {
			to := j.EndedAt
			if to == nil {
				to = ptr(j.StartedAt.Add(10 * time.Minute))
			}
			end := *to
			if j.Box == repoShort(j.Repo) {
				end = end.Add(2 * time.Hour)
				if i+1 < len(js) && js[i+1].StartedAt.Before(end) {
					end = js[i+1].StartedAt
				}
			}
			lives = append(lives, life{j.StartedAt.Add(-time.Minute), end, j.Size, 0.35 + 0.3*w.rnd.Float64()})
		}
	}
	for _, b := range w.boxes {
		lives = append(lives, life{b.CreatedAt, w.now, b.Size, float64(b.MemUsed) / float64(max(b.MemLimit, 1))})
	}
	for _, l := range lives {
		mem := float64(bytesOf(sizeOf(l.size).Memory)) * l.mem
		for i := range h.Buckets {
			bs := h.Buckets[i].Start
			be := bs.Add(step)
			lo, hi := later(l.from, bs), earlier(l.to, be)
			if !hi.After(lo) {
				continue
			}
			share := hi.Sub(lo).Hours()
			h.Buckets[i].BoxHours.BySize[l.size] = round2(h.Buckets[i].BoxHours.BySize[l.size] + share)
			h.Buckets[i].MemUsed += int64(mem * share / step.Hours())
		}
	}
	for _, j := range done {
		i := at(j.StartedAt)
		if i < 0 {
			continue
		}
		b := &h.Buckets[i]
		b.Jobs.ByResult[j.Result()]++
		b.Jobs.ByKind[j.Kind]++
		b.Jobs.ByRepo[repoShort(j.Repo)]++
		b.Jobs.ByAgent[j.Agent.Session]++
		if j.EndedAt != nil && j.State == dash.JobDone {
			durs[i][repoShort(j.Repo)] = append(durs[i][repoShort(j.Repo)], j.EndedAt.Sub(j.StartedAt).Seconds())
		}
	}
	for i, byRepo := range durs {
		for r, ds := range byRepo {
			h.Buckets[i].MedianSeconds.ByRepo[r] = median(ds)
		}
	}
	for _, r := range w.refusals {
		if i := at(r.at); i >= 0 {
			h.Buckets[i].Refusals.ByWhy[r.why]++
		}
	}
	// Queued jobs count when they were queued; those that started, their wait when they started.
	waits := make([][]float64, n)
	for _, j := range w.jobs {
		if j.QueuedAt == nil {
			continue
		}
		if i := at(*j.QueuedAt); i >= 0 {
			h.Buckets[i].Queued.Jobs++
		}
		if i := at(j.StartedAt); i >= 0 && j.State != dash.JobQueued {
			waits[i] = append(waits[i], j.StartedAt.Sub(*j.QueuedAt).Seconds())
		}
	}
	for i, ws := range waits {
		if len(ws) > 0 {
			h.Buckets[i].Queued.Started, h.Buckets[i].Queued.MedianWaitSeconds = len(ws), median(ws)
		}
	}
	for _, s := range w.segments {
		if !s.End.After(start) {
			continue
		}
		s.Start = later(s.Start, start)
		h.Segments = append(h.Segments, s)
	}
	if len(h.Segments) == 0 {
		h.Segments = append(h.Segments, dash.Segment{Start: start, End: w.now, Mode: w.machine.Mode})
	}
	return h
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

func median(ds []float64) float64 {
	slices.Sort(ds)
	n := len(ds)
	if n%2 == 1 {
		return math.Round(ds[n/2])
	}
	return math.Round((ds[n/2-1] + ds[n/2]) / 2)
}

// repoShort is a repo's last path element, as the page names it.
func repoShort(url string) string {
	return url[strings.LastIndex(url, "/")+1:]
}

// tail is the last dash.TailBytes of a log, from a line's start.
func tail(log string) string {
	if len(log) <= dash.TailBytes {
		return log
	}
	t := log[len(log)-dash.TailBytes:]
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = t[i+1:]
	}
	return t
}

// The scenarios.

func buildEmpty(w *world) {
	w.machine.WSLMemory = 1200 << 20
	w.machine.Config = hostConfig(false) // a bare host: no GPU
	w.segment(w.now.Add(-24*time.Hour), w.now, dash.ModeShared)
}

func buildBusy(w *world) {
	w.machine.Mode = dash.ModeFull
	lb := w.box("lantern", "medium", lantern, dash.BoxRunning, false, 5*time.Hour, 23*time.Hour+40*time.Minute)
	w.load(lb, 0.71, 0.93)
	tb := w.box("tidepool-3c1a", "medium", tidepool, dash.BoxRunning, true, 4*time.Minute, 23*time.Hour+56*time.Minute)
	w.load(tb, 0.58, 0.81)
	ob := w.box("orbit-android", "small", orbit, dash.BoxRunning, false, 3*time.Hour, 74*time.Minute)
	w.load(ob, 0.22, 0.02)
	w.running(lb, lantern, 0, "feat/resumable-sync", 6*time.Minute+12*time.Second, dash.JobRunning)
	w.running(tb, tidepool, 0, "feat/router-cache", 3*time.Minute+40*time.Second, dash.JobRunning)
	w.history(w.now.Add(-24*time.Hour), 34)
	w.segment(w.now.Add(-24*time.Hour), w.now.Add(-15*time.Hour), dash.ModeShared)
	w.segment(w.now.Add(-15*time.Hour), w.now.Add(-12*time.Hour), dash.ModeMyPC)
	w.segment(w.now.Add(-12*time.Hour), w.now.Add(-3*time.Hour), dash.ModeFull)
	w.segment(w.now.Add(-3*time.Hour), w.now, dash.ModeFull)
	w.merge()
	w.refuse()
}

func buildMyPC(w *world) {
	w.machine.Mode = dash.ModeMyPC
	lb := w.box("lantern", "medium", lantern, dash.BoxFrozen, false, 2*time.Hour, 23*time.Hour+10*time.Minute)
	w.load(lb, 0.66, 0)
	sb := w.box("sparrow-ml", "medium", sparrow, dash.BoxFrozen, false, 50*time.Minute, 70*time.Minute)
	w.load(sb, 0.31, 0)
	w.running(lb, lantern, 0, "fix/store-lock-order", 22*time.Minute, dash.JobPaused)
	w.history(w.now.Add(-24*time.Hour), 26)
	w.segment(w.now.Add(-24*time.Hour), w.now.Add(-7*time.Hour), dash.ModeShared)
	w.segment(w.now.Add(-7*time.Hour), w.now.Add(-18*time.Minute), dash.ModeFull)
	w.segment(w.now.Add(-18*time.Minute), w.now, dash.ModeMyPC)
	w.refuse()
	w.refuseRecent(dash.RefusedMyPC, 3, 15*time.Minute)
	w.queued(lantern, 1, "fix/store-lock-order", dash.RefusedMyPC, 4*time.Minute+10*time.Second)
	w.queued(tidepool, 2, "main", dash.RefusedMyPC, 55*time.Second)
}

func buildFull(w *world) {
	w.machine.Mode = dash.ModeFull
	gb := w.box("sparrow-ml-9d04", "gpu", sparrow, dash.BoxRunning, true, 26*time.Minute, 23*time.Hour+34*time.Minute)
	w.load(gb, 0.83, 0.97)
	lb := w.box("lantern", "medium", lantern, dash.BoxRunning, false, 3*time.Hour, 23*time.Hour+50*time.Minute)
	w.load(lb, 0.64, 0.88)
	w.running(gb, sparrow, 1, "exp/lora-rank", 25*time.Minute, dash.JobRunning)
	w.running(lb, lantern, 3, "main", 9*time.Minute, dash.JobRunning)
	w.history(w.now.Add(-24*time.Hour), 30)
	w.segment(w.now.Add(-24*time.Hour), w.now.Add(-9*time.Hour), dash.ModeShared)
	w.segment(w.now.Add(-9*time.Hour), w.now, dash.ModeFull)
	w.refuseRecent(dash.RefusedFull, 9, 3*time.Hour)
	w.refuseRecent(dash.RefusedGPUBusy, 4, 25*time.Minute)
	w.queued(tidepool, 1, "feat/router-cache", dash.RefusedFull, 14*time.Minute+20*time.Second)
	w.queued(sparrow, 1, "fix/tokenizer-pad", dash.RefusedGPUBusy, 6*time.Minute+5*time.Second)
	w.queued(orbit, 0, "fix/sync-retry", dash.RefusedFull, 2*time.Minute+40*time.Second)
}

// versions puts boxd on release newer and the enforcer on release older, a build behind. They
// have differed for behind: boxd started then, with the enforcer already on older.
func (w *world) versions(newer, older string, behind time.Duration) (*dash.Version, *dash.Version) {
	built := w.machine.Boxd.Time.Add(-3 * time.Hour)
	since := w.now.Add(-behind) // boxd's start; the tray's key held its value already, so the same
	b := &dash.Version{Commit: w.machine.Boxd.Commit, Time: w.machine.Boxd.Time, Tag: newer, Since: &since}
	t := &dash.Version{Commit: "1378501c4d2e8f7a6b5c4d3e2f1a0b9c8d7e6f5a", Time: &built, Tag: older, Since: &since}
	return b, t
}

func buildTrayDown(w *world) {
	seen := w.now.Add(-14 * time.Minute)
	w.machine.TrayLastSeen = &seen
	w.machine.ModeRequested = &dash.ModeRequest{Mode: dash.ModeMyPC, At: w.now.Add(-5 * time.Minute), By: "phone"}
	// boxd has run a newer release for 3h and the tray hasn't updated to it, so the page warns.
	w.machine.Boxd, w.machine.Tray = w.versions("v0.3.0", "v0.2.0", 3*time.Hour)
	tb := w.box("tidepool", "medium", tidepool, dash.BoxRunning, false, 90*time.Minute, 23*time.Hour+30*time.Minute)
	w.load(tb, 0.49, 0.45)
	w.running(tb, tidepool, 0, "fix/hydration-flash", 2*time.Minute+5*time.Second, dash.JobRunning)
	w.history(w.now.Add(-24*time.Hour), 22)
	w.segment(w.now.Add(-24*time.Hour), w.now, dash.ModeShared)
}

func buildHeadless(w *world) {
	w.machine.Mode = dash.ModeFull
	// boxd updated 20 minutes ago; the enforcer's hourly check hasn't come round yet.
	w.machine.Boxd, w.machine.Tray = w.versions("v0.3.1", "v0.3.0", 20*time.Minute)
	w.machine.Enforcer = dash.EnforcerHeadless
	w.machine.ModeRequested = &dash.ModeRequest{Mode: dash.ModeShared, At: w.now.Add(-7 * time.Minute), By: "phone"}
	lb := w.box("lantern", "large", lantern, dash.BoxRunning, false, 35*time.Minute, 23*time.Hour+40*time.Minute)
	w.load(lb, 0.55, 0.96)
	w.running(lb, lantern, 3, "main", 12*time.Minute, dash.JobRunning)
	w.history(w.now.Add(-24*time.Hour), 18)
	w.segment(w.now.Add(-24*time.Hour), w.now.Add(-9*time.Hour), dash.ModeShared)
	w.segment(w.now.Add(-9*time.Hour), w.now, dash.ModeFull)
}

func buildLost(w *world) {
	ob := w.box("orbit-android", "large", orbit, dash.BoxRunning, false, 40*time.Minute, 23*time.Hour+21*time.Minute)
	w.load(ob, 0.77, 0.9)
	w.running(ob, orbit, 0, "feat/offline-queue", 11*time.Minute, dash.JobRunning)
	w.history(w.now.Add(-24*time.Hour), 20)
	// The GPU box was stopped in My PC while boxd was down; when boxd came
	// back the box had been reaped, so its job is lost.
	started := w.now.Add(-52 * time.Minute)
	id := started.Format("20060102-150405") + "-" + w.hex(4)
	rn := sparrow.runs[1]
	ended := w.now.Add(-8 * time.Minute)
	j := dash.Job{
		Key: dash.JobKey("sparrow-ml-51ee", id, started), ID: id, Box: "sparrow-ml-51ee", BoxUID: uidOf("sparrow-ml-51ee", started), Machine: machineID,
		State: dash.JobLost, StartedAt: started, EndedAt: &ended, Reason: rn.reason, Command: rn.command,
		Kind: dash.KindGPU, Repo: sparrow.url, Branch: "exp/lora-rank", Commit: w.hex(40), Agent: agents[4],
		Size: "gpu", GPU: true,
	}
	j.Tail = tail(w.log(j, 14, true))
	w.jobs = append(w.jobs, j)
	w.segment(w.now.Add(-24*time.Hour), w.now.Add(-50*time.Minute), dash.ModeShared)
	w.segment(w.now.Add(-50*time.Minute), w.now.Add(-20*time.Minute), dash.ModeMyPC)
	w.segment(w.now.Add(-20*time.Minute), w.now, dash.ModeShared)
	w.refuse()
}

func buildWeek(w *world) {
	since := w.now.Add(-7 * 24 * time.Hour)
	lb := w.box("lantern", "medium", lantern, dash.BoxRunning, false, 80*time.Minute, 23*time.Hour+48*time.Minute)
	w.load(lb, 0.62, 0.9)
	w.running(lb, lantern, 1, "feat/metrics-export", 90*time.Second, dash.JobRunning)
	w.history(since, 38)
	w.days(since)
	w.machine.Mode = w.segments[len(w.segments)-1].Mode
	w.refuse()
	w.refuseRecent(dash.RefusedFull, 6, 5*24*time.Hour)
	w.refuseRecent(dash.RefusedGPUBusy, 3, 6*24*time.Hour)
}

// Logs.

// log writes a plausible log for j of about n lines. live leaves it
// unfinished, as a running job's is.
func (w *world) log(j dash.Job, n int, live bool) string {
	var b strings.Builder
	r := repoShort(j.Repo)
	fmt.Fprintf(&b, "$ %s\n", j.Command)
	fail := j.Exit != nil && *j.Exit != 0 && *j.Exit != 130
	pkgs := []string{"internal/store", "internal/sync", "internal/api", "internal/config", "internal/metrics", "cmd/" + r, "internal/wire", "internal/cache"}
	files := []string{"src/router/cache.test.ts", "src/store/session.test.ts", "src/ui/Button.test.tsx", "src/api/client.test.ts", "src/hydrate.test.ts"}
	for i := range n {
		switch j.Kind {
		case dash.KindGoTest, dash.KindGoBuild:
			p := pkgs[i%len(pkgs)]
			if j.Kind == dash.KindGoBuild {
				fmt.Fprintf(&b, "github.com/acme/%s/%s\n", r, p)
				continue
			}
			if i%7 == 3 {
				fmt.Fprintf(&b, "?   \tgithub.com/acme/%s/%s\t[no test files]\n", r, p)
			} else {
				fmt.Fprintf(&b, "ok  \tgithub.com/acme/%s/%s\t%.3fs\n", r, p, 0.4+w.rnd.Float64()*9)
			}
		case dash.KindJSTest:
			fmt.Fprintf(&b, " ✓ %s (%d tests) %dms\n", files[i%len(files)], 3+w.rnd.IntN(30), 40+w.rnd.IntN(900))
		case dash.KindGradle:
			tasks := []string{":app:compileDebugKotlin", ":app:processDebugResources", ":core:testDebugUnitTest", ":app:testDebugUnitTest", ":sync:testDebugUnitTest"}
			fmt.Fprintf(&b, "> Task %s\n", tasks[i%len(tasks)])
		case dash.KindPyTest:
			fmt.Fprintf(&b, "tests/test_%s.py %s [%3d%%]\n", []string{"tokenizer", "data", "model", "schedule"}[i%4], strings.Repeat(".", 8+w.rnd.IntN(30)), (i+1)*100/max(n, 1))
		case dash.KindGPU:
			fmt.Fprintf(&b, "step %4d/200  loss %.4f  lr 2.0e-4  %.1f it/s  gpu mem 6.%d GiB\n", (i+1)*200/max(n*3, 1), 2.8-float64(i)*0.04+w.rnd.Float64()*0.05, 3+w.rnd.Float64(), w.rnd.IntN(9))
		default:
			fmt.Fprintf(&b, "  CC   build/%s/obj%02d.o\n", r, i)
		}
	}
	if live {
		return b.String()
	}
	switch {
	case j.State == dash.JobLost:
	case j.Exit != nil && *j.Exit == 130:
		b.WriteString("^C\nbox: cancelled\n")
	case fail && j.Kind == dash.KindGoTest:
		fmt.Fprintf(&b, "--- FAIL: TestResumeAfterPartialChunk (0.42s)\n    sync_test.go:88: got 3 chunks, want 4\nFAIL\nFAIL\tgithub.com/acme/%s/internal/sync\t2.104s\nFAIL\n", r)
	case fail && j.Kind == dash.KindJSTest:
		b.WriteString(" ✗ src/router/cache.test.ts > evicts the oldest entry\n   AssertionError: expected 3 to be 2\n Test Files  1 failed | 41 passed (42)\n")
	case fail:
		b.WriteString("error: process exited with status 1\n")
	case j.Kind == dash.KindJSTest:
		b.WriteString(" Test Files  42 passed (42)\n      Tests  611 passed (611)\n")
	case j.Kind == dash.KindGradle:
		b.WriteString("BUILD SUCCESSFUL in 7m 12s\n")
	case j.Kind == dash.KindPyTest:
		b.WriteString("======== 318 passed in 412.80s ========\n")
	}
	return b.String()
}

// For the devserver, which plays a scenario forward.

// Player makes new boxes, jobs and log lines for a scenario being played.
type Player struct{ w *world }

// NewPlayer returns a Player whose choices follow seed.
func NewPlayer(seed uint64) *Player {
	w := newWorld("player", time.Now())
	w.rnd = rand.New(rand.NewPCG(seed, 0x9a7))
	return &Player{w}
}

// Rand is the player's source of chance.
func (p *Player) Rand() *rand.Rand { return p.w.rnd }

// Start makes a job that starts now and the box it runs on: the repo's own
// box when taken says that name is free, else a throwaway.
func (p *Player) Start(now time.Time, taken func(name string) bool) (dash.Box, dash.Job) {
	w := p.w
	w.now = now.UTC().Truncate(time.Second)
	r := repos[w.rnd.IntN(len(repos))]
	ri := w.rnd.IntN(len(r.runs))
	rn := r.runs[ri]
	name := r.short
	if taken(name) || rn.size != "medium" || rn.gpu {
		name = r.short + "-" + w.hex(4)
	}
	w.boxes, w.jobs = nil, nil
	b := w.box(name, rn.size, r, dash.BoxRunning, name != r.short, 0, 24*time.Hour)
	w.load(b, 0.2+0.3*w.rnd.Float64(), 0.6+0.35*w.rnd.Float64())
	j := *w.running(b, r, ri, r.branches[w.rnd.IntN(len(r.branches))], 0, dash.JobRunning)
	return *b, j
}

// StartQueued makes the running job a queued q becomes, as boxd's starter does: a new key and
// start, the same ID, box and QueuedAt. The box is q's, made now; the devserver reuses one that
// exists instead.
func (p *Player) StartQueued(now time.Time, q dash.Job) (dash.Box, dash.Job) {
	w := p.w
	w.now = now.UTC().Truncate(time.Second)
	r := slices.IndexFunc(repos, func(r *repo) bool { return r.url == q.Repo })
	rp := repos[max(r, 0)]
	w.boxes = nil
	b := w.box(q.Box, q.Size, rp, dash.BoxRunning, q.Box != rp.short, 0, 24*time.Hour)
	w.load(b, 0.2+0.3*w.rnd.Float64(), 0.6+0.35*w.rnd.Float64())
	j := q
	j.Key, j.State, j.StartedAt, j.Position, j.Why = dash.JobKey(q.Box, q.ID, w.now), dash.JobRunning, w.now, 0, ""
	j.BoxUID = b.UID
	return *b, j
}

// Lines returns n more lines of j's log.
func (p *Player) Lines(j dash.Job, n int) string {
	full := p.w.log(j, n, true)
	return full[strings.IndexByte(full, '\n')+1:]
}

// End returns the lines a finished j's log ends with.
func (p *Player) End(j dash.Job) string {
	full := p.w.log(j, 0, false)
	return full[strings.IndexByte(full, '\n')+1:]
}

// Tail is the last dash.TailBytes of a log, from a line's start.
func Tail(log string) string { return tail(log) }
