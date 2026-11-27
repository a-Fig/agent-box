package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/jobrun"
	"github.com/a-Fig/agent-box/internal/mode"
)

func TestProbeFleetCandidatesIndependentAndTyped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slowStarted := make(chan struct{})
	fastDone := make(chan struct{})
	var workBoxdCalls atomic.Int32

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
		case "--project agents project get fast:agents user.agent-box.mode":
			return Result{Stdout: "full"}, nil
		case "--project agents project get fast:agents limits.instances":
			return Result{Stdout: "4"}, nil
		case "--project agents project get fast:agents restricted.devices.unix-char":
			return Result{Stdout: "allow"}, nil
		case "--project work project get custom:work user.agent-box.mode":
			return Result{Stdout: "shared"}, nil
		case "--project work project get custom:work limits.instances":
			return Result{Stdout: "2"}, nil
		case "--project work project get custom:work restricted.devices.unix-char":
			return Result{Stdout: "block"}, nil
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
	a := newApp()
	a.Now = func() time.Time { return time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC) }
	a.R = fleetTestRunner(func(_ context.Context, cmd Cmd) (Result, error) {
		switch strings.Join(cmd.Args, " ") {
		case "--project agents list pc: --format json":
			return Result{Stdout: `[{"name":"busy","status":"Running","config":{"user.box.size":"medium"}}]`}, nil
		case "--project agents project get pc:agents user.agent-box.mode":
			return Result{Stdout: "full"}, nil
		case "--project agents project get pc:agents limits.instances":
			return Result{Stdout: "1"}, nil
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
			switch strings.Join(cmd.Args, " ") {
			case "--project agents list pc: --format json":
				return Result{Stdout: "[]"}, nil
			case "--project agents project get pc:agents user.agent-box.mode":
				return Result{Stdout: "full"}, nil
			case "--project agents project get pc:agents limits.instances":
				return Result{Stdout: "3"}, nil
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

func TestProbeFleetCandidatePooledThrowawayRequiresIdleAndPoolEnabled(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	origin := "https://github.com/example/repo.git"
	med := pickTestSize(t, "medium")
	repoKey := throwawayRepoKey(origin, med)

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
				joined := strings.Join(cmd.Args, " ")
				switch joined {
				case "--project agents list pc: --format json":
					return Result{Stdout: makeInsts(tc.exp)}, nil
				case "--project agents project get pc:agents user.agent-box.mode":
					return Result{Stdout: "full"}, nil
				case "--project agents project get pc:agents limits.instances":
					return Result{Stdout: "1"}, nil
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
