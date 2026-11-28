//go:build windows

package main

import (
	"context"
	"os/exec"
	"runtime"
	"time"
	"unsafe"

	"github.com/a-Fig/agent-box/internal/dash"
)

var (
	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

// memoryStatusEx is MEMORYSTATUSEX.
type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

// usageReader reads the machine's use. CPU use is over the time since the last read, the
// heartbeat's 30s. A first read, or one long after the last (an enforcer that stood aside, or the
// distro down), samples half a second instead, so it says what is busy now. Only the enforcer's
// loop calls it.
type usageReader struct {
	last   *cpuTimes
	lastAt time.Time
}

func (r *usageReader) read(ctx context.Context) *dash.HostUsage {
	var ms memoryStatusEx
	ms.length = uint32(unsafe.Sizeof(ms))
	if ok, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms))); ok == 0 {
		return nil
	}
	now, ok := systemTimes()
	if !ok {
		return nil
	}
	prev := r.last
	if prev == nil || time.Since(r.lastAt) > 2*heartbeatEvery {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
		p := now
		prev = &p
		if now, ok = systemTimes(); !ok {
			return nil
		}
	}
	r.last, r.lastAt = &now, time.Now()
	return &dash.HostUsage{
		At:       time.Now().UTC().Truncate(time.Second),
		CPUs:     runtime.NumCPU(),
		CPUPct:   busyPct(*prev, now),
		MemTotal: int64(ms.totalPhys),
		MemUsed:  int64(ms.totalPhys - ms.availPhys),
		GPU:      gpuUsage(ctx),
	}
}

func systemTimes() (cpuTimes, bool) {
	var idle, kernel, user [2]uint32 // FILETIME
	ok, _, _ := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user)))
	ft := func(f [2]uint32) uint64 { return uint64(f[1])<<32 | uint64(f[0]) }
	return cpuTimes{ft(idle), ft(kernel), ft(user)}, ok != 0
}

// gpuUsage asks nvidia-smi, which the NVIDIA driver installs; nil without one.
func gpuUsage(ctx context.Context) *dash.GPUUsage {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := hidden(exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=name,memory.total,memory.used,utilization.gpu", "--format=csv,noheader,nounits")).Output()
	if err != nil {
		return nil
	}
	return parseNvidiaSMI(string(out))
}
