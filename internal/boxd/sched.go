package boxd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// HostConfig is Machine.Config: the size table and the expiry constants, and whether this host
// has a GPU for a GPU box.
func HostConfig(gpu bool) dash.Config {
	c := dash.Config{GPU: gpu, IdleExpiry: int(idleTTL / time.Second), JobExpiry: int(jobTTL / time.Second),
		KeptAfterJob: int(jobrun.EndedKeep / time.Second)}
	for _, s := range mode.Sizes {
		mem, _ := mode.ParseBytes(s.Memory)
		disk, _ := mode.ParseBytes(s.Disk)
		c.Sizes = append(c.Sizes, dash.SizeSpec{Name: s.Name, SharedCPUs: s.SharedCPUs, FullCPUs: s.FullCPUs, Memory: mem, Disk: disk, GPU: s.GPU})
	}
	return c
}

// sizeOf is a waiting job's size.
func (it *queued) sizeOf() mode.Size {
	if sz, ok := mode.LookupSize(it.Size); ok {
		return sz
	}
	sz, _ := mode.LookupSize(mode.DefaultSize)
	return sz
}

// waitsFor says what a job waits for now, or "" when it can start. A job
// that can start only once kept throwaways or idle repo boxes are deleted
// gets them in it.evict, the fewest that make room in the order spareBoxes
// lists them; the starter deletes them first.
func (w *Watcher) waitsFor(it *queued, byName map[string]Instance) string {
	it.evict = nil
	why := w.roomFor(it, byName, w.machine.Room)
	if why != dash.RefusedFull && why != dash.RefusedGPUBusy {
		return why
	}
	spare := w.spareBoxes(w.lastIns, w.Now().UTC())
	for k := 1; k <= len(spare); k++ {
		gone := spare[:k]
		rest := slices.DeleteFunc(slices.Clone(w.lastIns), func(in Instance) bool {
			return slices.ContainsFunc(gone, func(g Instance) bool { return g.Name == in.Name })
		})
		_, _, room := allot(w.lastProj.Config, withoutDisk(w.lastProj, gone), rest)
		if w.roomFor(it, byName, room) == "" {
			it.evict = slices.Clone(gone)
			return ""
		}
	}
	return why
}

// withoutDisk is proj with the disk of the gone boxes taken off its use, by
// size as box status counts it. allot takes disk from Incus's project total,
// which deleting boxes in a sum doesn't lower; without this a queued job
// short of disk never made room (#103).
func withoutDisk(proj Project, gone []Instance) Project {
	d, ok := proj.Resources["disk"]
	if !ok || d.Usage < 0 {
		return proj
	}
	for _, g := range gone {
		b, _ := mode.ParseBytes(mode.SizeOf(g.Config).Disk)
		d.Usage = max(d.Usage-b, 0)
	}
	proj.Resources = maps.Clone(proj.Resources)
	proj.Resources["disk"] = d
	return proj
}

// spareThrowaways lists the throwaway boxes kept after their job ended
// (jobrun.EndedKeep, from box or boxd's reapEnded) with nothing running in
// them, the first to expire first. A kept box is there for box pull and the full log; a queued job
// that needs its room comes first, as box's own claimGPU deletes an idle GPU
// throwaway. A box launched for a job that hasn't started yet isn't one of
// these: no job has ended there, and its launch expiry is 2h unless box run
// was given a shorter --ttl. Nor is one a person extended (mode.Held).
func (w *Watcher) spareThrowaways(ins []Instance, now time.Time) []Instance {
	var out []Instance
	for _, in := range ins {
		if in.Name != w.idleStopping && !w.boxBusy(in.Name, "") && keptThrowaway(in, now) && w.Hist != nil && w.Hist.EndedHere(in) {
			out = append(out, in)
		}
	}
	slices.SortFunc(out, func(a, b Instance) int {
		ta, _ := time.Parse(time.RFC3339, a.Config[jobrun.ExpiresKey])
		tb, _ := time.Parse(time.RFC3339, b.Config[jobrun.ExpiresKey])
		return ta.Compare(tb)
	})
	return out
}

