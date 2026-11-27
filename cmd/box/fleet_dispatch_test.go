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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/mode"
)

// fakeFleetHost simulates one Incus remote/project for fleet dispatch tests.
type fakeFleetHost struct {
	remote         string
	project        string
	mode           string
	insts          []instance
	keys           map[string]string
	unreachable    bool
	refuseLaunch   string
	execCode       int
	execStdout     string
	detachFollow   bool
	queueCapable   bool
	queueHTTPCode  int
	loseQueueReply bool

	mu          sync.Mutex
	pushedFiles []string
	startJobs   []string
	followJobs  []string
	endReports  []string
	enqueued    []dash.QueueRequest
	cancelled   []string
	boxdHits    int
}

func (h *fakeFleetHost) projectKeys() map[string]string {
	m := map[string]string{
		mode.ProjectModeKey: h.mode,
		"limits.instances":  "4",
		gpuKey:              "block",
	}
	for k, v := range h.keys {
		m[k] = v
	}
	return m
}

type fakeFleet struct {
	hosts     map[[2]string]*fakeFleetHost // keyed by (remote, project)
	dashHosts map[string]*fakeFleetHost    // keyed by dashboard URL host prefix
	mu        sync.Mutex
	snapshots int
}

func newFakeFleet(t *testing.T, hosts ...*fakeFleetHost) *fakeFleet {
	t.Helper()
	ff := &fakeFleet{
		hosts:     map[[2]string]*fakeFleetHost{},
		dashHosts: map[string]*fakeFleetHost{},
	}
	for _, h := range hosts {
		ff.hosts[[2]string{h.remote, h.project}] = h
	}
	return ff
}

