package boxd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/jobrun"
	"github.com/a-Fig/agent-box/internal/mode"
)

// The scheduler and the starter. Each poll, boxd works out what every
// waiting job waits for, and starts at most one: the oldest job of the
// session with the fewest jobs running, among those that can start now
// (docs/queue.md). A start launches or reuses the box and starts the job as
// box run does (internal/jobrun), so it then looks like any other job.

const (
	workRoot  = "/work"
	idleTTL   = 2 * time.Hour  // a box launched for a job, as box new gives it
	jobTTL    = 24 * time.Hour // while a job runs, as box run sets it
	startTime = 10 * time.Minute
)

var errNoRoom = errors.New("no room")

// sizeOf is a waiting job's size.
func (it *queued) sizeOf() mode.Size {
	if sz, ok := mode.LookupSize(it.Size); ok {
		return sz
	}
	sz, _ := mode.LookupSize(mode.DefaultSize)
	return sz
}

// waitsFor says what a job waits for now, or "" when it can start. A job
// that can start only once kept throwaways are deleted gets their names in
// it.evict; the starter deletes them first.
func (w *Watcher) waitsFor(it *queued, byName map[string]Instance) string {
	it.evict = nil
	why := w.roomFor(it, byName, w.machine.Room)
	if why != dash.RefusedFull && why != dash.RefusedGPUBusy {
		return why
	}
	spare := w.spareThrowaways(w.lastIns, w.Now().UTC())
	for k := 1; k <= len(spare); k++ {
		gone := spare[:k]
		rest := slices.DeleteFunc(slices.Clone(w.lastIns), func(in Instance) bool {
			return slices.ContainsFunc(gone, func(g Instance) bool { return g.Name == in.Name })
		})
		_, _, room := allot(w.lastProj.Config, w.lastProj, rest)
		if w.roomFor(it, byName, room) == "" {
			for _, g := range gone {
				it.evict = append(it.evict, g.Name)
			}
			return ""
		}
	}
	return why
}

// spareThrowaways lists the throwaway boxes kept after their job ended
// (jobrun.EndedKeep) with nothing running in them, the first to expire
// first. A kept box is there for box pull and the full log; a queued job
// that needs its room comes first, as box's own claimGPU deletes an idle GPU
// throwaway. A box launched for a job that hasn't started yet isn't one of
// these: no job has ended there, and its launch expiry is 2h unless box run
// was given a shorter --ttl.
func (w *Watcher) spareThrowaways(ins []Instance, now time.Time) []Instance {
	busy := map[string]bool{}
	for _, j := range w.live {
		busy[j.Box] = true
	}
	var out []Instance
	for _, in := range ins {
		exp, err := time.Parse(time.RFC3339, in.Config[jobrun.ExpiresKey])
		if in.Config[keyThrowaway] != "true" || busy[in.Name] || in.Status == "Frozen" || err != nil ||
			exp.After(now.Add(jobrun.EndedKeep+time.Minute)) || w.Hist == nil || len(w.Hist.EndedOn(in.Name, in.CreatedAt)) == 0 {
			continue
		}
		out = append(out, in)
	}
	slices.SortFunc(out, func(a, b Instance) int {
		ta, _ := time.Parse(time.RFC3339, a.Config[jobrun.ExpiresKey])
		tb, _ := time.Parse(time.RFC3339, b.Config[jobrun.ExpiresKey])
		return ta.Compare(tb)
	})
	return out
}

