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
	"github.com/a-Fig/agent-box/internal/jobrun"
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
	queueError     string
	preStartError  string
	lateBox        *instance
	priority       *int
	cloudPaused    bool
	onBundle       func() // during bundle upload, with mu held

	mu          sync.Mutex
	pushedFiles []string
	pushedMeta  []jobMeta
	startJobs   []string
	followJobs  []string
	endReports  []string
	enqueued    []dash.QueueRequest
	cancelled   []string
	boxdHits    int
	listCalls   int
	queuedJobs  []dash.Job
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
		h.listCalls++
		if h.listCalls == 2 && h.lateBox != nil {
			h.insts = append(h.insts, *h.lateBox)
		}
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
			if h.preStartError == "expiry" && strings.HasPrefix(args[3], expiresKey+"=") {
				return Result{Code: 1, Stderr: "connection reset"}, nil
			}
			for i := range h.insts {
				if h.insts[i].Name == name {
					if h.insts[i].Config == nil {
						h.insts[i].Config = map[string]string{}
					}
					for _, setting := range args[3:] {
						k, v, _ := strings.Cut(setting, "=")
						h.insts[i].Config[k] = v
					}
				}
			}
			return Result{}, nil
		}
	case "file":
		if strings.HasSuffix(args[len(args)-1], ".bundle") {
			if h.preStartError == "bundle" {
				return Result{Code: 1, Stderr: "connection reset"}, nil
			}
			if h.onBundle != nil {
				h.onBundle()
			}
		}
		if strings.HasSuffix(args[len(args)-1], ".meta") {
			raw, err := os.ReadFile(args[len(args)-2])
			if err != nil {
				return Result{}, err
			}
			var meta jobMeta
			if err := json.Unmarshal(raw, &meta); err != nil {
				return Result{}, err
			}
			h.pushedMeta = append(h.pushedMeta, meta)
		}
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
		body, _ := json.Marshal(fleetBoxdSnapshot{Queue: h.queueCapable, Priority: h.priority, Jobs: append([]dash.Job{}, h.queuedJobs...), CloudPaused: h.cloudPaused})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}, nil
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
		if h.queueHTTPCode != 0 && h.queueHTTPCode != http.StatusCreated {
			message := h.queueError
			if message == "" {
				message = "queue full"
			}
			body, _ := json.Marshal(dash.ErrorBody{Error: message})
			return &http.Response{StatusCode: h.queueHTTPCode, Body: io.NopCloser(bytes.NewReader(body))}, nil
		}
		job := qr.Box + "." + qr.Ref
		if qr.New {
			job = qr.Box + "-a1b2." + qr.Ref
		}
		if h.loseQueueReply {
			box, id, _ := strings.Cut(job, ".")
			at := jobStart(job)
			h.queuedJobs = append(h.queuedJobs, dash.Job{ID: id, Box: box, State: dash.JobQueued, StartedAt: at, QueuedAt: &at})
			return nil, io.ErrUnexpectedEOF
		}
		rep, _ := json.Marshal(dash.QueueReply{Job: job, Position: 1, Why: dash.RefusedFull})
		return &http.Response{StatusCode: http.StatusCreated, Body: io.NopCloser(bytes.NewReader(rep))}, nil
	case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/api/queue/"):
		bare := strings.TrimPrefix(req.URL.Path, "/api/queue/")
		for _, job := range h.queuedJobs {
			if job.Box+"."+job.ID == bare {
				body, _ := json.Marshal(job)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}, nil
			}
		}
		box, _, _ := strings.Cut(bare, ".")
		j, _ := json.Marshal(dash.Job{ID: bare, Box: box, State: dash.JobRunning, StartedAt: time.Date(2026, 11, 26, 12, 0, 1, 0, time.UTC)})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(j))}, nil
	case req.Method == http.MethodPost && strings.HasPrefix(req.URL.Path, "/api/queue/") && strings.HasSuffix(req.URL.Path, "/cancel"):
		bare := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/api/queue/"), "/cancel")
		h.cancelled = append(h.cancelled, bare)
		h.queuedJobs = slices.DeleteFunc(h.queuedJobs, func(j dash.Job) bool { return j.Box+"."+j.ID == bare })
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
	cfg := beta.insts[0].Config
	if owner, _ := jobrun.SetupOwner(cfg); owner != id.Bare || cfg[expiresKey] != a.Now().Add(idleTTL).UTC().Format(time.RFC3339) {
		t.Fatalf("custom-project box after completion: %v", cfg)
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
	// An earlier filtered listing may have created a second, bare result.
	a.recordEnd(id.Bare, jobEnd{Code: 1})
	out.Reset()
	if code := run(context.Background(), a, []string{"jobs", "--box", "widget"}, nil); code != 0 || !slices.Contains(strings.Fields(out.String()), r.Job) {
		t.Fatalf("box jobs --box code=%d out=%q", code, out.String())
	}
}

