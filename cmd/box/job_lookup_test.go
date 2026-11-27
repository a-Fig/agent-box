package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
)

type lookupRunner func(context.Context, Cmd) (Result, error)

func (r lookupRunner) Run(ctx context.Context, cmd Cmd) (Result, error) { return r(ctx, cmd) }

func writeLookupTestHosts(t *testing.T, hosts ...fleetHost) {
	t.Helper()
	path := hostsTestPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(map[string][]fleetHost{"hosts": hosts})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, config, 0o600); err != nil {
		t.Fatal(err)
	}
}

func lookupTestApp(t *testing.T, westProject string) (*App, *bytes.Buffer, fleetHost, fleetHost) {
	t.Helper()
	west := fleetHost{Name: "West", Remote: "west", Project: westProject, DashboardURL: "https://west.example.com/dashboard/"}
	east := fleetHost{Name: "East", Remote: "east", Project: "agents", DashboardURL: "https://east.example.com/panel"}
	writeLookupTestHosts(t, west, east)
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
	a, out, west, east := lookupTestApp(t, "work")
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

func TestStoppedJobLogPagingUsesResolvedHost(t *testing.T) {
	a, out, west, _ := lookupTestApp(t, "agents")
	id := requireQualifiedJobID(t, west, testBareJobID)
	exit := 7
	started := jobStart(id.Bare)
	_, suffix, _ := strings.Cut(id.Bare, ".")
	j := dash.Job{Box: id.Box, ID: suffix, BoxUID: "west-original", State: dash.JobDone, StartedAt: started, Exit: &exit}
	j.Key = dash.JobKey(j.Box, j.ID, j.StartedAt)
	const log = "aé\xffmore bytes"
	want := log[1:3]
	a.R = lookupRunner(func(_ context.Context, cmd Cmd) (Result, error) {
		if len(cmd.Args) != 8 || cmd.Args[1] != west.Project || cmd.Args[3] != west.Remote+":"+id.Box ||
			!strings.Contains(cmd.Args[7], id.Bare) || strings.Contains(cmd.Args[7], id.ID) {
			t.Fatalf("log call used the wrong target or ID: %q", cmd.Args)
		}
		return Result{Code: 1, Stderr: notRunning}, nil
	})
	var calls []string
	oldServer := false
	a.HTTP = func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.Host != "west.example.com" {
			t.Fatalf("stopped page used the wrong boxd: %s %s", req.Method, req.URL)
		}
		calls = append(calls, req.URL.EscapedPath())
		var body any
		switch req.URL.EscapedPath() {
		case "/dashboard/api/queue/" + id.Bare:
			body = j
		case "/dashboard/api/jobs/" + url.PathEscape(j.Key) + "/log":
			if req.URL.Query().Get("offset") != "1" || req.URL.Query().Get("limit") != "2" || req.URL.Query().Get("stopped") != "1" {
				t.Fatalf("stopped page request = %s", req.URL)
			}
			body = dash.LogChunk{Data: want, Offset: 3, Stopped: !oldServer, LogBytes: int64(len(log))}
		default:
			t.Fatalf("unexpected boxd path %s", req.URL)
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(raw))}, nil
	}
	ctx := context.Background()
	if code := run(ctx, a, []string{"job", id.ID, "--offset", "1", "--limit", "2"}, strings.NewReader("")); code != 0 || out.String() != want {
		t.Fatalf("stopped CLI page = %d %q", code, out.String())
	}
	params, err := json.Marshal(map[string]any{"name": "box_job", "arguments": map[string]any{"id": id.ID, "offset": 1, "limit": 2}})
	if err != nil {
		t.Fatal(err)
	}
	result := callTool(ctx, a, params, nil)
	page, ok := result["structuredContent"].(map[string]any)
	if result["isError"] != false || !ok || page["job"] != id.ID || page["host"] != west.Name ||
		page["log"] != want || page["nextOffset"] != int64(3) || page["logBytes"] != int64(len(log)) || page["more"] != true {
		t.Fatalf("stopped MCP page = %+v", result)
	}
	if len(calls) != 4 {
		t.Fatalf("CLI and MCP made %d queue and page reads, want 4", len(calls))
	}

	oldServer = true
	if err := writeJSON(a.statePath(id.ID, ".result"), jobEnd{Code: exit, Tail: "local tail"}); err != nil {
		t.Fatal(err)
	}
	result = callTool(ctx, a, params, nil)
	raw, err := json.Marshal(result["structuredContent"])
	if err != nil || !strings.Contains(string(raw), `"tail":"local tail"`) {
		t.Fatalf("old boxd did not fall back to the local tail: %s, %v", raw, err)
	}
	oldServer = false
	result = callTool(ctx, a, params, nil)
	if page, ok := result["structuredContent"].(map[string]any); !ok || page["log"] != want {
		t.Fatalf("available stopped log did not take precedence over local tail: %+v", result)
	}
}

