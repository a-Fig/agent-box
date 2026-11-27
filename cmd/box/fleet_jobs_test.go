package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestFleetJobsListUniqueDestinationsWithPartialResults(t *testing.T) {
	a, out, west, east := lookupTestApp(t, "work")
	a.HTTP = func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("dashboard offline") }
	alias := west
	alias.Name = "West alias"
	westAgents := fleetHost{Name: "West agents", Remote: west.Remote, Project: "agents"}
	offline := fleetHost{Name: "Offline", Remote: "offline", Project: "agents"}
	writeLookupTestHosts(t, west, alias, offline, east, westAgents)
	a.Remote, a.Project = "unregistered", "agents"
	errs := new(bytes.Buffer)
	a.Err = errs
	var mu sync.Mutex
	reads := map[[2]string]int{}
	const newerID = "repo.20261126-224000-1234"
	a.R = lookupRunner(func(_ context.Context, cmd Cmd) (Result, error) {
		if len(cmd.Args) < 4 || cmd.Args[0] != "--project" {
			return Result{}, fmt.Errorf("unexpected Incus args %q", cmd.Args)
		}
		remote, _, _ := strings.Cut(cmd.Args[3], ":")
		key := [2]string{remote, cmd.Args[1]}
		switch cmd.Args[2] {
		case "list":
			mu.Lock()
			reads[key]++
			mu.Unlock()
			if key == [2]string{"offline", "agents"} {
				return Result{}, fmt.Errorf("host offline")
			}
			switch key {
			case [2]string{"west", "work"}, [2]string{"east", "agents"}, [2]string{"west", "agents"}, [2]string{"unregistered", "agents"}:
				return Result{Stdout: `[{"name":"repo","status":"Running"}]`}, nil
			}
		case "exec":
			if len(cmd.Args) == 8 && cmd.Args[3] == remote+":repo" && cmd.Args[7] == jobsScript() {
				if key == [2]string{"east", "agents"} {
					return Result{Stdout: testBareJobID + " running - 8\n" + newerID + " done 0 9\n"}, nil
				}
				return Result{Stdout: testBareJobID + " running - 8\n"}, nil
			}
		}
		return Result{}, fmt.Errorf("unexpected Incus args %q", cmd.Args)
	})
	wantIDs := []string{
		requireQualifiedJobID(t, east, newerID).ID,
		requireQualifiedJobID(t, west, testBareJobID).ID,
		requireQualifiedJobID(t, east, testBareJobID).ID,
		requireQualifiedJobID(t, westAgents, testBareJobID).ID,
		testBareJobID,
	}
	if c := run(context.Background(), a, []string{"jobs"}, nil); c != 0 {
		t.Fatalf("box jobs exited %d: %s", c, errs)
	}
	cli := out.String()
	if !strings.HasPrefix(cli, "HOST") || !strings.Contains(cli, westAgents.Name) || strings.Contains(cli, alias.Name) || !strings.Contains(errs.String(), "Offline (offline/agents): host offline") {
		t.Fatalf("fleet CLI output = %q, stderr = %q", cli, errs)
	}
	fields := strings.Fields(cli)
	for _, id := range wantIDs {
		if first := slices.Index(fields, id); first < 0 || slices.Contains(fields[first+1:], id) {
			t.Fatalf("fleet CLI omitted or repeated %s: %q", id, cli)
		}
	}
	res := callTool(context.Background(), a, json.RawMessage(`{"name":"box_job"}`), nil)
	structured, _ := res["structuredContent"].(map[string]any)
	jobs, _ := structured["jobs"].([]JobInfo)
	if res["isError"] != false || len(jobs) != len(wantIDs) || !strings.Contains(fmt.Sprint(res["content"]), "5 job(s), 4 running") || !strings.Contains(fmt.Sprint(res["content"]), "on West agents") || !strings.Contains(fmt.Sprint(structured["warnings"]), "Offline (offline/agents)") {
		t.Fatalf("fleet MCP result = %v", res)
	}
	wantHosts := []string{east.Name, west.Name, east.Name, westAgents.Name, "unregistered"}
	for i, job := range jobs {
		if job.Job != wantIDs[i] || job.Host != wantHosts[i] {
			t.Fatalf("job %d = %+v, want %s on %s", i, job, wantIDs[i], wantHosts[i])
		}
	}
	wantReads := map[[2]string]int{{"west", "work"}: 2, {"east", "agents"}: 2, {"west", "agents"}: 2, {"offline", "agents"}: 2, {"unregistered", "agents"}: 2}
	if !maps.Equal(reads, wantReads) {
		t.Fatalf("destination reads = %v, want %v", reads, wantReads)
	}
}

