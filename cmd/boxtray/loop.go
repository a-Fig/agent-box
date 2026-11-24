package main

import (
	"context"
	"time"

	"github.com/a-Fig/agent-box/internal/mode"
)

// Sensors are the Windows-side inputs and outputs; fakes stand in for tests.
type Sensors interface {
	Gaming() (bool, string)      // a game owns the PC
	HostUp(context.Context) bool // the WSL distro is running (we never start it)
	HoldAwake(on bool)           // keep the PC from sleeping
}

// run polls until ctx ends. It ticks every interval and immediately on wake
// (a menu click). report is called with every result.
//
// While the distro is down it does nothing to Incus: any wsl.exe call would
// start it again, undoing "Release memory".
func run(ctx context.Context, m *mode.Manager, s Sensors, interval time.Duration, wake <-chan struct{}, report func(mode.Status)) {
	tick := func() {
		var st mode.Status
		if s.HostUp(ctx) {
			gaming, reason := s.Gaming()
			st = m.Tick(ctx, time.Now(), gaming, reason)
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
