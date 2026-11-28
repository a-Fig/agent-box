package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/mode"
)

func TestBusyPct(t *testing.T) {
	a := cpuTimes{idle: 100, kernel: 300, user: 100} // kernel includes idle
	for _, c := range []struct {
		b    cpuTimes
		want float64
	}{
		{cpuTimes{idle: 175, kernel: 400, user: 100}, 25}, // 100 ticks, 75 idle
		{cpuTimes{idle: 100, kernel: 350, user: 150}, 100},
		{a, 0}, // no time passed
		{cpuTimes{idle: 50, kernel: 400, user: 200}, 0}, // a counter went back
	} {
		if got := busyPct(a, c.b); got != c.want {
			t.Errorf("busyPct(%v) = %v, want %v", c.b, got, c.want)
		}
	}
}

func TestParseNvidiaSMI(t *testing.T) {
	g := parseNvidiaSMI("NVIDIA GeForce RTX 3080, 10240, 1536, 7\nNVIDIA second, 1, 1, 1\n")
	if g == nil || g.Name != "NVIDIA GeForce RTX 3080" || g.MemTotal != 10240<<20 || g.MemUsed != 1536<<20 || g.Pct != 7 {
		t.Fatalf("got %+v", g)
	}
	for _, bad := range []string{"", "NVIDIA-SMI has failed", "x, 0, 0, 0", "x, a, 1, 2"} {
		if g := parseNvidiaSMI(bad); g != nil {
			t.Errorf("%q: got %+v", bad, g)
		}
	}
}

// The heartbeat writes the machine's use after tray-seen, with every beat it writes, and a reading
// that fails writes nothing: the heartbeat still counts.
func TestHeartbeatWritesHostUsage(t *testing.T) {
	inc := &recIncus{}
	hb := newHeartbeat(inc, mode.EnforcerTray, t.Logf)
	reads := 0
	hb.usage = func(context.Context) *dash.HostUsage {
		reads++
		if reads == 2 {
			return nil
		}
		return &dash.HostUsage{CPUs: 24, CPUPct: 12.5, MemTotal: 32 << 30, MemUsed: 9 << 30}
	}
	t0 := time.Date(2026, 11, 27, 3, 0, 0, 0, time.UTC)
	hb.beat(context.Background(), t0)
	hb.beat(context.Background(), t0.Add(30*time.Second))
	got := inc.snapshot()
	if len(got) != 6 || !strings.HasPrefix(got[3], mode.ProjectHostUsageKey+`={"at":`) || !strings.Contains(got[3], `"cpus":24`) ||
		got[5] != "user.agent-box.tray-seen=2026-11-27T03:00:30Z" {
		t.Fatalf("writes = %v", got)
	}
}
