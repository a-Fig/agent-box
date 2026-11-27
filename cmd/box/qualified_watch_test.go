package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
)

func TestQualifiedWatchRestartsOnSavedHosts(t *testing.T) {
	a, _, west, east := lookupTestApp(t, "work")
	t.Setenv("T3CODE_HOME", t.TempDir())
	a.Thread = "thread-owner"
	pusher := &fakePusher{}
	a.Push = pusher
	var spawns []string
	a.Spawn = func(dir, job string) error {
		if dir != a.StateDir {
			t.Fatalf("spawn state = %s, want %s", dir, a.StateDir)
		}
		spawns = append(spawns, job)
		return nil
	}
	westID := requireQualifiedJobID(t, west, testBareJobID).ID
	eastID := requireQualifiedJobID(t, east, testBareJobID).ID
	if !a.startWatchOnHost(jobMeta{}, westID, west, false, false, false) || !a.startWatchOnHost(jobMeta{}, eastID, east, false, false, true) {
		t.Fatal("qualified watchers did not start")
	}
	if !slices.Equal(spawns, []string{westID, eastID}) {
		t.Fatalf("spawned = %v", spawns)
	}
	for _, tc := range []struct {
		id   string
		host fleetHost
	}{{westID, west}, {eastID, east}} {
		rec, err := a.readWatchRecord(tc.id)
		want := jobLocator{Name: tc.host.Name, Remote: tc.host.Remote, Project: tc.host.Project, DashboardURL: tc.host.DashboardURL}
		if err != nil || rec.Host == nil || *rec.Host != want || rec.Thread != "thread-owner" {
			t.Fatalf("record for %s = %+v, %v", tc.id, rec, err)
		}
	}

	// West is removed, East's dashboard changes, and the launcher inherits
	// new defaults. Recovery must keep the original Incus and boxd targets.
	restarted := *a
	restarted.Remote, restarted.Project, restarted.Thread = "replacement", "other", "thread-other"
	moved := east
	moved.DashboardURL = "https://example.com/moved"
	writeLookupTestHosts(t, moved)
	spawns = nil
	restarted.safetyNet("")
	if len(spawns) != 0 {
		t.Fatalf("another thread recovered watchers: %v", spawns)
	}
	restarted.Thread = "thread-owner"
	restarted.safetyNet(westID)
	if !slices.Equal(spawns, []string{eastID}) {
		t.Fatalf("recovery used the wrong ID for its skip key: %v", spawns)
	}
	spawns = nil
	restarted.safetyNet("")
	slices.Sort(spawns)
	wantSpawns := []string{westID, eastID}
	slices.Sort(wantSpawns)
	if !slices.Equal(spawns, wantSpawns) {
		t.Fatalf("restarted watchers = %v, want %v", spawns, wantSpawns)
	}

	var follows []string
	restarted.R = lookupRunner(func(_ context.Context, cmd Cmd) (Result, error) {
		if len(cmd.Args) < 4 {
			return Result{}, fmt.Errorf("unexpected Incus call: %q", cmd.Args)
		}
		project, boxArg := cmd.Args[1], cmd.Args[3]
		if cmd.Args[2] == "config" {
			boxArg = cmd.Args[4]
		}
		target := project + "/" + boxArg
		var code int
		switch target {
		case "work/west:repo":
			code = 4
		case "agents/east:repo":
			code = 7
		default:
			t.Fatalf("watcher contacted the wrong Incus destination: %q", cmd.Args)
		}
		if cmd.Args[2] == "config" {
			return Result{Code: 1}, nil
		}
		if cmd.Args[2] != "exec" || len(cmd.Args) != 8 {
			t.Fatalf("unexpected Incus call: %q", cmd.Args)
		}
		switch {
		case cmd.Args[7] == followScript(testBareJobID):
			follows = append(follows, target)
			fmt.Fprintln(cmd.Stdout, target+" tail")
			fmt.Fprintf(cmd.Stderr, "%s%d\n", exitMarker, code)
			return Result{}, nil
		case strings.HasPrefix(cmd.Args[7], "# cancelled\n") && strings.Contains(cmd.Args[7], testBareJobID+".cancelled") && !strings.Contains(cmd.Args[7], "h1-"):
			return Result{}, nil
		default:
			t.Fatalf("watcher sent the wrong guest script: %q", cmd.Args)
			return Result{}, nil
		}
	})
	queueCalls, endReports := 0, 0
	restarted.HTTP = func(req *http.Request) (*http.Response, error) {
		queueCalls++
		if req.Method != http.MethodGet || req.URL.String() != east.DashboardURL+"/api/queue/"+testBareJobID {
			t.Fatalf("recovered queue request = %s %s", req.Method, req.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"state":"running","startedAt":"2026-11-26T22:38:50Z"}`))}, nil
	}
	restarted.Post = func(_ context.Context, address string, body []byte) error {
		endReports++
		var end dash.JobEndedBody
		if err := json.Unmarshal(body, &end); err != nil || address != east.DashboardURL+"/api/jobs/ended" || end.Box+"."+end.ID != testBareJobID || end.Exit != 7 {
			t.Fatalf("end report = %s %s, %v", address, body, err)
		}
		return nil
	}
	restarted.Thread = ""
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, id := range []string{westID, eastID} {
		if err := restarted.Watch(ctx, id); err != nil {
			t.Fatal(err)
		}
		if !restarted.exists(id, ".ended") || !restarted.exists(id, ".result") || restarted.notifiedBy(id) != "watcher" || restarted.exists(id, ".watch") {
			t.Fatalf("wrong retained state for %s", id)
		}
		if err := restarted.Watch(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if queueCalls != 1 || endReports != 1 || !slices.Equal(follows, []string{"work/west:repo", "agents/east:repo"}) {
		t.Fatalf("queue requests %d, end reports %d, follows %v", queueCalls, endReports, follows)
	}
	if pusher.count() != 2 || pusher.sent[0].thread != "thread-owner" || pusher.sent[1].thread != "thread-owner" ||
		!strings.Contains(pusher.sent[0].msg, westID+" on "+west.Name+" ended, exit 4") ||
		!strings.Contains(pusher.sent[1].msg, eastID+" on "+east.Name+" ended, exit 7") {
		t.Fatalf("wrong delivery records: %v", pusher.sent)
	}
	for id, host := range map[string]string{westID: west.Name, eastID: east.Name} {
		end, ok := restarted.recordedEnd(id)
		if !ok || end.Host != host {
			t.Fatalf("recorded end for %s = %+v, %v, want host %s", id, end, ok, host)
		}
	}
	for _, ext := range []string{".json", ".result", ".ended", ".notified", ".watch", ".sending"} {
		if restarted.exists(testBareJobID, ext) {
			t.Fatalf("qualified watcher wrote an unqualified %s record", ext)
		}
	}
}

func TestQualifiedWatchRejectsInvalidSavedRecords(t *testing.T) {
	a, _, west, east := lookupTestApp(t, "work")
	a.Getenv = func(string) string { return "" }
	a.Thread = "thread-owner"
	a.Push = &fakePusher{}
	a.Spawn = func(_, job string) error {
		t.Fatalf("invalid watch record spawned %s", job)
		return nil
	}
	a.R = lookupRunner(func(_ context.Context, cmd Cmd) (Result, error) {
		t.Fatalf("invalid watch record contacted Incus: %q", cmd.Args)
		return Result{}, nil
	})
	a.HTTP = func(req *http.Request) (*http.Response, error) {
		t.Fatalf("invalid watch record contacted boxd: %s", req.URL)
		return nil, nil
	}
	id := requireQualifiedJobID(t, west, testBareJobID).ID
	if a.startWatchOnHost(jobMeta{}, id, east, false, false, false) || a.exists(id, ".json") {
		t.Fatal("mismatched locator was saved at startup")
	}
	for _, tc := range []struct {
		name, job, want string
		host            *jobLocator
		queued          bool
	}{
		{name: "different job", job: requireQualifiedJobID(t, east, testBareJobID).ID, want: "record does not match"},
		{name: "missing host", job: id, want: "no saved host"},
		{name: "different project", job: id, host: &jobLocator{Remote: west.Remote, Project: east.Project}, want: "saved host does not match"},
		{name: "invalid dashboard", job: id, host: &jobLocator{Remote: west.Remote, Project: west.Project, DashboardURL: "https://user@example.com"}, want: "invalid saved dashboard URL"},
		{name: "queue outside agents", job: id, host: &jobLocator{Remote: west.Remote, Project: west.Project}, queued: true, want: "boxd cannot follow queued job"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := writeJSON(a.statePath(id, ".json"), jobRecord{Job: tc.job, Thread: a.Thread, Host: tc.host, Queued: tc.queued}); err != nil {
				t.Fatal(err)
			}
			a.safetyNet("")
			if err := a.Watch(context.Background(), id); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Watch = %v, want %q", err, tc.want)
			}
			if a.exists(id, ".ended") || a.exists(id, ".watch") || a.exists(id, ".notified") {
				t.Fatal("invalid watcher left result or lease state")
			}
		})
	}
}
