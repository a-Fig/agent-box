package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/a-Fig/agent-box/internal/mode"
)

func intPtr(n int) *int { return &n }

func pickTestSize(t *testing.T, name string) mode.Size {
	t.Helper()
	sz, err := lookupSize(name)
	if err != nil {
		t.Fatal(err)
	}
	return sz
}

func testSizes(small, medium, large, gpu int) []SizeRoom {
	return []SizeRoom{
		{Size: mode.Sizes[0], Fits: small},
		{Size: mode.Sizes[1], Fits: medium},
		{Size: mode.Sizes[2], Fits: large},
		{Size: mode.Sizes[3], Fits: gpu},
	}
}

func testCPUSizes(small, medium, large int) []SizeRoom {
	return []SizeRoom{
		{Size: mode.Sizes[0], Fits: small},
		{Size: mode.Sizes[1], Fits: medium},
		{Size: mode.Sizes[2], Fits: large},
	}
}

func readyHost(name, remote, project string) fleetCandidate {
	return fleetCandidate{
		Host:          fleetHost{Name: name, Remote: remote, Project: project},
		Reachable:     true,
		Mode:          string(mode.Full),
		Sizes:         testSizes(4, 2, 1, 1),
		HasGPU:        true,
		BoxdAvailable: project == mode.Project,
		Queued:        intPtr(0),
	}
}

func TestResolveFleetTargetsBypassAndPinning(t *testing.T) {
	def := fleetHost{Name: "pc", Remote: "pc", Project: mode.Project}
	configured := []fleetHost{
		{Name: "alpha", Remote: "r1", Project: mode.Project},
		{Name: "beta", Remote: "r2", Project: "work"},
	}

	t.Run("no registry and no pin bypasses fleet probes completely", func(t *testing.T) {
		targets, bypass, err := resolveFleetTargets(def, nil, "", "")
		if err != nil || !bypass || !slices.Equal(targets, []fleetHost{def}) {
			t.Fatalf("resolveFleetTargets = %v, %v, %v", targets, bypass, err)
		}
	})

	t.Run("no registry accepts pin matching default host or remote", func(t *testing.T) {
		targets, bypass, err := resolveFleetTargets(def, nil, "pc", "")
		if err != nil || !bypass || !slices.Equal(targets, []fleetHost{def}) {
			t.Fatalf("resolveFleetTargets = %v, %v, %v", targets, bypass, err)
		}
		unnamed := fleetHost{Remote: "pc", Project: mode.Project}
		targets, bypass, err = resolveFleetTargets(unnamed, nil, "pc", "")
		if err != nil || !bypass || !slices.Equal(targets, []fleetHost{unnamed}) {
			t.Fatalf("unnamed resolveFleetTargets = %v, %v, %v", targets, bypass, err)
		}
	})

	t.Run("no registry rejects unknown pin", func(t *testing.T) {
		if _, _, err := resolveFleetTargets(def, nil, "other", ""); err == nil || !strings.Contains(err.Error(), `host "other" is not configured`) {
			t.Fatalf("err = %v, want unknown host", err)
		}
	})

	t.Run("explicit --box without --host stays on default host and bypasses fleet", func(t *testing.T) {
		targets, bypass, err := resolveFleetTargets(def, configured, "", "named-box")
		if err != nil || !bypass || !slices.Equal(targets, []fleetHost{def}) {
			t.Fatalf("resolveFleetTargets = %v, %v, %v", targets, bypass, err)
		}
	})

	t.Run("explicit --box with --host pins that configured host", func(t *testing.T) {
		targets, bypass, err := resolveFleetTargets(def, configured, "beta", "named-box")
		if err != nil || bypass || !slices.Equal(targets, []fleetHost{configured[1]}) {
			t.Fatalf("resolveFleetTargets = %v, %v, %v", targets, bypass, err)
		}
	})

	t.Run("registry preserves configured order without pin", func(t *testing.T) {
		targets, bypass, err := resolveFleetTargets(def, configured, "", "")
		if err != nil || bypass || !slices.Equal(targets, configured) {
			t.Fatalf("resolveFleetTargets = %v, %v, %v", targets, bypass, err)
		}
		targets[0].Name = "mutated"
		if configured[0].Name != "alpha" {
			t.Fatal("resolveFleetTargets aliased configured slice")
		}
	})

	t.Run("registry rejects unknown pin and lists available hosts", func(t *testing.T) {
		_, _, err := resolveFleetTargets(def, configured, "gamma", "")
		if err == nil || !strings.Contains(err.Error(), "alpha, beta") {
			t.Fatalf("err = %v, want available hosts listed", err)
		}
	})

	t.Run("whitespace-only pin is rejected", func(t *testing.T) {
		if _, _, err := resolveFleetTargets(def, configured, "   ", ""); err == nil || !strings.Contains(err.Error(), "cannot be empty") {
			t.Fatalf("err = %v, want empty host name error", err)
		}
	})
}

