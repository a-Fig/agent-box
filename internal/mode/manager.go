package mode

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ExpiresKey is the instance config key `box` sets to an RFC3339 UTC time after
// which the instance is deleted.
const ExpiresKey = "user.expires"

// HoldKey is the RFC3339 UTC time a person asked a box to live until: an
// extend from the dashboard, or box new --ttl. Only those write it. Whatever
// sets ExpiresKey writes Held of its own deadline, the reapers delete a box
// only once both times have passed, and nothing takes a held box for room.
// Kept apart from ExpiresKey so a job's end can't mistake an extend for the
// job's own deadline (#198), as Kubernetes keeps a finalizer apart from the
// scheduler's state.
const HoldKey = "user.box.hold-until"

// Held is the later of t and the hold in an instance's config. A box is held
// at now when Held(cfg, now).After(now).
func Held(cfg map[string]string, t time.Time) time.Time {
	if h, err := time.Parse(time.RFC3339, cfg[HoldKey]); err == nil && h.After(t) {
		return h
	}
	return t
}

const (
	// FrozenStop is how long an instance may stay frozen in My PC before it is
	// stopped to free its RAM.
	FrozenStop = 15 * time.Minute
	// FrozenSwappedStop replaces FrozenStop for a CPU box whose reclaim ran
	// and left it holding ReclaimLeft or less: frozen and in swap, it costs
	// Windows next to no RAM, so its job may wait for Tyler much longer.
	FrozenSwappedStop = 4 * time.Hour
	// ProjectRecheck is how often the mode label is read back (and
	// rewritten if it differs) even though nothing changed here. Two enforcers
	// can overlap for a tick when Tyler logs in or out (the headless one
	// and the tray); whichever ran last may have left its own values.
	ProjectRecheck = time.Minute
	// GPUStoppedTTL is how long a GPU box the tray stopped is kept (by
	// shortening its user.expires), so `box job` can still say what happened
	// and `box pull` can fetch partial output. Its job is gone either way.
	GPUStoppedTTL = 30 * time.Minute
)

// Status is what one Tick found and did.
type Status struct {
	Chosen  Mode  // the mode picked and applied
	Running int   // instances running after this tick
	Down    bool  // the Incus host was not reachable (or Release memory runs), nothing was applied
	Err     error // joined errors from this tick; the failed parts retry next tick
	// Reclaimed lists the reclaims that ended since the last tick, with
	// what each box still holds, for the log. Nothing acts on it.
	Reclaimed []Reclaimed
}

// Manager drives instances toward the chosen mode. Call Tick regularly;
// every step is idempotent, so a failed call is retried on the next tick.
type Manager struct {
	Incus Incus
	// Reclaim, if set, lets every box swap up to its memory limit and pushes
	// each box frozen in My PC out to swap, in the background (reclaim.go).
	Reclaim Reclaimer

	// Handshake turns on the mode handshake (request.go): the manager adopts
	// the project's chosen mode, publishes its own picks there, and applies
	// the dashboard's requests. Both enforcers set it.
	Handshake bool
	// SeedChosen makes a manager that finds no chosen mode in the project
	// publish its own, so the tray's saved pick becomes the project's. The
	// headless enforcer leaves it off: its Full is a default, not a choice.
	SeedChosen bool
	// Persist, if set, is called with a mode adopted from the project or a
	// request, so the tray can save it to boxtray.json like a menu click.
	Persist func(Mode)
	// Logf, if set, records adopted and dropped requests, and each box reaped.
	Logf func(format string, args ...any)

	mu          sync.Mutex // guards chosen, choiceAt, chosenDirty
	chosen      Mode
	choiceAt    time.Time // when chosen was picked: a click, or a request's time
	chosenDirty bool      // chosen is not yet stored in ProjectChosenKey
	seedAt      time.Time // the time a seeded pick is stored under (choiceAt stays zero)

	chosenSynced bool // ProjectChosenKey was read once (guarded by tickMu)

	tickMu      sync.Mutex // serialises Tick (it calls Incus)
	applied     Mode       // mode the fields below belong to
	projectDone bool       // mode label set for applied
	projectAt   time.Time  // when the label was last written or read back
	frozenSince map[string]frozenAt
	rc          reclaims
	swapSet     map[string]int64  // box -> init PID whose memory.swap.max was confirmed last tick
	ended       map[string]string // box -> state it ended the last applyInstances in
	badExpires  map[string]string // instance -> unparseable user.expires already reported
	last        Status

	// Release memory (BeginRelease) holds this while `wsl --terminate`
	// runs; no tick, reclaim or swap call starts then. cancelMu guards
	// tickCancel, which cancels the tick in progress.
	releasing  atomic.Bool
	cancelMu   sync.Mutex
	tickCancel context.CancelFunc
	workCtx    context.Context // the caller's context, for reclaims (guarded by tickMu)
}