func TestStoppedJobLogDoesNotCrossProjectsForSameBareID(t *testing.T) {
	a, _, west, _ := lookupTestApp(t, "work")
	a.Remote, a.dashboardURL = west.Remote, west.DashboardURL
	exit := 3
	box, suffix, _ := strings.Cut(testBareJobID, ".")
	j := dash.Job{Box: box, ID: suffix, BoxUID: "agents-only", State: dash.JobDone, StartedAt: jobStart(testBareJobID), Exit: &exit, Tail: "legacy tail"}
	j.Key = dash.JobKey(j.Box, j.ID, j.StartedAt)
	a.R = lookupRunner(func(_ context.Context, cmd Cmd) (Result, error) {
		if len(cmd.Args) != 8 || cmd.Args[1] != a.Project || cmd.Args[3] != west.Remote+":"+box {
			t.Fatalf("local page used the wrong Incus project or remote: %q", cmd.Args)
		}
		return Result{Code: 1, Stderr: notRunning}, nil
	})
	var fullReads int
	a.HTTP = func(req *http.Request) (*http.Response, error) {
		var payload any = j
		if strings.HasSuffix(req.URL.Path, "/log") {
			fullReads++
			payload = dash.LogChunk{Stopped: true, Offset: 5, EOF: true, LogBytes: 5, Data: "there"}
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(raw))}, nil
	}

	ctx := context.Background()
	a.Project = "agents"
	lookup, err := a.lookupJob(testBareJobID)
	if err != nil {
		t.Fatal(err)
	}
	if p, _, err := lookup.page(ctx, 0, 0); err != nil || p.Text != "there" || fullReads != 1 {
		t.Fatalf("agents page = %+v, %v, full reads %d", p, err, fullReads)
	}
	a.Project = "work"
	lookup, err = a.lookupJob(testBareJobID)
	if err != nil {
		t.Fatal(err)
	}
	if p, end, err := lookup.page(ctx, 0, 0); err == nil || p.Text != "" || end == nil || end.Tail != j.Tail || fullReads != 1 {
		t.Fatalf("work project page = %+v, end %+v, %v, full reads %d", p, end, err, fullReads)
	}
}

func TestStoppedJobLogBoundsOptionalReadWithLocalTail(t *testing.T) {
	a, _, west, _ := lookupTestApp(t, "agents")
	id := requireQualifiedJobID(t, west, testBareJobID)
	a.R = lookupRunner(func(context.Context, Cmd) (Result, error) {
		return Result{Code: 1, Stderr: notRunning}, nil
	})
	a.HTTP = func(req *http.Request) (*http.Response, error) {
		deadline, ok := req.Context().Deadline()
		if !ok || time.Until(deadline) > stoppedPageWithTailBudget {
			t.Fatalf("optional lookup lacks the short tail budget: %s, %t", deadline, ok)
		}
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	if err := writeJSON(a.statePath(id.ID, ".result"), jobEnd{Code: 2, Tail: "kept tail"}); err != nil {
		t.Fatal(err)
	}
	lookup, err := a.lookupJob(id.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p, end, err := lookup.page(context.Background(), 0, 0); err == nil || p.Text != "" || end == nil || end.Tail != "kept tail" {
		t.Fatalf("read with unavailable boxd = %+v, end %+v, %v", p, end, err)
	}
}

func TestStoppedJobLogPageKeepsRawBytes(t *testing.T) {
	a, _, west, _ := lookupTestApp(t, "agents")
	id := requireQualifiedJobID(t, west, testBareJobID)
	exit := 0
	_, suffix, _ := strings.Cut(id.Bare, ".")
	j := dash.Job{Box: id.Box, ID: suffix, BoxUID: "west-original", State: dash.JobDone, StartedAt: jobStart(id.Bare), Exit: &exit}
	j.Key = dash.JobKey(j.Box, j.ID, j.StartedAt)
	lookup, err := a.lookupJob(id.ID)
	if err != nil {
		t.Fatal(err)
	}
	chunk := dash.LogChunk{Stopped: true, Offset: 2, LogBytes: 2, EOF: true, Data: "a�", DataBase64: base64.StdEncoding.EncodeToString([]byte("a\xff"))}
	lookup.app.HTTP = func(req *http.Request) (*http.Response, error) {
		raw, _ := json.Marshal(chunk)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(raw))}, nil
	}
	p, ok := lookup.stoppedPage(context.Background(), j, 0, 2)
	if !ok || p.Text != "a\xff" || p.More {
		t.Fatalf("raw page = %+v, ok %t", p, ok)
	}
	chunk.Offset++
	if p, ok := lookup.stoppedPage(context.Background(), j, 0, 2); ok {
		t.Fatalf("wrong next offset produced a page: %+v", p)
	}
}