func TestPickFleetHostStartEligibility(t *testing.T) {
	large := pickTestSize(t, "large")
	medium := pickTestSize(t, "medium")

	t.Run("skips unreachable, probe error, mypc, unrecognized mode, and full hosts", func(t *testing.T) {
		down := readyHost("down", "r1", mode.Project)
		down.Reachable = false
		down.Error = "connection refused"

		timedOut := readyHost("timed-out", "r2", mode.Project)
		timedOut.Error = "context deadline exceeded"

		paused := readyHost("paused", "r3", mode.Project)
		paused.Mode = string(mode.MyPC)

		weirdMode := readyHost("weird", "r4", mode.Project)
		weirdMode.Mode = "maintenance"

		noLarge := readyHost("small-only", "r5", mode.Project)
		noLarge.Sizes = testSizes(2, 1, 0, 0)

		okHost := readyHost("ok", "r6", mode.Project)
		okHost.Mode = "" // no tray (Linux server) is startable

		pick, err := pickFleetHost([]fleetCandidate{down, timedOut, paused, weirdMode, noLarge, okHost}, pickRequest{Size: large, ExplicitSize: true})
		if err != nil {
			t.Fatal(err)
		}
		if pick.Action != pickStart || pick.Host.Name != "ok" {
			t.Fatalf("pick = %+v, want start on ok", pick)
		}
	})

	t.Run("unlimited room (Fits == -1) is startable; missing size fails closed", func(t *testing.T) {
		missing := readyHost("missing", "r1", mode.Project)
		missing.Sizes = nil

		unlimited := readyHost("unlimited", "r2", mode.Project)
		unlimited.Sizes = testCPUSizes(-1, -1, -1)

		pick, err := pickFleetHost([]fleetCandidate{missing, unlimited}, pickRequest{Size: medium, ExplicitSize: true})
		if err != nil {
			t.Fatal(err)
		}
		if pick.Action != pickStart || pick.Host.Name != "unlimited" {
			t.Fatalf("pick = %+v, want start on unlimited", pick)
		}
	})

	t.Run("non-agents project can start when it has room", func(t *testing.T) {
		fullAgents := readyHost("full-agents", "r1", mode.Project)
		fullAgents.Sizes = testCPUSizes(0, 0, 0)

		customProject := readyHost("work-host", "r2", "work")
		customProject.Sizes = testCPUSizes(2, 1, 0)

		pick, err := pickFleetHost([]fleetCandidate{fullAgents, customProject}, pickRequest{Size: medium, ExplicitSize: true, Queue: true})
		if err != nil {
			t.Fatal(err)
		}
		if pick.Action != pickStart || pick.Host.Name != "work-host" {
			t.Fatalf("pick = %+v, want start on work-host", pick)
		}
	})
}