// roomFor says what a job waits for with the given room, or "" when it can start.
func (w *Watcher) roomFor(it *queued, byName map[string]Instance, room dash.Room) string {
	m := w.machine
	switch {
	case m.Yielding:
		return dash.RefusedYielding
	case m.Mode == dash.ModeMyPC:
		return dash.RefusedMyPC
	}
	// A box of a job whose start was cut short is the job's own, even a
	// throwaway: startQueued looks in it before starting again.
	if in, ok := byName[it.Box]; ok && (!it.New || w.Queue.startMark(it) != nil) {
		switch in.Status {
		case "Running":
			return ""
		case "Frozen":
			return dash.RefusedMyPC
		}
		return dash.RefusedFull // stopped: starting it takes room, which box run leaves alone too
	}
	if _, ok := byName[it.Box]; ok {
		return dash.RefusedFull // a throwaway's name is taken; it waits for a fresh one
	}
	sz := it.sizeOf()
	var fits int
	switch sz.Name {
	case "small":
		fits = room.Small
	case "medium":
		fits = room.Medium
	case "large":
		fits = room.Large
	case mode.GPUSize:
		if room.GPU < 1 {
			return dash.RefusedGPUBusy
		}
		fits = 1
	}
	if fits < 1 {
		return dash.RefusedFull
	}
	return ""
}

// noRoomBackoff is how long a job waits after Incus refused its launch for
// room that boxd's own count said was there.
const noRoomBackoff = 30 * time.Second

// schedule runs at the end of each poll, with w.mu held. now is when the
// poll began: a start that finished after it ran on room this poll's data
// doesn't show yet, so nothing starts until the next poll.
func (w *Watcher) schedule(now time.Time, ins []Instance) {
	if w.Queue == nil {
		return
	}
	stale := w.startDone.After(now)
	byName := map[string]Instance{}
	for _, in := range ins {
		byName[in.Name] = in
	}
	running := map[string]int{}
	for _, j := range w.live {
		running[j.Agent.Session]++
	}
	var pick *queued
	pos := 0
	for _, it := range w.Queue.list() {
		if now.Sub(it.QueuedAt) > dash.QueueMaxAge && !w.Queue.isStarting(it) && !w.mayHaveStarted(it, byName, true) {
			w.dropQueued(it, now, "waited over 24 hours")
			continue
		}
		pos++
		j := it.view
		j.Position, j.Why = pos, w.waitsFor(it, byName)
		if w.Queue.isStarting(it) {
			j.Why = ""
		}
		if old, ok := w.qview[it.Job]; !ok || old.Position != j.Position || old.Why != j.Why {
			w.qview[it.Job] = j
			w.emit(dash.Event{Name: dash.EventJob, Data: j})
		}
		if j.Why == "" && w.starting == "" && !stale && !now.Before(w.noRoom[it.Job]) &&
			(pick == nil || running[j.Agent.Session] < running[pick.view.Agent.Session]) {
			pick = it
		}
	}
	if pick == nil || w.machine.Mode == "" && !w.machine.Reachable {
		return
	}
	_, exists := byName[pick.Box]
	w.starting = pick.Job
	w.Queue.setStarting(pick, true)
	go w.startQueued(pick, exists && (!pick.New || w.Queue.startMark(pick) != nil), slices.Clone(pick.evict))
}

// mayHaveStarted says whether a start that a restart cut short may have
// started the job: it came back from disk marked, no start has looked in its
// box since, and the box is there, or boxd hasn't listed the boxes yet (known
// is false). Until a start looks, the job isn't cancelled or dropped as if it
// were only waiting. A box that never runs again is reaped at its expiry,
// which ends this. A start that fails within one boxd isn't one: it found the
// job absent, as box run would.
func (w *Watcher) mayHaveStarted(it *queued, byName map[string]Instance, known bool) bool {
	if !w.Queue.wasRestored(it) {
		return false
	}
	_, ok := byName[it.Box]
	return ok || !known
}

// QueuedView is a waiting job with its place and wait, if it is waiting.
func (w *Watcher) QueuedView(job string) (dash.Job, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.Queue.get(job); !ok {
		j, ok := w.started[job]
		return j, ok
	}
	j, ok := w.qview[job]
	if !ok {
		it, _ := w.Queue.get(job)
		j = it.view
	}
	return j, true
}

