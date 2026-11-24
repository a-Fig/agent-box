package mode

import (
	"strings"
	"testing"
	"time"
)

func gpuFake(expires string) *fake {
	f := newFake("g", "Running", "a", "Running")
	f.config["g"] = map[string]string{SizeKey: GPUSize, ExpiresKey: expires}
	return f
}

const later = "2026-01-02T12:00:00Z" // t0 + 24h

func TestMyPCStopsGPUBoxInsteadOfPausing(t *testing.T) {
	f := gpuFake(later)
	m := New(f, MyPC)
	tick(m, 0, false)
	equal(t, f.take(), []string{
		"project user.agent-box.mode=mypc",
		"set g user.box.stopped=mypc", "stop g", "set g user.expires=2026-01-01T12:30:00Z",
		"pause a",
	})
	tick(m, time.Minute, false)
	equal(t, f.take(), nil)
	// Back in Shared, the GPU box stays stopped; the other box resumes.
	m.SetChosen(Shared)
	tick(m, 2*time.Minute, false)
	for _, c := range f.take() {
		if c == "resume g" || c == "start g" {
			t.Errorf("GPU box restarted: %s", c)
		}
	}
	if f.state["g"] != "Stopped" || f.state["a"] != "Running" {
		t.Errorf("states = %v", f.state)
	}
}

func TestGamingStopsGPUBoxWithReason(t *testing.T) {
	f := gpuFake(later)
	f.state["g"] = "Frozen" // an agent can't revive VRAM by pausing first
	m := New(f, Full)
	tick(m, 0, true)
	equal(t, f.take(), []string{
		"project user.agent-box.mode=mypc", "project user.agent-box.reason=gaming: steam:570",
		"set g user.box.stopped=gaming: steam:570", "stop g", "set g user.expires=2026-01-01T12:30:00Z",
		"pause a",
	})
	// The reaper deletes it once the short expiry passes, still yielding.
	tick(m, GPUStoppedTTL+time.Second, true)
	equal(t, f.take(), []string{"delete g", "stop a"}) // a has been frozen 15 minutes
}

func TestGPUStopKeepsEarlierExpiry(t *testing.T) {
	f := gpuFake("2026-01-01T12:10:00Z")
	f.config["g"][StoppedKey] = "mypc"
	m := New(f, MyPC)
	tick(m, 0, false)
	equal(t, f.take(), []string{"project user.agent-box.mode=mypc", "stop g", "pause a"})
}

func TestGPUStopFailurePausesAndRetries(t *testing.T) {
	f := gpuFake(later)
	f.failOn = "stop g"
	m := New(f, MyPC)
	st := tick(m, 0, false)
	if st.Err == nil || f.state["g"] != "Frozen" {
		t.Fatalf("err %v, state %s", st.Err, f.state["g"])
	}
	// No shortened expiry for a box that may resume: the reaper would delete it.
	for _, c := range f.take() {
		if strings.Contains(c, ExpiresKey) {
			t.Errorf("expiry changed before the stop worked: %s", c)
		}
	}
	f.failOn = ""
	tick(m, time.Second, false)
	equal(t, f.take(), []string{"stop g", "set g user.expires=2026-01-01T12:30:01Z"})
}

func TestGPUBoxCPUs(t *testing.T) {
	f := newFake("g", "Frozen")
	f.config["g"] = map[string]string{SizeKey: GPUSize}
	m := New(f, Shared)
	tick(m, 0, false)
	equal(t, f.take(), []string{"project user.agent-box.mode=shared", "set g limits.cpu=4", "set g limits.cpu.priority=0", "resume g"})
	m.SetChosen(Full)
	tick(m, time.Second, false)
	equal(t, f.take(), []string{"project user.agent-box.mode=full", "set g limits.cpu=6", "set g limits.cpu.priority=10"})
	if !IsGPU(f.config["g"]) || IsGPU(map[string]string{SizeKey: "large"}) {
		t.Error("IsGPU")
	}
}
