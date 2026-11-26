package boxd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
)

// The history is JSON lines in Dir (docs/ui.md, "History"): events-DATE.jsonl,
// kept EventDays, and samples-DATE.jsonl, kept SampleDays. Every line has
// "v": 1; a reader skips a line it can't parse, so a crash mid-write loses
// that line only.
const (
	EventDays  = 180
	SampleDays = 14
	TailDays   = 30 // a job's kept tail is dropped from its events after this
)

// Event types in the events files.
const (
	recJobStart    = "job-start"
	recJobEnd      = "job-end"
	recBoxCreate   = "box-create"
	recBoxDelete   = "box-delete"
	recMode        = "mode" // the mode changed
	recControl     = "control"
	actionEvict    = "evict" // a control: the queue deleted kept box Target for job By's room
	recRefusal     = "refusal"
	recQueueAdd    = "queue-add"    // a job was queued
	recQueueStart  = "queue-start"  // boxd started it; seconds is the wait
	recQueueCancel = "queue-cancel" // cancelled while it waited
	recQueueDrop   = "queue-drop"   // it could never start, or waited too long
)

// record is one line of an events file.
type record struct {
	V       int               `json:"v"`
	At      time.Time         `json:"at"`
	Type    string            `json:"type"`
	Job     *dash.Job         `json:"job,omitempty"`
	Seconds float64           `json:"seconds,omitempty"` // job-end: how long it ran
	Box     *boxRec           `json:"box,omitempty"`
	Mode    *modeRec          `json:"mode,omitempty"`
	Refusal *dash.RefusalBody `json:"refusal,omitempty"`
	Control *controlRec       `json:"control,omitempty"`
}

type boxRec struct {
	Name      string    `json:"name"`
	Size      string    `json:"size"`
	GPU       bool      `json:"gpu"`
	CreatedAt time.Time `json:"createdAt"`
}

func (b boxRec) id() string { return b.Name + "@" + b.CreatedAt.UTC().Format(time.RFC3339Nano) }

type modeRec struct {
	Mode string `json:"mode"`
}

// controlRec is something done from the page (step 4 of docs/ui.md).
type controlRec struct {
	Action string `json:"action"`
	Target string `json:"target"`
	By     string `json:"by"`
}

// sample is one line of a samples file, each minute, for the machine. Files
// written before 2026-11-25 also hold a line per box ("kind":"box"), which
// nothing reads.
type sample struct {
	V        int       `json:"v"`
	At       time.Time `json:"at"`
	Kind     string    `json:"kind"` // machine
	MemUsed  int64     `json:"memUsed"`
	MemCap   int64     `json:"memCap,omitempty"`
	MemAlloc int64     `json:"memAlloc,omitempty"` // what boxes hold of the cap
	WSL      int64     `json:"wslMemory,omitempty"`
}

type boxSpan struct {
	rec   boxRec
	start time.Time
	end   *time.Time
}

// unsetTime reports a zero or Unix-epoch timestamp that Incus reports while
// an instance is still being created.
func unsetTime(t time.Time) bool {
	return t.IsZero() || t.Unix() <= 0
}

type modeAt struct {
	at time.Time
	modeRec
}

type waitAt struct {
	at      time.Time
	seconds float64
}

// evictAt is when the queue deleted a kept throwaway, and for which job.
type evictAt struct {
	at  time.Time
	job string
}

type refusalAt struct {
	at  time.Time
	why string
}

// History writes the files and answers questions from them. It keeps every
// event in memory: a busy day is a few hundred lines.
type History struct {
	dir string
	now func() time.Time

	mu       sync.Mutex
	jobs     map[string]*dash.Job
	boxes    map[string]*boxSpan
	modes    []modeAt
	refusals []refusalAt
	queueAdd []time.Time
	waits    []waitAt
	evicted  map[string]evictAt  // kept throwaways the queue deleted for room, by box name
	memDays  map[string][]memAgg // done days' minute samples, by UTC hour
}