func (ff *fakeFleet) Run(ctx context.Context, c Cmd) (Result, error) {
	if c.Name == "git" {
		if len(c.Args) > 0 && c.Args[0] == "bundle" {
			ff.mu.Lock()
			ff.snapshots++
			ff.mu.Unlock()
		}
		return execRunner{}.Run(ctx, c)
	}
	if c.Name != "incus" || len(c.Args) < 3 || c.Args[0] != "--project" {
		return Result{Code: 1, Stderr: fmt.Sprintf("unexpected cmd %s %q", c.Name, c.Args)}, nil
	}
	project := c.Args[1]
	args := c.Args[2:]

	// Determine target remote from the Incus args.
	remote := ""
	switch args[0] {
	case "list":
		remote, _, _ = strings.Cut(args[1], ":")
	case "project":
		if len(args) >= 3 {
			remote, _, _ = strings.Cut(args[2], ":")
		}
	case "launch":
		remote, _, _ = strings.Cut(args[2], ":")
	case "start", "delete", "exec":
		remote, _, _ = strings.Cut(args[1], ":")
	case "config":
		if len(args) >= 3 {
			remote, _, _ = strings.Cut(args[2], ":")
		}
	case "file":
		if len(args) >= 4 {
			remote, _, _ = strings.Cut(args[len(args)-1], ":")
		}
	}
	h := ff.hosts[[2]string{remote, project}]
	if h == nil {
		return Result{Code: 1, Stderr: "Error: unknown remote " + remote + "/" + project}, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.unreachable {
		return Result{Code: 1, Stderr: "Error: connection refused"}, nil
	}

	switch args[0] {
	case "list":
		b, _ := json.Marshal(h.insts)
		return Result{Stdout: string(b)}, nil
	case "project":
		if res, ok := respondHealthyProject(c, h.projectKeys(), "{}"); ok {
			return res, nil
		}
	case "launch":
		if h.refuseLaunch != "" {
			return Result{Code: 1, Stderr: h.refuseLaunch}, nil
		}
		_, name, _ := strings.Cut(args[2], ":")
		cfg := map[string]string{}
		for i, arg := range args {
			if arg == "-c" && i+1 < len(args) {
				k, v, _ := strings.Cut(args[i+1], "=")
				cfg[k] = v
			}
		}
		h.insts = append(h.insts, instance{Name: name, Status: "Running", Config: cfg})
		return Result{}, nil
	case "start":
		_, name, _ := strings.Cut(args[1], ":")
		for i := range h.insts {
			if h.insts[i].Name == name {
				h.insts[i].Status = "Running"
			}
		}
		return Result{}, nil
	case "delete":
		_, name, _ := strings.Cut(args[1], ":")
		h.insts = slices.DeleteFunc(h.insts, func(in instance) bool { return in.Name == name })
		return Result{}, nil
	case "config":
		_, name, _ := strings.Cut(args[2], ":")
		if args[1] == "get" {
			for _, in := range h.insts {
				if in.Name == name {
					return Result{Stdout: in.Config[args[3]] + "\n"}, nil
				}
			}
			return Result{Stdout: "\n"}, nil
		}
		if args[1] == "set" {
			k, v, _ := strings.Cut(args[3], "=")
			for i := range h.insts {
				if h.insts[i].Name == name {
					if h.insts[i].Config == nil {
						h.insts[i].Config = map[string]string{}
					}
					h.insts[i].Config[k] = v
				}
			}
			return Result{}, nil
		}
	case "file":
		h.pushedFiles = append(h.pushedFiles, args[len(args)-1])
		return Result{}, nil
	case "exec":
		script := args[len(args)-1]
		switch {
		case len(args) >= 2 && args[len(args)-2] == "-lc":
			h.startJobs = append(h.startJobs, script)
			return Result{}, nil
		case script == jobsScript():
			_, name, _ := strings.Cut(args[1], ":")
			return Result{Stdout: name + ".20261126-120000-0001 done 0 8\n"}, nil
		case strings.HasPrefix(script, "# follow\n"):
			h.followJobs = append(h.followJobs, script)
			if h.detachFollow {
				<-ctx.Done()
				return Result{}, ctx.Err()
			}
			if c.Stdout != nil && h.execStdout != "" {
				io.WriteString(c.Stdout, h.execStdout)
			}
			if c.Stderr != nil {
				fmt.Fprintf(c.Stderr, "%s%d exited\n", exitMarker, h.execCode)
			}
			return Result{}, nil
		default:
			return Result{Stdout: ""}, nil
		}
	}
	return Result{Code: 1, Stderr: "unhandled incus args: " + strings.Join(args, " ")}, nil
}

func (ff *fakeFleet) httpDo(req *http.Request) (*http.Response, error) {
	base := req.URL.Scheme + "://" + req.URL.Host
	h := ff.dashHosts[base]
	if h == nil {
		return nil, fmt.Errorf("unknown dashboard %s", base)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.boxdHits++
	switch {
	case req.Method == http.MethodGet && req.URL.Path == "/api/state":
		body := fmt.Sprintf(`{"queue":%t,"jobs":[]}`, h.queueCapable)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	case req.Method == http.MethodPost && req.URL.Path == "/api/queue":
		mr, err := req.MultipartReader()
		if err != nil {
			return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":"bad multipart"}`))}, nil
		}
		var qr dash.QueueRequest
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":"read part"}`))}, nil
			}
			if part.FormName() == "request" {
				json.NewDecoder(part).Decode(&qr)
			} else {
				io.Copy(io.Discard, part)
			}
		}
		h.enqueued = append(h.enqueued, qr)
		if h.loseQueueReply {
			return nil, io.ErrUnexpectedEOF
		}
		if h.queueHTTPCode != 0 && h.queueHTTPCode != http.StatusCreated {
			return &http.Response{StatusCode: h.queueHTTPCode, Body: io.NopCloser(strings.NewReader(`{"error":"queue full"}`))}, nil
		}
		job := qr.Box + "." + qr.Ref
		if qr.New {
			job = qr.Box + "-a1b2." + qr.Ref
		}
		rep, _ := json.Marshal(dash.QueueReply{Job: job, Position: 1, Why: dash.RefusedFull})
		return &http.Response{StatusCode: http.StatusCreated, Body: io.NopCloser(bytes.NewReader(rep))}, nil
	case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/api/queue/"):
		bare := strings.TrimPrefix(req.URL.Path, "/api/queue/")
		box, _, _ := strings.Cut(bare, ".")
		j, _ := json.Marshal(dash.Job{ID: bare, Box: box, State: dash.JobRunning, StartedAt: time.Date(2026, 11, 26, 12, 0, 1, 0, time.UTC)})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(j))}, nil
	case req.Method == http.MethodPost && strings.HasPrefix(req.URL.Path, "/api/queue/") && strings.HasSuffix(req.URL.Path, "/cancel"):
		bare := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/api/queue/"), "/cancel")
		h.cancelled = append(h.cancelled, bare)
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader(""))}, nil
	default:
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
}

