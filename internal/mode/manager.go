package mode

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ExpiresKey is the instance config key `box` sets to an RFC3339 UTC time after
// which the instance is deleted.
const ExpiresKey = "user.expires"

const (
	// FrozenStop is how long an instance may stay frozen in My PC before it is
	// stopped to free its RAM.
	FrozenStop = 15 * time.Minute
	// YieldDebounce is how long the game signal must stay clear before the
	// chosen mode comes back, so alt-tab doesn't freeze and thaw the boxes.
	YieldDebounce = 20 * time.Second
	// GPUStoppedTTL is how long a GPU box the tray stopped is kept (by
	// shortening its user.expires), so `box job` can still say what happened
	// and `box pull` can fetch partial output. Its job is gone either way.
	GPUStoppedTTL = 30 * time.Minute
)

// Status is what one Tick found and did.
type Status struct {
	Chosen    Mode   // what the user picked
	Effective Mode   // what is applied: MyPC while yielding, else Chosen
	Yielding  bool   // a game is running (or just ended, within the debounce)
	Reason    string // why, e.g. "steam:570"
	Running   int    // instances running after this tick
	Down      bool   // the Incus host was not reachable, nothing was applied
	Err       error  // joined errors from this tick; the failed parts retry next tick
}

// Manager drives instances toward the effective mode. Call Tick regularly;
// every step is idempotent, so a failed call is retried on the next tick.
type Manager struct {
	Incus Incus

	mu     sync.Mutex // guards chosen
	chosen Mode

	tickMu      sync.Mutex // serialises Tick (it calls Incus)
	yielding    bool
	clearSince  time.Time
	reason      string
	applied     Mode   // effective mode the fields below belong to
	projectDone bool   // mode label set for applied
	pubReason   string // user.agent-box.reason last written ("" = never or cleared)
	frozenSince map[string]time.Time
	badExpires  map[string]string // instance -> unparseable user.expires already reported
	last        Status
}

// New returns a Manager that starts in the chosen mode.
func New(inc Incus, chosen Mode) *Manager {
	return &Manager{Incus: inc, chosen: chosen}
}

// Chosen returns the user's pick.
func (m *Manager) Chosen() Mode { m.mu.Lock(); defer m.mu.Unlock(); return m.chosen }

// SetChosen records a new pick; the next Tick applies it.
func (m *Manager) SetChosen(c Mode) { m.mu.Lock(); m.chosen = c; m.mu.Unlock() }

// Last returns the result of the most recent Tick.
func (m *Manager) Last() Status { m.tickMu.Lock(); defer m.tickMu.Unlock(); return m.last }

// Tick feeds the current game signal and brings Incus in line with the
// effective mode.
func (m *Manager) Tick(ctx context.Context, now time.Time, gaming bool, reason string) Status {
	m.tickMu.Lock()
	defer m.tickMu.Unlock()

	m.updateYield(now, gaming, reason)
	st := Status{Chosen: m.Chosen(), Yielding: m.yielding, Reason: m.reason}
	st.Effective = st.Chosen
	if m.yielding {
		st.Effective = MyPC
	}
	if st.Effective != m.applied {
		m.applied, m.projectDone = st.Effective, false
		m.frozenSince = map[string]time.Time{}
	}

	var errs []error
	if !m.projectDone {
		if err := m.applyProject(ctx, st.Effective); err != nil {
			errs = append(errs, err)
		} else {
			m.projectDone = true
		}
	}
	// Why the PC is yielding, for `box status`. Written only on change, so the
	// common case (never yielding) costs no calls.
	if want := yieldReason(m.yielding, m.reason); want != m.pubReason {
		if err := m.Incus.SetProjectConfig(ctx, ProjectReasonKey, want); err != nil {
			errs = append(errs, err)
		} else {
			m.pubReason = want
		}
	}
	ins, err := m.Incus.List(ctx)
	if err != nil {
		errs = append(errs, err)
		st.Running = m.last.Running
	} else {
		ins, errs = m.reap(ctx, now, ins, errs)
		why := string(MyPC)
		if m.yielding {
			why = yieldReason(true, m.reason)
		}
		st.Running, errs = m.applyInstances(ctx, now, st.Effective, why, ins, errs)
	}
	st.Err = errors.Join(errs...)
	m.last = st
	return st
}

