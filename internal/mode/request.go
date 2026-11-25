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

// ModeRequest is what the page writes to ProjectModeRequestKey.
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

// adopt takes a pick from the project or the page.
func (m *Manager) adopt(c Mode, at time.Time) {
	m.mu.Lock()
	m.chosen, m.choiceAt = c, at
	m.mu.Unlock()
	if m.Persist != nil {
		m.Persist(c)
	}
}

// handshake runs at the start of a Tick when Handshake is set. It first syncs
// the chosen mode with the project, then publishes a pick made here, then
// applies a pending request unless a game is running. Every step that fails is
// retried next tick.
func (m *Manager) handshake(ctx context.Context, now time.Time) (errs []error) {
	if !m.chosenSynced {
		cur, err := m.Incus.ProjectConfig(ctx, ProjectChosenKey)
		if err != nil {
			return []error{err}
		}
		var v chosenValue
		if json.Unmarshal([]byte(cur), &v) == nil && v.At.After(m.choiceAtLocked()) {
			if _, ok := Parse(string(v.Mode)); ok {
				m.adopt(v.Mode, v.At)
				m.logf("mode %s adopted from the project (chosen %s)", v.Mode, v.At.Format(time.RFC3339))
			}
		} else if cur == "" && m.SeedChosen {
			m.mu.Lock()
			m.choiceAt, m.chosenDirty = now, true
			m.mu.Unlock()
		}
		m.chosenSynced = true
	}
	if err := m.publishChosen(ctx); err != nil {
		errs = append(errs, err)
	}
	if m.yielding { // a game wins; the request stays pending
		return errs
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
		return append(errs, m.clearRequest(ctx))
	}
	if _, ok := Parse(string(req.Mode)); !ok {
		m.logf("dropping a mode request for %q from %s", req.Mode, req.By)
		return append(errs, m.clearRequest(ctx))
	}
	switch {
	case now.Sub(req.At) > RequestMaxAge:
		m.logf("dropping a mode request for %s from %s: %s old", req.Mode, req.By, now.Sub(req.At).Round(time.Second))
	case req.At.Before(m.choiceAtLocked()):
		m.logf("dropping a mode request for %s from %s: a newer choice was made here", req.Mode, req.By)
	default:
		m.adopt(req.Mode, req.At)
		m.mu.Lock()
		m.chosenDirty = true
		m.mu.Unlock()
		m.logf("mode %s requested by %s", req.Mode, req.By)
		// The pick must be stored before the request goes, or a failure in
		// between would lose it.
		if err := m.publishChosen(ctx); err != nil {
			return append(errs, err)
		}
	}
	return append(errs, m.clearRequest(ctx))
}

func (m *Manager) choiceAtLocked() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.choiceAt
}

// publishChosen writes ProjectChosenKey if a pick is waiting to be stored.
func (m *Manager) publishChosen(ctx context.Context) error {
	m.mu.Lock()
	dirty, v := m.chosenDirty, chosenValue{m.chosen, m.choiceAt}
	m.mu.Unlock()
	if !dirty {
		return nil
	}
	b, _ := json.Marshal(v)
	if err := m.Incus.SetProjectConfig(ctx, ProjectChosenKey, string(b)); err != nil {
		return fmt.Errorf("store the chosen mode: %w", err)
	}
	m.mu.Lock()
	if m.choiceAt.Equal(v.At) { // not replaced while writing
		m.chosenDirty = false
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) clearRequest(ctx context.Context) error {
	if err := m.Incus.SetProjectConfig(ctx, ProjectModeRequestKey, ""); err != nil {
		return fmt.Errorf("clear the mode request: %w", err)
	}
	return nil
}
