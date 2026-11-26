package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/a-Fig/agent-box/internal/mode"
	"github.com/a-Fig/agent-box/internal/version"
)

// Sensors are the Windows-side inputs and outputs; fakes stand in for tests.
type Sensors interface {
	HostUp(context.Context) bool     // the WSL distro is running; asking never starts it
	StartHost(context.Context) error // start the distro and keep it running
	HoldAwake(on bool)               // keep the PC from sleeping
}

// hostStartEvery is how often run tries again to start a distro that stays
// down. A try that a release cut short doesn't count.
const hostStartEvery = time.Minute

// errStandAside is what a start returns when a tray appeared just before it.
var errStandAside = errors.New("an interactive tray is running")

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
	// Maintain, if set, is called after every tick, with whether this process is the enforcer
	// (false while it stands aside); the self-update (update.go) throttles itself.
	Maintain func(ctx context.Context, active bool)
}

// run polls until ctx ends. It ticks every interval and immediately on wake
// (a menu click). report is called with every result (not while idle).
//
// While the distro is down it does nothing to Incus. It starts the distro
// again (StartHost, at most every hostStartEvery), but never while "Release
// memory" runs: once that has finished, the distro comes back without the
// boxes' memory, so agent-box stays reachable (#204).
func run(ctx context.Context, cfg loopCfg, s Sensors, interval time.Duration, wake <-chan struct{}, report func(mode.Status)) {
	logf := func(format string, args ...any) {
		if cfg.Logf != nil {
			cfg.Logf(format, args...)
		}
	}
	var m *mode.Manager
	var lastStart time.Time
	tick := func() {
		if cfg.Idle != nil && cfg.Idle() {
			if m != nil {
				logf("an interactive tray is running: standing aside")
				m = nil
				s.HoldAwake(false)
			}
			if cfg.Maintain != nil {
				cfg.Maintain(ctx, false)
			}
			return
		}
		if m == nil {
			m = cfg.Manager()
			logf("enforcing as %s", m.Chosen())
		}
		var st mode.Status
		if s.HostUp(ctx) {
			now := time.Now()
			st = m.Tick(ctx, now)
			if cfg.Beat != nil && !st.Down {
				cfg.Beat(ctx, now)
			}
		} else {
			st = mode.Status{Chosen: m.Chosen(), Down: true}
			if now := time.Now(); now.Sub(lastStart) >= hostStartEvery {
				// Under the manager's guard, so the start and this
				// enforcer's release never overlap. Idle is asked again
				// there: a tray that has just started owns the release.
				tried, err := m.StartHost(ctx, func(ctx context.Context) error {
					if cfg.Idle != nil && cfg.Idle() {
						return errStandAside
					}
					logf("WSL distro is not running: starting it")
					return s.StartHost(ctx)
				})
				switch {
				case errors.Is(err, errStandAside):
				case tried:
					lastStart = now
					if err != nil {
						logf("starting the WSL distro: %v", err)
					}
				case err != nil:
					logf("starting the WSL distro, cut short by Release memory: %v", err)
				}
			}
		}
		s.HoldAwake(st.Running > 0)
		report(st)
		if cfg.Maintain != nil {
			cfg.Maintain(ctx, true)
		}
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
