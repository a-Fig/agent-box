package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fleetTestRunner func(context.Context, Cmd) (Result, error)

func (r fleetTestRunner) Run(ctx context.Context, cmd Cmd) (Result, error) {
	return r(ctx, cmd)
}

func TestStatusWithoutConfigDirectory(t *testing.T) {
	t.Setenv("APPDATA", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	a, _ := testApp(&fakePC{t: t, mode: "full"})
	s, err := a.fleetStatus(context.Background(), "", false)
	if err != nil || !s.Reachable || s.Mode != "full" || len(s.Hosts) != 0 {
		t.Fatalf("status = %+v, %v", s, err)
	}
}

func TestFleetProbesIndependently(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slowStarted := make(chan struct{})
	fastQueueRead := make(chan struct{})
	a := newApp()
	a.Remote, a.Project = "fast", "work"
	a.R = fleetTestRunner(func(ctx context.Context, cmd Cmd) (Result, error) {
		switch strings.Join(cmd.Args, " ") {
		case "--project frozen list slow: --format json":
			close(slowStarted)
			select {
			case <-fastQueueRead:
				return Result{}, fmt.Errorf("remote unreachable")
			case <-ctx.Done():
				return Result{}, ctx.Err()
			}
		case "--project work list fast: --format json":
			select {
			case <-slowStarted:
				return Result{Stdout: "[]"}, nil
			case <-ctx.Done():
				return Result{}, ctx.Err()
			}
		case "--project work project get fast:work user.agent-box.mode":
			return Result{Stdout: "full"}, nil
		case "--project work project get fast:work limits.instances":
			return Result{Stdout: "3"}, nil
		}
		return Result{Code: 1}, nil
	})
	a.HTTP = func(req *http.Request) (*http.Response, error) {
		close(fastQueueRead)
		if req.URL.String() != "https://example.com/dashboard/api/state" || req.Method != "GET" {
			t.Errorf("queue probe = %s %s", req.Method, req.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"jobs":[{"state":"queued"},{"state":"running"}]}`))}, nil
	}
	s := a.probeFleet(ctx, "", []fleetHost{
		{Name: "down", Remote: "slow", Project: "frozen"},
		{Name: "ready", Remote: "fast", Project: "work", DashboardURL: "https://example.com/dashboard/"},
	}, 1, 2)
	if !s.Available || s.Remote != "fast" || len(s.Hosts) != 2 {
		t.Fatalf("selected host = %+v", s)
	}
	if s.Hosts[0].Reachable || !strings.Contains(s.Hosts[0].Error, "remote unreachable") || s.Hosts[0].Queued != nil {
		t.Fatalf("unreachable host = %+v", s.Hosts[0])
	}
	ready := s.Hosts[1]
	if !ready.Reachable || ready.Mode != "full" || ready.Queued == nil || *ready.Queued != 1 || len(ready.Sizes) == 0 || ready.Sizes[0].Fits != 3 {
		t.Fatalf("ready host = %+v", ready)
	}
	var out strings.Builder
	writeStatus(&out, s)
	if !strings.HasPrefix(out.String(), "hosts: 1 available, 1 of 2 reachable\n") ||
		!strings.Contains(out.String(), "selected host fast: available:") {
		t.Fatalf("status output = %q", out.String())
	}
}

func TestFleetStatusTimeoutAfterConnecting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	a := newApp()
	a.R = fleetTestRunner(func(ctx context.Context, cmd Cmd) (Result, error) {
		switch cmd.Args[len(cmd.Args)-1] {
		case "json":
			if cmd.Args[2] == "list" {
				return Result{Stdout: "[]"}, nil
			}
		case "user.agent-box.mode":
			return Result{Stdout: "full"}, nil
		case "limits.instances":
			<-ctx.Done()
			return Result{}, ctx.Err()
		}
		return Result{Code: 1}, nil
	})
	s := a.probeFleet(ctx, "", []fleetHost{{Name: "primary", Remote: "pc", Project: "agents"}}, 0, 1)
	row := s.Hosts[0]
	if !s.Reachable || s.Available || !row.Reachable || row.Available || row.Mode != "full" || len(row.Sizes) != 0 || row.Error != "context deadline exceeded" {
		t.Fatalf("timed out status = %+v; row = %+v", s, row)
	}
	var out strings.Builder
	writeStatus(&out, s)
	if !strings.Contains(out.String(), "capacity unknown (context deadline exceeded)") {
		t.Fatalf("status output = %q", out.String())
	}
}

func TestFleetStatusKeepsUnregisteredTargetOutOfRegistry(t *testing.T) {
	path := hostsTestPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"hosts":[{"name":"pc","remote":"spare"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newApp()
	a.Remote, a.Project = "pc", "agents"
	a.R = fleetTestRunner(func(_ context.Context, cmd Cmd) (Result, error) {
		if strings.Join(cmd.Args, " ") == "--project agents list spare: --format json" {
			return Result{Stdout: "[]"}, nil
		}
		return Result{Code: 1}, nil
	})
	s, err := a.fleetStatus(context.Background(), "", false)
	if err != nil || s.Reachable || s.Available || s.Remote != "pc" || len(s.Hosts) != 1 || s.Hosts[0].Remote != "spare" || !s.Hosts[0].Available {
		t.Fatalf("status = %+v, %v", s, err)
	}
}

func TestWriteFleetPriorityDisplay(t *testing.T) {
	t.Run("omits priority when all hosts have zero priority", func(t *testing.T) {
		var out strings.Builder
		writeFleet(&out, []fleetHostStatus{
			{Name: "primary", Remote: "pc", Project: "agents", Reachable: true, Available: true, Mode: "full", Room: "2 medium", GPU: "free"},
			{Name: "backup", Remote: "spare", Project: "agents", Error: "connection refused"},
		})
		if strings.Contains(out.String(), "priority") {
			t.Fatalf("unexpected priority output for equal-zero hosts:\n%s", out.String())
		}
	})

	t.Run("shows priority when any host has non-zero priority", func(t *testing.T) {
		var out strings.Builder
		writeFleet(&out, []fleetHostStatus{
			{Name: "primary", Remote: "pc", Project: "agents", Priority: 10, Reachable: true, Available: true, Mode: "full", Room: "2 medium", GPU: "free"},
			{Name: "backup", Remote: "spare", Project: "agents", Priority: 0, Reachable: true, Available: true, Mode: "shared", Room: "1 medium", GPU: "unavailable"},
			{Name: "offline", Remote: "cold", Project: "agents", Priority: -5, Error: "connection refused"},
		})
		want := "hosts: 2 available, 2 of 3 reachable\n" +
			"  primary (pc/agents): priority 10, full, room 2 medium, GPU free, queue unknown\n" +
			"  backup (spare/agents): priority 0, shared, room 1 medium, GPU unavailable, queue unknown\n" +
			"  offline (cold/agents): priority -5, unavailable (connection refused)\n"
		if out.String() != want {
			t.Fatalf("writeFleet:\n%s\nwant:\n%s", out.String(), want)
		}
	})

	t.Run("probeFleet prefers boxd priority and falls back to config hint", func(t *testing.T) {
		a := newApp()
		a.Remote, a.Project = "pc", "agents"
		a.R = fleetTestRunner(func(_ context.Context, cmd Cmd) (Result, error) {
			if len(cmd.Args) >= 4 && cmd.Args[2] == "list" {
				return Result{Stdout: "[]"}, nil
			}
			return Result{Code: 1}, nil
		})
		a.HTTP = func(req *http.Request) (*http.Response, error) {
			if req.URL.Host == "pc.example.com" {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"priority":25,"jobs":[]}`))}, nil
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"jobs":[]}`))}, nil
		}
		s := a.probeFleet(context.Background(), "", []fleetHost{
			{Name: "primary", Remote: "pc", Project: "agents", DashboardURL: "https://pc.example.com", Priority: 10},
			{Name: "backup", Remote: "spare", Project: "agents", DashboardURL: "https://spare.example.com", Priority: 5},
		}, 0, 2)
		if len(s.Hosts) != 2 || s.Hosts[0].Priority != 25 || s.Hosts[1].Priority != 5 {
			t.Fatalf("probeFleet hosts = %+v, want priorities [25 5]", s.Hosts)
		}
		var out strings.Builder
		writeFleet(&out, s.Hosts)
		if !strings.Contains(out.String(), "primary (pc/agents): priority 25,") || !strings.Contains(out.String(), "backup (spare/agents): priority 5,") {
			t.Fatalf("writeFleet = %q", out.String())
		}
	})
}