// New returns a Manager that starts in the chosen mode.
func New(inc Incus, chosen Mode) *Manager {
	return &Manager{Incus: inc, chosen: chosen}
}

// Chosen returns the user's pick.
func (m *Manager) Chosen() Mode { m.mu.Lock(); defer m.mu.Unlock(); return m.chosen }

// SetChosen records a new pick; the next Tick applies it.
func (m *Manager) SetChosen(c Mode) { m.SetChosenAt(c, time.Now()) }

// Tick brings Incus in line with the chosen mode.
func (m *Manager) Tick(ctx context.Context, now time.Time) Status {
	m.tickMu.Lock()
	defer m.tickMu.Unlock()

	// A tick that starts while Release memory runs calls nothing: any
	// wsl.exe -d agents would start the distro again. One already running is
	// cancelled by BeginRelease, so it stops calling out.
	m.workCtx = ctx // reclaims outlive the tick
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.cancelMu.Lock()
	m.tickCancel = cancel
	m.cancelMu.Unlock()
	defer func() { m.cancelMu.Lock(); m.tickCancel = nil; m.cancelMu.Unlock() }()
	if m.releasing.Load() {
		return Status{Chosen: m.Chosen(), Down: true}
	}

	var hsErrs []error
	if m.Handshake {
		hsErrs = m.handshake(ctx, now)
		if !m.Synced() {
			// Not read yet: applying the default could undo an earlier pick.
			m.last = Status{Chosen: m.Chosen(), Down: true, Running: m.last.Running, Err: errors.Join(hsErrs...)}
			return m.last
		}
	}
	st := Status{Chosen: m.Chosen()}
	eff := st.Chosen
	if eff != m.applied {
		m.applied, m.projectDone = eff, false
		m.frozenSince = map[string]frozenAt{}
		m.rc.forgetAll()
	}
	m.rc.init()

	errs := hsErrs
	if m.projectDone && now.Sub(m.projectAt) >= ProjectRecheck {
		// Read the label back once a minute; write only if it differs, so a
		// quiet minute costs one read and no write.
		m.projectAt = now
		if cur, err := m.Incus.ProjectConfig(ctx, ProjectModeKey); err == nil && cur != string(eff) {
			m.projectDone = false
		}
	}
	if !m.projectDone {
		if err := m.applyProject(ctx, eff); err != nil {
			errs = append(errs, err)
		} else {
			m.projectDone, m.projectAt = true, now
		}
	}
	ins, err := m.Incus.List(ctx)
	if err != nil {
		errs = append(errs, err)
		st.Running = m.last.Running
	} else {
		ins, errs = m.reap(ctx, now, ins, errs)
		if m.Reclaim != nil {
			var rerrs []error
			st.Reclaimed, rerrs = m.rc.harvest(now)
			errs = append(errs, rerrs...)
		}
		// Pausing, GPU stops and thawing come first; upkeep that the owner
		// doesn't wait for (the memory cap, swap limits) runs after.
		st.Running, errs = m.applyInstances(ctx, now, eff, ins, errs)
		if ctx.Err() == nil {
			if err := m.applyMemoryCap(ctx, eff, ins); err != nil {
				errs = append(errs, err)
			}
		}
		if m.Reclaim != nil && ctx.Err() == nil {
			errs = append(errs, m.ensureSwap(ctx, ins, m.ended)...)
		}
	}
	st.Err = errors.Join(errs...)
	m.last = st
	return st
}