// Queued lists the waiting jobs in order.
func (w *Watcher) Queued() []dash.Job {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.queuedJobs()
}

func (w *Watcher) queuedJobs() []dash.Job {
	out := []dash.Job{}
	if w.Queue == nil {
		return out
	}
	for i, it := range w.Queue.list() {
		j, ok := w.qview[it.Job]
		if !ok {
			j = it.view
			j.Position = i + 1
		}
		out = append(out, j)
	}
	return out
}

// Enqueued records a job just added and works out its place and wait.
func (w *Watcher) Enqueued(it *queued) dash.QueueReply {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.init()
	w.record(record{Type: recQueueAdd, At: it.QueuedAt, Job: &it.view})
	if _, ok := w.Queue.get(it.Job); !ok { // a poll started it already
		return dash.QueueReply{Job: it.Job, Key: it.view.Key, Position: 0}
	}
	byName := map[string]Instance{}
	for _, in := range w.lastIns {
		byName[in.Name] = in
	}
	list := w.Queue.list()
	j := it.view
	for i, o := range list {
		if o == it {
			j.Position = i + 1
		}
	}
	j.Why = w.waitsFor(it, byName)
	w.qview[it.Job] = j
	w.emit(dash.Event{Name: dash.EventJob, Data: j})
	return dash.QueueReply{Job: it.Job, Key: j.Key, Position: j.Position, Why: j.Why}
}

// CancelQueued drops a waiting job; false when it isn't waiting (or is
// being started right now, or may have started before a restart).
func (w *Watcher) CancelQueued(job, by string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	it, ok := w.Queue.get(job)
	if !ok || w.Queue.isStarting(it) {
		return false
	}
	byName := map[string]Instance{}
	for _, in := range w.lastIns {
		byName[in.Name] = in
	}
	if w.mayHaveStarted(it, byName, w.listed) {
		return false
	}
	w.Queue.Remove(job)
	delete(w.qview, job)
	w.record(record{Type: recQueueCancel, Job: &it.view, Control: &controlRec{Action: "cancel", Target: job, By: by}})
	w.emit(dash.Event{Name: dash.EventJobGone, Data: dash.JobGone{Key: it.view.Key}})
	return true
}

// FrontQueued moves a waiting job to the front.
func (w *Watcher) FrontQueued(job, by string) (dash.Job, bool) {
	if !w.Queue.Front(job) {
		return dash.Job{}, false
	}
	w.mu.Lock()
	w.record(record{Type: recControl, Control: &controlRec{Action: "front", Target: job, By: by}})
	jobs := w.queuedJobs()
	w.mu.Unlock()
	for _, j := range jobs {
		if j.Box+"."+j.ID == job {
			j.Position = 1 // the next poll announces the new order
			return j, true
		}
	}
	return dash.Job{}, false
}

// dropQueued removes a job that will never start, as lost. With w.mu held.
func (w *Watcher) dropQueued(it *queued, now time.Time, why string) {
	w.Queue.Remove(it.Job)
	delete(w.qview, it.Job)
	j := it.view
	j.State, j.EndedAt, j.Why = dash.JobLost, &now, ""
	w.logf("boxd: queue: dropping %s: %s", it.Job, why)
	w.record(record{Type: recQueueDrop, At: now, Job: &j})
	w.record(record{Type: recJobEnd, At: now, Job: &j})
	w.emit(dash.Event{Name: dash.EventJob, Data: j})
}