func TestFleetRunCustomProjectKeepsFiniteExpiryWhenDetached(t *testing.T) {
	for _, tc := range []struct {
		name      string
		throwaway bool
		reuse     bool
	}{
		{name: "repo box"},
		{name: "throwaway", throwaway: true},
		{name: "reused repo box protected during upload", reuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := fleetHost{Name: "beta", Remote: "r2", Project: "work"}
			beta := &fakeFleetHost{remote: host.Remote, project: host.Project, mode: "full", detachFollow: true}
			ff := newFakeFleet(t, beta)
			a, out, dir := fleetDispatchTestApp(t, ff, []fleetHost{host})
			now := a.Now()
			deadline := now.Add(jobTTL).UTC().Format(time.RFC3339)
			idleDeadline := now.Add(idleTTL).UTC().Format(time.RFC3339)
			if tc.reuse {
				beta.insts = []instance{{Name: "widget", Status: "Running", Config: map[string]string{
					mode.SizeKey: "medium", expiresKey: idleDeadline,
				}}}
			}
			uploaded := false
			beta.onBundle = func() {
				uploaded = true
				in := beta.insts[0]
				if owner, until := jobrun.SetupOwner(in.Config); owner == "" || !until.Equal(now.Add(jobrun.SetupTTL)) || in.Config[expiresKey] != deadline {
					t.Fatalf("missing finite setup reservation during upload: %v", in.Config)
				}
				if tc.reuse {
					in.Config[expiresKey] = idleDeadline // an older client collected the previous result
					if idleRepoConfig(in, now) {
						t.Fatal("pressure eviction admitted a repo box during upload")
					}
				}
			}
			r, err := a.Run(context.Background(), RunOpts{Dir: dir, Cmd: []string{"true"},
				New: tc.throwaway, Wait: 20 * time.Millisecond, Reason: "Check finite expiry for a detached job in a custom project."})
			if err != nil || !uploaded || r.State != stateDetached || r.Host != host.Name || len(beta.insts) != 1 {
				t.Fatalf("run = %+v, boxes = %v, error = %v\n%s", r, beta.insts, err, out)
			}
			cfg := beta.insts[0].Config
			if cfg[expiresKey] != deadline {
				t.Fatalf("detached custom-project box should have 24h expiry: %v", cfg)
			}
			if !r.Source.Valid() || len(beta.pushedMeta) != 1 {
				t.Fatalf("detached source = %+v, guest metadata = %+v", r.Source, beta.pushedMeta)
			}
			requireSourceRevision(t, beta.pushedMeta[0].Source, gitT(t, dir, "rev-parse", "HEAD"), false, r.Source.SnapshotCommit)
		})
	}
}

