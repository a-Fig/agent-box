package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/jobrun"
	"github.com/a-Fig/agent-box/internal/mode"
)

// respondHealthyProject handles Incus project get / project info for a healthy
// host whose unset keys return exit 0 with an empty string (as real Incus does).
func respondHealthyProject(cmd Cmd, keys map[string]string, infoJSON string) (Result, bool) {
	if len(cmd.Args) >= 5 && cmd.Args[2] == "project" && cmd.Args[3] == "get" {
		key := cmd.Args[5]
		return Result{Stdout: keys[key] + "\n"}, true
	}
	if len(cmd.Args) >= 6 && cmd.Args[2] == "project" && cmd.Args[3] == "info" {
		if infoJSON == "" {
			infoJSON = "{}"
		}
		return Result{Stdout: infoJSON}, true
	}
	return Result{}, false
}

func TestProbeFleetCandidatesIndependentAndTyped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slowStarted := make(chan struct{})
	fastDone := make(chan struct{})
	var workBoxdCalls atomic.Int32

	fastKeys := map[string]string{
		mode.ProjectModeKey: "full",
		"limits.instances":  "4",
		gpuKey:              "allow",
	}
	workKeys := map[string]string{
		mode.ProjectModeKey: "shared",
		"limits.instances":  "2",
		gpuKey:              "block",
	}

	a := newApp()
	a.Now = func() time.Time { return time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC) }
	a.R = fleetTestRunner(func(ctx context.Context, cmd Cmd) (Result, error) {
		joined := strings.Join(cmd.Args, " ")
		switch joined {
		case "--project agents list slow: --format json":
			close(slowStarted)
			select {
			case <-fastDone:
				return Result{}, fmt.Errorf("connection refused")
			case <-ctx.Done():
				return Result{}, ctx.Err()
			}
		case "--project agents list fast: --format json":
			select {
			case <-slowStarted:
				insts := []instance{
					{Name: "repo", Status: "Running", Config: map[string]string{mode.SizeKey: "medium", expiresKey: "2026-11-26T14:00:00Z"}},
					{Name: "gpu-other", Status: "Running", Config: map[string]string{mode.SizeKey: mode.GPUSize}},
				}
				b, _ := json.Marshal(insts)
				return Result{Stdout: string(b)}, nil
			case <-ctx.Done():
				return Result{}, ctx.Err()
			}
		case "--project work list custom: --format json":
			return Result{Stdout: "[]"}, nil
		}
		if len(cmd.Args) >= 5 && cmd.Args[2] == "project" {
			if strings.HasPrefix(cmd.Args[4], "fast:") {
				if res, ok := respondHealthyProject(cmd, fastKeys, "{}"); ok {
					return res, nil
				}
			}
			if strings.HasPrefix(cmd.Args[4], "custom:") {
				if res, ok := respondHealthyProject(cmd, workKeys, "{}"); ok {
					return res, nil
				}
			}
		}
		if len(cmd.Args) >= 5 && cmd.Args[2] == "exec" && cmd.Args[3] == "fast:repo" {
			return Result{Stdout: ""}, nil
		}
		return Result{Code: 1}, nil
	})
	a.HTTP = func(req *http.Request) (*http.Response, error) {
		if strings.HasPrefix(req.URL.String(), "https://work.example.com") {
			workBoxdCalls.Add(1)
			t.Errorf("boxd should not be contacted for non-agents project: %s", req.URL)
		}
		if req.URL.String() == "https://fast.example.com/api/state" {
			close(fastDone)
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader(`{"queue":true,"jobs":[{"state":"queued"},{"state":"queued"},{"state":"running"}]}`)),
			}, nil
		}
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
	}

	targets := []fleetHost{
		{Name: "down", Remote: "slow", Project: mode.Project, DashboardURL: "https://slow.example.com"},
		{Name: "fast", Remote: "fast", Project: mode.Project, DashboardURL: "https://fast.example.com"},
		{Name: "work", Remote: "custom", Project: "work", DashboardURL: "https://work.example.com"},
	}
	cands := a.probeFleetCandidates(ctx, targets, pickRequest{Repo: "repo"}, "")
	if len(cands) != 3 {
		t.Fatalf("len(cands) = %d, want 3", len(cands))
	}
	if cands[0].Reachable || !strings.Contains(cands[0].Error, "connection refused") || cands[0].BoxdAvailable {
		t.Fatalf("down candidate = %+v", cands[0])
	}
	fast := cands[1]
	if !fast.Reachable || fast.Error != "" || fast.Mode != "full" || !fast.HasGPU || !slices.Equal(fast.GPUHolders, []string{"gpu-other"}) ||
		!fast.BoxdAvailable || fast.Queued == nil || *fast.Queued != 2 || fast.RepoBox == nil || fast.RepoBox.Name != "repo" {
		t.Fatalf("fast candidate = %+v", fast)
	}
	work := cands[2]
	if !work.Reachable || work.Error != "" || work.HasGPU || work.BoxdAvailable || work.Queued != nil || work.WarmThrowaway || workBoxdCalls.Load() != 0 {
		t.Fatalf("work candidate = %+v, boxd calls = %d", work, workBoxdCalls.Load())
	}

	pick := pickFleetHost(cands, pickRequest{Repo: "repo"})
	if pick.Action != pickStart || pick.Host.Name != "fast" || pick.Affinity != affinityDirectWarm {
		t.Fatalf("pick = %+v, want start on fast", pick)
	}
}

