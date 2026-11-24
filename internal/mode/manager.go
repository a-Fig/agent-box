package mode

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

const (
	// FrozenStop is how long an instance may stay frozen in My PC before it is
	// stopped to free its RAM.
	FrozenStop = 15 * time.Minute
	// YieldDebounce is how long the game signal must stay clear before the
	// chosen mode comes back, so alt-tab doesn't freeze and thaw the boxes.
	YieldDebounce = 20 * time.Second
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
	applied     Mode // effective mode the fields below belong to
	projectDone bool // mode label set for applied
	frozenSince map[string]time.Time
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
	ins, err := m.Incus.List(ctx)
	if err != nil {
		errs = append(errs, err)
		st.Running = m.last.Running
	} else {
		st.Running, errs = m.applyInstances(ctx, now, st.Effective, ins, errs)
	}
	st.Err = errors.Join(errs...)
	m.last = st
	return st
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

// applyProject records the mode for `box`. It never touches project limits.
func (m *Manager) applyProject(ctx context.Context, eff Mode) error {
	return m.Incus.SetProjectConfig(ctx, ProjectModeKey, string(eff))
}

func (m *Manager) applyInstances(ctx context.Context, now time.Time, eff Mode, ins []Instance, errs []error) (running int, _ []error) {
	seen := map[string]bool{}
	for _, in := range ins {
		seen[in.Name] = true
		state := strings.ToLower(in.Status)
		var err error
		if eff == MyPC {
			state, err = m.freeze(ctx, now, in.Name, state)
		} else {
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

// thaw is the Shared/Full step for one instance. Every tick it compares the
// instance's own config with what the mode wants and fixes only the keys that
// differ, so a limit an agent changed is put back, and nothing is called when
// nothing changed. Limits go on before the resume so a thawed box is never
// briefly uncapped.
func (m *Manager) thaw(ctx context.Context, eff Mode, in Instance, state string) (string, error) {
	lim := eff.limits()
	want := [...][2]string{{"limits.cpu", lim.cpu}, {"limits.cpu.priority", lim.priority}}
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