func keptThrowaway(in Instance, now time.Time) bool {
	return keptThrowawayWindow(in, now) && !jobrun.ThrowawayClaimed(in.Config, now)
}

func keptThrowawayWindow(in Instance, now time.Time) bool {
	exp, err := time.Parse(time.RFC3339, in.Config[jobrun.ExpiresKey])
	return in.Config[jobrun.ThrowawayKey] == "true" && in.Status != "Frozen" && err == nil &&
		!exp.After(now.Add(jobrun.EndedKeep+time.Minute)) && !mode.Held(in.Config, now).After(now)
}

// claimThrowaway reserves an idle CPU throwaway for client-side job setup.
// The same conditional write that takes it out of the pool protects its
// expiry. A client that disappears leaves a claim that expires on its own.
func (w *Watcher) claimThrowaway(ctx context.Context, box, repoKey, size, job string) (bool, error) {
	return w.reserveThrowaway(ctx, box, job, func(in Instance) bool {
		return warmClaimable(in, w.Now()) && in.Config[jobrun.ThrowawayRepoKey] == repoKey &&
			mode.SizeOf(in.Config).Name == size
	})
}

func warmClaimable(in Instance, now time.Time) bool {
	expires, _ := time.Parse(time.RFC3339, in.Config[jobrun.ExpiresKey])
	return in.Status == "Running" && in.Config[jobrun.ThrowawayRepoKey] != "" && expires.After(now.Add(controlTimeout))
}

// A run and an eviction compete for the same Incus reservation. The empty
// owner on an eviction fences expiry writes from the previous job.
func (w *Watcher) reserveThrowaway(ctx context.Context, box, job string, matches func(Instance) bool) (bool, error) {
	return w.Incus.SetConfigIf(ctx, box, func(in Instance) map[string]string {
		now := w.Now().UTC()
		if !keptThrowaway(in, now) || !matches(in) {
			return nil
		}
		switch in.Status {
		case "Running":
			out, err := w.Incus.Exec(ctx, box, jobrun.KeptScript(workRoot))
			if err != nil || strings.TrimSpace(string(out)) != "kept" {
				return nil
			}
		case "Stopped":
			if w.Hist == nil || !w.Hist.EndedHere(in) {
				return nil
			}
		default:
			return nil
		}
		expires, _ := time.Parse(time.RFC3339, in.Config[jobrun.ExpiresKey])
		until := now.Add(jobrun.ThrowawayClaimTTL)
		if expires.Before(until) && (job != "" || in.Config[jobrun.ThrowawayRepoKey] != "") {
			expires = until
		}
		changes := map[string]string{
			jobrun.ExpiresKey:        expires.Format(time.RFC3339),
			jobrun.ThrowawayClaimKey: until.Format(time.RFC3339),
			jobrun.ThrowawayOwnerKey: job,
		}
		if job != "" && in.Config[jobrun.ThrowawayStopKey] == jobrun.ThrowawayStopped {
			changes[jobrun.ThrowawayStopKey] = ""
		}
		return changes
	})
}

func (w *Watcher) evictPooled(ctx context.Context, box, uid string) (bool, error) {
	return w.evictThrowaway(ctx, box, uid, true)
}

func (w *Watcher) evictThrowaway(ctx context.Context, box, uid string, pooled bool) (bool, error) {
	reserved, err := w.reserveThrowaway(ctx, box, "", func(in Instance) bool {
		w.mu.Lock()
		busy := w.idleStopping == box || w.boxBusy(box, "")
		w.mu.Unlock()
		return !busy && boxUID(in) == uid && (in.Config[jobrun.ThrowawayRepoKey] != "") == pooled
	})
	if err != nil || !reserved {
		return false, err
	}
	err = jobrun.RetryETag(ctx, func() error {
		ins, err := w.Incus.Instances(ctx)
		if err != nil {
			return err
		}
		for _, in := range ins {
			if in.Name == box {
				now := w.Now()
				if boxUID(in) != uid || in.Status == "Frozen" || mode.Held(in.Config, now).After(now) ||
					in.Config[jobrun.ThrowawayOwnerKey] != "" || jobrun.IdleStopReserved(in.Config) || !jobrun.ThrowawayClaimed(in.Config, now) {
					return errNotIdle
				}
				_, err := w.Incus.CLI(ctx, "", "delete", box, "--force")
				return err
			}
		}
		return nil
	})
	if errors.Is(err, errNotIdle) {
		return false, nil
	}
	return err == nil, err
}