func TestProbeFleetCandidateBoxdQueueCapabilityRequiresExplicitBit(t *testing.T) {
	keys := map[string]string{
		mode.ProjectModeKey: "full",
		"limits.instances":  "1",
		gpuKey:              "allow",
	}
	a := newApp()
	a.Now = func() time.Time { return time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC) }
	a.R = fleetTestRunner(func(_ context.Context, cmd Cmd) (Result, error) {
		if strings.Join(cmd.Args, " ") == "--project agents list pc: --format json" {
			return Result{Stdout: `[{"name":"busy","status":"Running","config":{"user.box.size":"medium"}}]`}, nil
		}
		if res, ok := respondHealthyProject(cmd, keys, "{}"); ok {
			return res, nil
		}
		return Result{Code: 1}, nil
	})

	for _, tc := range []struct {
		name      string
		body      string
		wantAvail bool
		wantQueue *int
	}{
		{name: "absent queue bit (W.Queue nil or older boxd)", body: `{"jobs":[{"state":"queued"}]}`, wantAvail: false},
		{name: "explicit queue false", body: `{"queue":false,"jobs":[]}`, wantAvail: false},
		{name: "explicit queue true", body: `{"queue":true,"jobs":[{"state":"queued"}]}`, wantAvail: true, wantQueue: intPtr(1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a.HTTP = func(_ *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}
			cands := a.probeFleetCandidates(context.Background(), []fleetHost{{Name: "pc", Remote: "pc", Project: mode.Project, DashboardURL: "https://pc.example.com"}}, pickRequest{Queue: true}, "")
			c := cands[0]
			if c.BoxdAvailable != tc.wantAvail {
				t.Fatalf("BoxdAvailable = %v, want %v (cand=%+v)", c.BoxdAvailable, tc.wantAvail, c)
			}
			if (c.Queued == nil) != (tc.wantQueue == nil) || (tc.wantQueue != nil && *c.Queued != *tc.wantQueue) {
				t.Fatalf("Queued = %v, want %v", c.Queued, tc.wantQueue)
			}
			pick := pickFleetHost(cands, pickRequest{Queue: true})
			if tc.wantAvail && pick.Action != pickQueue {
				t.Fatalf("pick = %+v, want queue", pick)
			}
			if !tc.wantAvail && pick.Action != pickRefuse {
				t.Fatalf("pick = %+v, want refuse without queue capability", pick)
			}
		})
	}
}