// reap deletes every instance whose user.expires and hold (Held) are in the past and returns
// the rest. It runs in every mode, My PC included, because deleting frees RAM
// and disk. An instance with no user.expires is left alone; one with a value
// that isn't RFC3339 is left alone and reported once. A failed delete is
// reported and retried next tick, since the instance is still listed.
func (m *Manager) reap(ctx context.Context, now time.Time, ins []Instance, errs []error) ([]Instance, []error) {
	if m.badExpires == nil {
		m.badExpires = map[string]string{}
	}
	live := make(map[string]bool, len(ins))
	keep := ins[:0:0]
	for _, in := range ins {
		live[in.Name] = true
		val, ok := in.Config[ExpiresKey]
		if !ok {
			delete(m.badExpires, in.Name)
			keep = append(keep, in)
			continue
		}
		exp, err := time.Parse(time.RFC3339, val)
		if err != nil {
			if m.badExpires[in.Name] != val {
				m.badExpires[in.Name] = val
				errs = append(errs, fmt.Errorf("%s: ignoring unparseable %s %q: %w", in.Name, ExpiresKey, val, err))
			}
			keep = append(keep, in)
			continue
		}
		delete(m.badExpires, in.Name)
		if !Held(in.Config, exp).Before(now) {
			keep = append(keep, in)
			continue
		}
		if err := m.Incus.Delete(ctx, in.Name); err != nil {
			errs = append(errs, err)
			keep = append(keep, in) // still there; counted and retried next tick
			continue
		}
		m.logf("reaped %s (expired %s)", in.Name, val) // so a box that vanished can be explained (#156)
		delete(m.frozenSince, in.Name)
	}
	for name := range m.badExpires {
		if !live[name] {
			delete(m.badExpires, name)
		}
	}
	return keep, errs
}

// applyProject records the mode for `box`. It never touches project limits.
func (m *Manager) applyProject(ctx context.Context, eff Mode) error {
	return m.Incus.SetProjectConfig(ctx, ProjectModeKey, string(eff))
}

// applyMemoryCap sets the project's limits.memory to the mode's budget
// (MemoryBudgets), or to what existing instances already hold if that is
// more, since Incus refuses a cap below it. So the cap tightens as boxes go
// away. It writes only when the value differs.
func (m *Manager) applyMemoryCap(ctx context.Context, eff Mode, ins []Instance) error {
	want, ok := ParseBytes(MemoryBudgets[eff])
	if !ok {
		return fmt.Errorf("no memory budget for mode %q", eff)
	}
	want = max(want, AllocatedMemory(ins))
	cur, err := m.Incus.ProjectConfig(ctx, MemoryCapKey)
	if err != nil {
		return err
	}
	if have, ok := ParseBytes(cur); ok && have == want {
		return nil
	}
	return m.Incus.SetProjectConfig(ctx, MemoryCapKey, FormatBytes(want))
}

// AllocatedMemory is the limits.memory of every instance added up, stopped
// ones too, the way Incus counts it against the project cap: the expanded
// config (profiles included), then the instance's own, then its size.
func AllocatedMemory(ins []Instance) int64 {
	var sum int64
	for _, in := range ins {
		v, ok := in.ExpandedConfig[MemoryCapKey]
		if !ok {
			v = in.Config[MemoryCapKey]
		}
		n, ok := ParseBytes(v)
		if !ok {
			n, _ = ParseBytes(SizeOf(in.Config).Memory)
		}
		sum += n
	}
	return sum
}