func fleetDispatchTestApp(t *testing.T, ff *fakeFleet, hosts []fleetHost) (*App, *bytes.Buffer, string) {
	t.Helper()
	if hosts != nil {
		writeLookupTestHosts(t, hosts...)
	} else {
		hostsTestPath(t) // empty config dir -> no hosts.json
	}
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "widget")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, repoDir, "init", "-q")
	write(t, filepath.Join(repoDir, "a.txt"), "hi\n")
	gitT(t, repoDir, "add", "a.txt")
	gitT(t, repoDir, "commit", "-q", "-m", "init")

	var out bytes.Buffer
	seq := 0
	a := &App{
		R:        ff,
		Remote:   "pc",
		Project:  mode.Project,
		Image:    "agent-base",
		Out:      &out,
		Err:      &out,
		StateDir: t.TempDir(),
		Now:      func() time.Time { return time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC) },
		Rand:     func() string { seq++; return fmt.Sprintf("%04x", seq) },
		HTTP:     ff.httpDo,
		Post: func(_ context.Context, addr string, body []byte) error {
			for base, h := range ff.dashHosts {
				if strings.HasPrefix(addr, base) {
					h.mu.Lock()
					h.endReports = append(h.endReports, addr+" "+string(body))
					h.mu.Unlock()
				}
			}
			return nil
		},
	}
	return a, &out, repoDir
}

