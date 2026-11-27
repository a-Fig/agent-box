package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/a-Fig/agent-box/internal/mode"
)

func TestCapacityRefusalRefreshesTheSelectedHost(t *testing.T) {
	for _, tc := range []struct {
		name, memory, want string
		cli, queue         bool
	}{
		{name: "CLI no queue", memory: "3GiB", want: "small", cli: true},
		{name: "MCP no queue", memory: "5GiB", want: "medium"},
		{name: "MCP queued", memory: "3GiB", want: "small", queue: true},
		{name: "no smaller size fits", memory: "1GiB"},
		{name: "failed capacity refresh", memory: "unreadable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := fleetHost{Name: "worker", Remote: "worker", Project: mode.Project, DashboardURL: "https://worker.example.com"}
			worker := &fakeFleetHost{remote: host.Remote, project: host.Project, mode: "full", queueCapable: true,
				keys: map[string]string{"limits.memory": "12GiB"}, refuseLaunch: memoryRefusal}
			ff := newFakeFleet(t, worker)
			ff.dashHosts[host.DashboardURL] = worker
			a, out, dir := fleetDispatchTestApp(t, ff, []fleetHost{host})
			a.Getenv = envMap(map[string]string{"T3CODE_THREAD_ID": "thread-test"})
			a.Push = &fakePusher{}
			a.Spawn = func(string, string) error { return nil }
			ctx := context.Background()
			if status := a.jobHostApp(host).Status(ctx, ""); !sizeFits(status.Sizes, "large") {
				t.Fatalf("fixture status had no room: %+v", status)
			}
			var launches []string
			a.R = lookupRunner(func(ctx context.Context, c Cmd) (Result, error) {
				if c.Name == "incus" && len(c.Args) >= 3 && c.Args[2] == "launch" {
					launches = append(launches, strings.Join(c.Args, " "))
					worker.mu.Lock()
					worker.keys["limits.memory"] = tc.memory // capacity changes at launch
					worker.mu.Unlock()
				}
				return ff.Run(ctx, c)
			})

			var text string
			if tc.cli {
				t.Chdir(dir)
				code := run(ctx, a, []string{"run", "--host", host.Name, "--new", "--size", "large", "--no-queue", "--reason", testReason, "true"}, nil)
				if code != exitPaused {
					t.Fatalf("CLI = %d, %q", code, out)
				}
				text = out.String()
			} else {
				state := stateRefused
				if tc.queue {
					state = stateQueued
				}
				text = checkMCPRunCapacity(t, a, map[string]any{
					"dir": dir, "new": true, "size": "large", "command": "true", "reason": testReason, "queue": tc.queue,
				}, host.Name, state, "memory", tc.want)
			}
			wantOutcome := "refused:"
			if tc.queue {
				wantOutcome = "queued job"
			}
			if !strings.Contains(text, wantOutcome) || !strings.Contains(text, "capacity limit on worker: memory.") {
				t.Fatalf("outcome or limit missing: %q", text)
			}
			if tc.want == "" {
				if strings.Contains(text, "appears to fit") {
					t.Fatalf("unexpected smaller size: %q", text)
				}
			} else if !strings.Contains(text, "A "+tc.want+" box appears to fit now if it meets the job's needs.") {
				t.Fatalf("fresh capacity estimate missing: %q", text)
			}
			if len(launches) != 1 || !strings.Contains(launches[0], "user.box.size=large") {
				t.Fatalf("launch changed size: %v", launches)
			}
			if tc.queue {
				if len(worker.enqueued) != 1 || worker.enqueued[0].Size != "large" {
					t.Fatalf("queue changed size: %+v", worker.enqueued)
				}
			} else if len(worker.enqueued) != 0 {
				t.Fatalf("queue disabled but submitted %d jobs", len(worker.enqueued))
			}
		})
	}
}

func TestCapacityPreservesGPURequirement(t *testing.T) {
	host := fleetHost{Name: "worker", Remote: "worker", Project: mode.Project, DashboardURL: "https://worker.example.com"}
	worker := &fakeFleetHost{remote: host.Remote, project: host.Project, mode: "full", queueCapable: true,
		keys: map[string]string{gpuKey: "allow"}, insts: []instance{{Name: "training", Status: "Running", Config: gpuConfig()}}}
	ff := newFakeFleet(t, worker)
	ff.dashHosts[host.DashboardURL] = worker
	a, out, dir := fleetDispatchTestApp(t, ff, []fleetHost{host})
	a.Getenv = envMap(map[string]string{"T3CODE_THREAD_ID": "thread-test"})
	a.Push = &fakePusher{}
	a.Spawn = func(string, string) error { return nil }
	text := checkMCPRunCapacity(t, a, map[string]any{"dir": dir, "gpu": true, "command": "true", "reason": testReason},
		host.Name, stateQueued, "gpu", "")
	if len(worker.enqueued) != 1 || !worker.enqueued[0].GPU || worker.enqueued[0].Size != mode.GPUSize {
		t.Fatalf("queue lost GPU requirement: %+v", worker.enqueued)
	}
	t.Chdir(dir)
	if code := run(context.Background(), a, []string{"run", "--gpu", "--no-queue", "--reason", testReason, "true"}, nil); code != exitPaused ||
		!strings.Contains(out.String(), "refused:") || !strings.Contains(out.String(), "capacity limit on worker: GPU") || len(worker.enqueued) != 1 {
		t.Fatalf("GPU refusal = %d, %q", code, out)
	}
	for _, s := range []string{text, out.String()} {
		if strings.Contains(s, "appears to fit") || strings.Contains(s, "run on CPU") {
			t.Fatalf("GPU result suggested switching to CPU: %q", s)
		}
	}
}

