package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func cancelTool(a *App, id string) map[string]any {
	params := json.RawMessage(fmt.Sprintf(`{"name":"box_cancel","arguments":{"id":%q}}`, id))
	return callTool(context.Background(), a, params, nil)
}

func TestQualifiedCancelQueuedWithoutLocalMarker(t *testing.T) {
	a, _, west, east := lookupTestApp(t, "agents")
	westID := requireQualifiedJobID(t, west, testBareJobID).ID
	eastID := requireQualifiedJobID(t, east, testBareJobID).ID
	a.R = lookupRunner(func(_ context.Context, cmd Cmd) (Result, error) {
		t.Fatalf("queued cancellation called Incus: %q", cmd.Args)
		return Result{}, nil
	})
	requests := 0
	a.HTTP = func(req *http.Request) (*http.Response, error) {
		requests++
		if req.Method != http.MethodPost || req.URL.String() != "https://west.example.com/dashboard/api/queue/"+testBareJobID+"/cancel" {
			t.Fatalf("queued cancellation called %s %s", req.Method, req.URL)
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	}
	res := cancelTool(a, westID)
	data, _ := res["structuredContent"].(map[string]any)
	if res["isError"] != false || data["job"] != westID || data["cancelled"] != true || data["exitCode"] != exitDetached || requests != 1 {
		t.Fatalf("queued cancellation result = %v, requests = %d", res, requests)
	}
	if a.notifiedBy(westID) != "cancel" || a.exists(testBareJobID, ".notified") || a.exists(eastID, ".notified") {
		t.Fatal("queued cancellation did not claim only the qualified West job")
	}
}

func TestQualifiedCancelSameBareIDOnTwoHosts(t *testing.T) {
	a, out, west, east := lookupTestApp(t, "agents")
	westID := requireQualifiedJobID(t, west, testBareJobID).ID
	eastID := requireQualifiedJobID(t, east, testBareJobID).ID
	const queuePath = "/api/queue/" + testBareJobID
	a.WatchPoll = time.Millisecond
	var requests, cancelled []string
	a.HTTP = func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Method+" "+req.URL.String())
		// West's job is being started. The second check sees it running, so
		// cancellation must go to Incus even with no local queue marker.
		if req.URL.Host == "west.example.com" {
			if req.Method == http.MethodPost {
				return &http.Response{StatusCode: http.StatusConflict, Body: http.NoBody}, nil
			}
			state := `{"state":"queued"}`
			if len(requests) == 4 {
				state = `{"state":"running"}`
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(state))}, nil
		}
		return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody}, nil
	}
	a.R = lookupRunner(func(_ context.Context, cmd Cmd) (Result, error) {
		if cmd.Args[2] == "config" {
			return Result{Code: 1}, nil // idle expiry probe
		}
		if len(cmd.Args) != 8 || cmd.Args[2] != "exec" || cmd.Args[7] != cancelScript(testBareJobID) {
			t.Fatalf("wrong Incus cancel script: %q", cmd.Args)
		}
		cancelled = append(cancelled, cmd.Args[1]+"/"+cmd.Args[3])
		return Result{Stdout: "cancelled 130\n"}, nil
	})
	ctx := context.Background()
	if code := run(ctx, a, []string{"cancel", westID}, nil); code != 0 || !strings.Contains(out.String(), "cancelled "+westID+" (exit 130)") {
		t.Fatalf("West CLI cancel = %d, %q", code, out.String())
	}
	if a.notifiedBy(westID) != "cancel" || a.exists(eastID, ".notified") || a.exists(testBareJobID, ".notified") {
		t.Fatal("West cancel updated another job's notification claim")
	}
	res := cancelTool(a, eastID)
	data, _ := res["structuredContent"].(map[string]any)
	if res["isError"] != false || data["job"] != eastID || data["cancelled"] != true || data["exitCode"] != exitDetached {
		t.Fatalf("East MCP cancel = %v", res)
	}
	if !slices.Equal(cancelled, []string{"agents/west:repo", "agents/east:repo"}) || a.notifiedBy(eastID) != "cancel" || a.exists(testBareJobID, ".notified") {
		t.Fatalf("wrong Incus destination or claim: %v", cancelled)
	}
	westURL := "https://west.example.com/dashboard" + queuePath
	eastURL := "https://east.example.com/panel" + queuePath
	if !slices.Equal(requests, []string{"POST " + westURL + "/cancel", "GET " + westURL, "POST " + westURL + "/cancel", "GET " + westURL, "POST " + eastURL + "/cancel", "GET " + eastURL}) {
		t.Fatalf("wrong daemon calls: %v", requests)
	}
}