func TestProbeFleetCandidateTimeoutIsolation(t *testing.T) {
	keys := map[string]string{
		mode.ProjectModeKey: "full",
		"limits.instances":  "3",
		gpuKey:              "allow",
	}

	t.Run("timeout during Incus probe fails closed", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
		defer cancel()
		var boxdCalled atomic.Bool
		a := newApp()
		a.R = fleetTestRunner(func(ctx context.Context, cmd Cmd) (Result, error) {
			switch cmd.Args[len(cmd.Args)-1] {
			case "json":
				return Result{Stdout: "[]"}, nil
			case "user.agent-box.mode":
				return Result{Stdout: "full"}, nil
			case gpuKey:
				return Result{Stdout: "allow"}, nil
			case "limits.instances":
				<-ctx.Done()
				return Result{}, ctx.Err()
			}
			return Result{Code: 1}, nil
		})
		a.HTTP = func(_ *http.Request) (*http.Response, error) {
			boxdCalled.Store(true)
			return nil, fmt.Errorf("unexpected")
		}
		cands := a.probeFleetCandidates(ctx, []fleetHost{{Name: "pc", Remote: "pc", Project: mode.Project, DashboardURL: "https://pc.example.com"}}, pickRequest{}, "")
		c := cands[0]
		if !c.Reachable || c.Error != "context deadline exceeded" || len(c.Sizes) != 0 || c.BoxdAvailable || boxdCalled.Load() {
			t.Fatalf("incus timeout candidate = %+v, boxdCalled=%v", c, boxdCalled.Load())
		}
		if pick := pickFleetHost(cands, pickRequest{}); pick.Action != pickRefuse {
			t.Fatalf("pick = %+v, want refuse", pick)
		}
	})

	t.Run("timeout in optional boxd probe keeps Incus start capacity usable", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		a := newApp()
		a.R = fleetTestRunner(func(_ context.Context, cmd Cmd) (Result, error) {
			if strings.Join(cmd.Args, " ") == "--project agents list pc: --format json" {
				return Result{Stdout: "[]"}, nil
			}
			if res, ok := respondHealthyProject(cmd, keys, "{}"); ok {
				return res, nil
			}
			return Result{Code: 1}, nil
		})
		a.HTTP = func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		cands := a.probeFleetCandidates(ctx, []fleetHost{{Name: "pc", Remote: "pc", Project: mode.Project, DashboardURL: "https://pc.example.com"}}, pickRequest{}, "")
		c := cands[0]
		if !c.Reachable || c.Error != "" || len(c.Sizes) == 0 || c.BoxdAvailable || c.Queued != nil {
			t.Fatalf("boxd timeout candidate = %+v, want usable Incus capacity without boxd", c)
		}
		if pick := pickFleetHost(cands, pickRequest{}); pick.Action != pickStart || pick.Host.Name != "pc" {
			t.Fatalf("pick = %+v, want start on pc", pick)
		}
	})
}

