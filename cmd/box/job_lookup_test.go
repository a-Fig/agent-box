package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
)

type lookupRunner func(context.Context, Cmd) (Result, error)

func (r lookupRunner) Run(ctx context.Context, cmd Cmd) (Result, error) { return r(ctx, cmd) }

func lookupTestApp(t *testing.T) (*App, *bytes.Buffer, fleetHost, fleetHost) {
	t.Helper()
	path := hostsTestPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	west := fleetHost{Name: "West", Remote: "west", Project: "work", DashboardURL: "https://west.example.com/dashboard/"}
	east := fleetHost{Name: "East", Remote: "east", Project: "agents", DashboardURL: "https://east.example.com/panel"}
	config, err := json.Marshal(map[string][]fleetHost{"hosts": {west, east}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, config, 0o600); err != nil {
		t.Fatal(err)
	}
	out := new(bytes.Buffer)
	a := newApp()
	a.Remote, a.Project, a.Out, a.Err, a.StateDir = "east", "agents", out, io.Discard, t.TempDir()
	a.Push, a.Post = nil, nil
	a.PausePoll = time.Hour
	return a, out, west, east
}

func lookupTool(t *testing.T, a *App, id string, paged bool) (map[string]any, map[string]any) {
	t.Helper()
	args := map[string]any{"id": id}
	if paged {
		args["offset"] = 0
	}
	params, err := json.Marshal(map[string]any{"name": "box_job", "arguments": args})
	if err != nil {
		t.Fatal(err)
	}
	res := callTool(context.Background(), a, params, nil)
	var structured map[string]any
	if value := res["structuredContent"]; value != nil {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &structured); err != nil {
			t.Fatal(err)
		}
	}
	return res, structured
}

func TestQualifiedJobLogRoutesByHost(t *testing.T) {
	a, out, west, east := lookupTestApp(t)
	a.R = lookupRunner(func(_ context.Context, cmd Cmd) (Result, error) {
		if len(cmd.Args) != 8 || !strings.Contains(cmd.Args[7], testBareJobID) || strings.Contains(cmd.Args[7], "h1-") {
			t.Fatalf("log call did not use the bare ID: %q", cmd.Args)
		}
		switch cmd.Args[1] + "/" + cmd.Args[3] {
		case "work/west:repo":
			return Result{Stdout: "done 0 8\nwest log"}, nil
		case "agents/east:repo":
			return Result{Stdout: "done 0 8\neast log"}, nil
		default:
			t.Fatalf("wrong Incus project or remote: %q", cmd.Args)
			return Result{}, nil
		}
	})
	for _, tc := range []struct{ id, host, log string }{
		{requireQualifiedJobID(t, west, testBareJobID).ID, west.Name, "west log"},
		{requireQualifiedJobID(t, east, testBareJobID).ID, east.Name, "east log"},
		{testBareJobID, "", "east log"},
	} {
		out.Reset()
		args := []string{"job", tc.id, "--offset", "0"}
		if code := run(context.Background(), a, args, strings.NewReader("")); code != 0 || out.String() != tc.log {
			t.Fatalf("CLI log %s = %d, %q", tc.id, code, out.String())
		}
		res, data := lookupTool(t, a, tc.id, true)
		if res["isError"] != false || data["job"] != tc.id || data["log"] != tc.log || (tc.host == "" && data["host"] != nil) || (tc.host != "" && data["host"] != tc.host) {
			t.Fatalf("MCP log %s = %v", tc.id, res)
		}
	}
}