// reap deletes every instance whose user.expires is in the past and returns
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
		if !exp.Before(now) {
			keep = append(keep, in)
			continue
		}
		if err := m.Incus.Delete(ctx, in.Name); err != nil {
			errs = append(errs, err)
			keep = append(keep, in) // still there; counted and retried next tick
			continue
		}
		delete(m.frozenSince, in.Name)
	}
	for name := range m.badExpires {
		if !live[name] {
			delete(m.badExpires, name)
		}
	}
	return keep, errs
}

// updateYield starts yielding at once but stops only after YieldDebounce of
// continuous quiet.
func (m *Manager) updateYield(now time.Time, gaming bool, reason string) {
	switch {
	case gaming:
		m.yielding, m.reason, m.clearSince = true, reason, time.Time{}
	case m.yielding:
		if m.clearSince.IsZero() {
			m.clearSince = now
		}
		if now.Sub(m.clearSince) >= YieldDebounce {
			m.yielding, m.reason, m.clearSince = false, "", time.Time{}
		}
	}
}

// yieldReason is the value of ProjectReasonKey: empty unless yielding.
func yieldReason(yielding bool, reason string) string {
	if !yielding {
		return ""
	}
	if reason == "" {
		return "gaming"
	}
	return "gaming: " + reason
}

// applyProject records the mode for `box`. It never touches project limits.
func (m *Manager) applyProject(ctx context.Context, eff Mode) error {
	return m.Incus.SetProjectConfig(ctx, ProjectModeKey, string(eff))
}

// applyInstances brings each instance in line with eff. why is what a GPU
// box stopped in My PC is told (StoppedKey).
func (m *Manager) applyInstances(ctx context.Context, now time.Time, eff Mode, why string, ins []Instance, errs []error) (running int, _ []error) {
	seen := map[string]bool{}
	for _, in := range ins {
		seen[in.Name] = true
		state := strings.ToLower(in.Status)
		var err error
		switch {
		case eff == MyPC && IsGPU(in.Config):
			state, err = m.stopGPU(ctx, now, in, state, why)
		case eff == MyPC:
			state, err = m.freeze(ctx, now, in.Name, state)
		default:
			state, err = m.thaw(ctx, eff, in, state)
		}
		if err != nil {
			errs = append(errs, err)
		}
		if state == "running" {
			running++
		}
	}
	for name := range m.frozenSince {
		if !seen[name] {
			delete(m.frozenSince, name)
		}
	}
	return running, errs
}

// freeze is the My PC step for one instance; it returns the state it ends in.
func (m *Manager) freeze(ctx context.Context, now time.Time, name, state string) (string, error) {
	switch state {
	case "running":
		if err := m.Incus.Pause(ctx, name); err != nil {
			return state, err
		}
		m.frozenSince[name] = now
		return "frozen", nil
	case "frozen":
		since, ok := m.frozenSince[name]
		if !ok {
			m.frozenSince[name] = now
		} else if now.Sub(since) >= FrozenStop {
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
// CUDA process keeps its VRAM and a game needs it back. Before stopping it
// records why (StoppedKey); once the stop worked it shortens the box's expiry
// to GPUStoppedTTL, since its job died with it. Shortening it earlier would
// let the reaper delete a box whose stop failed and that later resumed. If
// the stop fails the box is paused, so it at least stops computing, and the
// stop is retried next tick; so is a failed expiry write, for a stopped box
// that carries the mark.
func (m *Manager) stopGPU(ctx context.Context, now time.Time, in Instance, state, why string) (string, error) {
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
	if in.Config[StoppedKey] != why {
		if err := m.Incus.SetInstanceConfig(ctx, in.Name, StoppedKey, why); err != nil {
			errs = append(errs, err)
		}
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
	shorten()
	return "stopped", errors.Join(errs...)
}

// thaw is the Shared/Full step for one instance. Every tick it compares the
// instance's own config with what its size (user.box.size, medium if unset)
// and the mode want and fixes only the keys that differ, so a limit an agent
// changed is put back, and nothing is called when nothing changed. Limits go on before the resume so a thawed box is never
// briefly uncapped.
func (m *Manager) thaw(ctx context.Context, eff Mode, in Instance, state string) (string, error) {
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
	for _, k := range legacyKeys {
		if _, ok := in.Config[k]; ok {
			if err := m.Incus.UnsetInstanceConfig(ctx, in.Name, k); err != nil {
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