func TestPickFleetHostGPUSelection(t *testing.T) {
	gpu := pickTestSize(t, mode.GPUSize)

	noGPU := readyHost("cpu-only", "r1", mode.Project)
	noGPU.HasGPU = false
	noGPU.Sizes = testCPUSizes(4, 2, 1)

	busyGPU := readyHost("busy-gpu", "r2", mode.Project)
	busyGPU.GPUBusy = true

	fullGPU := readyHost("full-gpu", "r3", mode.Project)
	fullGPU.Sizes = testSizes(2, 1, 0, 0)

	freeGPU := readyHost("free-gpu", "r4", mode.Project)

	pick, err := pickFleetHost([]fleetCandidate{noGPU, busyGPU, fullGPU, freeGPU}, pickRequest{Size: gpu, ExplicitSize: true, Queue: true})
	if err != nil {
		t.Fatal(err)
	}
	if pick.Action != pickStart || pick.Host.Name != "free-gpu" {
		t.Fatalf("pick = %+v, want start on free-gpu", pick)
	}

	t.Run("GPU prefers host with repo evidence among startable GPU hosts", func(t *testing.T) {
		coldGPU := readyHost("cold-gpu", "r1", mode.Project)
		warmGPU := readyHost("warm-gpu", "r2", mode.Project)
		warmGPU.RepoBox = &RepoBox{Name: "repo", Size: "medium", Status: "Running"}

		pick, err := pickFleetHost([]fleetCandidate{coldGPU, warmGPU}, pickRequest{Size: gpu, ExplicitSize: true, Repo: "repo"})
		if err != nil {
			t.Fatal(err)
		}
		if pick.Action != pickStart || pick.Host.Name != "warm-gpu" || pick.Affinity != affinityWarmHost {
			t.Fatalf("pick = %+v, want start on warm-gpu with warm-host affinity", pick)
		}
	})

	t.Run("GPU never queues on My PC or CPU-only host", func(t *testing.T) {
		mypcGPU := readyHost("mypc-gpu", "r1", mode.Project)
		mypcGPU.Mode = string(mode.MyPC)

		pick, err := pickFleetHost([]fleetCandidate{noGPU, mypcGPU}, pickRequest{Size: gpu, ExplicitSize: true, Queue: true})
		if err != nil {
			t.Fatal(err)
		}
		if pick.Action != pickRefuse || pick.Host != (fleetHost{}) {
			t.Fatalf("pick = %+v, want refuse", pick)
		}
		if !strings.Contains(pick.Reason, "no GPU") || !strings.Contains(pick.Reason, "GPU run doesn't wait") {
			t.Fatalf("refuse reason = %q", pick.Reason)
		}
	})

	t.Run("GPU with --box requires pinned host and checks room for stopped GPU box", func(t *testing.T) {
		stoppedGPUFull := readyHost("gpu-full", "r1", mode.Project)
		stoppedGPUFull.Sizes = testSizes(2, 1, 0, 0)
		stoppedGPUFull.RepoBox = &RepoBox{Name: "mygpu", Size: mode.GPUSize, Status: "Stopped"}

		pick, err := pickFleetHost([]fleetCandidate{stoppedGPUFull}, pickRequest{Size: gpu, ExplicitSize: true, Box: "mygpu", Host: "gpu-full", Queue: true})
		if err != nil || pick.Action != pickQueue || pick.Host.Name != "gpu-full" || pick.Why != refusedFull {
			t.Fatalf("stopped GPU full pick = %+v, %v", pick, err)
		}

		runningGPUFull := readyHost("gpu-running", "r2", mode.Project)
		runningGPUFull.Sizes = testSizes(2, 1, 0, 0)
		runningGPUFull.RepoBox = &RepoBox{Name: "mygpu", Size: mode.GPUSize, Status: "Running"}

		pick, err = pickFleetHost([]fleetCandidate{runningGPUFull}, pickRequest{Size: gpu, ExplicitSize: true, Box: "mygpu", Host: "gpu-running"})
		if err != nil || pick.Action != pickStart || pick.Host.Name != "gpu-running" || pick.Affinity != affinityDirectWarm {
			t.Fatalf("running GPU pick = %+v, %v", pick, err)
		}
	})
}

