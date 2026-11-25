package mode

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// The mode handshake (docs/ui.md). The dashboard asks for a mode by writing
// ProjectModeRequestKey; whichever enforcer is active adopts it. The pick the
// enforcers act on is kept in ProjectChosenKey, so the tray and the headless
// enforcer, which come and go, agree on it.
const (
	// ProjectModeRequestKey holds a ModeRequest as JSON, written by boxd.
	ProjectModeRequestKey = "user.agent-box.mode-request"
	// ProjectChosenKey holds the last choice, a click or an adopted request, as
	// JSON {"mode","at"}. It is the one source of truth for the chosen mode; the
	// tray's boxtray.json is only its local copy for when the project is unreachable.
	ProjectChosenKey = "user.agent-box.chosen"
	// RequestMaxAge is how old a request may be and still apply, so one written
	// while nobody enforced isn't applied days later.
	RequestMaxAge = 10 * time.Minute
)

// ModeRequest is what boxd writes to ProjectModeRequestKey for the page.
type ModeRequest struct {
	Mode Mode      `json:"mode"`
	At   time.Time `json:"at"`
	By   string    `json:"by"`
}

// chosenValue is the JSON in ProjectChosenKey.
type chosenValue struct {
	Mode Mode      `json:"mode"`
	At   time.Time `json:"at"`
}

// SetChosenAt records a pick made at the given time. The last pick wins by
// time, so a request older than this one is dropped.
func (m *Manager) SetChosenAt(c Mode, at time.Time) {
	m.mu.Lock()
	m.chosen, m.choiceAt, m.chosenDirty = c, at, true
	m.mu.Unlock()
}

func (m *Manager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

// adoptNewer takes a pick from the project (strictly newer than ours) or from
// a request (not older than ours), deciding under the lock so a click that
// lands meanwhile is never overwritten. It reports whether it adopted.
func (m *Manager) adoptNewer(c Mode, at time.Time, orEqual bool) bool {
	m.mu.Lock()
	newer := at.After(m.choiceAt) || (orEqual && at.Equal(m.choiceAt))
	if newer {
		m.chosen, m.choiceAt = c, at
	}
	m.mu.Unlock()
	if newer && m.Persist != nil {
		m.Persist(c)
	}
	return newer
}

// handshake runs at the start of a Tick when Handshake is set. It first syncs
// the chosen mode with the project (until that has worked the Tick applies
// nothing: see Synced), then publishes a pick made here, then applies a
// pending request. Every step that fails is retried next tick.
func (m *Manager) handshake(ctx context.Context, now time.Time) (errs []error) {
	if !m.chosenSynced {
		cur, err := m.Incus.ProjectConfig(ctx, ProjectChosenKey)
		if err != nil {
			return []error{err}
		}
		var v chosenValue
		if json.Unmarshal([]byte(cur), &v) == nil {
			if _, ok := Parse(string(v.Mode)); ok && m.adoptNewer(v.Mode, v.At, false) {
				m.logf("mode %s adopted from the project (chosen %s)", v.Mode, v.At.Format(time.RFC3339))
			}
		} else if cur == "" && m.SeedChosen {
			// Stored with the time of seeding, but not stamped on the local
			// pick: a request older than that is still newer than this old pick.
			m.mu.Lock()
			m.chosenDirty, m.seedAt = true, now
			m.mu.Unlock()
		}
		m.chosenSynced = true
	}
	if err := m.publishChosen(ctx); err != nil {
		errs = append(errs, err)
	}
	raw, err := m.Incus.ProjectConfig(ctx, ProjectModeRequestKey)
	if err != nil {
		return append(errs, err)
	}
	if raw == "" {
		return errs
	}
	var req ModeRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		m.logf("dropping an unreadable mode request %q: %v", raw, err)
		return append(errs, m.clearRequest(ctx, raw))
	}
	if _, ok := Parse(string(req.Mode)); !ok {
		m.logf("dropping a mode request for %q from %s", req.Mode, req.By)
		return append(errs, m.clearRequest(ctx, raw))
	}
	if now.Sub(req.At) > RequestMaxAge {
		m.logf("dropping a mode request for %s from %s: %s old", req.Mode, req.By, now.Sub(req.At).Round(time.Second))
	} else if m.adoptNewer(req.Mode, req.At, true) {
		m.mu.Lock()
		m.chosenDirty = true
		m.mu.Unlock()
		m.logf("mode %s requested by %s", req.Mode, req.By)
		// The pick must be stored before the request goes, or a failure in
		// between would lose it.
		if err := m.publishChosen(ctx); err != nil {
			return append(errs, err)
		}
	} else {
		m.logf("dropping a mode request for %s from %s: a newer choice was made here", req.Mode, req.By)
	}
	return append(errs, m.clearRequest(ctx, raw))
}

// Synced reports whether the chosen mode has been read from the project. A
// Tick with Handshake on applies nothing until it has: an enforcer that
// started while Incus was still coming up would otherwise apply its default
// (Full, for the headless one) over a pick made earlier.
func (m *Manager) Synced() bool { return !m.Handshake || m.chosenSynced }

// publishChosen writes ProjectChosenKey if a pick is waiting to be stored.
func (m *Manager) publishChosen(ctx context.Context) error {
	m.mu.Lock()
	dirty, v := m.chosenDirty, chosenValue{m.chosen, m.choiceAt}
	if v.At.IsZero() {
		v.At = m.seedAt
	}
	picked := m.choiceAt
	m.mu.Unlock()
	if !dirty {
		return nil
	}
	b, _ := json.Marshal(v)
	if err := m.Incus.SetProjectConfig(ctx, ProjectChosenKey, string(b)); err != nil {
		return fmt.Errorf("store the chosen mode: %w", err)
	}
	m.mu.Lock()
	if m.choiceAt.Equal(picked) { // not replaced while writing
		m.chosenDirty = false
	}
	m.mu.Unlock()
	return nil
}

// clearRequest empties the request key, but only if it still holds raw: a
// second request that arrived while this one was handled is left for the next tick.
func (m *Manager) clearRequest(ctx context.Context, raw string) error {
	cur, err := m.Incus.ProjectConfig(ctx, ProjectModeRequestKey)
	if err != nil {
		return fmt.Errorf("clear the mode request: %w", err)
	}
	if cur != raw {
		return nil
	}
	if err := m.Incus.SetProjectConfig(ctx, ProjectModeRequestKey, ""); err != nil {
		return fmt.Errorf("clear the mode request: %w", err)
	}
	return nil
}
