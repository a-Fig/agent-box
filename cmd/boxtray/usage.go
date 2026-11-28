package main

import (
	"strconv"
	"strings"

	"github.com/a-Fig/agent-box/internal/dash"
)

// The whole machine's use, reported with the heartbeat (#294). The readings are Windows calls
// (usage_windows.go); the arithmetic and parsing are here, so they are tested on every OS.

// cpuTimes is GetSystemTimes' idle, kernel and user times, in 100ns ticks. Kernel includes idle.
type cpuTimes struct{ idle, kernel, user uint64 }

// busyPct is how busy all CPUs were between a and b, 0 to 100; 0 when no time passed.
func busyPct(a, b cpuTimes) float64 {
	total := (b.kernel - a.kernel) + (b.user - a.user)
	if b.kernel < a.kernel || b.user < a.user || b.idle < a.idle || total == 0 {
		return 0
	}
	idle := b.idle - a.idle
	if idle > total {
		return 0
	}
	return float64(total-idle) / float64(total) * 100
}

// parseNvidiaSMI reads the first GPU from
// `nvidia-smi --query-gpu=name,memory.total,memory.used,utilization.gpu --format=csv,noheader,nounits`,
// whose sizes are MiB. nil when the line isn't that.
func parseNvidiaSMI(out string) *dash.GPUUsage {
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	f := strings.Split(line, ",")
	if len(f) != 4 {
		return nil
	}
	for i := range f {
		f[i] = strings.TrimSpace(f[i])
	}
	total, err1 := strconv.ParseInt(f[1], 10, 64)
	used, err2 := strconv.ParseInt(f[2], 10, 64)
	pct, err3 := strconv.ParseFloat(f[3], 64)
	if err1 != nil || err2 != nil || err3 != nil || total <= 0 {
		return nil
	}
	return &dash.GPUUsage{Name: f[0], MemTotal: total << 20, MemUsed: used << 20, Pct: pct}
}