func TestProbeFleetCandidateFailedCapReadFailsClosed(t *testing.T) {
	// Non-timeout EOF after incus list succeeds must fail closed instead of
	// treating failed cap reads as unlimited, while a healthy host with
	// genuinely unset limits (exit 0, empty string, {}) succeeds as unlimited.
	healthyKeys := map[string]string{
		mode.ProjectModeKey: "full",
		gpuKey:              "block",
		// limits.instances, limits.cpu, limits.memory genuinely unset
	}

	for _, failTarget := range []string{mode.ProjectModeKey, gpuKey, "limits.instances", "limits.cpu", "limits.memory", "info"} {
		t.Run("EOF on "+failTarget, func(t *testing.T) {
			var brokenBoxdCalls atomic.Int32
			a := newApp()
			a.R = fleetTestRunner(func(_ context.Context, cmd Cmd) (Result, error) {
				if strings.Join(cmd.Args, " ") == "--project agents list broken: --format json" ||
					strings.Join(cmd.Args, " ") == "--project agents list healthy: --format json" {
					return Result{Stdout: "[]"}, nil
				}
				if len(cmd.Args) >= 5 && cmd.Args[2] == "project" && strings.HasPrefix(cmd.Args[4], "broken:") {
					if (cmd.Args[3] == "info" && failTarget == "info") ||
						(cmd.Args[3] == "get" && len(cmd.Args) >= 6 && cmd.Args[5] == failTarget) {
						return Result{Code: 1, Stderr: "Error: EOF"}, nil
					}
					if res, ok := respondHealthyProject(cmd, healthyKeys, "{}"); ok {
						return res, nil
					}
				}
				if len(cmd.Args) >= 5 && cmd.Args[2] == "project" && strings.HasPrefix(cmd.Args[4], "healthy:") {
					if res, ok := respondHealthyProject(cmd, healthyKeys, "{}"); ok {
						return res, nil
					}
				}
				return Result{Code: 1}, nil
			})
			a.HTTP = func(req *http.Request) (*http.Response, error) {
				if strings.HasPrefix(req.URL.String(), "https://broken.example.com") {
					brokenBoxdCalls.Add(1)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"queue":true,"jobs":[]}`))}, nil
			}

			targets := []fleetHost{
				{Name: "broken", Remote: "broken", Project: mode.Project, DashboardURL: "https://broken.example.com"},
				{Name: "healthy", Remote: "healthy", Project: mode.Project, DashboardURL: "https://healthy.example.com"},
			}
			cands := a.probeFleetCandidates(context.Background(), targets, pickRequest{}, "")
			broken, healthy := cands[0], cands[1]
			if !broken.Reachable || broken.Error == "" || len(broken.Sizes) != 0 || broken.BoxdAvailable || brokenBoxdCalls.Load() != 0 {
				t.Fatalf("broken candidate = %+v, boxdCalls=%d; want failed closed", broken, brokenBoxdCalls.Load())
			}
			if !healthy.Reachable || healthy.Error != "" || len(healthy.Sizes) == 0 || healthy.Sizes[0].Fits != -1 {
				t.Fatalf("healthy candidate = %+v; want unlimited room from genuinely unset limits", healthy)
			}
			if pick := pickFleetHost(cands, pickRequest{}); pick.Action != pickStart || pick.Host.Name != "healthy" {
				t.Fatalf("pick = %+v, want start on healthy", pick)
			}
		})
	}
}

func TestProbeFleetCandidateGPUReclaimableVsBusy(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	gpu := pickTestSize(t, mode.GPUSize)
	med := pickTestSize(t, "medium")

	// Caps fit exactly one GPU box (1 slot, 12GiB RAM), so a new GPU box fits
	// only when the existing GPU box is reclaimable by claimGPU.
	keys := map[string]string{
		mode.ProjectModeKey: "shared",
		"limits.instances":  "1",
		"limits.cpu":        "24",
		"limits.memory":     "12GiB",
		gpuKey:              "allow",
	}

	probeWith := func(t *testing.T, inst instance, execRes Result, execErr error, req pickRequest) (fleetCandidate, fleetPick) {
		t.Helper()
		b, _ := json.Marshal([]instance{inst})
		a := newApp()
		a.Now = func() time.Time { return now }
		a.R = fleetTestRunner(func(_ context.Context, cmd Cmd) (Result, error) {
			if strings.Join(cmd.Args, " ") == "--project agents list pc: --format json" {
				return Result{Stdout: string(b)}, nil
			}
			if res, ok := respondHealthyProject(cmd, keys, "{}"); ok {
				return res, nil
			}
			if len(cmd.Args) >= 5 && cmd.Args[2] == "exec" && cmd.Args[3] == "pc:"+inst.Name {
				return execRes, execErr
			}
			return Result{Code: 1}, nil
		})
		a.HTTP = func(_ *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"queue":true,"jobs":[]}`))}, nil
		}
		cands := a.probeFleetCandidates(context.Background(), []fleetHost{{Name: "pc", Remote: "pc", Project: mode.Project, DashboardURL: "https://pc.example.com"}}, req, "")
		return cands[0], pickFleetHost(cands, req)
	}

	idleUnheld := instance{
		Name:      "gpu-old",
		Status:    "Running",
		CreatedAt: now.Add(-20 * time.Minute),
		Config: map[string]string{
			mode.SizeKey:    mode.GPUSize,
			throwawayKey:    "true",
			"limits.memory": "8GiB",
			expiresKey:      now.Add(20 * time.Hour).Format(time.RFC3339), // outside keptThrowawayConfig window
		},
	}

	t.Run("idle unheld throwaway past startup grace is reclaimable for GPU request only", func(t *testing.T) {
		gpuCand, gpuPick := probeWith(t, idleUnheld, Result{Stdout: ""}, nil, pickRequest{Size: gpu, ExplicitSize: true, Queue: true})
		if len(gpuCand.GPUHolders) != 0 || gpuPick.Action != pickStart || gpuPick.Host.Name != "pc" {
			t.Fatalf("GPU request: cand=%+v pick=%+v; want start with reclaimed GPU capacity", gpuCand, gpuPick)
		}

		// CPU request does not run claimGPU, so the same GPU box does NOT free CPU capacity.
		cpuCand, cpuPick := probeWith(t, idleUnheld, Result{Stdout: ""}, nil, pickRequest{Size: med, ExplicitSize: true, Queue: true})
		if cpuPick.Action != pickQueue || cpuPick.Why != refusedFull {
			t.Fatalf("CPU request: cand=%+v pick=%+v; want queue full (CPU cannot reclaim GPU via claimGPU)", cpuCand, cpuPick)
		}
	})

	t.Run("stopped unheld throwaway past startup grace without tray mark is reclaimable for GPU only", func(t *testing.T) {
		stoppedOld := idleUnheld
		stoppedOld.Status = "Stopped"
		_, gpuPick := probeWith(t, stoppedOld, Result{}, nil, pickRequest{Size: gpu, ExplicitSize: true, Queue: true})
		if gpuPick.Action != pickStart {
			t.Fatalf("GPU request on stopped old throwaway = %+v, want start", gpuPick)
		}
		_, cpuPick := probeWith(t, stoppedOld, Result{}, nil, pickRequest{Size: med, ExplicitSize: true, Queue: true})
		if cpuPick.Action != pickQueue || cpuPick.Why != refusedFull {
			t.Fatalf("CPU request on stopped old throwaway = %+v, want queue full", cpuPick)
		}
	})

	t.Run("held, starting, paused, busy, and unverified GPU holders stay busy", func(t *testing.T) {
		held := idleUnheld
		held.Config = map[string]string{
			mode.SizeKey:    mode.GPUSize,
			throwawayKey:    "true",
			"limits.memory": "8GiB",
			expiresKey:      now.Add(20 * time.Hour).Format(time.RFC3339),
			mode.HoldKey:    now.Add(time.Hour).Format(time.RFC3339),
		}
		starting := idleUnheld
		starting.CreatedAt = now.Add(-5 * time.Minute)
		paused := idleUnheld
		paused.Status = "Frozen"

		for _, tc := range []struct {
			name    string
			inst    instance
			execRes Result
			execErr error
		}{
			{name: "held idle throwaway", inst: held, execRes: Result{Stdout: ""}},
			{name: "starting within grace", inst: starting, execRes: Result{Stdout: ""}},
			{name: "paused GPU box", inst: paused},
			{name: "running job", inst: idleUnheld, execRes: Result{Stdout: "gpu-old.1\n"}},
			{name: "unverified exec failure", inst: idleUnheld, execRes: Result{Code: 1}, execErr: fmt.Errorf("exec EOF")},
		} {
			t.Run(tc.name, func(t *testing.T) {
				cand, pick := probeWith(t, tc.inst, tc.execRes, tc.execErr, pickRequest{Size: gpu, ExplicitSize: true, Queue: true})
				if !slices.Equal(cand.GPUHolders, []string{tc.inst.Name}) || pick.Action != pickQueue || pick.Why != refusedGPUBusy {
					t.Fatalf("%s: holders=%v pick=%+v; want busy + queue gpu-busy", tc.name, cand.GPUHolders, pick)
				}
			})
		}
	})
}