type memAgg struct {
	sum, n int64
	cap    int64
}

// OpenHistory reads the events files in dir, making dir if it is missing.
func OpenHistory(dir string, now func() time.Time) (*History, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	h := &History{dir: dir, now: now, jobs: map[string]*dash.Job{}, boxes: map[string]*boxSpan{}, evicted: map[string]evictAt{}, memDays: map[string][]memAgg{}}
	files, _ := filepath.Glob(filepath.Join(dir, "events-*.jsonl"))
	slices.Sort(files)
	for _, f := range files {
		eachLine(f, func(b []byte) {
			var r record
			if json.Unmarshal(b, &r) == nil && r.V == 1 {
				h.apply(r)
			}
		})
	}
	h.dropOldTails()
	return h, nil
}

func eachLine(path string, fn func([]byte)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if b := bytes.TrimSpace(sc.Bytes()); len(b) > 0 {
			fn(b)
		}
	}
}

// apply folds a record into memory.
func (h *History) apply(r record) {
	switch r.Type {
	case recQueueAdd:
		h.queueAdd = append(h.queueAdd, r.At)
		return
	case recQueueStart:
		h.waits = append(h.waits, waitAt{r.At, r.Seconds})
		return
	case recQueueCancel, recQueueDrop:
		return
	case recJobStart, recJobEnd:
		if r.Job == nil {
			return
		}
		j := *r.Job
		if old, ok := h.jobs[j.Key]; ok && r.Type == recJobStart && old.State != dash.JobRunning {
			return // an end already read wins over a repeated start
		}
		h.jobs[j.Key] = &j
	case recBoxCreate:
		if r.Box != nil {
			start := r.Box.CreatedAt
			if unsetTime(start) {
				start = r.At
			}
			h.boxes[r.Box.id()] = &boxSpan{rec: *r.Box, start: start}
		}
	case recBoxDelete:
		if r.Box != nil {
			if s, ok := h.boxes[r.Box.id()]; ok {
				at := r.At
				s.end = &at
			}
		}
	case recMode:
		if r.Mode != nil {
			h.modes = append(h.modes, modeAt{r.At, *r.Mode})
		}
	case recControl:
		if c := r.Control; c != nil && c.Action == actionEvict {
			h.evicted[c.Target] = evictAt{r.At, c.By}
		}
	case recRefusal:
		if r.Refusal != nil {
			h.refusals = append(h.refusals, refusalAt{r.At, r.Refusal.Why})
		}
	}
}