func TestQualifiedJobRetainsResultsByFullID(t *testing.T) {
	a, _, west, east := lookupTestApp(t, "work")
	dirty := true
	westSource := &dash.SourceRevision{Commit: strings.Repeat("a", 40), Dirty: &dirty, SnapshotCommit: strings.Repeat("b", 40)}
	eastSource := &dash.SourceRevision{Commit: strings.Repeat("c", 40), Dirty: &dirty, SnapshotCommit: strings.Repeat("d", 40)}
	posted := map[string]int{}
	a.Post = func(_ context.Context, url string, body []byte) error {
		var end dash.JobEndedBody
		if err := json.Unmarshal(body, &end); err != nil || end.Box+"."+end.ID != testBareJobID {
			t.Fatalf("ended report to %s did not use the bare ID: %s, %v", url, body, err)
		}
		posted[url] = end.Exit
		return nil
	}
	a.HTTP = func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "east.example.com" {
			t.Fatalf("non-agents lookup contacted boxd: %s", req.URL)
		}
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
			src := westSource
			if cmd.Args[3] == "east:repo" {
				code = 7
				src = eastSource
			}
			fmt.Fprint(cmd.Stdout, cmd.Args[3]+" tail")
			meta, err := json.Marshal(jobMeta{Source: src})
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(cmd.Stderr, metaMarker+base64.StdEncoding.EncodeToString(meta))
			fmt.Fprintf(cmd.Stderr, "%s%d\n", exitMarker, code)
			return Result{}, nil
		}
		return Result{Code: 1}, nil
	})
	westID := requireQualifiedJobID(t, west, testBareJobID).ID
	eastID := requireQualifiedJobID(t, east, testBareJobID).ID
	if err := writeJSON(a.statePath(westID, ".json"), jobRecord{Job: westID, Queued: true}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{westID, eastID, testBareJobID} {
		res, data := lookupTool(t, a, id, false)
		if res["isError"] != false || data["job"] != id || data["state"] != stateDone {
			t.Fatalf("attach %s = %v", id, res)
		}
	}
	if len(posted) != 1 || posted["https://east.example.com/panel/api/jobs/ended"] != 7 {
		t.Fatalf("ended reports went to %v", posted)
	}
	gone = true
	for id, want := range map[string]*dash.SourceRevision{westID: westSource, eastID: eastSource} {
		lookup, err := a.lookupJob(id)
		if err != nil {
			t.Fatal(err)
		}
		r, err := lookup.job(context.Background(), 0)
		if err != nil || r.State != stateDone {
			t.Fatalf("offline lookup %s = %+v, %v", id, r, err)
		}
		requireSourceRevision(t, r.Source, want.Commit, *want.Dirty, want.SnapshotCommit)
	}
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
	a, _, west, _ := lookupTestApp(t, "agents")
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
		if requests < 3 || cmd.Args[1] != "agents" || cmd.Args[3] != "west:repo" {
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
	a, _, _, _ := lookupTestApp(t, "work")
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