func TestProbeFleetCandidatePooledThrowawayRequiresIdleAndPoolEnabled(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	origin := "https://github.com/example/repo.git"
	med := pickTestSize(t, "medium")
	repoKey := throwawayRepoKey(origin, med)
	keys := map[string]string{
		mode.ProjectModeKey: "full",
		"limits.instances":  "1",
		gpuKey:              "allow",
	}

	makeInsts := func(exp string) string {
		insts := []instance{{
			Name:   "repo-abcd",
			Status: "Running",
			Config: map[string]string{
				throwawayKey:            "true",
				mode.SizeKey:            "medium",
				jobrun.ThrowawayRepoKey: repoKey,
				expiresKey:              exp,
			},
		}}
		b, _ := json.Marshal(insts)
		return string(b)
	}

	for _, tc := range []struct {
		name           string
		exp            string
		keptScriptOut  string // "kept" means idle with ended job; "" means job still running
		poolStatus     int
		req            pickRequest
		wantWarm       bool
		wantPoolProbed bool
	}{
		{
			name:           "idle candidate + pool enabled sets WarmThrowaway",
			exp:            now.Add(20 * time.Minute).Format(time.RFC3339),
			keptScriptOut:  "kept\n",
			poolStatus:     http.StatusNoContent,
			req:            pickRequest{Repo: "repo", New: true, Size: med},
			wantWarm:       true,
			wantPoolProbed: true,
		},
		{
			name:           "box with running job fails idle check and does not probe pool",
			exp:            now.Add(20 * time.Minute).Format(time.RFC3339),
			keptScriptOut:  "",
			poolStatus:     http.StatusNoContent,
			req:            pickRequest{Repo: "repo", New: true, Size: med},
			wantWarm:       false,
			wantPoolProbed: false,
		},
		{
			name:           "expired candidate does not qualify or probe pool",
			exp:            now.Add(-time.Minute).Format(time.RFC3339),
			keptScriptOut:  "kept\n",
			poolStatus:     http.StatusNoContent,
			req:            pickRequest{Repo: "repo", New: true, Size: med},
			wantWarm:       false,
			wantPoolProbed: false,
		},
		{
			name:           "idle candidate with pool disabled stays false",
			exp:            now.Add(20 * time.Minute).Format(time.RFC3339),
			keptScriptOut:  "kept\n",
			poolStatus:     http.StatusNotFound,
			req:            pickRequest{Repo: "repo", New: true, Size: med},
			wantWarm:       false,
			wantPoolProbed: true,
		},
		{
			name:           "fresh request skips pool probe",
			exp:            now.Add(20 * time.Minute).Format(time.RFC3339),
			keptScriptOut:  "kept\n",
			poolStatus:     http.StatusNoContent,
			req:            pickRequest{Repo: "repo", New: true, Fresh: true, Size: med},
			wantWarm:       false,
			wantPoolProbed: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var poolProbed atomic.Bool
			a := newApp()
			a.Now = func() time.Time { return now }
			a.R = fleetTestRunner(func(_ context.Context, cmd Cmd) (Result, error) {
				if strings.Join(cmd.Args, " ") == "--project agents list pc: --format json" {
					return Result{Stdout: makeInsts(tc.exp)}, nil
				}
				if res, ok := respondHealthyProject(cmd, keys, "{}"); ok {
					return res, nil
				}
				if len(cmd.Args) >= 5 && cmd.Args[2] == "exec" && cmd.Args[3] == "pc:repo-abcd" {
					return Result{Stdout: tc.keptScriptOut}, nil
				}
				return Result{Code: 1}, nil
			})
			a.HTTP = func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case "/api/state":
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"queue":true,"jobs":[]}`))}, nil
				case "/api/throwaway-pool":
					poolProbed.Store(true)
					return &http.Response{StatusCode: tc.poolStatus, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
			cands := a.probeFleetCandidates(context.Background(), []fleetHost{{Name: "pc", Remote: "pc", Project: mode.Project, DashboardURL: "https://pc.example.com"}}, tc.req, origin)
			if cands[0].WarmThrowaway != tc.wantWarm || poolProbed.Load() != tc.wantPoolProbed {
				t.Fatalf("WarmThrowaway=%v (want %v), poolProbed=%v (want %v)", cands[0].WarmThrowaway, tc.wantWarm, poolProbed.Load(), tc.wantPoolProbed)
			}
		})
	}
}

func TestProbeFleetTiers(t *testing.T) {
	openKeys := map[string]string{mode.ProjectModeKey: "full", "limits.instances": "4", gpuKey: "block"}
	fullKeys := map[string]string{mode.ProjectModeKey: "full", "limits.instances": "1", gpuKey: "block"}

	newProbeApp := func(t *testing.T, hostKeys map[string]map[string]string, insts map[string]string) (*App, *[]string) {
		t.Helper()
		var mu sync.Mutex
		var probed []string
		recorded := map[string]bool{}
		a := newApp()
		a.Now = func() time.Time { return time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC) }
		a.R = fleetTestRunner(func(_ context.Context, cmd Cmd) (Result, error) {
			if len(cmd.Args) >= 4 && cmd.Args[2] == "list" {
				remote, _, _ := strings.Cut(cmd.Args[3], ":")
				mu.Lock()
				if !recorded[remote] {
					recorded[remote] = true
					probed = append(probed, remote)
				}
				mu.Unlock()
				out, ok := insts[remote]
				if !ok {
					out = "[]"
				}
				return Result{Stdout: out}, nil
			}
			if len(cmd.Args) >= 5 && cmd.Args[2] == "project" {
				remote, _, _ := strings.Cut(cmd.Args[4], ":")
				if res, ok := respondHealthyProject(cmd, hostKeys[remote], "{}"); ok {
					return res, nil
				}
			}
			return Result{Code: 1}, nil
		})
		a.HTTP = func(_ *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"queue":true,"jobs":[]}`))}, nil
		}
		return a, &probed
	}

	t.Run("boxd-capable hosts are always probed together regardless of hosts.json hints", func(t *testing.T) {
		a, probed := newProbeApp(t, map[string]map[string]string{
			"r-alpha": openKeys,
			"r-beta":  openKeys,
		}, nil)
		a.HTTP = func(req *http.Request) (*http.Response, error) {
			switch req.URL.Host {
			case "alpha.example.com":
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"queue":true,"priority":0,"jobs":[]}`))}, nil
			case "beta.example.com":
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"queue":true,"priority":25,"jobs":[]}`))}, nil
			}
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		targets := []fleetHost{
			{Name: "alpha", Remote: "r-alpha", Project: mode.Project, DashboardURL: "https://alpha.example.com", Priority: 10},
			{Name: "beta", Remote: "r-beta", Project: mode.Project, DashboardURL: "https://beta.example.com", Priority: 0},
		}
		cands, rest := a.probeFleetTiers(context.Background(), targets, pickRequest{}, "")
		if len(cands) != 2 || len(rest) != 0 || len(*probed) != 2 {
			t.Fatalf("boxd-capable cands=%d rest=%d probed=%v, want both probed together", len(cands), len(rest), *probed)
		}
		if pick := pickFleetHost(cands, pickRequest{}); pick.Action != pickStart || pick.Host.Name != "beta" {
			t.Fatalf("pick = %+v, want beta (boxd priority 25)", pick)
		}
	})

	t.Run("stops after top non-boxd tier when top tier can start", func(t *testing.T) {
		a, probed := newProbeApp(t, map[string]map[string]string{
			"r-high": openKeys,
			"r-mid":  openKeys,
			"r-low":  openKeys,
		}, nil)
		targets := []fleetHost{
			{Name: "low", Remote: "r-low", Project: "work", Priority: 0},
			{Name: "high", Remote: "r-high", Project: "work", Priority: 10},
			{Name: "mid", Remote: "r-mid", Project: "work", Priority: 5},
		}
		cands, rest := a.probeFleetTiers(context.Background(), targets, pickRequest{}, "")
		if !slices.Equal(*probed, []string{"r-high"}) {
			t.Fatalf("probed = %v, want [r-high]", *probed)
		}
		if len(cands) != 1 || cands[0].Host.Name != "high" {
			t.Fatalf("cands = %+v, want [high]", cands)
		}
		if len(rest) != 2 || rest[0].Name != "mid" || rest[1].Name != "low" {
			t.Fatalf("rest = %+v, want [mid, low]", rest)
		}
	})

	t.Run("defers negative-priority non-boxd hosts below boxd tier", func(t *testing.T) {
		a, probed := newProbeApp(t, map[string]map[string]string{
			"r-boxd": openKeys,
			"r-low":  openKeys,
		}, nil)
		targets := []fleetHost{
			{Name: "low", Remote: "r-low", Project: "work", Priority: -1},
			{Name: "boxd", Remote: "r-boxd", Project: mode.Project, Priority: 0},
		}
		cands, rest := a.probeFleetTiers(context.Background(), targets, pickRequest{}, "")
		if !slices.Equal(*probed, []string{"r-boxd"}) || len(cands) != 1 || len(rest) != 1 || rest[0].Name != "low" {
			t.Fatalf("probed=%v cands=%+v rest=%+v, want [r-boxd] and rest=[low]", *probed, cands, rest)
		}
	})

	t.Run("probes deferred non-boxd tier when startable boxd host reports lower negative priority", func(t *testing.T) {
		a, probed := newProbeApp(t, map[string]map[string]string{
			"r-boxd": openKeys,
			"r-low":  openKeys,
			"r-sunk": openKeys,
		}, nil)
		a.HTTP = func(req *http.Request) (*http.Response, error) {
			if req.URL.Host == "boxd.example.com" {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"queue":true,"priority":-10,"jobs":[]}`))}, nil
			}
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		targets := []fleetHost{
			{Name: "boxd", Remote: "r-boxd", Project: mode.Project, DashboardURL: "https://boxd.example.com", Priority: 0},
			{Name: "low", Remote: "r-low", Project: "work", Priority: -1},
			{Name: "sunk", Remote: "r-sunk", Project: "work", Priority: -5},
		}
		cands, rest := a.probeFleetTiers(context.Background(), targets, pickRequest{}, "")
		if !slices.Equal(*probed, []string{"r-boxd", "r-low"}) || len(rest) != 1 || rest[0].Name != "sunk" {
			t.Fatalf("probed=%v rest=%+v, want [r-boxd r-low] and rest=[sunk]", *probed, rest)
		}
		if pick := pickFleetHost(cands, pickRequest{}); pick.Action != pickStart || pick.Host.Name != "low" {
			t.Fatalf("pick = %+v, want low (-1 beats boxd -10)", pick)
		}
	})

	t.Run("probes lower non-boxd tier when higher tier cannot start and stops at first startable tier", func(t *testing.T) {
		a, probed := newProbeApp(t, map[string]map[string]string{
			"r-high": fullKeys,
			"r-mid":  openKeys,
			"r-low":  openKeys,
		}, map[string]string{
			"r-high": `[{"name":"busy","status":"Running","config":{"user.box.size":"medium"}}]`,
		})
		targets := []fleetHost{
			{Name: "high", Remote: "r-high", Project: "work", Priority: 10},
			{Name: "mid", Remote: "r-mid", Project: "work", Priority: 5},
			{Name: "low", Remote: "r-low", Project: "work", Priority: 0},
		}
		cands, rest := a.probeFleetTiers(context.Background(), targets, pickRequest{Queue: true}, "")
		if !slices.Equal(*probed, []string{"r-high", "r-mid"}) {
			t.Fatalf("probed = %v, want [r-high r-mid]", *probed)
		}
		if len(cands) != 2 || len(rest) != 1 || rest[0].Name != "low" {
			t.Fatalf("cands=%d rest=%+v, want 2 cands and [low]", len(cands), rest)
		}
		if pick := pickFleetHost(cands, pickRequest{Queue: true}); pick.Action != pickStart || pick.Host.Name != "mid" {
			t.Fatalf("pick = %+v, want start on mid", pick)
		}
	})

	t.Run("equal priority hosts probe in one tier with empty rest", func(t *testing.T) {
		a, probed := newProbeApp(t, map[string]map[string]string{
			"r1": openKeys,
			"r2": openKeys,
		}, nil)
		targets := []fleetHost{
			{Name: "one", Remote: "r1", Project: mode.Project},
			{Name: "two", Remote: "r2", Project: mode.Project},
		}
		cands, rest := a.probeFleetTiers(context.Background(), targets, pickRequest{}, "")
		if len(cands) != 2 || len(rest) != 0 || len(*probed) != 2 {
			t.Fatalf("equal priority cands=%d rest=%d probed=%v", len(cands), len(rest), *probed)
		}
	})

	t.Run("boxd-reported priority overrides hosts.json hint even without queue capability", func(t *testing.T) {
		a, _ := newProbeApp(t, map[string]map[string]string{
			"r1": openKeys,
			"r2": openKeys,
			"r3": openKeys,
		}, nil)
		a.HTTP = func(req *http.Request) (*http.Response, error) {
			switch req.URL.Host {
			case "one.example.com":
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"queue":false,"priority":0,"jobs":[]}`))}, nil
			case "two.example.com":
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"queue":true,"priority":20,"jobs":[]}`))}, nil
			default:
				return nil, fmt.Errorf("boxd down")
			}
		}
		targets := []fleetHost{
			{Name: "one", Remote: "r1", Project: mode.Project, DashboardURL: "https://one.example.com", Priority: 15},
			{Name: "two", Remote: "r2", Project: mode.Project, DashboardURL: "https://two.example.com", Priority: 15},
			{Name: "three", Remote: "r3", Project: mode.Project, DashboardURL: "https://three.example.com", Priority: 15},
		}
		cands, rest := a.probeFleetTiers(context.Background(), targets, pickRequest{}, "")
		if len(rest) != 0 || cands[0].Priority != 0 || cands[1].Priority != 20 || cands[2].Priority != 15 {
			t.Fatalf("cands=%+v rest=%+v, want priorities [0 20 15]", cands, rest)
		}
		if pick := pickFleetHost(cands, pickRequest{}); pick.Action != pickStart || pick.Host.Name != "two" {
			t.Fatalf("pick = %+v, want two (boxd priority 20)", pick)
		}
	})
}