func TestPickFleetHostAffinityStoppedCapacityAndStableOrder(t *testing.T) {
	large := pickTestSize(t, "large")

	t.Run("prefers idle running repo box over stopped-with-room, busy, repo-warm-host, and cold", func(t *testing.T) {
		cold := readyHost("cold", "r1", mode.Project)
		repoWarm := readyHost("repo-warm", "r2", mode.Project)
		repoWarm.WarmThrowaway = true
		busy := readyHost("busy", "r3", mode.Project)
		busy.RepoBox = &RepoBox{Name: "repo", Size: "medium", Status: "Running", Running: []JobInfo{{Job: "repo.1"}}}
		stopped := readyHost("stopped", "r4", mode.Project)
		stopped.RepoBox = &RepoBox{Name: "repo", Size: "medium", Status: "Stopped"}
		idle := readyHost("idle", "r5", mode.Project)
		idle.RepoBox = &RepoBox{Name: "repo", Size: "medium", Status: "Running"}

		pick, err := pickFleetHost([]fleetCandidate{cold, repoWarm, busy, stopped, idle}, pickRequest{Repo: "repo"})
		if err != nil || pick.Host.Name != "idle" || pick.Affinity != affinityDirectWarm {
			t.Fatalf("tier 1 pick = %+v, %v", pick, err)
		}
		pick, err = pickFleetHost([]fleetCandidate{cold, repoWarm, busy, stopped}, pickRequest{Repo: "repo"})
		if err != nil || pick.Host.Name != "stopped" || pick.Affinity != affinityRepoStopped {
			t.Fatalf("tier 2 pick = %+v, %v", pick, err)
		}
		pick, err = pickFleetHost([]fleetCandidate{cold, repoWarm, busy}, pickRequest{Repo: "repo"})
		if err != nil || pick.Host.Name != "busy" || pick.Affinity != affinityRepoBusy {
			t.Fatalf("tier 3 pick = %+v, %v", pick, err)
		}
		pick, err = pickFleetHost([]fleetCandidate{cold, repoWarm}, pickRequest{Repo: "repo"})
		if err != nil || pick.Host.Name != "repo-warm" || pick.Affinity != affinityWarmHost {
			t.Fatalf("tier 4 pick = %+v, %v", pick, err)
		}
	})

	t.Run("running matching repo box starts without new room; stopped matching repo box requires capacity", func(t *testing.T) {
		coldRoom := readyHost("cold-room", "r1", mode.Project)

		stoppedFull := readyHost("stopped-full", "r2", mode.Project)
		stoppedFull.Sizes = testCPUSizes(0, 0, 0)
		stoppedFull.RepoBox = &RepoBox{Name: "repo", Size: "medium", Status: "Stopped"}

		// Stopped box without capacity is NOT startable, so cold-room wins.
		pick, err := pickFleetHost([]fleetCandidate{stoppedFull, coldRoom}, pickRequest{Repo: "repo"})
		if err != nil || pick.Action != pickStart || pick.Host.Name != "cold-room" {
			t.Fatalf("stopped-without-room pick = %+v, %v", pick, err)
		}

		// Stopped box alone without capacity queues (when boxd capable) instead of starting.
		pick, err = pickFleetHost([]fleetCandidate{stoppedFull}, pickRequest{Repo: "repo", Queue: true})
		if err != nil || pick.Action != pickQueue || pick.Host.Name != "stopped-full" || pick.Why != refusedFull {
			t.Fatalf("stopped-without-room queue = %+v, %v", pick, err)
		}

		// Already-running matching repo box starts even with 0 remaining room.
		runningFull := readyHost("running-full", "r3", mode.Project)
		runningFull.Sizes = testCPUSizes(0, 0, 0)
		runningFull.RepoBox = &RepoBox{Name: "repo", Size: "small", Status: "Running"}

		pick, err = pickFleetHost([]fleetCandidate{coldRoom, runningFull}, pickRequest{Repo: "repo"})
		if err != nil || pick.Action != pickStart || pick.Host.Name != "running-full" || pick.Affinity != affinityDirectWarm {
			t.Fatalf("running-without-room pick = %+v, %v", pick, err)
		}
	})

	t.Run("stopped repo box checks capacity for its own size when Size is unspecified", func(t *testing.T) {
		stoppedLarge := readyHost("stopped-large", "r1", mode.Project)
		stoppedLarge.Sizes = testCPUSizes(2, 1, 0) // medium fits, large does not
		stoppedLarge.RepoBox = &RepoBox{Name: "repo", Size: "large", Status: "Stopped"}

		coldRoom := readyHost("cold-room", "r2", mode.Project)

		pick, err := pickFleetHost([]fleetCandidate{stoppedLarge, coldRoom}, pickRequest{Repo: "repo"})
		if err != nil || pick.Action != pickStart || pick.Host.Name != "cold-room" {
			t.Fatalf("stopped-large pick = %+v, %v", pick, err)
		}
	})

	t.Run("no repo evidence or mismatched RepoBox name is not ranked", func(t *testing.T) {
		first := readyHost("first", "r1", mode.Project)
		second := readyHost("second", "r2", mode.Project)
		second.WarmThrowaway = true
		second.RepoBox = &RepoBox{Name: "other-repo", Size: "medium", Status: "Running"}

		// Without req.Repo, neither host has repo-scoped evidence; config order wins.
		pick, err := pickFleetHost([]fleetCandidate{first, second}, pickRequest{New: true})
		if err != nil || pick.Action != pickStart || pick.Host.Name != "first" || pick.Affinity != affinityCold {
			t.Fatalf("no-repo pick = %+v, %v", pick, err)
		}

		// Unnamed or different-repo RepoBox does not match req.Repo.
		unnamed := readyHost("unnamed", "r3", mode.Project)
		unnamed.RepoBox = &RepoBox{Size: "medium", Status: "Running"}
		other := readyHost("other", "r4", mode.Project)
		other.RepoBox = &RepoBox{Name: "other-repo", Size: "medium", Status: "Running"}
		pick, err = pickFleetHost([]fleetCandidate{first, unnamed, other}, pickRequest{Repo: "repo"})
		if err != nil || pick.Action != pickStart || pick.Host.Name != "first" || pick.Affinity != affinityCold {
			t.Fatalf("mismatched RepoBox pick = %+v, %v", pick, err)
		}
	})

	t.Run("size mismatch on repo box falls back to throwaway room check", func(t *testing.T) {
		fullMismatch := readyHost("mismatch-full", "r1", mode.Project)
		fullMismatch.Sizes = testCPUSizes(0, 0, 0)
		fullMismatch.RepoBox = &RepoBox{Name: "repo", Size: "small", Status: "Running"}

		roomHost := readyHost("has-large", "r2", mode.Project)

		pick, err := pickFleetHost([]fleetCandidate{fullMismatch, roomHost}, pickRequest{Repo: "repo", Size: large, ExplicitSize: true})
		if err != nil || pick.Action != pickStart || pick.Host.Name != "has-large" {
			t.Fatalf("pick = %+v, %v", pick, err)
		}
	})

	t.Run("throwaway run prefers claimable warm throwaway only on boxd-capable agents host", func(t *testing.T) {
		cold := readyHost("cold", "r1", mode.Project)

		nonAgentsWarm := readyHost("non-agents-warm", "r2", "other")
		nonAgentsWarm.Sizes = testCPUSizes(0, 0, 0)
		nonAgentsWarm.BoxdAvailable = true
		nonAgentsWarm.WarmThrowaway = true // ignored outside mode.Project

		noBoxdWarm := readyHost("no-boxd-warm", "r3", mode.Project)
		noBoxdWarm.Sizes = testCPUSizes(0, 0, 0)
		noBoxdWarm.BoxdAvailable = false
		noBoxdWarm.WarmThrowaway = true // ignored without boxd capability

		agentsWarm := readyHost("agents-warm", "r4", mode.Project)
		agentsWarm.Sizes = testCPUSizes(0, 0, 0)
		agentsWarm.WarmThrowaway = true

		pick, err := pickFleetHost([]fleetCandidate{cold, nonAgentsWarm, noBoxdWarm, agentsWarm}, pickRequest{Repo: "repo", New: true})
		if err != nil || pick.Action != pickStart || pick.Host.Name != "agents-warm" || pick.Affinity != affinityDirectWarm {
			t.Fatalf("pick = %+v, %v", pick, err)
		}

		// With --fresh, warm throwaway cannot be claimed so full host is skipped;
		// a host with room + warm repo evidence still beats a cold host.
		agentsWarmWithRoom := readyHost("agents-warm-room", "r5", mode.Project)
		agentsWarmWithRoom.WarmThrowaway = true
		pick, err = pickFleetHost([]fleetCandidate{cold, agentsWarm, agentsWarmWithRoom}, pickRequest{Repo: "repo", New: true, Fresh: true})
		if err != nil || pick.Action != pickStart || pick.Host.Name != "agents-warm-room" || pick.Affinity != affinityWarmHost {
			t.Fatalf("fresh pick = %+v, %v", pick, err)
		}
	})

	t.Run("frozen or throwaway-named repo box is not startable for non-new run", func(t *testing.T) {
		frozen := readyHost("frozen", "r1", mode.Project)
		frozen.RepoBox = &RepoBox{Name: "repo", Size: "medium", Status: "Frozen"}

		badThrowaway := readyHost("throwaway-name", "r2", mode.Project)
		badThrowaway.RepoBox = &RepoBox{Name: "repo", Size: "medium", Status: "Running"}
		badThrowaway.RepoBoxThrowaway = true

		clean := readyHost("clean", "r3", mode.Project)

		pick, err := pickFleetHost([]fleetCandidate{frozen, badThrowaway, clean}, pickRequest{Repo: "repo"})
		if err != nil || pick.Action != pickStart || pick.Host.Name != "clean" {
			t.Fatalf("pick = %+v, %v", pick, err)
		}
	})

	t.Run("ties break strictly by stable configuration order", func(t *testing.T) {
		first := readyHost("first", "r1", mode.Project)
		second := readyHost("second", "r2", mode.Project)
		third := readyHost("third", "r3", mode.Project)

		pick, err := pickFleetHost([]fleetCandidate{first, second, third}, pickRequest{Repo: "repo"})
		if err != nil || pick.Action != pickStart || pick.Host.Name != "first" {
			t.Fatalf("pick = %+v, %v", pick, err)
		}
		pick, err = pickFleetHost([]fleetCandidate{third, second, first}, pickRequest{Repo: "repo"})
		if err != nil || pick.Action != pickStart || pick.Host.Name != "third" {
			t.Fatalf("reversed pick = %+v, %v", pick, err)
		}
	})
}

