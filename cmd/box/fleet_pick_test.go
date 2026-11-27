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

	t.Run("compatible aliases collapse in registry order and resolve dashboard URL consistently", func(t *testing.T) {
		aliased := []fleetHost{
			{Name: "primary", Remote: "pc", Project: mode.Project},
			{Name: "primary-dash", Remote: "pc", Project: mode.Project, DashboardURL: "https://pc.example.com"},
			{Name: "spare", Remote: "spare", Project: mode.Project},
		}
		targets, bypass, err := resolveFleetTargets(def, aliased, "", "")
		want := []fleetHost{
			{Name: "primary", Remote: "pc", Project: mode.Project, DashboardURL: "https://pc.example.com"},
			{Name: "spare", Remote: "spare", Project: mode.Project},
		}
		if err != nil || bypass || !slices.Equal(targets, want) {
			t.Fatalf("unpinned aliases = %v, %v, %v; want %v", targets, bypass, err, want)
		}

		// Pinning an alias keeps that alias name with the tuple's resolved dashboard URL.
		targets, bypass, err = resolveFleetTargets(def, aliased, "primary", "")
		if err != nil || bypass || !slices.Equal(targets, []fleetHost{{Name: "primary", Remote: "pc", Project: mode.Project, DashboardURL: "https://pc.example.com"}}) {
			t.Fatalf("pinned primary = %v, %v, %v", targets, bypass, err)
		}
		targets, bypass, err = resolveFleetTargets(def, aliased, "primary-dash", "")
		if err != nil || bypass || !slices.Equal(targets, []fleetHost{{Name: "primary-dash", Remote: "pc", Project: mode.Project, DashboardURL: "https://pc.example.com"}}) {
			t.Fatalf("pinned primary-dash = %v, %v, %v", targets, bypass, err)
		}
	})

	t.Run("conflicting dashboard URLs on same tuple are rejected unpinned and pinned", func(t *testing.T) {
		conflicting := []fleetHost{
			{Name: "a", Remote: "pc", Project: mode.Project, DashboardURL: "https://one.example.com"},
			{Name: "b", Remote: "pc", Project: mode.Project, DashboardURL: "https://two.example.com"},
			{Name: "other", Remote: "spare", Project: mode.Project},
		}
		if _, _, err := resolveFleetTargets(def, conflicting, "", ""); err == nil || !strings.Contains(err.Error(), "conflicting dashboard URLs") {
			t.Fatalf("unpinned conflicting = %v, want conflicting dashboard URLs", err)
		}
		if _, _, err := resolveFleetTargets(def, conflicting, "a", ""); err == nil || !strings.Contains(err.Error(), "conflicting dashboard URLs") {
			t.Fatalf("pinned conflicting = %v, want conflicting dashboard URLs", err)
		}
		// Pinning an unrelated unambiguous tuple still succeeds.
		targets, bypass, err := resolveFleetTargets(def, conflicting, "other", "")
		if err != nil || bypass || !slices.Equal(targets, []fleetHost{{Name: "other", Remote: "spare", Project: mode.Project}}) {
			t.Fatalf("pinned unambiguous = %v, %v, %v", targets, bypass, err)
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

		pick := pickFleetHost([]fleetCandidate{down, timedOut, paused, weirdMode, noLarge, okHost}, pickRequest{Size: large, ExplicitSize: true})
		if pick.Action != pickStart || pick.Host.Name != "ok" {
			t.Fatalf("pick = %+v, want start on ok", pick)
		}
	})

	t.Run("unlimited room (Fits == -1) is startable; missing size fails closed", func(t *testing.T) {
		missing := readyHost("missing", "r1", mode.Project)
		missing.Sizes = nil

		unlimited := readyHost("unlimited", "r2", mode.Project)
		unlimited.Sizes = testCPUSizes(-1, -1, -1)

		pick := pickFleetHost([]fleetCandidate{missing, unlimited}, pickRequest{Size: medium, ExplicitSize: true})
		if pick.Action != pickStart || pick.Host.Name != "unlimited" {
			t.Fatalf("pick = %+v, want start on unlimited", pick)
		}
	})

	t.Run("non-agents project can start when it has room", func(t *testing.T) {
		fullAgents := readyHost("full-agents", "r1", mode.Project)
		fullAgents.Sizes = testCPUSizes(0, 0, 0)

		customProject := readyHost("work-host", "r2", "work")
		customProject.Sizes = testCPUSizes(2, 1, 0)

		pick := pickFleetHost([]fleetCandidate{fullAgents, customProject}, pickRequest{Size: medium, ExplicitSize: true, Queue: true})
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
	busyGPU.GPUHolders = []string{"other-gpu"}

	fullGPU := readyHost("full-gpu", "r3", mode.Project)
	fullGPU.Sizes = testSizes(2, 1, 0, 0)

	freeGPU := readyHost("free-gpu", "r4", mode.Project)

	pick := pickFleetHost([]fleetCandidate{noGPU, busyGPU, fullGPU, freeGPU}, pickRequest{Size: gpu, ExplicitSize: true, Queue: true})
	if pick.Action != pickStart || pick.Host.Name != "free-gpu" {
		t.Fatalf("pick = %+v, want start on free-gpu", pick)
	}

	t.Run("GPU prefers host with repo evidence among startable GPU hosts", func(t *testing.T) {
		coldGPU := readyHost("cold-gpu", "r1", mode.Project)
		warmGPU := readyHost("warm-gpu", "r2", mode.Project)
		warmGPU.RepoBox = &RepoBox{Name: "repo", Size: "medium", Status: "Running"}

		pick := pickFleetHost([]fleetCandidate{coldGPU, warmGPU}, pickRequest{Size: gpu, ExplicitSize: true, Repo: "repo"})
		if pick.Action != pickStart || pick.Host.Name != "warm-gpu" || pick.Affinity != affinityWarmHost {
			t.Fatalf("pick = %+v, want start on warm-gpu with warm-host affinity", pick)
		}
	})

	t.Run("GPU never queues on My PC or CPU-only host", func(t *testing.T) {
		mypcGPU := readyHost("mypc-gpu", "r1", mode.Project)
		mypcGPU.Mode = string(mode.MyPC)

		pick := pickFleetHost([]fleetCandidate{noGPU, mypcGPU}, pickRequest{Size: gpu, ExplicitSize: true, Queue: true})
		if pick.Action != pickRefuse || pick.Host != (fleetHost{}) {
			t.Fatalf("pick = %+v, want refuse", pick)
		}
		if !strings.Contains(pick.Reason, "no GPU") || !strings.Contains(pick.Reason, "GPU run doesn't wait") {
			t.Fatalf("refuse reason = %q", pick.Reason)
		}
	})

	t.Run("reusable GPU --box runs when it is the only GPU holder, even at allocation cap", func(t *testing.T) {
		runningSelf := readyHost("gpu-self", "r1", mode.Project)
		runningSelf.Sizes = testSizes(0, 0, 0, 0) // at allocation cap; existing box already counted
		runningSelf.GPUHolders = []string{"mygpu"}
		runningSelf.RepoBox = &RepoBox{Name: "mygpu", Size: mode.GPUSize, Status: "Running"}

		pick := pickFleetHost([]fleetCandidate{runningSelf}, pickRequest{Size: gpu, ExplicitSize: true, Box: "mygpu"})
		if pick.Action != pickStart || pick.Host.Name != "gpu-self" || pick.Affinity != affinityDirectWarm {
			t.Fatalf("running self-holder pick = %+v, want start", pick)
		}

		// Stopped named GPU box at the allocation cap can also start (ensureBox starts it).
		stoppedSelf := readyHost("gpu-stopped", "r2", mode.Project)
		stoppedSelf.Sizes = testSizes(0, 0, 0, 0)
		stoppedSelf.RepoBox = &RepoBox{Name: "mygpu", Size: mode.GPUSize, Status: "Stopped"}

		pick = pickFleetHost([]fleetCandidate{stoppedSelf}, pickRequest{Size: gpu, ExplicitSize: true, Box: "mygpu", Queue: true})
		if pick.Action != pickStart || pick.Host.Name != "gpu-stopped" || pick.Affinity != affinityRepoStopped {
			t.Fatalf("stopped GPU box at cap pick = %+v, want start", pick)
		}

		// Another GPU box holding the GPU still blocks the named GPU box.
		otherHolder := runningSelf
		otherHolder.GPUHolders = []string{"mygpu", "other-gpu"}

		pick = pickFleetHost([]fleetCandidate{otherHolder}, pickRequest{Size: gpu, ExplicitSize: true, Box: "mygpu", Queue: true})
		if pick.Action != pickQueue || pick.Host.Name != "gpu-self" || pick.Why != refusedGPUBusy {
			t.Fatalf("other-holder pick = %+v, want queue gpu-busy", pick)
		}
	})
}

func TestPickFleetHostAffinityStoppedAtCapAndStableOrder(t *testing.T) {
	large := pickTestSize(t, "large")

	t.Run("prefers idle running repo box over stopped, busy, repo-warm-host, and cold", func(t *testing.T) {
		cold := readyHost("cold", "r1", mode.Project)
		repoWarm := readyHost("repo-warm", "r2", mode.Project)
		repoWarm.WarmThrowaway = true
		busy := readyHost("busy", "r3", mode.Project)
		busy.RepoBox = &RepoBox{Name: "repo", Size: "medium", Status: "Running", Running: []JobInfo{{Job: "repo.1"}}}
		stopped := readyHost("stopped", "r4", mode.Project)
		stopped.RepoBox = &RepoBox{Name: "repo", Size: "medium", Status: "Stopped"}
		idle := readyHost("idle", "r5", mode.Project)
		idle.RepoBox = &RepoBox{Name: "repo", Size: "medium", Status: "Running"}

		pick := pickFleetHost([]fleetCandidate{cold, repoWarm, busy, stopped, idle}, pickRequest{Repo: "repo"})
		if pick.Host.Name != "idle" || pick.Affinity != affinityDirectWarm {
			t.Fatalf("tier 1 pick = %+v", pick)
		}
		pick = pickFleetHost([]fleetCandidate{cold, repoWarm, busy, stopped}, pickRequest{Repo: "repo"})
		if pick.Host.Name != "stopped" || pick.Affinity != affinityRepoStopped {
			t.Fatalf("tier 2 pick = %+v", pick)
		}
		pick = pickFleetHost([]fleetCandidate{cold, repoWarm, busy}, pickRequest{Repo: "repo"})
		if pick.Host.Name != "busy" || pick.Affinity != affinityRepoBusy {
			t.Fatalf("tier 3 pick = %+v", pick)
		}
		pick = pickFleetHost([]fleetCandidate{cold, repoWarm}, pickRequest{Repo: "repo"})
		if pick.Host.Name != "repo-warm" || pick.Affinity != affinityWarmHost {
			t.Fatalf("tier 4 pick = %+v", pick)
		}
	})

	t.Run("stopped repo box or named box at allocation cap is startable and not queued", func(t *testing.T) {
		coldRoom := readyHost("cold-room", "r1", mode.Project)

		stoppedAtCap := readyHost("stopped-at-cap", "r2", mode.Project)
		stoppedAtCap.Sizes = testCPUSizes(0, 0, 0) // already counts stoppedAtCap.RepoBox
		stoppedAtCap.RepoBox = &RepoBox{Name: "repo", Size: "medium", Status: "Stopped"}

		// Stopped existing repo box beats cold host even when Fits == 0 for additional boxes.
		pick := pickFleetHost([]fleetCandidate{coldRoom, stoppedAtCap}, pickRequest{Repo: "repo", Queue: true})
		if pick.Action != pickStart || pick.Host.Name != "stopped-at-cap" || pick.Affinity != affinityRepoStopped {
			t.Fatalf("stopped-at-cap fleet pick = %+v, want start on stopped-at-cap", pick)
		}

		// Alone at cap with Queue=true, it starts rather than queueing indefinitely.
		pick = pickFleetHost([]fleetCandidate{stoppedAtCap}, pickRequest{Repo: "repo", Queue: true})
		if pick.Action != pickStart || pick.Host.Name != "stopped-at-cap" {
			t.Fatalf("stopped-at-cap single pick = %+v, want start", pick)
		}

		// Named --box stopped at allocation cap on its single resolved host also starts.
		namedAtCap := readyHost("named-at-cap", "r3", mode.Project)
		namedAtCap.Sizes = testCPUSizes(0, 0, 0)
		namedAtCap.RepoBox = &RepoBox{Name: "custom-box", Size: "large", Status: "Stopped"}
		pick = pickFleetHost([]fleetCandidate{namedAtCap}, pickRequest{Box: "custom-box", Queue: true})
		if pick.Action != pickStart || pick.Host.Name != "named-at-cap" || pick.Affinity != affinityRepoStopped {
			t.Fatalf("stopped named-box at cap pick = %+v, want start", pick)
		}

		// Already-running matching repo box at cap also starts with highest affinity.
		runningAtCap := readyHost("running-at-cap", "r4", mode.Project)
		runningAtCap.Sizes = testCPUSizes(0, 0, 0)
		runningAtCap.RepoBox = &RepoBox{Name: "repo", Size: "small", Status: "Running"}
		pick = pickFleetHost([]fleetCandidate{stoppedAtCap, runningAtCap}, pickRequest{Repo: "repo"})
		if pick.Action != pickStart || pick.Host.Name != "running-at-cap" || pick.Affinity != affinityDirectWarm {
			t.Fatalf("running-at-cap pick = %+v, want start on running-at-cap", pick)
		}
	})

	t.Run("no repo evidence or mismatched RepoBox name is not ranked", func(t *testing.T) {
		first := readyHost("first", "r1", mode.Project)
		second := readyHost("second", "r2", mode.Project)
		second.WarmThrowaway = true
		second.RepoBox = &RepoBox{Name: "other-repo", Size: "medium", Status: "Running"}

		// Without req.Repo, neither host has repo-scoped evidence; config order wins.
		pick := pickFleetHost([]fleetCandidate{first, second}, pickRequest{New: true})
		if pick.Action != pickStart || pick.Host.Name != "first" || pick.Affinity != affinityCold {
			t.Fatalf("no-repo pick = %+v", pick)
		}

		// Unnamed or different-repo RepoBox does not match req.Repo.
		unnamed := readyHost("unnamed", "r3", mode.Project)
		unnamed.RepoBox = &RepoBox{Size: "medium", Status: "Running"}
		other := readyHost("other", "r4", mode.Project)
		other.RepoBox = &RepoBox{Name: "other-repo", Size: "medium", Status: "Running"}
		pick = pickFleetHost([]fleetCandidate{first, unnamed, other}, pickRequest{Repo: "repo"})
		if pick.Action != pickStart || pick.Host.Name != "first" || pick.Affinity != affinityCold {
			t.Fatalf("mismatched RepoBox pick = %+v", pick)
		}
	})

	t.Run("size mismatch on repo box falls back to throwaway room check", func(t *testing.T) {
		fullMismatch := readyHost("mismatch-full", "r1", mode.Project)
		fullMismatch.Sizes = testCPUSizes(0, 0, 0)
		fullMismatch.RepoBox = &RepoBox{Name: "repo", Size: "small", Status: "Running"}

		roomHost := readyHost("has-large", "r2", mode.Project)

		pick := pickFleetHost([]fleetCandidate{fullMismatch, roomHost}, pickRequest{Repo: "repo", Size: large, ExplicitSize: true})
		if pick.Action != pickStart || pick.Host.Name != "has-large" {
			t.Fatalf("pick = %+v", pick)
		}
	})

	t.Run("throwaway run prefers claimable warm throwaway on agents host only", func(t *testing.T) {
		cold := readyHost("cold", "r1", mode.Project)

		nonAgentsWarm := readyHost("non-agents-warm", "r2", "other")
		nonAgentsWarm.Sizes = testCPUSizes(0, 0, 0)
		nonAgentsWarm.BoxdAvailable = true
		nonAgentsWarm.WarmThrowaway = true // ignored outside mode.Project

		// A verified warm throwaway on agents works even when boxd queue is off.
		agentsWarmNoQueue := readyHost("agents-warm", "r3", mode.Project)
		agentsWarmNoQueue.Sizes = testCPUSizes(0, 0, 0)
		agentsWarmNoQueue.BoxdAvailable = false
		agentsWarmNoQueue.WarmThrowaway = true

		pick := pickFleetHost([]fleetCandidate{cold, nonAgentsWarm, agentsWarmNoQueue}, pickRequest{Repo: "repo", New: true})
		if pick.Action != pickStart || pick.Host.Name != "agents-warm" || pick.Affinity != affinityDirectWarm {
			t.Fatalf("pick = %+v", pick)
		}

		// With --fresh, warm throwaway cannot be claimed so full host is skipped;
		// a host with room + warm repo evidence still beats a cold host.
		agentsWarmWithRoom := readyHost("agents-warm-room", "r4", mode.Project)
		agentsWarmWithRoom.WarmThrowaway = true
		pick = pickFleetHost([]fleetCandidate{cold, agentsWarmNoQueue, agentsWarmWithRoom}, pickRequest{Repo: "repo", New: true, Fresh: true})
		if pick.Action != pickStart || pick.Host.Name != "agents-warm-room" || pick.Affinity != affinityWarmHost {
			t.Fatalf("fresh pick = %+v", pick)
		}
	})

	t.Run("frozen or throwaway-named repo box is not startable for non-new run", func(t *testing.T) {
		frozen := readyHost("frozen", "r1", mode.Project)
		frozen.RepoBox = &RepoBox{Name: "repo", Size: "medium", Status: "Frozen"}

		badThrowaway := readyHost("throwaway-name", "r2", mode.Project)
		badThrowaway.RepoBox = &RepoBox{Name: "repo", Size: "medium", Status: "Running"}
		badThrowaway.RepoBoxThrowaway = true

		clean := readyHost("clean", "r3", mode.Project)

		pick := pickFleetHost([]fleetCandidate{frozen, badThrowaway, clean}, pickRequest{Repo: "repo"})
		if pick.Action != pickStart || pick.Host.Name != "clean" {
			t.Fatalf("pick = %+v", pick)
		}
	})

	t.Run("ties break strictly by stable configuration order", func(t *testing.T) {
		first := readyHost("first", "r1", mode.Project)
		second := readyHost("second", "r2", mode.Project)
		third := readyHost("third", "r3", mode.Project)

		pick := pickFleetHost([]fleetCandidate{first, second, third}, pickRequest{Repo: "repo"})
		if pick.Action != pickStart || pick.Host.Name != "first" {
			t.Fatalf("pick = %+v", pick)
		}
		pick = pickFleetHost([]fleetCandidate{third, second, first}, pickRequest{Repo: "repo"})
		if pick.Action != pickStart || pick.Host.Name != "third" {
			t.Fatalf("reversed pick = %+v", pick)
		}
	})
}

func TestPickFleetHostQueueDestinationAndBoxdGuards(t *testing.T) {
	gpu := pickTestSize(t, mode.GPUSize)

	t.Run("start-now always beats queueing on a warm host", func(t *testing.T) {
		warmFull := readyHost("warm-full", "r1", mode.Project)
		warmFull.Sizes = testCPUSizes(0, 0, 0)
		warmFull.RepoBox = &RepoBox{Name: "repo", Size: "medium", Status: "Running"}

		coldFree := readyHost("cold-free", "r2", mode.Project)

		// New throwaway run needs room, so warmFull cannot start and coldFree wins over queueing.
		pick := pickFleetHost([]fleetCandidate{warmFull, coldFree}, pickRequest{Repo: "repo", New: true, Queue: true})
		if pick.Action != pickStart || pick.Host.Name != "cold-free" {
			t.Fatalf("pick = %+v", pick)
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
		pick := pickFleetHost([]fleetCandidate{customFull, noBoxdFull}, pickRequest{Queue: true})
		if pick.Action != pickRefuse ||
			!strings.Contains(pick.Reason, `boxd handles project "agents" only`) ||
			!strings.Contains(pick.Reason, "boxd queue capability not established") {
			t.Fatalf("refuse pick = %+v", pick)
		}

		// Unknown depth IS allowed when BoxdAvailable was positively established on agents.
		capableUnknownDepth := readyHost("capable-unknown", "r3", mode.Project)
		capableUnknownDepth.Sizes = testCPUSizes(0, 0, 0)
		capableUnknownDepth.BoxdAvailable = true
		capableUnknownDepth.Queued = nil

		pick = pickFleetHost([]fleetCandidate{customFull, noBoxdFull, capableUnknownDepth}, pickRequest{Queue: true})
		if pick.Action != pickQueue || pick.Host.Name != "capable-unknown" || pick.Why != refusedFull {
			t.Fatalf("capable-unknown pick = %+v", pick)
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
		fullShortWarm.RepoBox = &RepoBox{Name: "repo", Size: "medium", Status: "Running"}

		// Active + known + shorter + repo affinity beats all others (throwaway run needs room).
		req := pickRequest{Repo: "repo", New: true, Queue: true}
		pick := pickFleetHost([]fleetCandidate{mypcShort, fullUnknownDepth, fullLong, fullShortCold, fullShortWarm}, req)
		if pick.Action != pickQueue || pick.Host.Name != "full-short-warm" || pick.Why != refusedFull {
			t.Fatalf("best queue pick = %+v", pick)
		}

		// Shorter queue beats longer queue even if longer is earlier in config.
		pick = pickFleetHost([]fleetCandidate{mypcShort, fullUnknownDepth, fullLong, fullShortCold}, req)
		if pick.Action != pickQueue || pick.Host.Name != "full-short-cold" {
			t.Fatalf("shorter queue pick = %+v", pick)
		}

		// Known depth beats unknown depth among active hosts.
		pick = pickFleetHost([]fleetCandidate{mypcShort, fullUnknownDepth, fullLong}, req)
		if pick.Action != pickQueue || pick.Host.Name != "full-long" {
			t.Fatalf("known-depth pick = %+v", pick)
		}

		// Active host with positively established boxd capability beats mypc even with unknown depth.
		pick = pickFleetHost([]fleetCandidate{mypcShort, fullUnknownDepth}, req)
		if pick.Action != pickQueue || pick.Host.Name != "full-unknown-depth" {
			t.Fatalf("active-over-mypc pick = %+v", pick)
		}
	})

	t.Run("GPU queues on busy or full boxd-capable agents GPU host", func(t *testing.T) {
		noGPU := readyHost("cpu", "r1", mode.Project)
		noGPU.HasGPU = false
		noGPU.Sizes = testCPUSizes(4, 2, 1)

		busyOtherProject := readyHost("busy-work", "r2", "work")
		busyOtherProject.GPUHolders = []string{"other"}
		busyOtherProject.BoxdAvailable = true

		busyNoBoxd := readyHost("busy-noboxd", "r3", mode.Project)
		busyNoBoxd.GPUHolders = []string{"other"}
		busyNoBoxd.BoxdAvailable = false

		busyAgents := readyHost("busy-agents", "r4", mode.Project)
		busyAgents.GPUHolders = []string{"other"}
		busyAgents.Queued = intPtr(1)

		pick := pickFleetHost([]fleetCandidate{noGPU, busyOtherProject, busyNoBoxd, busyAgents}, pickRequest{Size: gpu, ExplicitSize: true, Queue: true})
		if pick.Action != pickQueue || pick.Host.Name != "busy-agents" || pick.Why != refusedGPUBusy {
			t.Fatalf("gpu queue pick = %+v", pick)
		}
	})

	t.Run("Queue false refuses instead of queueing", func(t *testing.T) {
		full := readyHost("full", "r1", mode.Project)
		full.Sizes = testCPUSizes(0, 0, 0)

		pick := pickFleetHost([]fleetCandidate{full}, pickRequest{Queue: false})
		if pick.Action != pickRefuse || pick.Why != refusedFull || pick.Host != (fleetHost{}) {
			t.Fatalf("no-queue pick = %+v", pick)
		}
	})
}

func TestPickFleetHostResolvedSingleAndExplicitBoxGuard(t *testing.T) {
	pinned := readyHost("pinned", "r2", mode.Project)

	t.Run("single resolved host starts, queues, or refuses without spilling over", func(t *testing.T) {
		pick := pickFleetHost([]fleetCandidate{pinned}, pickRequest{Repo: "repo"})
		if pick.Action != pickStart || pick.Host.Name != "pinned" {
			t.Fatalf("single start = %+v", pick)
		}

		pinnedFull := pinned
		pinnedFull.Sizes = testCPUSizes(0, 0, 0)
		pick = pickFleetHost([]fleetCandidate{pinnedFull}, pickRequest{Queue: true})
		if pick.Action != pickQueue || pick.Host.Name != "pinned" || pick.Why != refusedFull {
			t.Fatalf("single queue = %+v", pick)
		}

		pinnedDown := pinned
		pinnedDown.Reachable = false
		pinnedDown.Error = "timeout"
		pick = pickFleetHost([]fleetCandidate{pinnedDown}, pickRequest{Queue: true})
		if pick.Action != pickRefuse || pick.Host != (fleetHost{}) || !strings.Contains(pick.Reason, "pinned: unreachable") {
			t.Fatalf("single refuse = %+v", pick)
		}

		pinnedWork := readyHost("pinned-work", "r2", "work")
		pinnedWork.Sizes = testCPUSizes(0, 0, 0)
		pick = pickFleetHost([]fleetCandidate{pinnedWork}, pickRequest{Queue: true})
		if pick.Action != pickRefuse || !strings.Contains(pick.Reason, `boxd handles project "agents" only`) {
			t.Fatalf("single work refuse = %+v", pick)
		}
	})

	t.Run("explicit --box refuses multi-host candidate list", func(t *testing.T) {
		first := readyHost("first", "r1", mode.Project)
		first.RepoBox = &RepoBox{Name: "shared-name", Size: "medium", Status: "Running"}
		second := pinned
		second.RepoBox = &RepoBox{Name: "shared-name", Size: "medium", Status: "Running"}

		pick := pickFleetHost([]fleetCandidate{first, second}, pickRequest{Box: "shared-name"})
		if pick.Action != pickRefuse || pick.Host != (fleetHost{}) || !strings.Contains(pick.Reason, "single target host") {
			t.Fatalf("multi-host --box pick = %+v, want refuse", pick)
		}
	})

	t.Run("empty candidate list refuses cleanly", func(t *testing.T) {
		pick := pickFleetHost(nil, pickRequest{})
		if pick.Action != pickRefuse || !strings.Contains(pick.Reason, "no configured hosts") {
			t.Fatalf("empty pick = %+v", pick)
		}
	})
}