func TestFleetJobsPreserveDefaultListings(t *testing.T) {
	for _, filter := range []bool{false, true} {
		name := "missing registry"
		if filter {
			name = "explicit box with a registry"
		}
		t.Run(name, func(t *testing.T) {
			if filter {
				writeLookupTestHosts(t, fleetHost{Name: "Other", Remote: "elsewhere", Project: "agents"})
			} else {
				hostsTestPath(t)
			}
			a := newApp()
			a.Remote, a.Project, a.StateDir = "default", "work", t.TempDir()
			var out bytes.Buffer
			a.Out, a.Err = &out, &out
			a.R = lookupRunner(func(_ context.Context, cmd Cmd) (Result, error) {
				if cmd.Args[1] != "work" {
					t.Fatalf("legacy scan changed project: %q", cmd.Args)
				}
				switch {
				case cmd.Args[2] == "list" && cmd.Args[3] == "default:":
					return Result{Stdout: `[{"name":"repo","status":"Running"}]`}, nil
				case cmd.Args[2] == "exec" && cmd.Args[3] == "default:repo":
					return Result{Stdout: testBareJobID + " running - 8\n"}, nil
				default:
					t.Fatalf("legacy scan used wrong remote: %q", cmd.Args)
					return Result{}, nil
				}
			})
			args := []string{"jobs"}
			if filter {
				args = append(args, "--box", "repo")
			}
			if c := run(context.Background(), a, args, nil); c != 0 || !strings.HasPrefix(out.String(), "JOB ") || !strings.Contains(out.String(), testBareJobID) || strings.Contains(out.String(), "h1-") {
				t.Fatalf("legacy CLI = %d, %q", c, &out)
			}
			if !filter {
				res := callTool(context.Background(), a, json.RawMessage(`{"name":"box_job"}`), nil)
				data, err := json.Marshal(res)
				if err != nil || res["isError"] != false || !strings.Contains(string(data), testBareJobID) || strings.Contains(string(data), `"host"`) {
					t.Fatalf("legacy MCP result = %s, %v", data, err)
				}
			}
		})
	}
}

func TestFleetJobsReportUnavailableAndConflictingHosts(t *testing.T) {
	a, _, west, east := lookupTestApp(t, "work")
	a.HTTP = func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("dashboard offline") }
	alias := west
	alias.Name, alias.DashboardURL = "Conflicting alias", "https://example.com/conflict"
	writeLookupTestHosts(t, west, alias, east)
	a.R = lookupRunner(func(_ context.Context, cmd Cmd) (Result, error) {
		if cmd.Args[2] != "list" || cmd.Args[3] != "east:" {
			return Result{}, fmt.Errorf("unexpected Incus args %q", cmd.Args)
		}
		return Result{}, fmt.Errorf("host offline")
	})
	jobs, err := a.listJobs(context.Background(), "")
	if len(jobs) != 0 || err == nil || !strings.Contains(err.Error(), "conflicting dashboard URLs") || !strings.Contains(err.Error(), "East (east/agents): host offline") {
		t.Fatalf("listing with no reachable destination = %v, %v", jobs, err)
	}
}