func TestFleetRunSingleHostBypassNoRegistry(t *testing.T) {
	pc := &fakeFleetHost{remote: "pc", project: mode.Project, mode: "full", execStdout: "single-ok\n"}
	ff := newFakeFleet(t, pc)
	a, out, repoDir := fleetDispatchTestApp(t, ff, nil)

	r, err := a.Run(context.Background(), RunOpts{Dir: repoDir, Cmd: []string{"echo single-ok"}, Reason: "Single-host bypass regression check."})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	id, err := parseJobID(r.Job)
	if err != nil || id.hostTag != "" || r.Host != "" || r.fleetHost != nil || r.State != stateDone || r.Code != 0 {
		t.Fatalf("single-host result = %+v (id=%+v, err=%v)", r, id, err)
	}
	if !strings.Contains(out.String(), "single-ok") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestFleetRunPicksBestHostIssuesQualifiedIDAndBareGuestID(t *testing.T) {
	alpha := &fakeFleetHost{remote: "r1", project: mode.Project, mode: "full", queueCapable: true}
	beta := &fakeFleetHost{
		remote:     "r2",
		project:    "work", // non-agents project: zero boxd calls allowed
		mode:       "full",
		execStdout: "beta-ok\n",
		insts:      []instance{{Name: "widget", Status: "Running", Config: map[string]string{mode.SizeKey: "medium", expiresKey: "2026-11-26T14:00:00Z"}}},
	}
	ff := newFakeFleet(t, alpha, beta)
	ff.dashHosts["https://alpha.example.com"] = alpha
	ff.dashHosts["https://beta.example.com"] = beta

	hosts := []fleetHost{
		{Name: "alpha", Remote: "r1", Project: mode.Project, DashboardURL: "https://alpha.example.com"},
		{Name: "beta", Remote: "r2", Project: "work", DashboardURL: "https://beta.example.com"},
	}
	a, out, repoDir := fleetDispatchTestApp(t, ff, hosts)
	a.Remote, a.Project = beta.remote, beta.project

	var notices []jobNotice
	a.Notify = func(n jobNotice) { notices = append(notices, n) }

	r, err := a.Run(context.Background(), RunOpts{Dir: repoDir, Cmd: []string{"echo beta-ok"}, Reason: "Fleet dispatch affinity and qualified ID test."})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	if r.State != stateDone || r.Code != 0 || r.Host != "beta" || r.Box != "widget" || r.fleetHost == nil || *r.fleetHost != hosts[1] {
		t.Fatalf("result = %+v\n%s", r, out.String())
	}
	id, err := parseJobID(r.Job)
	if err != nil || id.hostTag == "" || !strings.HasPrefix(id.Bare, "widget.") {
		t.Fatalf("expected qualified ID, got %q (%+v, %v)", r.Job, id, err)
	}
	if !strings.Contains(out.String(), "box: job "+r.Job+" on widget on beta") {
		t.Fatalf("stderr missing qualified ID and host:\n%s", out.String())
	}
	if want := (jobNotice{Event: "starting_job", Box: "widget", Job: r.Job, Host: "beta"}); !slices.Equal(notices, []jobNotice{want}) {
		t.Fatalf("notices = %+v, want %+v", notices, want)
	}

	// Incus guest files/scripts must receive the bare ID, never the qualified prefix.
	if len(beta.pushedFiles) == 0 || !strings.Contains(beta.pushedFiles[0], id.Bare) || strings.Contains(beta.pushedFiles[0], jobHostPrefix) {
		t.Fatalf("beta pushedFiles = %v, want bare ID %s", beta.pushedFiles, id.Bare)
	}
	if len(beta.startJobs) != 1 || !strings.Contains(beta.startJobs[0], id.Bare) || strings.Contains(beta.startJobs[0], jobHostPrefix) {
		t.Fatalf("beta startJobs = %v, want bare ID", beta.startJobs)
	}
	if len(beta.followJobs) != 1 || !strings.Contains(beta.followJobs[0], id.Bare) || strings.Contains(beta.followJobs[0], jobHostPrefix) {
		t.Fatalf("beta followJobs = %v, want bare ID", beta.followJobs)
	}
	// Non-agents project must make zero boxd HTTP requests or end reports.
	if beta.boxdHits != 0 || len(beta.endReports) != 0 {
		t.Fatalf("non-agents beta contacted boxd: hits=%d endReports=%v", beta.boxdHits, beta.endReports)
	}
	// Kept result on client is recorded under the public qualified ID and resolves via lookupJob.
	if end, ok := a.recordedEnd(r.Job); !ok || end.Code != 0 || end.Host != "beta" || !strings.Contains(end.Tail, "beta-ok") {
		t.Fatalf("recordedEnd(%s) = %+v, %v", r.Job, end, ok)
	}
	lookup, err := a.lookupJob(r.Job)
	if err != nil || lookup.host != "beta" || lookup.app.Remote != "r2" || lookup.app.Project != "work" {
		t.Fatalf("lookupJob(%s) = %+v, %v", r.Job, lookup, err)
	}
	jobs, err := a.listJobs(context.Background(), "")
	if err != nil || len(jobs) != 1 || jobs[0].Job != r.Job || jobs[0].Host != r.Host {
		t.Fatalf("fleet jobs = %+v, %v, want %s on %s", jobs, err, r.Job, r.Host)
	}
}

func TestFleetRunFallsBackOnPreStartRaceButNotOnCommandFailure(t *testing.T) {
	alpha := &fakeFleetHost{
		remote:       "r1",
		project:      mode.Project,
		mode:         "full",
		queueCapable: true,
		refuseLaunch: memoryRefusal, // races full right at launch
	}
	beta := &fakeFleetHost{
		remote:       "r2",
		project:      mode.Project,
		mode:         "full",
		queueCapable: true,
		execStdout:   "ran-on-beta\n",
	}
	ff := newFakeFleet(t, alpha, beta)
	ff.dashHosts["https://alpha.example.com"] = alpha
	ff.dashHosts["https://beta.example.com"] = beta
	hosts := []fleetHost{
		{Name: "alpha", Remote: "r1", Project: mode.Project, DashboardURL: "https://alpha.example.com"},
		{Name: "beta", Remote: "r2", Project: mode.Project, DashboardURL: "https://beta.example.com"},
	}
	a, out, repoDir := fleetDispatchTestApp(t, ff, hosts)

	r, err := a.Run(context.Background(), RunOpts{Dir: repoDir, Cmd: []string{"echo ok"}, New: true, Queue: true, Reason: "Test fallback when first host races full."})
	if err != nil || r.State != stateDone || r.Code != 0 || r.Host != "beta" {
		t.Fatalf("fallback result = %+v, %v\n%s", r, err, out.String())
	}
	if ff.snapshots != 1 {
		t.Fatalf("snapshots = %d, want 1 reused across fallback", ff.snapshots)
	}
	if strings.Contains(out.String(), "nothing started") {
		t.Fatalf("unexpected refusal noise before fallback succeeded:\n%s", out.String())
	}

	// Command exiting 75 on the selected host is a real command result: never fall back.
	alpha.refuseLaunch = ""
	alpha.execCode = 75
	beta.startJobs = nil
	r, err = a.Run(context.Background(), RunOpts{Dir: repoDir, Cmd: []string{"exit 75"}, New: true, Queue: true, Reason: "Verify exit 75 command does not fail over."})
	if err != nil || r.State != stateDone || r.Code != 75 || r.Host != "alpha" || len(beta.startJobs) != 0 {
		t.Fatalf("command exit 75 result = %+v, %v, beta starts=%d", r, err, len(beta.startJobs))
	}
}

func TestFleetRunQueuesOnBestAgentsHostAndFollowsCLI(t *testing.T) {
	fullKeys := map[string]string{"limits.instances": "1"}
	busyInst := []instance{{Name: "other", Status: "Running", Config: map[string]string{mode.SizeKey: "medium"}}}

	workHost := &fakeFleetHost{remote: "r1", project: "work", mode: "full", keys: fullKeys, insts: busyInst, queueCapable: true}
	noQueueHost := &fakeFleetHost{remote: "r2", project: mode.Project, mode: "full", keys: fullKeys, insts: busyInst, queueCapable: false}
	overflow := &fakeFleetHost{remote: "r3", project: mode.Project, mode: "full", keys: fullKeys, insts: busyInst, queueCapable: true, execStdout: "queued-done\n"}

	ff := newFakeFleet(t, workHost, noQueueHost, overflow)
	ff.dashHosts["https://work.example.com"] = workHost
	ff.dashHosts["https://noq.example.com"] = noQueueHost
	ff.dashHosts["https://overflow.example.com"] = overflow

	hosts := []fleetHost{
		{Name: "work", Remote: "r1", Project: "work", DashboardURL: "https://work.example.com"},
		{Name: "noq", Remote: "r2", Project: mode.Project, DashboardURL: "https://noq.example.com"},
		{Name: "overflow", Remote: "r3", Project: mode.Project, DashboardURL: "https://overflow.example.com"},
	}
	a, out, repoDir := fleetDispatchTestApp(t, ff, hosts)

	r, err := a.Run(context.Background(), RunOpts{Dir: repoDir, Cmd: []string{"echo queued-done"}, Queue: true, Reason: "Test fleet queue destination selection."})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	if r.State != stateQueued || r.Host != "overflow" || r.fleetHost == nil || *r.fleetHost != hosts[2] {
		t.Fatalf("queued result = %+v\n%s", r, out.String())
	}
	id, err := parseJobID(r.Job)
	if err != nil || id.hostTag == "" {
		t.Fatalf("queued job ID should be qualified, got %q (%v)", r.Job, err)
	}
	if len(overflow.enqueued) != 1 || workHost.boxdHits != 0 || len(noQueueHost.enqueued) != 0 {
		t.Fatalf("enqueued overflow=%d workHits=%d noq=%d", len(overflow.enqueued), workHost.boxdHits, len(noQueueHost.enqueued))
	}
	if !strings.Contains(out.String(), "box: queued job "+r.Job+" on overflow,") {
		t.Fatalf("stderr = %q", out.String())
	}

	// CLI record + followQueuedResult must target overflow with bare IDs and return qualified result.
	info := jobMeta{Reason: "Test fleet queue follow.", Command: "echo queued-done"}
	a.recordQueuedOnHost(info, r.Job, r.fleetHost, false, r.throwaway)
	rec, err := a.readWatchRecord(r.Job)
	if err != nil || rec.Host == nil || rec.Host.Remote != "r3" || rec.Host.DashboardURL != "https://overflow.example.com" {
		t.Fatalf("recorded queued host = %+v, %v", rec, err)
	}
	followed, err := a.followQueuedResult(context.Background(), r)
	if err != nil || followed.State != stateDone || followed.Code != 0 || followed.Job != r.Job || followed.Host != "overflow" {
		t.Fatalf("followQueuedResult = %+v, %v", followed, err)
	}
	if len(overflow.followJobs) != 1 || strings.Contains(overflow.followJobs[0], jobHostPrefix) {
		t.Fatalf("overflow followJobs = %v, want bare ID", overflow.followJobs)
	}
}

func TestFleetRunQueueFallbackOnEnqueueFailure(t *testing.T) {
	fullKeys := map[string]string{"limits.instances": "1"}
	busyInst := []instance{{Name: "other", Status: "Running", Config: map[string]string{mode.SizeKey: "medium"}}}

	first := &fakeFleetHost{remote: "r1", project: mode.Project, mode: "full", keys: fullKeys, insts: busyInst, queueCapable: true, queueHTTPCode: http.StatusInsufficientStorage}
	second := &fakeFleetHost{remote: "r2", project: mode.Project, mode: "full", keys: fullKeys, insts: busyInst, queueCapable: true}

	ff := newFakeFleet(t, first, second)
	ff.dashHosts["https://first.example.com"] = first
	ff.dashHosts["https://second.example.com"] = second
	hosts := []fleetHost{
		{Name: "first", Remote: "r1", Project: mode.Project, DashboardURL: "https://first.example.com"},
		{Name: "second", Remote: "r2", Project: mode.Project, DashboardURL: "https://second.example.com"},
	}
	a, _, repoDir := fleetDispatchTestApp(t, ff, hosts)

	r, err := a.Run(context.Background(), RunOpts{Dir: repoDir, Cmd: []string{"true"}, Queue: true, Reason: "Test queue fallback when first queue is full."})
	if err != nil || r.State != stateQueued || r.Host != "second" || len(first.enqueued) != 1 || len(second.enqueued) != 1 {
		t.Fatalf("queue fallback = %+v, %v (enqueued first=%d second=%d)", r, err, len(first.enqueued), len(second.enqueued))
	}

	first.queueHTTPCode, first.loseQueueReply = 0, true
	second.enqueued = nil
	r, err = a.Run(context.Background(), RunOpts{Dir: repoDir, Cmd: []string{"true"}, Queue: true, Reason: "Test ambiguity when a queue response is lost."})
	if err == nil || !strings.Contains(err.Error(), "first: queue submission could not be confirmed") || r.Host != "first" || len(second.enqueued) != 0 {
		t.Fatalf("lost queue reply = %+v, %v (second enqueues=%d)", r, err, len(second.enqueued))
	}
}

func TestFleetRunCLIPinningAndBoxGuard(t *testing.T) {
	pc := &fakeFleetHost{remote: "pc", project: mode.Project, mode: "full", execStdout: "default-ok\n"}
	alpha := &fakeFleetHost{
		remote: "r1", project: mode.Project, mode: "full", queueCapable: true,
		insts: []instance{{Name: "widget", Status: "Running", Config: map[string]string{mode.SizeKey: "medium"}}},
	}
	beta := &fakeFleetHost{remote: "r2", project: mode.Project, mode: "full", queueCapable: true, execStdout: "beta-pinned\n"}

	ff := newFakeFleet(t, pc, alpha, beta)
	ff.dashHosts["https://alpha.example.com"] = alpha
	ff.dashHosts["https://beta.example.com"] = beta
	hosts := []fleetHost{
		{Name: "alpha", Remote: "r1", Project: mode.Project, DashboardURL: "https://alpha.example.com"},
		{Name: "beta", Remote: "r2", Project: mode.Project, DashboardURL: "https://beta.example.com"},
	}
	a, out, repoDir := fleetDispatchTestApp(t, ff, hosts)

	t.Chdir(repoDir)

	// CLI --host beta overrides alpha's warm repo affinity.
	code := run(context.Background(), a, []string{"run", "--host", "beta", "--reason", "Pin debug run to beta host.", "--", "echo", "beta-pinned"}, nil)
	if code != 0 || !strings.Contains(out.String(), "on beta\n") || len(alpha.startJobs) != 0 || len(beta.startJobs) != 1 {
		t.Fatalf("pinned CLI code=%d alpha=%d beta=%d\n%s", code, len(alpha.startJobs), len(beta.startJobs), out.String())
	}

	// Unknown --host fails with exit 2.
	out.Reset()
	code = run(context.Background(), a, []string{"run", "--host", "missing", "--reason", "Check unknown host error.", "--", "true"}, nil)
	if code != 2 || !strings.Contains(out.String(), `host "missing" is not configured`) {
		t.Fatalf("unknown --host code=%d out=%q", code, out.String())
	}

	// Pinned unreachable host refuses without spilling over to alpha.
	beta.unreachable = true
	out.Reset()
	code = run(context.Background(), a, []string{"run", "--host", "beta", "--reason", "Pinned down host must refuse.", "--", "true"}, nil)
	if code != exitPaused || len(alpha.startJobs) != 0 || !strings.Contains(out.String(), "beta: unreachable") {
		t.Fatalf("pinned down code=%d alpha=%d out=%q", code, len(alpha.startJobs), out.String())
	}

	// Explicit --box without --host bypasses fleet probes and runs on default host with bare ID.
	beta.unreachable = false
	out.Reset()
	r, err := a.Run(context.Background(), RunOpts{Dir: repoDir, Box: "namedbox", Cmd: []string{"echo default-ok"}, Reason: "Explicit box without host stays on default."})
	if err != nil || r.Host != "" || r.fleetHost != nil || strings.Contains(r.Job, jobHostPrefix) || len(pc.startJobs) != 1 {
		t.Fatalf("explicit --box bypass = %+v, %v (pc starts=%d)", r, err, len(pc.startJobs))
	}

	// Explicit --box WITH --host runs on pinned host with qualified ID.
	beta.startJobs = nil
	r, err = a.Run(context.Background(), RunOpts{Dir: repoDir, Box: "namedbox", Host: "beta", Cmd: []string{"echo beta-pinned"}, Reason: "Explicit box with host pin routes to beta."})
	if err != nil || r.Host != "beta" || r.Box != "namedbox" || !strings.Contains(r.Job, jobHostPrefix) || len(beta.startJobs) != 1 {
		t.Fatalf("explicit --box --host = %+v, %v", r, err)
	}
}

func TestMCPBoxRunFleetWatcherAndSchemaUnchanged(t *testing.T) {
	// Agent tool input schema must NOT expose host on box_run (#163).
	for _, tl := range tools {
		if tl.Name == "box_run" {
			props := tl.InputSchema["properties"].(map[string]any)
			if _, hasHost := props["host"]; hasHost {
				t.Fatal("box_run MCP schema must not expose host")
			}
		}
	}

	beta := &fakeFleetHost{remote: "r2", project: mode.Project, mode: "full", queueCapable: true, detachFollow: true}
	ff := newFakeFleet(t, beta)
	ff.dashHosts["https://beta.example.com"] = beta
	hosts := []fleetHost{{Name: "beta", Remote: "r2", Project: mode.Project, DashboardURL: "https://beta.example.com"}}
	a, _, repoDir := fleetDispatchTestApp(t, ff, hosts)
	a.Getenv = func(string) string { return "" }
	a.Thread = "thread-fleet"
	a.Push = &fakePusher{}
	a.InlineWait = 20 * time.Millisecond
	var spawned []string
	var spawnErr error
	a.Spawn = func(_, job string) error {
		if spawnErr != nil {
			return spawnErr
		}
		spawned = append(spawned, job)
		return nil
	}

	callRun := func(args map[string]any) map[string]any {
		params, _ := json.Marshal(map[string]any{"name": "box_run", "arguments": args})
		return callTool(context.Background(), a, params, func(jobNotice) {})
	}

	// Detached running fleet job starts watcher via startWatchOnHost with saved Host locator.
	res := callRun(map[string]any{"dir": repoDir, "command": "sleep 60", "reason": "Fleet MCP detached watcher check."})
	if res["isError"] == true {
		t.Fatalf("detached box_run error: %+v", res)
	}
	raw, _ := json.Marshal(res["structuredContent"])
	var sc struct {
		Job   string `json:"job"`
		Host  string `json:"host"`
		State string `json:"state"`
		Push  *bool  `json:"push"`
	}
	json.Unmarshal(raw, &sc)
	if sc.State != stateRunning || sc.Host != "beta" || sc.Push == nil || !*sc.Push || !strings.Contains(sc.Job, jobHostPrefix) {
		t.Fatalf("detached structuredContent = %+v (raw=%s)", sc, raw)
	}
	if !slices.Equal(spawned, []string{sc.Job}) {
		t.Fatalf("spawned = %v, want [%s]", spawned, sc.Job)
	}
	rec, err := a.readWatchRecord(sc.Job)
	if err != nil || rec.Host == nil || rec.Host.Name != "beta" || rec.Host.Remote != "r2" || rec.Host.DashboardURL != "https://beta.example.com" || rec.Queued {
		t.Fatalf("detached watch record = %+v, %v", rec, err)
	}

	if msg := endedMessage(rec, jobEnd{Code: 0}); !strings.Contains(msg, sc.Job+" on beta ended") {
		t.Fatalf("completion notice = %q", msg)
	}

	// Queued fleet job: when watcher spawn fails, box_run cancels via beta's boxd using the BARE job ID.
	beta.keys = map[string]string{"limits.instances": "1"}
	beta.insts = []instance{{Name: "busy", Status: "Running", Config: map[string]string{mode.SizeKey: "medium"}}}
	spawnErr = fmt.Errorf("spawn failed")
	res = callRun(map[string]any{"dir": repoDir, "command": "echo queued", "reason": "Fleet MCP queued cancel fallback check."})
	raw, _ = json.Marshal(res["structuredContent"])
	sc = struct {
		Job   string `json:"job"`
		Host  string `json:"host"`
		State string `json:"state"`
		Push  *bool  `json:"push"`
	}{}
	json.Unmarshal(raw, &sc)
	if sc.State != stateRefused || len(beta.cancelled) != 1 || strings.Contains(beta.cancelled[0], jobHostPrefix) {
		t.Fatalf("queued cancel fallback = %+v, cancelled=%v (raw=%s)", sc, beta.cancelled, raw)
	}
}