func TestQualifiedCancelNeverRoutesAnotherProjectThroughBoxd(t *testing.T) {
	a, _, work, agents := lookupTestApp(t, "work")
	work.Remote, work.DashboardURL = agents.Remote, agents.DashboardURL
	writeLookupTestHosts(t, work, agents)
	workID := requireQualifiedJobID(t, work, testBareJobID).ID
	agentsID := requireQualifiedJobID(t, agents, testBareJobID).ID
	a.HTTP = func(req *http.Request) (*http.Response, error) {
		t.Fatalf("work cancellation contacted the agents daemon: %s %s", req.Method, req.URL)
		return nil, nil
	}
	incusCalls := 0
	a.R = lookupRunner(func(_ context.Context, cmd Cmd) (Result, error) {
		if cmd.Args[1] != work.Project {
			t.Fatalf("work cancellation routed to %q", cmd.Args)
		}
		if cmd.Args[2] == "config" && cmd.Args[4] == work.Remote+":repo" {
			return Result{Code: 1}, nil
		}
		if len(cmd.Args) != 8 || cmd.Args[2] != "exec" || cmd.Args[3] != work.Remote+":repo" || cmd.Args[7] != cancelScript(testBareJobID) {
			t.Fatalf("wrong Incus cancel script: %q", cmd.Args)
		}
		incusCalls++
		return Result{Stdout: "cancelled 130\n"}, nil
	})
	res := cancelTool(a, workID)
	data, _ := res["structuredContent"].(map[string]any)
	if res["isError"] != false || data["job"] != workID || data["cancelled"] != true || incusCalls != 1 {
		t.Fatalf("work cancellation = %v, Incus execs = %d", res, incusCalls)
	}
	if a.notifiedBy(workID) != "cancel" || a.exists(agentsID, ".notified") || a.exists(testBareJobID, ".notified") {
		t.Fatal("work cancellation updated another project's notification claim")
	}
}

func TestQualifiedCancelRejectsUnknownHostWithoutMutation(t *testing.T) {
	a, _, _, _ := lookupTestApp(t, "agents")
	id := requireQualifiedJobID(t, fleetHost{Remote: "missing", Project: "agents"}, testBareJobID).ID
	a.R = lookupRunner(func(_ context.Context, cmd Cmd) (Result, error) {
		t.Fatalf("unknown host invoked Incus: %q", cmd.Args)
		return Result{}, nil
	})
	a.HTTP = func(req *http.Request) (*http.Response, error) {
		t.Fatalf("unknown host called boxd: %s %s", req.Method, req.URL)
		return nil, nil
	}
	if res := cancelTool(a, id); res["isError"] != true || !strings.Contains(fmt.Sprint(res["content"]), "not configured") {
		t.Fatalf("unknown host MCP result = %v", res)
	}
	if code := run(context.Background(), a, []string{"cancel", id}, nil); code == 0 {
		t.Fatal("CLI accepted an unknown cancellation target")
	}
	if files, err := os.ReadDir(a.StateDir); err != nil || len(files) != 0 {
		t.Fatalf("unknown host changed local state: %v, %v", files, err)
	}
}
