package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/a-Fig/agent-box/internal/mode"
	"github.com/a-Fig/agent-box/internal/version"
)

// Sensors are the Windows-side inputs and outputs; fakes stand in for tests.
type Sensors interface {
	Gaming() (bool, string)      // a game owns the PC
	HostUp(context.Context) bool // the WSL distro is running (we never start it)
	HoldAwake(on bool)           // keep the PC from sleeping
}

// loopCfg is everything run needs besides the sensors.
type loopCfg struct {
	// Manager returns the manager to tick. run calls it at the start and again
	// each time it resumes from idle, so a headless enforcer that stood aside
	// while the tray worked starts from a clean slate: its cached "mode label
	// already written" would otherwise be stale.
	Manager func() *mode.Manager
	// Idle, if set and true, means another enforcer holds the PC: run writes
	// nothing and calls nothing in Incus, and holds no power request.
	Idle func() bool
	// Beat, if set, is called after every tick that reached Incus; it throttles itself.
	Beat func(ctx context.Context, now time.Time)
	// Logf, if set, records handovers.
	Logf func(format string, args ...any)
}

// run polls until ctx ends. It ticks every interval and immediately on wake
// (a menu click). report is called with every result (not while idle).
//
// While the distro is down it does nothing to Incus: any wsl.exe call would
// start it again, undoing "Release memory".
func run(ctx context.Context, cfg loopCfg, s Sensors, interval time.Duration, wake <-chan struct{}, report func(mode.Status)) {
	logf := func(format string, args ...any) {
		if cfg.Logf != nil {
			cfg.Logf(format, args...)
		}
	}
	var m *mode.Manager
	tick := func() {
		if cfg.Idle != nil && cfg.Idle() {
			if m != nil {
				logf("an interactive tray is running: standing aside")
				m = nil
				s.HoldAwake(false)
			}
			return
		}
		if m == nil {
			m = cfg.Manager()
			logf("enforcing as %s", m.Chosen())
		}
		var st mode.Status
		if s.HostUp(ctx) {
			gaming, reason := s.Gaming()
			now := time.Now()
			st = m.Tick(ctx, now, gaming, reason)
			if cfg.Beat != nil && !st.Down {
				cfg.Beat(ctx, now)
			}
		} else {
			st = mode.Status{Chosen: m.Chosen(), Effective: m.Chosen(), Down: true}
		}
		s.HoldAwake(st.Running > 0)
		report(st)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	tick()
	for {
		select {
		case <-ctx.Done():
			s.HoldAwake(false)
			return
		case <-t.C:
		case <-wake:
		}
		tick()
	}
}

// heartbeatEvery is how often the active enforcer writes tray-seen.
const heartbeatEvery = 30 * time.Second

// heartbeat writes the project keys that say which enforcer is alive. Both
// the tray and the headless enforcer use it; only the active one calls it.
type heartbeat struct {
	inc   mode.Incus
	who   string // mode.EnforcerTray or mode.EnforcerHeadless
	build string // this binary's version, as JSON
	every time.Duration
	last  time.Time
	logf  func(format string, args ...any)
}

func newHeartbeat(inc mode.Incus, who string, logf func(string, ...any)) *heartbeat {
	b, _ := json.Marshal(version.Get())
	return &heartbeat{inc: inc, who: who, build: string(b), every: heartbeatEvery, logf: logf}
}

// beat writes the keys unless it did within the last h.every. Who comes
// first, so a dashboard that sees a fresh time also sees the right name, and
// the first beat (last is zero: this enforcer has just taken over) writes its
// version too. A failed write is retried on the next tick.
func (h *heartbeat) beat(ctx context.Context, now time.Time) {
	if !h.last.IsZero() && now.Sub(h.last) < h.every {
		return
	}
	if h.last.IsZero() {
		if err := h.inc.SetProjectConfig(ctx, mode.ProjectTrayVersionKey, h.build); err != nil {
			h.logf("heartbeat: %v", err)
			return
		}
	}
	if err := h.inc.SetProjectConfig(ctx, mode.ProjectEnforcerKey, h.who); err != nil {
		h.logf("heartbeat: %v", err)
		return
	}
	if err := h.inc.SetProjectConfig(ctx, mode.ProjectTraySeenKey, now.UTC().Format(time.RFC3339)); err != nil {
		h.logf("heartbeat: %v", err)
		return
	}
	h.last = now
}