// Record appends an event, masking the job's and the refusal's text first.
func (h *History) Record(r record) error {
	r.V = 1
	if r.At.IsZero() {
		r.At = h.now()
	}
	r.At = r.At.UTC()
	if r.Job != nil {
		j := *r.Job
		j.Command, j.Reason, j.Tail, j.Repo = Mask(j.Command), Mask(j.Reason), Mask(j.Tail), Mask(j.Repo)
		if r.Type == recJobStart {
			j.Tail = ""
		}
		r.Job = &j
	}
	if r.Refusal != nil {
		f := *r.Refusal
		f.Reason = Mask(f.Reason)
		r.Refusal = &f
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.apply(r)
	return appendLine(filepath.Join(h.dir, "events-"+r.At.Format("2006-01-02")+".jsonl"), b)
}

// Samples appends one minute's samples.
func (h *History) Samples(ss []sample) error {
	if len(ss) == 0 {
		return nil
	}
	var buf bytes.Buffer
	for _, s := range ss {
		s.V = 1
		b, err := json.Marshal(s)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	at := ss[0].At.UTC()
	return appendLine(filepath.Join(h.dir, "samples-"+at.Format("2006-01-02")+".jsonl"), bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

func appendLine(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Prune deletes files past their keep and drops kept tails older than
// TailDays from the events files.
func (h *History) Prune() error {
	now := h.now().UTC()
	day := func(name, prefix string) (time.Time, bool) {
		t, err := time.Parse("2006-01-02", strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".jsonl"))
		return t, err == nil
	}
	h.mu.Lock() // Record appends to these files
	defer h.mu.Unlock()
	var errs []error
	for _, k := range []struct {
		prefix string
		keep   int
	}{{"events-", EventDays}, {"samples-", SampleDays}} {
		files, _ := filepath.Glob(filepath.Join(h.dir, k.prefix+"*.jsonl"))
		for _, f := range files {
			t, ok := day(filepath.Base(f), k.prefix)
			if !ok {
				continue
			}
			age := now.Sub(t)
			switch {
			case age > time.Duration(k.keep+1)*24*time.Hour:
				if err := os.Remove(f); err != nil {
					errs = append(errs, err)
				}
			case k.prefix == "events-" && age > time.Duration(TailDays+1)*24*time.Hour:
				if err := stripTails(f); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	h.dropOldTails()
	if len(errs) > 0 {
		return fmt.Errorf("prune history: %v", errs)
	}
	return nil
}

// stripTails rewrites an events file without its jobs' tails, once.
func stripTails(path string) error {
	var out bytes.Buffer
	changed := false
	eachLine(path, func(b []byte) {
		var r record
		if json.Unmarshal(b, &r) == nil && r.Job != nil && r.Job.Tail != "" {
			r.Job.Tail = ""
			if nb, err := json.Marshal(r); err == nil {
				b, changed = nb, true
			}
		}
		out.Write(b)
		out.WriteByte('\n')
	})
	if !changed {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out.Bytes(), 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (h *History) dropOldTails() {
	cut := h.now().Add(-TailDays * 24 * time.Hour)
	for _, j := range h.jobs {
		if j.StartedAt.Before(cut) {
			j.Tail = ""
		}
	}
}

// AllJobs returns every recorded job, newest first.
func (h *History) AllJobs() []dash.Job {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]dash.Job, 0, len(h.jobs))
	for _, j := range h.jobs {
		out = append(out, *j)
	}
	slices.SortFunc(out, newest)
	return out
}

// Job returns a recorded job.
func (h *History) Job(key string) (dash.Job, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	j, ok := h.jobs[key]
	if !ok {
		return dash.Job{}, false
	}
	return *j, true
}

// EndedOn names the jobs ("<box>.<id>", as box names their files) recorded as
// ended on the box in, so a scan can skip their details.
func (h *History) EndedOn(in Instance) map[string]bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]bool{}
	for _, j := range h.jobs {
		if j.Box == in.Name && (j.State == dash.JobDone || j.State == dash.JobLost) && ranOnInstance(*j, in) {
			out[in.Name+"."+j.ID] = true
		}
	}
	return out
}

// EndedHere says whether a job recorded on in, by its UID (ranOnInstance),
// has ended. A job recorded without a UID never counts: ranOn's fallback by
// name and creation time could credit a predecessor's job to a same-name box
// made within the grace (holds), one whose first job is still starting.
func (h *History) EndedHere(in Instance) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, j := range h.jobs {
		if j.BoxUID != "" && (j.State == dash.JobDone || j.State == dash.JobLost) && ranOnInstance(*j, in) {
			return true
		}
	}
	return false
}

// LastEnded is the job that started last of those recorded on the box
// on the box in, if it has ended: the one whose 24h the box holds. A
// queued job's name is when it was queued, so the newest name may not be it.
func (h *History) LastEnded(in Instance) (dash.Job, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out dash.Job
	for _, j := range h.jobs {
		if j.Box == in.Name && ranOnInstance(*j, in) && j.StartedAt.After(out.StartedAt) {
			out = *j
		}
	}
	return out, out.Box != "" && (out.State == dash.JobDone || out.State == dash.JobLost)
}

// Open returns the jobs recorded as started but not ended, and the boxes
// recorded as created but not deleted: what a restart reconciles.
func (h *History) Open() (jobs []dash.Job, boxes []boxRec) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, j := range h.jobs {
		if j.State == dash.JobRunning || j.State == dash.JobPaused {
			jobs = append(jobs, *j)
		}
	}
	for _, b := range h.boxes {
		if b.end == nil {
			boxes = append(boxes, b.rec)
		}
	}
	return jobs, boxes
}

// LastMode is the last mode recorded, or nil.
func (h *History) LastMode() *modeRec {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.modes) == 0 {
		return nil
	}
	m := h.modes[len(h.modes)-1].modeRec
	return &m
}

// Filter is GET /api/jobs's query.
type Filter struct {
	Repo, Agent, Kind, Machine, Result, Q string
	// Box, when its UID is set, keeps the jobs that ran on that one box
	// (dash.Job.RanOn), not those of another box of its name, and asks for
	// the page's Last.
	Box dash.Box
	// Before is the last job of the page before: the page is the jobs after
	// it in newest-first order.
	Before *dash.Job
	Limit  int
}

// newest orders jobs newest first: by startedAt, then by key, since start
// times have one-second resolution.
func newest(a, b dash.Job) int {
	if c := b.StartedAt.Compare(a.StartedAt); c != 0 {
		return c
	}
	return strings.Compare(b.Key, a.Key)
}

func (f Filter) match(j *dash.Job) bool {
	if f.Repo != "" && f.Repo != j.Repo && f.Repo != repoShort(j.Repo) {
		return false
	}
	if (f.Agent != "" && f.Agent != j.Agent.Session) || (f.Kind != "" && f.Kind != j.Kind) ||
		(f.Machine != "" && f.Machine != j.Machine) || (f.Result != "" && f.Result != j.Result()) ||
		(f.Box.UID != "" && !j.RanOn(f.Box)) {
		return false
	}
	if f.Before != nil && newest(*f.Before, *j) >= 0 {
		return false
	}
	if q := strings.ToLower(f.Q); q != "" && !strings.Contains(strings.ToLower(j.Reason+"\n"+j.Command+"\n"+j.Branch), q) {
		return false
	}
	return true
}

// Finished returns finished jobs that match f, newest first.
func (h *History) Finished(f Filter) dash.JobsPage {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	var p dash.JobsPage
	h.mu.Lock()
	var all []dash.Job
	for _, j := range h.jobs {
		if j.State != dash.JobDone && j.State != dash.JobLost {
			continue
		}
		if f.match(j) {
			all = append(all, *j)
		}
		if f.Box.UID != "" && j.RanOn(f.Box) && (p.Last == nil || lastEnded(*j, *p.Last)) {
			last := *j
			p.Last = &last
		}
	}
	h.mu.Unlock()
	slices.SortFunc(all, newest)
	p.Jobs, p.More = all[:min(len(all), f.Limit)], len(all) > f.Limit
	if p.Jobs == nil {
		p.Jobs = []dash.Job{}
	}
	return p
}

// lastEnded says whether a ended after b, ties broken by key so the answer
// doesn't depend on map order.
func lastEnded(a, b dash.Job) bool {
	ea, eb := endOf(a), endOf(b)
	if !ea.Equal(eb) {
		return ea.After(eb)
	}
	return a.Key > b.Key
}

func endOf(j dash.Job) time.Time {
	if j.EndedAt != nil {
		return *j.EndedAt
	}
	return j.StartedAt
}

// repoShort is a repo's last path element without .git, as the page names it.
func repoShort(url string) string {
	url = strings.TrimSuffix(strings.TrimSuffix(url, "/"), ".git")
	return url[strings.LastIndex(url, "/")+1:]
}

// Live is what the watcher knows now, for the history's last bucket.
type Live struct {
	Mode   modeRec
	MemCap int64
}

// Range builds GET /api/history: 24 hourly buckets or 7 daily ones ending
// with the one holding now.
func (h *History) Range(rng string, live Live) dash.History {
	now := h.now().UTC()
	step, n := time.Hour, 24
	start := now.Truncate(time.Hour).Add(-23 * time.Hour)
	if rng == dash.RangeWeek {
		step, n = 24*time.Hour, 7
		start = now.Truncate(24 * time.Hour).Add(-6 * 24 * time.Hour)
	}
	out := dash.History{Range: rng, Buckets: make([]dash.Bucket, n), Segments: []dash.Segment{}}
	durs := make([]map[string][]float64, n)
	for i := range out.Buckets {
		out.Buckets[i] = dash.Bucket{
			Start: start.Add(time.Duration(i) * step),
			Jobs: dash.JobCounts{ByResult: map[string]int{}, ByKind: map[string]int{}, ByRepo: map[string]int{},
				ByAgent: map[string]int{}},
			BoxHours:      dash.BoxHours{BySize: map[string]float64{}},
			Refusals:      dash.RefusalCounts{ByWhy: map[string]int{}},
			MedianSeconds: dash.Medians{ByRepo: map[string]float64{}},
		}
		durs[i] = map[string][]float64{}
	}
	at := func(t time.Time) int {
		if t.Before(start) {
			return -1
		}
		if i := int(t.Sub(start) / step); i < n {
			return i
		}
		return -1
	}

	h.mu.Lock()
	for _, j := range h.jobs {
		i := at(j.StartedAt)
		if i < 0 {
			continue
		}
		b := &out.Buckets[i]
		if r := j.Result(); r != "" {
			b.Jobs.ByResult[r]++
		}
		b.Jobs.ByKind[j.Kind]++
		b.Jobs.ByRepo[repoShort(j.Repo)]++
		b.Jobs.ByAgent[j.Agent.Session]++
		if j.State == dash.JobDone && j.EndedAt != nil {
			r := repoShort(j.Repo)
			durs[i][r] = append(durs[i][r], j.EndedAt.Sub(j.StartedAt).Seconds())
		}
	}
	for _, s := range h.boxes {
		if unsetTime(s.start) {
			continue
		}
		to := now
		if s.end != nil {
			to = *s.end
		}
		for i := range out.Buckets {
			bs := out.Buckets[i].Start
			lo, hi := later(s.start, bs), earlier(to, bs.Add(step))
			if hi.After(lo) {
				out.Buckets[i].BoxHours.BySize[s.rec.Size] = round2(out.Buckets[i].BoxHours.BySize[s.rec.Size] + hi.Sub(lo).Hours())
			}
		}
	}
	for _, r := range h.refusals {
		if i := at(r.at); i >= 0 {
			out.Buckets[i].Refusals.ByWhy[r.why]++
		}
	}
	for _, t := range h.queueAdd {
		if i := at(t); i >= 0 {
			out.Buckets[i].Queued.Jobs++
		}
	}
	waits := make([][]float64, n)
	for _, wt := range h.waits {
		if i := at(wt.at); i >= 0 {
			waits[i] = append(waits[i], wt.seconds)
		}
	}
	for i, ws := range waits {
		if len(ws) > 0 {
			out.Buckets[i].Queued.Started, out.Buckets[i].Queued.MedianWaitSeconds = len(ws), median(ws)
		}
	}
	out.Segments = segments(h.modes, start, now, live.Mode)
	h.mu.Unlock()

	for i, byRepo := range durs {
		for r, ds := range byRepo {
			out.Buckets[i].MedianSeconds.ByRepo[r] = median(ds)
		}
	}
	h.fillMemory(out.Buckets, step, live.MemCap)
	return out
}

// segments is the mode timeline from start to now: the mode in force at
// start, then each change.
func segments(modes []modeAt, start, now time.Time, cur modeRec) []dash.Segment {
	var segs []dash.Segment
	open := func(at time.Time, m modeRec) {
		if n := len(segs); n > 0 {
			if segs[n-1].Mode == m.Mode {
				return
			}
			segs[n-1].End = at
		}
		segs = append(segs, dash.Segment{Start: at, End: now, Mode: m.Mode})
	}
	for i, m := range modes {
		if m.at.After(now) {
			break
		}
		if !m.at.After(start) {
			if i+1 == len(modes) || modes[i+1].at.After(start) {
				open(start, m.modeRec)
			}
			continue
		}
		open(m.at, m.modeRec)
	}
	if len(segs) == 0 && cur.Mode != "" {
		open(start, cur)
	}
	if segs == nil {
		segs = []dash.Segment{}
	}
	return segs
}

// fillMemory sets each bucket's MemUsed (the mean of the machine's minute
// samples) and MemCap (the largest cap sampled, else the cap now).
func (h *History) fillMemory(bs []dash.Bucket, step time.Duration, capNow int64) {
	if len(bs) == 0 {
		return
	}
	today := h.now().UTC().Format("2006-01-02")
	from, to := bs[0].Start, bs[len(bs)-1].Start.Add(step)
	sums := make([]memAgg, len(bs))
	for d := from.Truncate(24 * time.Hour); d.Before(to); d = d.Add(24 * time.Hour) {
		date := d.Format("2006-01-02")
		hours := h.memDay(date, date != today)
		for hr, a := range hours {
			t := d.Add(time.Duration(hr) * time.Hour)
			if t.Before(from) || !t.Before(to) {
				continue
			}
			i := int(t.Sub(from) / step)
			sums[i].sum += a.sum
			sums[i].n += a.n
			sums[i].cap = max(sums[i].cap, a.cap)
		}
	}
	for i := range bs {
		if sums[i].n > 0 {
			bs[i].MemUsed = sums[i].sum / sums[i].n
		}
		bs[i].MemCap = sums[i].cap
		if bs[i].MemCap == 0 {
			bs[i].MemCap = capNow
		}
	}
}

// memDay reads a day's machine samples into 24 hourly sums; a finished day
// is cached.
func (h *History) memDay(date string, done bool) []memAgg {
	h.mu.Lock()
	if a, ok := h.memDays[date]; ok {
		h.mu.Unlock()
		return a
	}
	h.mu.Unlock()
	a := make([]memAgg, 24)
	eachLine(filepath.Join(h.dir, "samples-"+date+".jsonl"), func(b []byte) {
		if !bytes.Contains(b, []byte(`"machine"`)) {
			return
		}
		var s sample
		if json.Unmarshal(b, &s) == nil && s.V == 1 && s.Kind == "machine" {
			hr := s.At.UTC().Hour()
			a[hr].sum += s.MemUsed
			a[hr].n++
			a[hr].cap = max(a[hr].cap, s.MemCap)
		}
	})
	if done {
		h.mu.Lock()
		h.memDays[date] = a
		h.mu.Unlock()
	}
	return a
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

// JobByID finds the newest recorded job of a box name and ID.
func (h *History) JobByID(box, id string) (dash.Job, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var best *dash.Job
	for _, j := range h.jobs {
		if j.Box == box && j.ID == id && (best == nil || j.StartedAt.After(best.StartedAt)) {
			best = j
		}
	}
	if best == nil {
		return dash.Job{}, false
	}
	j := *best
	if ev, ok := h.evicted[box]; ok && ev.at.After(j.StartedAt) {
		j.Evicted = ev.job
	}
	return j, true
}

// BoxRepo returns the repo of the newest recorded job on the box created at
// created, or "".
func (h *History) BoxRepo(box string, created time.Time) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var best *dash.Job
	for _, j := range h.jobs {
		if j.Box == box && j.Repo != "" && holds(created, j.StartedAt) && (best == nil || j.StartedAt.After(best.StartedAt)) {
			best = j
		}
	}
	if best == nil {
		return ""
	}
	return best.Repo
}