// setPoolExpiry changes only the owner's deadline. A fast job may finish
// before the client confirms its start, so promotion also needs a live job.
func (w *Watcher) setPoolExpiry(ctx context.Context, box, job string, ended bool) (bool, error) {
	return w.Incus.SetConfigIf(ctx, box, func(in Instance) map[string]string {
		return w.poolExpiryConfig(ctx, in, job, ended)
	})
}

func (w *Watcher) poolExpiryConfig(ctx context.Context, in Instance, job string, ended bool) map[string]string {
	if in.Config[jobrun.ThrowawayRepoKey] == "" || in.Config[jobrun.ThrowawayOwnerKey] != job {
		return nil
	}
	if in.Status != "Running" && (in.Status != "Frozen" || ended) {
		return nil
	}
	now := w.Now().UTC()
	cur, err := time.Parse(time.RFC3339, in.Config[jobrun.ExpiresKey])
	lease := in.Config[jobrun.ThrowawayClaimKey] != ""
	ttl := jobTTL
	if ended {
		ttl = jobrun.EndedKeep
		if err == nil && !lease && !cur.After(now.Add(ttl)) {
			return nil
		}
	} else if !lease {
		return nil
	}
	if in.Status == "Frozen" {
		// StartScript writes the job's .repo before spawning its wrapper.
		// That file is readable while exec is blocked by the freeze.
		p := in.Name + workRoot + "/.box/" + job + ".repo"
		out, err := w.Incus.CLI(ctx, "", "file", "pull", p, "-")
		if err != nil || !validBoxName.MatchString(strings.TrimSpace(string(out))) {
			return nil
		}
	} else {
		out, err := w.Incus.Exec(ctx, in.Name, jobrun.RunningScript(workRoot))
		running := strings.Fields(string(out))
		if err != nil || (ended && len(running) != 0) || (!ended && !slices.Contains(running, job)) {
			return nil
		}
	}
	return map[string]string{
		jobrun.ExpiresKey:        mode.Held(in.Config, now.Add(ttl)).Format(time.RFC3339),
		jobrun.ThrowawayClaimKey: "",
	}
}

// idleRepoBoxes lists the repo boxes (not throwaways) idle since their last
// job, the longest idle first, for a queued job short of room (#187), as box's
// launch takes them. A repo box is a cache: deleting it costs the repo's next
// run a fresh checkout and a cold build cache. Idle is what spareThrowaways
// checks: running, no job running or being started there (boxBusy), one
// ended in boxd's history on this very box (its UID: EndedHere), and an
// expiry still the idle one set when its last job ended (idleTTL), not a
// later job's 24h. Never a held one (mode.Held):
// a person's extend, or box new --ttl. With w.mu held.
func (w *Watcher) idleRepoBoxes(ins []Instance, now time.Time) []Instance {
	var out []Instance
	for _, in := range ins {
		if !w.boxBusy(in.Name, "") && idleRepo(in, now) && w.Hist != nil && w.Hist.EndedHere(in) {
			out = append(out, in)
		}
	}
	slices.SortFunc(out, func(a, b Instance) int { // the expiry is the last end plus idleTTL
		ta, _ := time.Parse(time.RFC3339, a.Config[jobrun.ExpiresKey])
		tb, _ := time.Parse(time.RFC3339, b.Config[jobrun.ExpiresKey])
		return ta.Compare(tb)
	})
	return out
}

// idleRepo says whether in's config is an idle repo box's: running, not a
// throwaway, its expiry still the idle one and not held.
func idleRepo(in Instance, now time.Time) bool {
	exp, err := time.Parse(time.RFC3339, in.Config[jobrun.ExpiresKey])
	return in.Config[jobrun.ThrowawayKey] != "true" && in.Status == "Running" && err == nil &&
		!exp.After(now.Add(idleTTL+time.Minute)) && !mode.Held(in.Config, now).After(now)
}