func TestQualifiedJobRetainsResultsByFullID(t *testing.T) {
	a, _, west, east := lookupTestApp(t)
	posted := map[string]int{}
	a.Post = func(_ context.Context, url string, body []byte) error {
		var end dash.JobEndedBody
		if err := json.Unmarshal(body, &end); err != nil || end.Box+"."+end.ID != testBareJobID {
			t.Fatalf("ended report to %s did not use the bare ID: %s, %v", url, body, err)
		}
		posted[url] = end.Exit
		return nil
	}
	a.HTTP = func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody}, nil
	}
	gone := false
	a.R = lookupRunner(func(_ context.Context, cmd Cmd) (Result, error) {
		if len(cmd.Args) == 8 && cmd.Args[2] == "exec" {
			if gone {
				return Result{Code: 3}, nil
			}
			if !strings.Contains(cmd.Args[7], testBareJobID) || strings.Contains(cmd.Args[7], "h1-") {
				t.Fatalf("attach sent a qualified ID to Incus: %q", cmd.Args)
			}
			code := 4
			if cmd.Args[3] == "east:repo" {
				code = 7
			}
			fmt.Fprint(cmd.Stdout, cmd.Args[3]+" tail")
			fmt.Fprintf(cmd.Stderr, "%s%d\n", exitMarker, code)
			return Result{}, nil
		}
		return Result{Code: 1}, nil
	})
	westID := requireQualifiedJobID(t, west, testBareJobID).ID
	eastID := requireQualifiedJobID(t, east, testBareJobID).ID
	for _, id := range []string{westID, eastID, testBareJobID} {
		res, data := lookupTool(t, a, id, false)
		if res["isError"] != false || data["job"] != id || data["state"] != stateDone {
			t.Fatalf("attach %s = %v", id, res)
		}
	}
	if len(posted) != 2 || posted["https://west.example.com/dashboard/api/jobs/ended"] != 4 || posted["https://east.example.com/panel/api/jobs/ended"] != 7 {
		t.Fatalf("ended reports went to %v", posted)
	}
	gone = true
	for _, tc := range []struct {
		id, tail string
		code     float64
	}{
		{westID, "west:repo tail", 4},
		{eastID, "east:repo tail", 7},
		{testBareJobID, "east:repo tail", 7},
	} {
		for _, paged := range []bool{true, false} {
			res, data := lookupTool(t, a, tc.id, paged)
			tail, _ := data["tail"].(string)
			if res["isError"] != false || data["job"] != tc.id || data["exitCode"] != tc.code || !strings.HasSuffix(tail, tc.tail) || (paged && tail != tc.tail) {
				t.Fatalf("retained %s (paged %t) = %v", tc.id, paged, res)
			}
		}
	}
}

func TestQualifiedJobLooksUpQueueWithoutLocalMarker(t *testing.T) {
	a, _, west, _ := lookupTestApp(t)
	id := requireQualifiedJobID(t, west, testBareJobID).ID
	requests := 0
	a.HTTP = func(req *http.Request) (*http.Response, error) {
		requests++
		if req.Method != http.MethodGet || req.URL.String() != "https://west.example.com/dashboard/api/queue/"+testBareJobID {
			t.Fatalf("queue request = %s %s", req.Method, req.URL)
		}
		state := `{"box":"repo","position":2,"state":"queued"}`
		if requests > 2 {
			state = `{"box":"repo","state":"done","exit":7,"tail":"queued tail"}`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(state))}, nil
	}
	a.R = lookupRunner(func(_ context.Context, cmd Cmd) (Result, error) {
		if requests < 3 || cmd.Args[1] != "work" || cmd.Args[3] != "west:repo" {
			t.Fatalf("unexpected Incus call: %q", cmd.Args)
		}
		return Result{Code: 3}, nil // the queued job ended and its box is gone
	})
	res, data := lookupTool(t, a, id, false)
	if res["isError"] != false || data["job"] != id || data["state"] != stateQueued || data["position"] != 2.0 || data["host"] != west.Name {
		t.Fatalf("queued MCP result = %v", res)
	}
	if code := run(context.Background(), a, []string{"job", id}, strings.NewReader("")); code != 7 {
		t.Fatalf("CLI follow returned %d, want queued job's exit code 7", code)
	}
}

func TestUnknownQualifiedJobHostNeverContactsDefault(t *testing.T) {
	a, _, _, _ := lookupTestApp(t)
	id := requireQualifiedJobID(t, fleetHost{Remote: "west", Project: "other"}, testBareJobID).ID
	a.R = lookupRunner(func(_ context.Context, cmd Cmd) (Result, error) {
		t.Fatalf("unknown host called Incus: %q", cmd.Args)
		return Result{}, nil
	})
	a.HTTP = func(req *http.Request) (*http.Response, error) {
		t.Fatalf("unknown host called boxd: %s", req.URL)
		return nil, nil
	}
	for _, paged := range []bool{true, false} {
		res, _ := lookupTool(t, a, id, paged)
		if res["isError"] != true || !strings.Contains(fmt.Sprint(res["content"]), "not configured") {
			t.Fatalf("unknown qualified job (paged %t) = %v", paged, res)
		}
	}
	if code := run(context.Background(), a, []string{"job", id}, strings.NewReader("")); code == 0 {
		t.Fatal("CLI accepted an unknown job host")
	}
}