// applyInstances brings each instance in line with eff.
func (m *Manager) applyInstances(ctx context.Context, now time.Time, eff Mode, ins []Instance, errs []error) (running int, _ []error) {
	m.ended = map[string]string{}
	seen := map[string]bool{}
	for _, in := range ins {
		seen[in.Name] = true
		if ctx.Err() != nil { // Release memory: call out no more
			continue
		}
		state := strings.ToLower(in.Status)
		var err error
		switch {
		case eff == MyPC && IsGPU(in.Config):
			state, err = m.stopGPU(ctx, now, in, state)
		case eff == MyPC:
			prev := state
			state, err = m.freeze(ctx, now, in.Name, pidOf(in), state)
			if m.Reclaim != nil && err == nil {
				m.reclaimFrozen(now, in, prev, state)
			}
		case IsGPU(in.Config) && state == "stopped" && in.Config[StoppedKey] != "":
			// Stays stopped; stopGPU retries a failed expiry write.
			state, err = m.stopGPU(ctx, now, in, state)
		default:
			state, err = m.thaw(ctx, eff, in, state)
		}
		if err != nil {
			errs = append(errs, err)
		}
		if state == "running" {
			running++
		}
		m.ended[in.Name] = state
	}
	for name := range m.frozenSince {
		if !seen[name] {
			delete(m.frozenSince, name)
		}
	}
	m.rc.prune(seen)
	return running, errs
}

// frozenAt is when an instance was first seen frozen, for one identity: a
// box recreated under the same name (a new init PID) starts a new timer.
type frozenAt struct {
	at  time.Time
	pid int64
}

// frozenLimit is how long box name (init PID pid) may stay frozen: 4 hours
// once a reclaim of this freeze ran and left ReclaimLeft or less, else 15
// minutes (no reclaim yet, one that left more, or no swap to go to).
func (m *Manager) frozenLimit(name string, pid int64) time.Duration {
	if j := m.rc.jobs[name]; m.Reclaim != nil && j != nil && pid > 0 && j.pid == pid && j.done && j.left <= ReclaimLeft {
		return FrozenSwappedStop
	}
	return FrozenStop
}

// freeze is the My PC step for one instance; it returns the state it ends in.
func (m *Manager) freeze(ctx context.Context, now time.Time, name string, pid int64, state string) (string, error) {
	switch state {
	case "running":
		if err := m.Incus.Pause(ctx, name); err != nil {
			return state, err
		}
		m.frozenSince[name] = frozenAt{now, pid}
		return "frozen", nil
	case "frozen":
		since, ok := m.frozenSince[name]
		if !ok || since.pid != pid {
			m.frozenSince[name] = frozenAt{now, pid}
		} else if now.Sub(since.at) >= m.frozenLimit(name, pid) {
			if err := m.Incus.Stop(ctx, name); err != nil {
				return state, err
			}
			delete(m.frozenSince, name)
			return "stopped", nil
		}
	default:
		delete(m.frozenSince, name)
	}
	return state, nil
}