func TestPickFleetHostQueueDestinationAndBoxdGuards(t *testing.T) {
	gpu := pickTestSize(t, mode.GPUSize)

	t.Run("start-now always beats queueing on a warm host", func(t *testing.T) {
		warmFull := readyHost("warm-full", "r1", mode.Project)
		warmFull.Sizes = testCPUSizes(0, 0, 0)
		warmFull.RepoBox = &RepoBox{Name: "repo", Size: "medium", Status: "Stopped"}

		coldFree := readyHost("cold-free", "r2", mode.Project)

		pick, err := pickFleetHost([]fleetCandidate{warmFull, coldFree}, pickRequest{Repo: "repo", Queue: true})
		if err != nil || pick.Action != pickStart || pick.Host.Name != "cold-free" {
			t.Fatalf("pick = %+v, %v", pick, err)
		}
	})

	t.Run("queue requires positively established boxd capability on agents project", func(t *testing.T) {
		customFull := readyHost("custom-full", "r1", "work")
		customFull.Sizes = testCPUSizes(0, 0, 0)
		customFull.BoxdAvailable = true
		customFull.Queued = intPtr(0)

		noBoxdFull := readyHost("no-boxd", "r2", mode.Project)
		noBoxdFull.Sizes = testCPUSizes(0, 0, 0)
		noBoxdFull.BoxdAvailable = false
		noBoxdFull.Queued = nil

		// Neither non-agents nor unverified boxd can accept queued jobs.
		pick, err := pickFleetHost([]fleetCandidate{customFull, noBoxdFull}, pickRequest{Queue: true})
		if err != nil || pick.Action != pickRefuse ||
			!strings.Contains(pick.Reason, `boxd handles project "agents" only`) ||
			!strings.Contains(pick.Reason, "boxd queue capability not established") {
			t.Fatalf("refuse pick = %+v, %v", pick, err)
		}

		// Unknown depth IS allowed when BoxdAvailable was positively established on agents.
		capableUnknownDepth := readyHost("capable-unknown", "r3", mode.Project)
		capableUnknownDepth.Sizes = testCPUSizes(0, 0, 0)
		capableUnknownDepth.BoxdAvailable = true
		capableUnknownDepth.Queued = nil

		pick, err = pickFleetHost([]fleetCandidate{customFull, noBoxdFull, capableUnknownDepth}, pickRequest{Queue: true})
		if err != nil || pick.Action != pickQueue || pick.Host.Name != "capable-unknown" || pick.Why != refusedFull {
			t.Fatalf("capable-unknown pick = %+v, %v", pick, err)
		}
	})

	t.Run("queue ranking: active before mypc > known depth > shorter queue > repo affinity > config order", func(t *testing.T) {
		mypcShort := readyHost("mypc-short", "r1", mode.Project)
		mypcShort.Mode = string(mode.MyPC)
		mypcShort.Queued = intPtr(0)

		fullUnknownDepth := readyHost("full-unknown-depth", "r2", mode.Project)
		fullUnknownDepth.Sizes = testCPUSizes(0, 0, 0)
		fullUnknownDepth.BoxdAvailable = true
		fullUnknownDepth.Queued = nil

		fullLong := readyHost("full-long", "r3", mode.Project)
		fullLong.Sizes = testCPUSizes(0, 0, 0)
		fullLong.Queued = intPtr(4)

		fullShortCold := readyHost("full-short-cold", "r4", mode.Project)
		fullShortCold.Sizes = testCPUSizes(0, 0, 0)
		fullShortCold.Queued = intPtr(1)

		fullShortWarm := readyHost("full-short-warm", "r5", mode.Project)
		fullShortWarm.Sizes = testCPUSizes(0, 0, 0)
		fullShortWarm.Queued = intPtr(1)
		fullShortWarm.RepoBox = &RepoBox{Name: "repo", Size: "medium", Status: "Stopped"}

		// Active + known + shorter + repo affinity beats all others.
		pick, err := pickFleetHost([]fleetCandidate{mypcShort, fullUnknownDepth, fullLong, fullShortCold, fullShortWarm}, pickRequest{Repo: "repo", Queue: true})
		if err != nil || pick.Action != pickQueue || pick.Host.Name != "full-short-warm" || pick.Why != refusedFull {
			t.Fatalf("best queue pick = %+v, %v", pick, err)
		}

		// Shorter queue beats longer queue even if longer is earlier in config.
		pick, err = pickFleetHost([]fleetCandidate{mypcShort, fullUnknownDepth, fullLong, fullShortCold}, pickRequest{Repo: "repo", Queue: true})
		if err != nil || pick.Action != pickQueue || pick.Host.Name != "full-short-cold" {
			t.Fatalf("shorter queue pick = %+v, %v", pick, err)
		}

		// Known depth beats unknown depth among active hosts.
		pick, err = pickFleetHost([]fleetCandidate{mypcShort, fullUnknownDepth, fullLong}, pickRequest{Repo: "repo", Queue: true})
		if err != nil || pick.Action != pickQueue || pick.Host.Name != "full-long" {
			t.Fatalf("known-depth pick = %+v, %v", pick, err)
		}

		// Active host with positively established boxd capability beats mypc even with unknown depth.
		pick, err = pickFleetHost([]fleetCandidate{mypcShort, fullUnknownDepth}, pickRequest{Repo: "repo", Queue: true})
		if err != nil || pick.Action != pickQueue || pick.Host.Name != "full-unknown-depth" {
			t.Fatalf("active-over-mypc pick = %+v, %v", pick, err)
		}
	})

	t.Run("GPU queues on busy or full boxd-capable agents GPU host", func(t *testing.T) {
		noGPU := readyHost("cpu", "r1", mode.Project)
		noGPU.HasGPU = false
		noGPU.Sizes = testCPUSizes(4, 2, 1)

		busyOtherProject := readyHost("busy-work", "r2", "work")
		busyOtherProject.GPUBusy = true
		busyOtherProject.BoxdAvailable = true

		busyNoBoxd := readyHost("busy-noboxd", "r3", mode.Project)
		busyNoBoxd.GPUBusy = true
		busyNoBoxd.BoxdAvailable = false

		busyAgents := readyHost("busy-agents", "r4", mode.Project)
		busyAgents.GPUBusy = true
		busyAgents.Queued = intPtr(1)

		pick, err := pickFleetHost([]fleetCandidate{noGPU, busyOtherProject, busyNoBoxd, busyAgents}, pickRequest{Size: gpu, ExplicitSize: true, Queue: true})
		if err != nil || pick.Action != pickQueue || pick.Host.Name != "busy-agents" || pick.Why != refusedGPUBusy {
			t.Fatalf("gpu queue pick = %+v, %v", pick, err)
		}
	})

	t.Run("Queue false refuses instead of queueing", func(t *testing.T) {
		full := readyHost("full", "r1", mode.Project)
		full.Sizes = testCPUSizes(0, 0, 0)

		pick, err := pickFleetHost([]fleetCandidate{full}, pickRequest{Queue: false})
		if err != nil || pick.Action != pickRefuse || pick.Why != refusedFull || pick.Host != (fleetHost{}) {
			t.Fatalf("no-queue pick = %+v, %v", pick, err)
		}
	})
}