func TestFleetRunRecoversQualifiedResultWhenCoolingInterruptsFollow(t *testing.T) {
	host := fleetHost{Name: "beta", Remote: "r2", Project: mode.Project, DashboardURL: "https://beta.example.com"}
	beta := &fakeFleetHost{remote: host.Remote, project: host.Project, mode: "full"}
	ff := newFakeFleet(t, beta)
	ff.dashHosts[host.DashboardURL] = beta
	a, out, dir := fleetDispatchTestApp(t, ff, []fleetHost{host})
	var job string
	a.Notify = func(n jobNotice) {
		if n.Event == "starting_job" {
			job = n.Job
		}
	}
	a.R = lookupRunner(func(ctx context.Context, cmd Cmd) (Result, error) {
		if isScript(cmd, "# follow") {
			id, err := parseJobID(job)
			if err != nil || id.hostTag == "" {
				t.Fatalf("following without a qualified job ID: %q, %v", job, err)
			}
			a.recordEnd(id.Bare, jobEnd{Code: 3})
			a.recordEnd(id.ID, jobEnd{Host: host.Name, Code: 7})
			return Result{}, nil // The guest cooled before the exit marker reached the client.
		}
		return ff.Run(ctx, cmd)
	})
	r, err := a.Run(context.Background(), RunOpts{
		Dir: dir, New: true, Cmd: []string{"exit 7"}, Reason: "Recover a fleet job result after cooling interrupts follow.",
	})
	if err != nil || r.Job != job || r.Host != host.Name || r.State != stateDone || r.Code != 7 || !r.Source.Valid() ||
		!strings.Contains(out.String(), "using the recorded result") {
		t.Fatalf("run = %+v, %v; out=%q", r, err, out)
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
	if r.Capacity == nil || !slices.Equal(r.Capacity.Limits, []string{"instances"}) || r.Capacity.SmallerSize != "" {
		t.Fatalf("queue capacity detail = %+v, want instances without a smaller size", r.Capacity)
	}
	id, err := parseJobID(r.Job)
	if err != nil || id.hostTag == "" {
		t.Fatalf("queued job ID should be qualified, got %q (%v)", r.Job, err)
	}
	if len(overflow.enqueued) != 1 || workHost.boxdHits != 0 || len(noQueueHost.enqueued) != 0 {
		t.Fatalf("enqueued overflow=%d workHits=%d noq=%d", len(overflow.enqueued), workHost.boxdHits, len(noQueueHost.enqueued))
	}
	var queuedMeta jobMeta
	if err := json.Unmarshal(overflow.enqueued[0].Meta, &queuedMeta); err != nil || !r.Source.Valid() {
		t.Fatalf("queue metadata = %+v, result source = %+v, error = %v", queuedMeta, r.Source, err)
	}
	requireSourceRevision(t, queuedMeta.Source, gitT(t, repoDir, "rev-parse", "HEAD"), false, r.Source.SnapshotCommit)
	if !strings.Contains(out.String(), "box: queued job "+r.Job+" on overflow,") {
		t.Fatalf("stderr = %q", out.String())
	}

	// CLI record + followQueuedResult must target overflow with bare IDs and return qualified result.
	info := jobMeta{Reason: "Test fleet queue follow.", Command: "echo queued-done", Source: r.Source}
	a.recordQueuedOnHost(info, r.Job, r.fleetHost, false, r.throwaway)
	rec, err := a.readWatchRecord(r.Job)
	if err != nil || rec.Host == nil || rec.Host.Remote != "r3" || rec.Host.DashboardURL != "https://overflow.example.com" {
		t.Fatalf("recorded queued host = %+v, %v", rec, err)
	}
	followed, err := a.followQueuedResult(context.Background(), r)
	if err != nil || followed.State != stateDone || followed.Code != 0 || followed.Job != r.Job || followed.Host != "overflow" {
		t.Fatalf("followQueuedResult = %+v, %v", followed, err)
	}
	requireSourceRevision(t, followed.Source, r.Source.Commit, false, r.Source.SnapshotCommit)
	end, ok := a.recordedEnd(r.Job)
	if !ok || end.Host != r.Host {
		t.Fatalf("retained queue result = %+v, found = %t", end, ok)
	}
	requireSourceRevision(t, end.Source, r.Source.Commit, false, r.Source.SnapshotCommit)
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
	// Queue state can still answer while the Incus hosts are unavailable.
	first.unreachable, second.unreachable = true, true
	jobs, err := a.listJobs(context.Background(), "")
	if err != nil || len(jobs) != 1 || jobs[0].Host != "first" || jobs[0].State != stateQueued {
		t.Fatalf("jobs after lost queue reply = %+v, %v", jobs, err)
	}
	pending := first.queuedJobs[0]
	id := requireQualifiedJobID(t, hosts[0], pending.Box+"."+pending.ID)
	if jobs[0].Job != id.ID {
		t.Fatalf("recovered ID = %s, want %s", jobs[0].Job, id.ID)
	}
	// A started or dropped queue job can appear in boxd before Incus has
	// any guest log. Recent non-queue jobs should not enter via this path.
	first.queuedJobs = append(first.queuedJobs, dash.Job{ID: pending.ID, Box: "unrelated", State: dash.JobRunning, StartedAt: pending.StartedAt})
	for _, state := range []string{dash.JobRunning, dash.JobLost, dash.JobDone} {
		first.queuedJobs[0].State = state
		jobs, err := a.listJobs(context.Background(), "")
		if err != nil || len(jobs) != 1 || jobs[0].Job != id.ID || jobs[0].State != state {
			t.Fatalf("recover %s job = %+v, %v", state, jobs, err)
		}
	}
	first.queuedJobs[0].State = dash.JobQueued
	var listing bytes.Buffer
	a.Out = &listing
	if code := run(context.Background(), a, []string{"jobs"}, nil); code != 0 || !strings.Contains(listing.String(), id.ID) || !strings.Contains(listing.String(), "queued") {
		t.Fatalf("box jobs code=%d listing=%q", code, listing.String())
	}
	if code := run(context.Background(), a, []string{"cancel", id.ID}, nil); code != 0 || !slices.Equal(first.cancelled, []string{id.Bare}) || len(second.cancelled) != 0 {
		t.Fatalf("cancel recovered job code=%d first=%v second=%v", code, first.cancelled, second.cancelled)
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

	beta.unreachable = false
	beta.keys = map[string]string{gpuKey: "allow"}
	beta.insts = []instance{{Name: "namedbox", Status: "Running", Config: map[string]string{mode.SizeKey: "medium"}}}
	out.Reset()
	code = run(context.Background(), a, []string{"run", "--host", "beta", "--box", "namedbox", "--gpu", "--reason", "A CPU box is invalid for a GPU request.", "--", "true"}, nil)
	if code != 2 || !strings.Contains(out.String(), "isn't a GPU box") || len(beta.startJobs) != 1 {
		t.Fatalf("pinned CPU box with --gpu: code=%d out=%q", code, out.String())
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

	if !rec.Source.Valid() || !strings.Contains(string(raw), `"snapshotCommit":"`+rec.Source.SnapshotCommit+`"`) {
		t.Fatalf("detached watch source = %+v, structuredContent = %s", rec.Source, raw)
	}
	if msg := endMessage(rec, jobEnd{Code: 0, Source: rec.Source}); !strings.Contains(msg, sc.Job+" on beta ended") || !strings.Contains(msg, sourceLine(rec.Source)) {
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

func TestFleetRunQueuesTheLiveSizeFallback(t *testing.T) {
	host := fleetHost{Name: "first", Remote: "r1", Project: mode.Project, DashboardURL: "https://first.example.com"}
	first := &fakeFleetHost{
		remote: host.Remote, project: host.Project, mode: "full", queueCapable: true,
		lateBox:      &instance{Name: "widget", Status: "Running", Config: map[string]string{mode.SizeKey: "small"}},
		refuseLaunch: memoryRefusal,
	}
	ff := newFakeFleet(t, first)
	ff.dashHosts[host.DashboardURL] = first
	a, out, dir := fleetDispatchTestApp(t, ff, []fleetHost{host})
	r, err := a.Run(context.Background(), RunOpts{
		Dir: dir, Size: "medium", Queue: true, Cmd: []string{"true"}, Reason: "Queue the requested size after a smaller repo box appears.",
	})
	if err != nil || r.State != stateQueued || len(first.enqueued) != 1 || !strings.Contains(out.String(), "widget is small") {
		t.Fatalf("run = %+v, %v, enqueued=%+v, out=%q", r, err, first.enqueued, out)
	}
	q := first.enqueued[0]
	if !q.New || q.Box != "widget" || q.Size != "medium" {
		t.Fatalf("queued stale target: %+v", q)
	}
}

func TestFleetRunRetriesPreStartSetupFailures(t *testing.T) {
	for _, failure := range []string{"bundle", "expiry", "repo launch"} {
		t.Run(failure, func(t *testing.T) {
			first := &fakeFleetHost{remote: "r1", project: "work", mode: "full", preStartError: failure}
			second := &fakeFleetHost{remote: "r2", project: "work", mode: "full"}
			if failure == "repo launch" {
				first.refuseLaunch = "Failed creating instance from image: Instance metadata contained disallowed entries: [/var/lib/incus/...]"
			}
			ff := newFakeFleet(t, first, second)
			hosts := []fleetHost{
				{Name: "first", Remote: first.remote, Project: first.project},
				{Name: "second", Remote: second.remote, Project: second.project},
			}
			a, out, dir := fleetDispatchTestApp(t, ff, hosts)
			var starts []jobNotice
			a.Notify = func(n jobNotice) {
				if n.Event == "starting_job" {
					starts = append(starts, n)
				}
			}
			r, err := a.Run(context.Background(), RunOpts{
				Dir: dir, New: failure != "repo launch", Cmd: []string{"true"}, Reason: "Retry on a healthy host when the first host fails during setup.",
			})
			if err != nil || r.Code != 0 || r.Host != "second" || len(first.startJobs) != 0 || len(second.startJobs) != 1 ||
				len(starts) != 1 || starts[0].Host != "second" || ff.snapshots != 1 {
				t.Fatalf("run = %+v, %v; starts=%+v, snapshots=%d; out=%q", r, err, starts, ff.snapshots, out)
			}
		})
	}
}

func TestFleetRunDistinguishesQueueStorageFailureFromInvalidRequest(t *testing.T) {
	keys := map[string]string{"limits.instances": "1"}
	insts := []instance{{Name: "busy", Status: "Running", Config: map[string]string{mode.SizeKey: "medium"}}}
	first := &fakeFleetHost{
		remote: "r1", project: mode.Project, mode: "full", keys: keys, insts: insts, queueCapable: true,
		queueHTTPCode: http.StatusBadRequest, queueError: "storing the snapshot: no space left on device",
	}
	second := &fakeFleetHost{remote: "r2", project: mode.Project, mode: "full", keys: keys, insts: insts, queueCapable: true}
	ff := newFakeFleet(t, first, second)
	hosts := []fleetHost{
		{Name: "first", Remote: first.remote, Project: first.project, DashboardURL: "https://first.example.com"},
		{Name: "second", Remote: second.remote, Project: second.project, DashboardURL: "https://second.example.com"},
	}
	ff.dashHosts[hosts[0].DashboardURL], ff.dashHosts[hosts[1].DashboardURL] = first, second
	a, out, dir := fleetDispatchTestApp(t, ff, hosts)
	opts := RunOpts{Dir: dir, Queue: true, Cmd: []string{"true"}, Reason: "Try another queue after a confirmed host storage failure."}
	r, err := a.Run(context.Background(), opts)
	if err != nil || r.State != stateQueued || r.Host != "second" || len(first.enqueued) != 1 || len(second.enqueued) != 1 {
		t.Fatalf("storage rejection = %+v, %v; %s", r, err, out)
	}

	opts.Host = "first"
	out.Reset()
	r, err = a.Run(context.Background(), opts)
	if err != nil || r.Code != exitPaused || !strings.Contains(out.String(), "storing the snapshot") {
		t.Fatalf("single queue storage rejection = %+v, %v; %s", r, err, out)
	}

	opts.Host = ""
	first.queueError = "invalid bundle request"
	r, err = a.Run(context.Background(), opts)
	if err == nil || r.Code != 1 || !strings.Contains(err.Error(), "invalid bundle request") || len(second.enqueued) != 1 {
		t.Fatalf("invalid request = %+v, %v; second queue requests=%d", r, err, len(second.enqueued))
	}
}

func TestFleetRunTieredFallbackOnTurnAway(t *testing.T) {
	t.Run("turn-away from higher-priority tier probes and starts on deferred lower tier", func(t *testing.T) {
		high := &fakeFleetHost{
			remote:       "r1",
			project:      mode.Project,
			mode:         "full",
			queueCapable: true,
			refuseLaunch: memoryRefusal,
		}
		low := &fakeFleetHost{
			remote:     "r2",
			project:    "work",
			mode:       "full",
			execStdout: "ran-on-low\n",
		}
		ff := newFakeFleet(t, high, low)
		ff.dashHosts["https://high.example.com"] = high
		hosts := []fleetHost{
			{Name: "low", Remote: "r2", Project: "work", Priority: -1},
			{Name: "high", Remote: "r1", Project: mode.Project, DashboardURL: "https://high.example.com", Priority: 10},
		}
		a, out, repoDir := fleetDispatchTestApp(t, ff, hosts)

		r, err := a.Run(context.Background(), RunOpts{
			Dir: repoDir, Cmd: []string{"echo ok"}, New: true, Queue: true,
			Reason: "Fall back to deferred lower-priority tier after higher tier turns away.",
		})
		if err != nil || r.State != stateDone || r.Code != 0 || r.Host != "low" {
			t.Fatalf("tiered fallback = %+v, %v\n%s", r, err, out.String())
		}
		if high.listCalls == 0 || low.listCalls == 0 {
			t.Fatalf("expected both tiers probed after turn-away, got high=%d low=%d", high.listCalls, low.listCalls)
		}
	})

	t.Run("turn-away from higher tier probes lower tier then queues on higher tier if lower is also full", func(t *testing.T) {
		high := &fakeFleetHost{
			remote:       "r1",
			project:      mode.Project,
			mode:         "full",
			queueCapable: true,
			refuseLaunch: memoryRefusal,
		}
		low := &fakeFleetHost{
			remote:  "r2",
			project: "work",
			mode:    "full",
			keys:    map[string]string{"limits.instances": "1"},
			insts:   []instance{{Name: "busy", Status: "Running", Config: map[string]string{mode.SizeKey: "medium"}}},
		}
		ff := newFakeFleet(t, high, low)
		ff.dashHosts["https://high.example.com"] = high
		hosts := []fleetHost{
			{Name: "low", Remote: "r2", Project: "work", Priority: -1},
			{Name: "high", Remote: "r1", Project: mode.Project, DashboardURL: "https://high.example.com", Priority: 10},
		}
		a, out, repoDir := fleetDispatchTestApp(t, ff, hosts)

		r, err := a.Run(context.Background(), RunOpts{
			Dir: repoDir, Cmd: []string{"echo queued"}, New: true, Queue: true,
			Reason: "Queue on higher-priority tier when lower tier is also full.",
		})
		if err != nil || r.State != stateQueued || r.Host != "high" || len(high.enqueued) != 1 || len(low.enqueued) != 0 {
			t.Fatalf("tiered queue fallback = %+v, %v (high=%d low=%d)\n%s", r, err, len(high.enqueued), len(low.enqueued), out.String())
		}
		if low.listCalls == 0 {
			t.Fatalf("expected lower tier to be probed before queueing on higher tier")
		}
	})
}

func TestFleetRunBoxdAuthoritativePriority(t *testing.T) {
	t.Run("two clients with empty hosts.json priority rank hosts by boxd-reported priority", func(t *testing.T) {
		alpha := &fakeFleetHost{remote: "r1", project: mode.Project, mode: "full", queueCapable: true, priority: intPtr(5), execStdout: "alpha\n"}
		beta := &fakeFleetHost{remote: "r2", project: mode.Project, mode: "full", queueCapable: true, priority: intPtr(20), execStdout: "beta\n"}
		ff := newFakeFleet(t, alpha, beta)
		ff.dashHosts["https://alpha.example.com"] = alpha
		ff.dashHosts["https://beta.example.com"] = beta

		// Client 1 lists alpha then beta (both omitting priority).
		a1, out1, dir1 := fleetDispatchTestApp(t, ff, []fleetHost{
			{Name: "alpha", Remote: "r1", Project: mode.Project, DashboardURL: "https://alpha.example.com"},
			{Name: "beta", Remote: "r2", Project: mode.Project, DashboardURL: "https://beta.example.com"},
		})
		r1, err := a1.Run(context.Background(), RunOpts{Dir: dir1, Cmd: []string{"echo ok"}, New: true, Reason: "Client 1 agrees on boxd-reported host priority."})
		if err != nil || r1.Host != "beta" {
			t.Fatalf("client 1 run = %+v, %v\n%s", r1, err, out1.String())
		}

		// Client 2 lists beta then alpha (also omitting priority).
		a2, out2, dir2 := fleetDispatchTestApp(t, ff, []fleetHost{
			{Name: "beta", Remote: "r2", Project: mode.Project, DashboardURL: "https://beta.example.com"},
			{Name: "alpha", Remote: "r1", Project: mode.Project, DashboardURL: "https://alpha.example.com"},
		})
		r2, err := a2.Run(context.Background(), RunOpts{Dir: dir2, Cmd: []string{"echo ok"}, New: true, Reason: "Client 2 agrees on boxd-reported host priority."})
		if err != nil || r2.Host != "beta" {
			t.Fatalf("client 2 run = %+v, %v\n%s", r2, err, out2.String())
		}
	})

	t.Run("boxd-reported priority overrides differing hosts.json hints across tiers", func(t *testing.T) {
		alpha := &fakeFleetHost{remote: "r1", project: mode.Project, mode: "full", queueCapable: true, priority: intPtr(0), execStdout: "alpha\n"}
		beta := &fakeFleetHost{remote: "r2", project: mode.Project, mode: "full", queueCapable: true, priority: intPtr(25), execStdout: "beta\n"}
		ff := newFakeFleet(t, alpha, beta)
		ff.dashHosts["https://alpha.example.com"] = alpha
		ff.dashHosts["https://beta.example.com"] = beta

		// Even though alpha has a higher hosts.json hint (10) than beta (0), both
		// boxd-capable hosts are probed in the initial tier and beta (boxd 25) wins.
		hosts := []fleetHost{
			{Name: "alpha", Remote: "r1", Project: mode.Project, DashboardURL: "https://alpha.example.com", Priority: 10},
			{Name: "beta", Remote: "r2", Project: mode.Project, DashboardURL: "https://beta.example.com", Priority: 0},
		}
		a, out, dir := fleetDispatchTestApp(t, ff, hosts)
		r, err := a.Run(context.Background(), RunOpts{Dir: dir, Cmd: []string{"echo ok"}, New: true, Reason: "Verify boxd priority overrides differing hosts.json hints."})
		if err != nil || r.Host != "beta" || alpha.listCalls == 0 || beta.listCalls == 0 || len(alpha.startJobs) != 0 || len(beta.startJobs) != 1 {
			t.Fatalf("override run = %+v, %v (alpha list=%d start=%d; beta list=%d start=%d)\n%s",
				r, err, alpha.listCalls, len(alpha.startJobs), beta.listCalls, len(beta.startJobs), out.String())
		}
	})

	t.Run("negative boxd priority probes deferred higher-priority non-boxd tier and launches there", func(t *testing.T) {
		boxdLow := &fakeFleetHost{remote: "r1", project: mode.Project, mode: "full", queueCapable: true, priority: intPtr(-10), execStdout: "boxd-low\n"}
		workHigher := &fakeFleetHost{remote: "r2", project: "work", mode: "full", execStdout: "work-higher\n"}
		ff := newFakeFleet(t, boxdLow, workHigher)
		ff.dashHosts["https://boxd.example.com"] = boxdLow

		hosts := []fleetHost{
			{Name: "boxd-low", Remote: "r1", Project: mode.Project, DashboardURL: "https://boxd.example.com", Priority: 0},
			{Name: "work-higher", Remote: "r2", Project: "work", Priority: -1},
		}
		a, out, dir := fleetDispatchTestApp(t, ff, hosts)
		r, err := a.Run(context.Background(), RunOpts{Dir: dir, Cmd: []string{"echo ok"}, New: true, Reason: "Deferred -1 custom host must beat startable -10 boxd host."})
		if err != nil || r.State != stateDone || r.Code != 0 || r.Host != "work-higher" || workHigher.listCalls == 0 || len(boxdLow.startJobs) != 0 || len(workHigher.startJobs) != 1 {
			t.Fatalf("negative boxd fallback = %+v, %v (work list=%d start=%d; boxd start=%d)\n%s",
				r, err, workHigher.listCalls, len(workHigher.startJobs), len(boxdLow.startJobs), out.String())
		}
	})

	t.Run("turn-away fallback to negative boxd host also probes higher-priority deferred non-boxd tier", func(t *testing.T) {
		boxdTop := &fakeFleetHost{remote: "r1", project: mode.Project, mode: "full", queueCapable: true, priority: intPtr(10), refuseLaunch: memoryRefusal}
		boxdLow := &fakeFleetHost{remote: "r2", project: mode.Project, mode: "full", queueCapable: true, priority: intPtr(-10), execStdout: "boxd-low\n"}
		workHigher := &fakeFleetHost{remote: "r3", project: "work", mode: "full", execStdout: "work-higher\n"}
		ff := newFakeFleet(t, boxdTop, boxdLow, workHigher)
		ff.dashHosts["https://top.example.com"] = boxdTop
		ff.dashHosts["https://low.example.com"] = boxdLow

		hosts := []fleetHost{
			{Name: "boxd-top", Remote: "r1", Project: mode.Project, DashboardURL: "https://top.example.com"},
			{Name: "boxd-low", Remote: "r2", Project: mode.Project, DashboardURL: "https://low.example.com"},
			{Name: "work-higher", Remote: "r3", Project: "work", Priority: -1},
		}
		a, out, dir := fleetDispatchTestApp(t, ff, hosts)
		r, err := a.Run(context.Background(), RunOpts{Dir: dir, Cmd: []string{"echo ok"}, New: true, Reason: "After boxd-top turns away, -1 deferred host must beat -10 boxd host."})
		if err != nil || r.State != stateDone || r.Code != 0 || r.Host != "work-higher" || workHigher.listCalls == 0 || len(boxdLow.startJobs) != 0 || len(workHigher.startJobs) != 1 {
			t.Fatalf("turn-away deferred fallback = %+v, %v (work list=%d start=%d; low start=%d)\n%s",
				r, err, workHigher.listCalls, len(workHigher.startJobs), len(boxdLow.startJobs), out.String())
		}
	})

	t.Run("turn-away fallback stops before lower deferred tier when existing -10 boxd candidate beats -20 tier", func(t *testing.T) {
		boxdTop := &fakeFleetHost{remote: "r1", project: mode.Project, mode: "full", queueCapable: true, priority: intPtr(10), refuseLaunch: memoryRefusal}
		boxdLow := &fakeFleetHost{remote: "r2", project: mode.Project, mode: "full", queueCapable: true, priority: intPtr(-10), execStdout: "boxd-low\n"}
		workMid := &fakeFleetHost{
			remote: "r3", project: "work", mode: "full",
			keys:  map[string]string{"limits.instances": "1"},
			insts: []instance{{Name: "busy", Status: "Running", Config: map[string]string{mode.SizeKey: "medium"}}},
		}
		workSunk := &fakeFleetHost{remote: "r4", project: "work", mode: "full", execStdout: "work-sunk\n"}
		ff := newFakeFleet(t, boxdTop, boxdLow, workMid, workSunk)
		ff.dashHosts["https://top.example.com"] = boxdTop
		ff.dashHosts["https://low.example.com"] = boxdLow

		hosts := []fleetHost{
			{Name: "boxd-top", Remote: "r1", Project: mode.Project, DashboardURL: "https://top.example.com"},
			{Name: "boxd-low", Remote: "r2", Project: mode.Project, DashboardURL: "https://low.example.com"},
			{Name: "work-mid", Remote: "r3", Project: "work", Priority: -1},
			{Name: "work-sunk", Remote: "r4", Project: "work", Priority: -20},
		}
		a, out, dir := fleetDispatchTestApp(t, ff, hosts)
		r, err := a.Run(context.Background(), RunOpts{Dir: dir, Cmd: []string{"echo ok"}, New: true, Reason: "Full -1 tier must fall back to existing -10 boxd host without probing -20 tier."})
		if err != nil || r.State != stateDone || r.Code != 0 || r.Host != "boxd-low" || workMid.listCalls == 0 || workSunk.listCalls != 0 || len(boxdLow.startJobs) != 1 {
			t.Fatalf("fallback preserving existing -10 = %+v, %v (mid list=%d sunk list=%d low start=%d)\n%s",
				r, err, workMid.listCalls, workSunk.listCalls, len(boxdLow.startJobs), out.String())
		}
	})
}

func TestFleetRunThreeTierProbeAndLaunchOrder(t *testing.T) {
	recordingApp := func(t *testing.T, t1, t2, t3 *fakeFleetHost) (*App, *[]string, *[]string, *int, *bytes.Buffer, string) {
		t.Helper()
		ff := newFakeFleet(t, t1, t2, t3)
		hosts := []fleetHost{
			{Name: "tier3", Remote: "r3", Project: "work", Priority: 0},
			{Name: "tier1", Remote: "r1", Project: "work", Priority: 20},
			{Name: "tier2", Remote: "r2", Project: "work", Priority: 10},
		}
		a, out, dir := fleetDispatchTestApp(t, ff, hosts)
		var mu sync.Mutex
		var probeOrder, launchOrder []string
		seenProbe := map[string]bool{}
		var t3ListCallsAtT2Launch int
		a.R = lookupRunner(func(ctx context.Context, cmd Cmd) (Result, error) {
			if cmd.Name == "incus" && len(cmd.Args) >= 4 {
				switch cmd.Args[2] {
				case "list":
					remote, _, _ := strings.Cut(cmd.Args[3], ":")
					mu.Lock()
					if !seenProbe[remote] {
						seenProbe[remote] = true
						probeOrder = append(probeOrder, remote)
					}
					mu.Unlock()
				case "launch":
					remote, _, _ := strings.Cut(cmd.Args[4], ":")
					mu.Lock()
					launchOrder = append(launchOrder, remote)
					if remote == "r2" {
						t3.mu.Lock()
						t3ListCallsAtT2Launch = t3.listCalls
						t3.mu.Unlock()
					}
					mu.Unlock()
				}
			}
			return ff.Run(ctx, cmd)
		})
		return a, &probeOrder, &launchOrder, &t3ListCallsAtT2Launch, out, dir
	}

	t.Run("stops before tier 3 when tier 2 can start", func(t *testing.T) {
		t1 := &fakeFleetHost{
			remote: "r1", project: "work", mode: "full",
			keys:  map[string]string{"limits.instances": "1"},
			insts: []instance{{Name: "busy", Status: "Running", Config: map[string]string{mode.SizeKey: "medium"}}},
		}
		t2 := &fakeFleetHost{remote: "r2", project: "work", mode: "full", execStdout: "tier2-ok\n"}
		t3 := &fakeFleetHost{remote: "r3", project: "work", mode: "full", execStdout: "tier3-ok\n"}

		a, probeOrder, launchOrder, _, out, dir := recordingApp(t, t1, t2, t3)
		r, err := a.Run(context.Background(), RunOpts{Dir: dir, Cmd: []string{"echo ok"}, New: true, Reason: "Tier 2 start must not probe tier 3."})
		if err != nil || r.State != stateDone || r.Code != 0 || r.Host != "tier2" {
			t.Fatalf("run = %+v, %v\n%s", r, err, out.String())
		}
		if !slices.Equal(*probeOrder, []string{"r1", "r2"}) || !slices.Equal(*launchOrder, []string{"r2"}) || t3.listCalls != 0 {
			t.Fatalf("probeOrder=%v launchOrder=%v t3.listCalls=%d, want [r1 r2], [r2], 0", *probeOrder, *launchOrder, t3.listCalls)
		}
	})

	t.Run("probes and launches tier 3 only after tier 1 and tier 2 both turn away", func(t *testing.T) {
		t1 := &fakeFleetHost{remote: "r1", project: "work", mode: "full", refuseLaunch: memoryRefusal}
		t2 := &fakeFleetHost{remote: "r2", project: "work", mode: "full", refuseLaunch: memoryRefusal}
		t3 := &fakeFleetHost{remote: "r3", project: "work", mode: "full", execStdout: "tier3-ok\n"}

		a, probeOrder, launchOrder, t3AtT2, out, dir := recordingApp(t, t1, t2, t3)
		r, err := a.Run(context.Background(), RunOpts{Dir: dir, Cmd: []string{"echo ok"}, New: true, Queue: true, Reason: "Three-tier fallback after tier 1 and tier 2 turn away."})
		if err != nil || r.State != stateDone || r.Code != 0 || r.Host != "tier3" {
			t.Fatalf("run = %+v, %v\n%s", r, err, out.String())
		}
		if *t3AtT2 != 0 {
			t.Fatalf("tier 3 was probed (%d times) before tier 2 turned away", *t3AtT2)
		}
		if !slices.Equal(*probeOrder, []string{"r1", "r2", "r3"}) || !slices.Equal(*launchOrder, []string{"r1", "r2", "r3"}) {
			t.Fatalf("probeOrder=%v launchOrder=%v, want [r1 r2 r3] and [r1 r2 r3]", *probeOrder, *launchOrder)
		}
	})
}