// startQueued starts a job and settles the queue after.
func (w *Watcher) startQueued(it *queued, reuse bool, evict []string) {
	ctx, cancel := context.WithTimeout(context.Background(), startTime)
	defer cancel()
	var err error
	for _, box := range evict {
		if _, err = w.Incus.CLI(ctx, "", "delete", box, "--force"); err != nil {
			err = fmt.Errorf("%w: deleting kept %s for room: %v", errNoRoom, box, err)
			break
		}
		w.logf("boxd: queue: deleted kept throwaway %s for %s's room", box, it.Job)
	}
	started := w.Now().UTC()
	mark := w.Queue.startMark(it)
	if err == nil {
		// Whatever comes of it, this start looks in the box (or found none).
		defer w.Queue.looked(it)
	}
	switch {
	case err != nil:
	case mark != nil && reuse && w.startedInBox(ctx, it):
		// A boxd before this one started it and stopped before it could
		// say so: the job runs, or ran, already.
		started = *mark
		w.logf("boxd: queue: %s had started before boxd restarted", it.Job)
	default:
		if err = w.Queue.markStarting(it, started); err == nil {
			err = w.startJob(ctx, it, reuse, started)
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.starting = ""
	w.startDone = w.Now().UTC()
	w.Queue.setStarting(it, false)
	switch {
	case errors.Is(err, errNoRoom):
		w.logf("boxd: queue: %s: no room yet: %v", it.Job, err) // it waits on
		w.noRoom[it.Job] = w.startDone.Add(noRoomBackoff)
	case err != nil && (w.machine.Yielding || w.machine.Mode == dash.ModeMyPC):
		// A game or My PC froze the box mid-start: wait for it to end.
		w.logf("boxd: queue: %s: paused mid-start: %v", it.Job, err)
	case err != nil:
		w.dropQueued(it, w.Now().UTC(), err.Error())
	default:
		w.Queue.Remove(it.Job)
		delete(w.qview, it.Job)
		delete(w.noRoom, it.Job)
		j := it.view
		j.State, j.StartedAt, j.Why = dash.JobRunning, started, ""
		j.Key = dash.JobKey(j.Box, j.ID, started)
		w.started[it.Job] = j // GET /api/queue/{job} until a scan sees it
		w.record(record{Type: recQueueStart, At: started, Job: &j, Seconds: started.Sub(it.QueuedAt).Seconds()})
		// The job's key changes from its queued time to its start; the next
		// scan announces it running.
		w.emit(dash.Event{Name: dash.EventJobGone, Data: dash.JobGone{Key: it.view.Key}})
	}
}

// startedInBox says whether the job's files are in its box: its start got
// as far as the start script. Failing to look says no, as box run would
// start it.
func (w *Watcher) startedInBox(ctx context.Context, it *queued) bool {
	look, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	remote := workRoot + "/.box/" + it.Job
	_, err := w.Incus.Exec(look, it.Box, "test -e "+remote+".log -o -e "+remote+".pid -o -e "+remote+".exit")
	return err == nil
}

// startJob does what box run does once it has a box: launch one if needed,
// push the snapshot and the .meta, set the job's expiry and start it.
func (w *Watcher) startJob(ctx context.Context, it *queued, reuse bool, now time.Time) (err error) {
	box, sz := it.Box, it.sizeOf()
	if !reuse {
		flags, devices := jobrun.LaunchFlags(sz, now.Add(idleTTL), it.New)
		args := append([]string{"launch", w.image(), box}, flags...)
		if _, err := w.Incus.CLI(ctx, devices, args...); err != nil {
			if jobrun.LimitRefusal.MatchString(err.Error()) {
				return fmt.Errorf("%w: %v", errNoRoom, err)
			}
			return fmt.Errorf("launch %s: %w", box, err)
		}
		if it.New {
			defer func() {
				if err != nil {
					w.Incus.CLI(context.WithoutCancel(ctx), "", "delete", box, "--force")
				}
			}()
		}
	}
	remote := workRoot + "/.box/" + it.Job
	if _, err := w.Incus.CLI(ctx, "", "file", "push", "--create-dirs", w.Queue.BundlePath(it.Job), box+remote+".bundle"); err != nil {
		return fmt.Errorf("push the snapshot: %w", err)
	}
	m := map[string]any{}
	if json.Unmarshal(it.Meta, &m) != nil || m == nil {
		m = map[string]any{} // check accepts objects only; a file from before may hold null
	}
	m["v"], m["queuedAt"], m["startedAt"] = 1, it.QueuedAt.Format(time.RFC3339), now.Format(time.RFC3339)
	b, _ := json.Marshal(m)
	tmp := filepath.Join(w.Queue.dir, it.Job+".meta.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	defer os.Remove(tmp)
	if _, err := w.Incus.CLI(ctx, "", "file", "push", "--create-dirs", tmp, box+remote+".meta"); err != nil {
		return fmt.Errorf("push the .meta: %w", err)
	}
	// The tray sets a new box's CPU limits within a tick of its launch, just
	// now (jobrun.RetryETag).
	if err := jobrun.RetryETag(ctx, func() error {
		_, err := w.Incus.CLI(ctx, "", "config", "set", box, jobrun.ExpiresKey+"="+now.Add(jobTTL).Format(time.RFC3339))
		return err
	}); err != nil {
		return fmt.Errorf("set the expiry: %w", err)
	}
	payload := jobrun.Payload(it.Env, it.Command, time.Duration(it.Timeout)*time.Second)
	if _, err := w.Incus.CLI(ctx, "", "exec", box, "--", "bash", "-lc", jobrun.StartScript(workRoot, it.Job, it.Ref, it.Checkout, payload)); err != nil {
		// The job may have started and only the wait for its PID or the
		// stream failed, as box run allows for: then it runs, and is ours.
		check, cancel := context.WithTimeout(context.WithoutCancel(ctx), execTimeout)
		defer cancel()
		if _, perr := w.Incus.Exec(check, box, "test -e "+remote+".pid -o -e "+remote+".exit"); perr == nil {
			return nil
		}
		return fmt.Errorf("start: %w", err)
	}
	return nil
}

func (w *Watcher) image() string {
	if w.Image != "" {
		return w.Image
	}
	return "agent-base"
}

// reapQueuedThrowaway hands a throwaway box that boxd launched for a queued
// job to the reaper once that job has ended and nothing else runs there: no
// box run is following it to clean up. As box run does, it keeps the box for
// jobrun.EndedKeep first, so box pull and the full log still work. With w.mu
// held.
//
// A repo box it reused or launched gets box new's idle expiry back instead,
// as box run's markIdle does once a job ends.
func (w *Watcher) reapQueuedThrowaway(j *dash.Job) {
	if j.QueuedAt == nil {
		return
	}
	for _, o := range w.live {
		if o.Box == j.Box && o.Key != j.Key {
			return
		}
	}
	box, started := j.Box, j.StartedAt
	go func() {
		// Read the box now: the poll's own list may predate it.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		ins, err := w.Incus.Instances(ctx)
		if err != nil {
			return
		}
		for _, in := range ins {
			if in.Name != box || !holds(in.CreatedAt, started) {
				continue
			}
			ttl := idleTTL
			if in.Config[keyThrowaway] == "true" {
				ttl = jobrun.EndedKeep
			}
			read := in.Config[jobrun.ExpiresKey]
			first := true
			err = jobrun.RetryETag(ctx, func() error {
				// After a lost race, look again: if the expiry moved, a job
				// started there (box run sets 24h) and it is not ours to cut.
				if !first {
					again, err := w.Incus.Instances(ctx)
					if err != nil {
						return err
					}
					for _, cur := range again {
						if cur.Name == box && cur.Config[jobrun.ExpiresKey] != read {
							return nil
						}
					}
				}
				first = false
				_, err := w.Incus.CLI(ctx, "", "config", "set", box, jobrun.ExpiresKey+"="+w.Now().UTC().Add(ttl).Format(time.RFC3339))
				return err
			})
			if err != nil {
				w.logf("boxd: queue: after %s's job: %v", box, err)
			}
		}
	}()
}