func TestPickFleetHostPinnedAndExplicitBoxGuards(t *testing.T) {
	first := readyHost("first", "r1", mode.Project)
	pinned := readyHost("pinned", "r2", mode.Project)

	t.Run("pin selects pinned host even when another host has higher affinity", func(t *testing.T) {
		firstWarm := first
		firstWarm.RepoBox = &RepoBox{Name: "repo", Size: "medium", Status: "Running"}

		pick, err := pickFleetHost([]fleetCandidate{firstWarm, pinned}, pickRequest{Repo: "repo", Host: "pinned"})
		if err != nil || pick.Action != pickStart || pick.Host.Name != "pinned" {
			t.Fatalf("pinned start = %+v, %v", pick, err)
		}
	})

	t.Run("pin queues on pinned host instead of spilling over to another startable host", func(t *testing.T) {
		pinnedFull := pinned
		pinnedFull.Sizes = testCPUSizes(0, 0, 0)

		pick, err := pickFleetHost([]fleetCandidate{first, pinnedFull}, pickRequest{Host: "pinned", Queue: true})
		if err != nil || pick.Action != pickQueue || pick.Host.Name != "pinned" || pick.Why != refusedFull {
			t.Fatalf("pinned queue = %+v, %v", pick, err)
		}
	})

	t.Run("pin refuses when pinned host cannot start or queue, never falls back", func(t *testing.T) {
		pinnedDown := pinned
		pinnedDown.Reachable = false
		pinnedDown.Error = "timeout"

		pick, err := pickFleetHost([]fleetCandidate{first, pinnedDown}, pickRequest{Host: "pinned", Queue: true})
		if err != nil || pick.Action != pickRefuse || pick.Host != (fleetHost{}) || !strings.Contains(pick.Reason, "host pinned: unreachable") {
			t.Fatalf("pinned refuse = %+v, %v", pick, err)
		}
	})

	t.Run("pin on non-agents full host refuses instead of queueing elsewhere", func(t *testing.T) {
		pinnedWork := readyHost("pinned-work", "r2", "work")
		pinnedWork.Sizes = testCPUSizes(0, 0, 0)

		pick, err := pickFleetHost([]fleetCandidate{first, pinnedWork}, pickRequest{Host: "pinned-work", Queue: true})
		if err != nil || pick.Action != pickRefuse || !strings.Contains(pick.Reason, `boxd handles project "agents" only`) {
			t.Fatalf("pinned work refuse = %+v, %v", pick, err)
		}
	})

	t.Run("explicit --box without --host rejects multi-host pool", func(t *testing.T) {
		firstBox := first
		firstBox.RepoBox = &RepoBox{Name: "shared-name", Size: "medium", Status: "Running"}
		secondBox := pinned
		secondBox.RepoBox = &RepoBox{Name: "shared-name", Size: "medium", Status: "Running"}

		if _, err := pickFleetHost([]fleetCandidate{firstBox, secondBox}, pickRequest{Box: "shared-name"}); err == nil || !strings.Contains(err.Error(), "requires --host") {
			t.Fatalf("err = %v, want requires --host error", err)
		}
	})

	t.Run("unknown or blank pin errors", func(t *testing.T) {
		if _, err := pickFleetHost([]fleetCandidate{first, pinned}, pickRequest{Host: "missing"}); err == nil || !strings.Contains(err.Error(), `host "missing" is not configured`) {
			t.Fatalf("err = %v, want unknown host error", err)
		}
		if _, err := pickFleetHost([]fleetCandidate{first, pinned}, pickRequest{Host: "   "}); err == nil || !strings.Contains(err.Error(), "cannot be empty") {
			t.Fatalf("err = %v, want empty host name error", err)
		}
	})
}