// stopGPU is the My PC step for a GPU box: stop, not pause, because a frozen
// CUDA process keeps its VRAM and the owner needs it back. Before stopping it
// marks the box (StoppedKey); once the stop worked it shortens the box's expiry
// to GPUStoppedTTL, since its job died with it. Shortening it earlier would
// let the reaper delete a box whose stop failed and that later resumed. If
// the stop fails the box is paused, so it at least stops computing, and the
// stop is retried next tick; so is a failed expiry write, for a stopped box
// that carries the mark.
func (m *Manager) stopGPU(ctx context.Context, now time.Time, in Instance, state string) (string, error) {
	var errs []error
	shorten := func() {
		soon := now.Add(GPUStoppedTTL)
		if exp, err := time.Parse(time.RFC3339, in.Config[ExpiresKey]); err != nil || exp.After(soon) {
			if err := m.Incus.SetInstanceConfig(ctx, in.Name, ExpiresKey, soon.UTC().Format(time.RFC3339)); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if state != "running" && state != "frozen" {
		delete(m.frozenSince, in.Name)
		if state == "stopped" && in.Config[StoppedKey] != "" {
			shorten()
		}
		return state, errors.Join(errs...)
	}
	why := string(MyPC)
	mark := func() error { return m.Incus.SetInstanceConfig(ctx, in.Name, StoppedKey, why) }
	markErr := error(nil)
	if in.Config[StoppedKey] != why {
		markErr = mark()
	}
	if err := m.Incus.Stop(ctx, in.Name); err != nil {
		errs = append(errs, err)
		if state == "running" {
			if perr := m.Incus.Pause(ctx, in.Name); perr != nil {
				errs = append(errs, perr)
			} else {
				state = "frozen"
			}
		}
		return state, errors.Join(errs...)
	}
	delete(m.frozenSince, in.Name)
	if markErr != nil { // once more: unmarked, box can't reclaim the stopped box
		if err := mark(); err != nil {
			errs = append(errs, err)
		}
	}
	shorten()
	return "stopped", errors.Join(errs...)
}

// thaw is the Shared/Full step for one instance. Every tick it compares the
// instance's own config with what its size (user.box.size, medium if unset)
// and the mode want and fixes only the keys that differ, so a limit an agent
// changed is put back, and nothing is called when nothing changed. Limits go on before the resume so a thawed box is never
// briefly uncapped.
func (m *Manager) thaw(ctx context.Context, eff Mode, in Instance, state string) (string, error) {
	// Leave a stopped, never-started instance alone: on Incus 7.6 (2026-11-24,
	// btrfs + GHSA-5pgx-q9xx-j3qv), CreateInternal inserts the row as Stopped
	// with last_used_at=1970-01-01T00:00:00Z before EnsureImage unpacks the
	// image. Writing instance config in that window creates
	// containers/<project>_<name> as an empty plain directory before `btrfs
	// subvolume snapshot` runs, failing the launch. Once a box has started
	// (including one later stopped by FrozenStop or FrozenSwappedStop),
	// last_used_at is set and thaw updates its limits as usual.
	if state == "stopped" && in.LastUsedAt.Unix() <= 0 {
		return state, nil
	}
	cpus := strconv.Itoa(SizeOf(in.Config).CPUs(eff))
	want := [...][2]string{{"limits.cpu", cpus}, {"limits.cpu.priority", eff.priority()}}
	var errs []error
	for _, kv := range want {
		if cur, ok := in.Config[kv[0]]; !ok || cur != kv[1] {
			if err := m.Incus.SetInstanceConfig(ctx, in.Name, kv[0], kv[1]); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return state, err // stay frozen until the limits are right
	}
	if state == "frozen" {
		if err := m.Incus.Resume(ctx, in.Name); err != nil {
			return state, err
		}
		return "running", nil
	}
	return state, nil
}

// BeginRelease is called before the tray's "Release memory" runs `wsl
// --terminate`. It returns false, doing nothing, if a release is already
// running: one termination at a time. Otherwise it cancels the tick in
// progress and waits for it, drops queued and running reclaims and waits
// for their wsl.exe calls to end, and keeps any new tick from calling out
// until EndRelease.
func (m *Manager) BeginRelease() bool {
	if !m.releasing.CompareAndSwap(false, true) {
		return false
	}
	m.cancelMu.Lock()
	if m.tickCancel != nil {
		m.tickCancel()
	}
	m.cancelMu.Unlock()
	m.tickMu.Lock()
	m.rc.forgetAll()
	m.tickMu.Unlock()
	m.rc.wait()
	return true
}

// EndRelease lets ticks and reclaims call out again.
func (m *Manager) EndRelease() { m.releasing.Store(false) }