// spareBoxes is what a queued job short of room may delete, in the order it
// takes them: kept throwaways, then idle repo boxes. With w.mu held.
func (w *Watcher) spareBoxes(ins []Instance, now time.Time) []Instance {
	return append(w.spareThrowaways(ins, now), w.idleRepoBoxes(ins, now)...)
}

// roomFor says what a job waits for with the given room, or "" when it can start.
func (w *Watcher) roomFor(it *queued, byName map[string]Instance, room dash.Room) string {
	if w.machine.Mode == dash.ModeMyPC {
		return dash.RefusedMyPC
	}
	// A box of a job whose start was cut short is the job's own, even a
	// throwaway: startQueued looks in it before starting again.
	if in, ok := byName[it.Box]; ok && (!it.New || w.Queue.startMark(it) != nil) {
		if in.Name == w.idleStopping || jobrun.IdleStopReserved(in.Config) {
			return dash.RefusedFull
		}
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
	in, exists := byName[pick.Box]
	reuse := exists && (!pick.New || w.Queue.startMark(pick) != nil)
	sz := pick.sizeOf()
	if reuse {
		sz = mode.SizeOf(in.Config)
	}
	w.starting = pick.Job
	w.Queue.setStarting(pick, true)
	go w.startQueued(pick, reuse, sz, slices.Clone(pick.evict))
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

// errNotIdle means the selected box is no longer safe to evict.
var errNotIdle = errors.New("no longer idle")

// stillIdle checks for running jobs, then reads the box again to verify its
// UID, status, expiry and hold before each delete attempt.
func (w *Watcher) stillIdle(ctx context.Context, in Instance) bool {
	noRunningJobs := false
	if in.Status == "Running" {
		out, err := w.Incus.Exec(ctx, in.Name, jobrun.RunningScript(workRoot))
		noRunningJobs = err == nil && strings.TrimSpace(string(out)) == ""
	}
	ins, err := w.Incus.Instances(ctx)
	if err != nil {
		return false
	}
	for _, current := range ins {
		if current.Name == in.Name {
			if boxUID(current) != boxUID(in) || (current.Status == "Running" && !noRunningJobs) {
				return false
			}
			if in.Config[jobrun.ThrowawayKey] == "true" {
				return keptThrowaway(current, w.Now())
			}
			return idleRepo(current, w.Now())
		}
	}
	return false
}

// startQueued starts a job and settles the queue after.
func (w *Watcher) startQueued(it *queued, reuse bool, sz mode.Size, evict []Instance) {
	ctx, cancel := context.WithTimeout(context.Background(), startTime)
	defer cancel()
	var err error
	for _, in := range evict {
		box, what := in.Name, "kept throwaway"
		if in.Config[jobrun.ThrowawayKey] != "true" {
			what = "idle box"
		}
		if in.Config[jobrun.ThrowawayKey] == "true" {
			var evicted bool
			evicted, err = w.evictThrowaway(ctx, box, boxUID(in), in.Config[jobrun.ThrowawayRepoKey] != "")
			if err == nil && !evicted {
				err = errNotIdle
			}
		} else {
			err = jobrun.RetryETag(ctx, func() error {
				if !w.stillIdle(ctx, in) {
					return errNotIdle
				}
				_, err := w.Incus.CLI(ctx, "", "delete", box, "--force")
				return err
			})
		}
		if errors.Is(err, errNotIdle) {
			err = fmt.Errorf("%w: %s is no longer idle", errNoRoom, box)
			break
		}
		if err != nil {
			err = fmt.Errorf("%w: deleting %s %s for room: %v", errNoRoom, what, box, err)
			break
		}
		w.logf("boxd: queue: deleted %s %s for %s's room", what, box, it.Job)
		w.record(record{Type: recControl, Control: &controlRec{Action: actionEvict, Target: box, By: it.Job}})
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
			err = w.startJob(ctx, it, reuse, sz, started)
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
	case err != nil && w.machine.Mode == dash.ModeMyPC:
		// My PC froze the box mid-start: wait for it to end.
		w.logf("boxd: queue: %s: paused mid-start: %v", it.Job, err)
	case err != nil:
		w.dropQueued(it, w.Now().UTC(), err.Error())
	default:
		w.Queue.Remove(it.Job)
		delete(w.qview, it.Job)
		delete(w.noRoom, it.Job)
		j := it.view
		j.State, j.StartedAt, j.Why = dash.JobRunning, started, ""
		j.Size, j.GPU = sz.Name, sz.GPU
		j.Kind = dash.Classify(j.Command, j.GPU)
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
func (w *Watcher) startJob(ctx context.Context, it *queued, reuse bool, sz mode.Size, now time.Time) (err error) {
	box := it.Box
	if !reuse {
		flags, devices := jobrun.LaunchFlags(sz, now.Add(idleTTL), it.New)
		args := append([]string{"launch", "agent-base", box}, flags...)
		if _, err := w.Incus.CLI(ctx, devices, args...); err != nil {
			if jobrun.LimitRefusal.MatchString(err.Error()) {
				return fmt.Errorf("%w: %v", errNoRoom, err)
			}
			return fmt.Errorf("launch %s: %w", box, err)
		}
		if it.New {
			defer func() {
				if err != nil {
					w.delete(context.WithoutCancel(ctx), box)
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
	m["size"], m["gpu"] = sz.Name, sz.GPU
	b, _ := json.Marshal(m)
	tmp := filepath.Join(w.Queue.dir, it.Job+".meta.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	defer os.Remove(tmp)
	if _, err := w.Incus.CLI(ctx, "", "file", "push", "--create-dirs", tmp, box+remote+".meta"); err != nil {
		return fmt.Errorf("push the .meta: %w", err)
	}
	// The job's 24h, or the box's hold if later. SetConfigIf retries the ETag
	// race with the tray, which sets a new box's CPU limits within a tick of
	// its launch, just now.
	if _, err := w.Incus.SetConfigIf(ctx, box, func(in Instance) map[string]string {
		return map[string]string{jobrun.ExpiresKey: mode.Held(in.Config, now.Add(jobTTL)).UTC().Format(time.RFC3339)}
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

// reapEnded gives a box whose job has ended, with nothing else running
// there, box new's idle expiry, idleTTL. box shortens the expiry only when it
// reads a job's exit (markIdle), so a job nobody read (push false and no
// box_job, a watcher gone, a box run left detached) kept its 24h job expiry
// (#53). A throwaway gets jobrun.EndedKeep, as box gives it, so a launch
// short of room can take it (spareThrowaways, box's keptThrowaways): its exit
// and tail are in boxd's history, which box job reads once the box is gone
// (#90). A repo box gets idleTTL.
// With w.mu held.
//
// Each try of the write (SetConfigIf) reads the box, then checks, as
// markIdle does, that no other job runs or is being started there, then
// writes with the box's ETag. A job that starts there sets its 24h after its
// PID file exists: before the check, the check sees it; after it, the write
// fails on its ETag and is decided again, or comes after ours and wins.
func (w *Watcher) reapEnded(j *dash.Job) {
	box, own := j.Box, j.Box+"."+j.ID
	job := *j
	if w.boxBusy(box, own) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, err := w.Incus.SetConfigIf(ctx, box, func(in Instance) map[string]string {
			now := w.Now().UTC()
			if in.Config[jobrun.ThrowawayRepoKey] != "" {
				if ranOnInstance(job, in) {
					return w.poolExpiryConfig(ctx, in, own, true)
				}
				return nil
			}
			until, ok := endedExpiry(in, job, now)
			if !ok || in.Status != "Running" || jobrun.ThrowawayClaimed(in.Config, now) {
				return nil
			}
			w.mu.Lock()
			busy := w.boxBusy(box, own)
			w.mu.Unlock()
			if busy {
				return nil
			}
			out, err := w.Incus.Exec(ctx, box, jobrun.RunningScript(workRoot))
			if err != nil {
				return nil
			}
			for _, name := range strings.Fields(string(out)) {
				if name != own {
					return nil // another job runs there
				}
			}
			return map[string]string{jobrun.ExpiresKey: until.Format(time.RFC3339)}
		})
		if err != nil {
			w.logf("boxd: after %s's job: %v", box, err)
		}
	}()
}

// sweepEnded catches, once for each box, on the first poll that scans it
// running (a box frozen when boxd starts waits for its thaw), the reapEnded
// that settle missed: a box whose jobs had all ended in boxd's history (the scan skipped
// every one), with none of them live, whose expiry is still its last job's
// own 24h. A boxd restarted while the job ended, or before reapEnded's write,
// left a throwaway that 24h (#156). An expiry shortened already, or extended
// from the page since, is no longer the job's, so the sweep leaves it;
// reapEnded does the rest as it would. Once, so a late extend that lands
// near the job's 24h isn't undone. With w.mu held.
func (w *Watcher) sweepEnded(now time.Time, ins []Instance, scans map[string][]scanned) {
	seen := make(map[string]bool, len(ins))
	for _, in := range ins {
		key := in.Name + "@" + in.CreatedAt.UTC().Format(time.RFC3339Nano)
		jobs, scanned := scans[in.Name]
		if w.swept[key] {
			seen[key] = true
			continue
		}
		if in.Status != "Running" || !scanned {
			continue // swept once it is
		}
		seen[key] = true
		if len(jobs) == 0 || w.boxBusy(in.Name, "") {
			continue
		}
		ended := true
		for _, s := range jobs {
			if !s.Skipped || s.State == "running" || s.State == "starting" {
				ended = false // one boxd hasn't recorded as ended goes through settle
				break
			}
		}
		if !ended {
			continue
		}
		last, ok := w.Hist.LastEnded(in)
		if !ok {
			continue
		}
		cur, err := time.Parse(time.RFC3339, in.Config[jobrun.ExpiresKey])
		if err != nil || cur.Before(last.StartedAt.Add(jobTTL-jobExpirySlack)) {
			continue // not the job's 24h: shortened already, or extended from the page since
		}
		if _, ok := endedExpiry(in, last, now); ok {
			w.reapEnded(&last)
		}
	}
	w.swept = seen
}

// boxBusy says whether a job other than own runs, or is being started, in
// box, as far as boxd knows. With w.mu held.
func (w *Watcher) boxBusy(box, own string) bool {
	if w.Queue != nil {
		for _, j := range w.Queue.StartingFor(box) {
			if j.Box+"."+j.ID != own {
				return true
			}
		}
	}
	for _, o := range w.live {
		if o.Box == box && o.Box+"."+o.ID != own {
			return true
		}
	}
	if b, _, _ := strings.Cut(w.starting, "."); w.starting != "" && b == box && w.starting != own {
		return true
	}
	for name, sj := range w.started {
		if sj.Box == box && name != own {
			return true
		}
	}
	return false
}

// jobExpirySlack is how far past its start plus jobTTL a job's own expiry
// may be: box run sets it a few seconds after the job's ID, and again once
// the job runs.
const jobExpirySlack = 10 * time.Minute

// endedExpiry is the expiry a box gets once the job that started at started
// has ended, and whether to write it: only to shorten an expiry no later
// than that job's own (a later job's 24h, or one set by hand, is left alone),
// on the job's box (ranOnInstance), and never past its hold (mode.Held).
func endedExpiry(in Instance, j dash.Job, now time.Time) (time.Time, bool) {
	started := j.StartedAt
	if !ranOnInstance(j, in) {
		return time.Time{}, false
	}
	cur, err := time.Parse(time.RFC3339, in.Config[jobrun.ExpiresKey])
	if err != nil || cur.After(started.Add(jobTTL+jobExpirySlack)) {
		return time.Time{}, false // not this job's expiry: a later job's, or set by hand
	}
	ttl := idleTTL
	if in.Config[jobrun.ThrowawayKey] == "true" {
		ttl = jobrun.EndedKeep // its result is in boxd's history too
	}
	until := mode.Held(in.Config, now.Add(ttl).Truncate(time.Second))
	if !cur.After(until) {
		return time.Time{}, false // already as short: box read the exit, or an extend
	}
	return until, true
}