func TestCapacityOmitsEstimateForUnverifiedDiskQuota(t *testing.T) {
	for _, tc := range []struct {
		name, resource, info, smaller string
	}{
		{"pool cap", "disk.pool.default", `{"resources":{"disk":{"limit":214748364800,"usage":0},"disk.pool.default":{"limit":107374182400,"usage":96636764160}}}`, ""},
		{"missing aggregate usage", "disk", `{}`, ""},
		{"verified aggregate usage", "disk", `{"resources":{"disk":{"limit":107374182400,"usage":85899345920}}}`, "small"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hostsTestPath(t)
			f := &fakePC{t: t, mode: "full", project: map[string]string{"info": tc.info},
				refuse: fmt.Sprintf(`Error: Reached maximum aggregate value "100GiB" for "limits.%s" in project "agents"`, tc.resource)}
			e := newPushEnv(t, f, "thread-test")
			text := checkMCPRunCapacity(t, e.a, e.runArgs(map[string]any{"new": true, "size": "large", "queue": false}, "true"),
				"", stateRefused, tc.resource, tc.smaller)
			if strings.Contains(text, "appears to fit") != (tc.smaller != "") {
				t.Fatalf("unexpected disk estimate: %q", text)
			}
		})
	}
}

func TestCapacityEstimateForNamedBoxDependsOnWhetherItExists(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(fmt.Sprintf("exists=%t", exists), func(t *testing.T) {
			hostsTestPath(t)
			f := &fakePC{t: t, refuse: memoryRefusal, mode: "full",
				project: map[string]string{"limits.memory": "3GiB", "info": "{}"}}
			if exists {
				f.insts = []instance{{Name: "named", Status: "Running", Config: map[string]string{mode.SizeKey: "large"}}}
				f.project["limits.memory"] = "10GiB" // still room for a small box
			}
			a, out := testApp(f)
			t.Chdir(newRepo(t))
			args := []string{"run", "--box", "named", "--size", "medium", "--no-queue", "--reason", testReason, "true"}
			code := run(context.Background(), a, args, nil)
			if code != exitPaused || strings.Contains(out.String(), "A small box appears to fit") == exists {
				t.Fatalf("named box run = %d, %q", code, out)
			}
			if !exists && (len(f.launches) != 1 || f.launches[0][2] != "pc:named") {
				t.Fatalf("launch did not use the requested name: %v", f.launches)
			}
		})
	}
}

func TestCapacityDistinguishesContainerAndInstanceLimits(t *testing.T) {
	for _, tc := range []struct{ resource, message string }{
		{"containers", `Error: Reached maximum number of instances of type "container" in project "agents"`},
		{"instances", `Error: Reached maximum number of instances in project "agents"`},
	} {
		t.Run(tc.resource, func(t *testing.T) {
			hostsTestPath(t)
			e := newPushEnv(t, &fakePC{t: t, refuse: tc.message}, "thread-test")
			checkMCPRunCapacity(t, e.a, e.runArgs(map[string]any{"new": true, "queue": false}, "true"),
				"", stateRefused, tc.resource, "")
		})
	}

	// Fleet preflight sees the same container quota before a launch.
	host := fleetHost{Name: "worker", Remote: "worker", Project: mode.Project}
	worker := &fakeFleetHost{remote: host.Remote, project: host.Project, mode: "full"}
	ff := newFakeFleet(t, worker)
	a, _, dir := fleetDispatchTestApp(t, ff, []fleetHost{host})
	a.R = lookupRunner(func(ctx context.Context, c Cmd) (Result, error) {
		if c.Name == "incus" && len(c.Args) > 3 && c.Args[2] == "project" && c.Args[3] == "info" {
			return Result{Stdout: `{"resources":{"containers":{"limit":1,"usage":1}}}`}, nil
		}
		if c.Name == "incus" && len(c.Args) > 2 && c.Args[2] == "launch" {
			t.Fatal("fleet attempted a launch despite the container cap")
		}
		return ff.Run(ctx, c)
	})
	checkMCPRunCapacity(t, a, map[string]any{"dir": dir, "new": true, "queue": false, "command": "true", "reason": testReason},
		host.Name, stateRefused, "containers", "")
}

func checkMCPRunCapacity(t *testing.T, a *App, args map[string]any, host, state, resource, smaller string) string {
	t.Helper()
	params, _ := json.Marshal(map[string]any{"name": "box_run", "arguments": args})
	res := callTool(context.Background(), a, params, nil)
	raw, _ := json.Marshal(res["structuredContent"])
	var got JobResult
	if err := json.Unmarshal(raw, &got); err != nil || res["isError"] != false || got.State != state || got.Host != host ||
		got.Capacity == nil || !slices.Equal(got.Capacity.Limits, []string{resource}) || got.Capacity.SmallerSize != smaller {
		t.Fatalf("MCP capacity = %s, %v", raw, res["content"])
	}
	return fmt.Sprint(res["content"])
}
