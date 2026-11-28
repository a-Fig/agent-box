package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/controller"
	"github.com/a-Fig/agent-box/internal/mode"
)

// ciSourceRepo makes base/owner/name.git, a bare repo with one commit that has
// ciJSON as .agent-box/ci.json ("" leaves it out), and returns the commit.
func ciSourceRepo(t *testing.T, base, repo, ciJSON string) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(filepath.Join(src, ".agent-box"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, src, "init", "-q")
	write(t, filepath.Join(src, "main.go"), "package main\n")
	if ciJSON != "" {
		write(t, filepath.Join(src, ".agent-box", "ci.json"), ciJSON)
	}
	gitT(t, src, "add", "-A")
	gitT(t, src, "commit", "-q", "-m", "init")
	sha := gitT(t, src, "rev-parse", "HEAD")
	bare := filepath.Join(base, filepath.FromSlash(repo)+".git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, src, "clone", "-q", "--bare", src, bare)
	gitT(t, bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	return sha
}

type ciTest struct {
	ms       *controller.MemServer
	srv      *httptest.Server
	a        *App
	pc       *fakeFleetHost
	cloud    *fakeRunnerV1
	base     string
	cacheDir string
	// afterCloud, if set, sees each cloud runner request once it is answered.
	afterCloud func(*http.Request)
}

// newCITest sets up a controller, a PC (priority 10) and optionally a cloud
// host (priority -1) in hosts.json, and an App whose Run goes to them.
func newCITest(t *testing.T, withCloud bool) *ciTest {
	t.Helper()
	ct := &ciTest{ms: &controller.MemServer{HostToken: "host-secret", ClientToken: "client-secret"}}
	ct.srv = httptest.NewServer(ct.ms)
	t.Cleanup(ct.srv.Close)
	ct.pc = &fakeFleetHost{remote: "pc", project: mode.Project, mode: "full", queueCapable: true, execStdout: "ci-ok\n"}
	ff := newFakeFleet(t, ct.pc)
	ff.dashHosts["https://pc.example.com"] = ct.pc
	hosts := []fleetHost{{Name: "pc", Remote: "pc", Project: mode.Project, DashboardURL: "https://pc.example.com", Priority: 10}}
	if withCloud {
		ct.cloud = newFakeRunnerV1(t)
		tok := ct.cloud.writeTokenFile(t, t.TempDir(), "modal.json")
		hosts = append(hosts, fleetHost{Name: "cloud", Kind: "modal", Endpoint: ct.cloud.endpoint(), TokenFile: tok, Mode: "function", Priority: -1})
	}
	a, _, _, _ := cloudDispatchTestApp(t, ff, hosts, func(req *http.Request) (*http.Response, error) {
		switch u := req.URL.String(); {
		case strings.HasPrefix(u, ct.srv.URL):
			return http.DefaultTransport.RoundTrip(req)
		case ct.cloud != nil && strings.HasPrefix(u, ct.cloud.endpoint()):
			resp, err := ct.cloud.doHTTP(req)
			if ct.afterCloud != nil {
				ct.afterCloud(req)
			}
			return resp, err
		}
		return ff.httpDo(req)
	})
	ct.a = a
	ct.base = t.TempDir()
	ct.cacheDir = filepath.Join(t.TempDir(), "cache")
	return ct
}

func (ct *ciTest) worker(hostClass, name string) *ciWorker {
	return &ciWorker{
		a:         ct.a,
		client:    &controller.Client{URL: ct.srv.URL, Token: "host-secret"},
		worker:    name,
		hostClass: hostClass,
		poll:      ciMinPoll,
		cacheDir:  ct.cacheDir,
		repoBase:  filepath.ToSlash(ct.base),
	}
}

func (ct *ciTest) job(t *testing.T, id string) controller.CIJob {
	t.Helper()
	j, ok := ct.ms.CIJob(id)
	if !ok {
		t.Fatalf("no job %s", id)
	}
	return j
}

func summary(j controller.CIJob) string {
	if j.Result == nil || j.Result.Summary == nil {
		return ""
	}
	return *j.Result.Summary
}

func exitCode(j controller.CIJob) *int {
	if j.Result == nil {
		return nil
	}
	return j.Result.ExitCode
}

// noWorktreesLeft checks that every job checkout under the cache was removed.
func (ct *ciTest) noWorktreesLeft(t *testing.T) {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(ct.cacheDir, "work"))
	if len(entries) != 0 {
		t.Fatalf("job checkouts left behind: %v", entries)
	}
}

func TestCIWorkerRunsJobThroughBoxRun(t *testing.T) {
	ct := newCITest(t, false)
	sha := ciSourceRepo(t, ct.base, "a-Fig/lantern", `{"command": "go test ./...", "size": "small", "timeout": "10m"}`)
	id := ct.ms.EnqueueCIJob("a-Fig/lantern", sha, controller.JobClassAny, nil)

	tok := filepath.Join(t.TempDir(), "host.token")
	write(t, tok, "host-secret\n")
	code := run(context.Background(), ct.a, []string{"ci", "worker", "--url", ct.srv.URL, "--token-file", tok,
		"--worker", "pc-1", "--cache-dir", ct.cacheDir, "--repo-base-url", filepath.ToSlash(ct.base), "--once"}, nil)
	if code != 0 {
		t.Fatalf("box ci worker exited %d", code)
	}
	j := ct.job(t, id)
	if j.State != controller.CIDone || exitCode(j) == nil || *exitCode(j) != 0 || j.ClaimedBy != "pc-1" {
		t.Fatalf("job = %+v, summary %q", j, summary(j))
	}
	if s := summary(j); !strings.Contains(s, "`go test ./...` on pc") || !strings.Contains(s, "passed") || !strings.Contains(s, "ci-ok") {
		t.Fatalf("summary = %q", s)
	}
	if len(ct.pc.startJobs) != 1 || !strings.Contains(ct.pc.startJobs[0], "go test ./...") {
		t.Fatalf("PC ran %q", ct.pc.startJobs)
	}
	if len(ct.pc.pushedMeta) != 1 || !strings.Contains(ct.pc.pushedMeta[0].Reason, "GitHub CI for a-Fig/lantern") {
		t.Fatalf("meta = %+v", ct.pc.pushedMeta)
	}
	if _, err := os.Stat(filepath.Join(ct.cacheDir, "a-Fig", "lantern.git", "HEAD")); err != nil {
		t.Fatalf("no bare cache: %v", err)
	}
	ct.noWorktreesLeft(t)
}

func TestCIWorkerReportsFailingCommand(t *testing.T) {
	ct := newCITest(t, false)
	ct.pc.execCode = 3
	sha := ciSourceRepo(t, ct.base, "a-Fig/lantern", `{"command": ["make", "test"]}`)
	id := ct.ms.EnqueueCIJob("a-Fig/lantern", sha, controller.JobClassAny, nil)
	if code := ct.worker(controller.HostClassHome, "pc-1").loop(context.Background(), true); code != 0 {
		t.Fatalf("loop = %d", code)
	}
	j := ct.job(t, id)
	if j.State != controller.CIFailed || exitCode(j) == nil || *exitCode(j) != 3 || !strings.Contains(summary(j), "exited 3") {
		t.Fatalf("job = %+v, summary %q", j, summary(j))
	}
	ct.noWorktreesLeft(t)
}

func TestCIWorkerFailsWithoutValidConfig(t *testing.T) {
	for name, cfg := range map[string]string{
		"missing":       "",
		"bad json":      `{"command": "go test`,
		"empty command": `{"command": ""}`,
		"unknown field": `{"command": "x", "comand": "y"}`,
		"bad size":      `{"command": "x", "size": "huge"}`,
		"bad timeout":   `{"command": "x", "timeout": "forever"}`,
	} {
		t.Run(name, func(t *testing.T) {
			ct := newCITest(t, false)
			sha := ciSourceRepo(t, ct.base, "a-Fig/lantern", cfg)
			id := ct.ms.EnqueueCIJob("a-Fig/lantern", sha, controller.JobClassAny, nil)
			ct.worker(controller.HostClassHome, "pc-1").loop(context.Background(), true)
			j := ct.job(t, id)
			if j.State != controller.CIFailed || exitCode(j) != nil || !strings.Contains(summary(j), ".agent-box/ci.json") || !strings.Contains(summary(j), "nothing ran") {
				t.Fatalf("job = %+v, summary %q", j, summary(j))
			}
			if len(ct.pc.startJobs) != 0 {
				t.Fatalf("PC ran %q", ct.pc.startJobs)
			}
			ct.noWorktreesLeft(t)
		})
	}
}

func TestCIWorkerCloudJobRunsOnlyOnCloud(t *testing.T) {
	ct := newCITest(t, true)
	sha := ciSourceRepo(t, ct.base, "a-Fig/lantern", `{"command": "go test ./..."}`)
	pr := 12
	id := ct.ms.EnqueueCIJob("a-Fig/lantern", sha, controller.JobClassCloud, &pr)
	if code := ct.worker(controller.HostClassCloud, "modal-1").loop(context.Background(), true); code != 0 {
		t.Fatalf("loop = %d", code)
	}
	j := ct.job(t, id)
	if j.State != controller.CIDone || !strings.Contains(summary(j), "on cloud") {
		t.Fatalf("job = %+v, summary %q", j, summary(j))
	}
	if ct.cloud.startHits != 1 {
		t.Fatalf("cloud starts = %d, want 1", ct.cloud.startHits)
	}
	// The PC has the higher priority and room, and was never asked.
	if len(ct.pc.startJobs) != 0 || ct.pc.listCalls != 0 || ct.pc.boxdHits != 0 || len(ct.pc.pushedFiles) != 0 {
		t.Fatalf("PC touched: starts %q, lists %d, boxd hits %d, pushes %q", ct.pc.startJobs, ct.pc.listCalls, ct.pc.boxdHits, ct.pc.pushedFiles)
	}
	ct.noWorktreesLeft(t)
}

func TestCIWorkerCloudOnlyNeedsCloudHost(t *testing.T) {
	ct := newCITest(t, false)
	sha := ciSourceRepo(t, ct.base, "a-Fig/lantern", `{"command": "go test ./..."}`)
	id := ct.ms.EnqueueCIJob("a-Fig/lantern", sha, controller.JobClassAny, nil)
	ct.worker(controller.HostClassCloud, "modal-1").loop(context.Background(), true)
	j := ct.job(t, id)
	if j.State != controller.CIFailed || !strings.Contains(summary(j), "needs a cloud host") {
		t.Fatalf("job = %+v, summary %q", j, summary(j))
	}
	if len(ct.pc.startJobs) != 0 || ct.pc.listCalls != 0 {
		t.Fatalf("PC touched: starts %q, lists %d", ct.pc.startJobs, ct.pc.listCalls)
	}
}

func TestCIWorkerHomeRefusesCloudJob(t *testing.T) {
	ct := newCITest(t, false)
	sha := ciSourceRepo(t, ct.base, "a-Fig/lantern", `{"command": "go test ./..."}`)
	id := ct.ms.EnqueueCIJob("a-Fig/lantern", sha, controller.JobClassCloud, nil)
	// A controller bug hands the fork PR's job to a home worker anyway.
	job, err := (&controller.Client{URL: ct.srv.URL, Token: "host-secret"}).NextCIJob(context.Background(), "pc-1", controller.HostClassCloud)
	if err != nil || job == nil {
		t.Fatalf("claim: %+v, %v", job, err)
	}
	ct.worker(controller.HostClassHome, "pc-1").handle(context.Background(), job)
	j := ct.job(t, id)
	if j.State != controller.CIFailed || exitCode(j) != nil || !strings.Contains(summary(j), "home hardware never runs") {
		t.Fatalf("job = %+v, summary %q", j, summary(j))
	}
	if _, err := os.Stat(ct.cacheDir); !os.IsNotExist(err) {
		t.Fatalf("cache dir touched: %v", err)
	}
	if len(ct.pc.startJobs) != 0 || ct.pc.listCalls != 0 {
		t.Fatalf("PC touched: starts %q, lists %d", ct.pc.startJobs, ct.pc.listCalls)
	}
}

func TestCIWorkerRejectsBadRepoAndSHA(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, c := range []struct{ repo, sha, want string }{
		{"../evil", sha, "owner/name"},
		{"a-Fig/..", sha, "owner/name"},
		{"a-Fig/lantern", "HEAD", "full hex SHA"},
		{"a-Fig/lantern", "--upload-pack=touch /tmp/x", "full hex SHA"},
	} {
		ct := newCITest(t, false)
		id := ct.ms.EnqueueCIJob(c.repo, c.sha, controller.JobClassAny, nil)
		ct.worker(controller.HostClassHome, "pc-1").loop(context.Background(), true)
		j := ct.job(t, id)
		if j.State != controller.CIFailed || !strings.Contains(summary(j), c.want) {
			t.Fatalf("%s@%s: job = %+v, summary %q", c.repo, c.sha, j, summary(j))
		}
		if _, err := os.Stat(ct.cacheDir); !os.IsNotExist(err) {
			t.Fatalf("%s@%s: cache dir touched: %v", c.repo, c.sha, err)
		}
	}
}

func TestCIWorkerEmptyQueueOnce(t *testing.T) {
	ct := newCITest(t, false)
	tok := filepath.Join(t.TempDir(), "host.token")
	write(t, tok, "host-secret")
	errOut := ct.a.Err.(interface{ String() string })
	code := run(context.Background(), ct.a, []string{"ci", "worker", "--url", ct.srv.URL, "--token-file", tok, "--worker", "pc-1", "--cache-dir", ct.cacheDir, "--once"}, nil)
	if code != 0 || !strings.Contains(errOut.String(), "no job queued") {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
}

func TestCIWorkerFlagsAndAuth(t *testing.T) {
	ct := newCITest(t, false)
	tok := filepath.Join(t.TempDir(), "host.token")
	write(t, tok, "host-secret")
	for _, args := range [][]string{
		{"ci"},
		{"ci", "worker", "--host-class", "laptop", "--url", ct.srv.URL, "--token-file", tok},
		{"ci", "worker", "--poll", "1s", "--url", ct.srv.URL, "--token-file", tok},
		{"ci", "worker", "--worker", "Bad_Name", "--url", ct.srv.URL, "--token-file", tok},
		{"ci", "worker", "--url", "http://example.com", "--token-file", tok},
	} {
		if code := run(context.Background(), ct.a, args, nil); code != 2 {
			t.Errorf("%q exited %d, want 2", args, code)
		}
	}

	hostsTestPath(t) // a fresh config dir: no real controller.json can supply a URL
	missing := filepath.Join(t.TempDir(), "boxd.json")
	if _, _, err := ciControllerAuth(ct.srv.URL, "", missing); err == nil || !strings.Contains(err.Error(), "--token-file") {
		t.Fatalf("no token: %v", err)
	}
	if _, _, err := ciControllerAuth("", tok, missing); err == nil || !strings.Contains(err.Error(), "--url") {
		t.Fatalf("no URL: %v", err)
	}
	boxd := filepath.Join(t.TempDir(), "boxd.json")
	write(t, boxd, `{"controller": {"url": "`+ct.srv.URL+`", "token_file": "`+filepath.ToSlash(tok)+`", "id": "pc"}}`)
	if u, got, err := ciControllerAuth("", "", boxd); err != nil || u != ct.srv.URL || got != "host-secret" {
		t.Fatalf("boxd.json fallback = %q, %q, %v", u, got, err)
	}
}

func TestBoundSummary(t *testing.T) {
	s := strings.Repeat("a", ciSummaryMax) + "TAIL"
	got := boundSummary(s)
	if len(got) > ciSummaryMax || !strings.HasSuffix(got, "TAIL") || !strings.HasPrefix(got, "aaa") {
		t.Fatalf("boundSummary: len %d, ends %q", len(got), got[len(got)-8:])
	}
}

// A fork can commit .agent-box as a symlink to a directory outside the
// checkout; the worker must not read (or run) the ci.json it points at.
func TestCIWorkerRefusesSymlinkedConfigDir(t *testing.T) {
	ct := newCITest(t, false)
	outside := t.TempDir()
	write(t, filepath.Join(outside, "ci.json"), `{"command": "echo OUTSIDE_SECRET"}`)

	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, src, "init", "-q")
	write(t, filepath.Join(src, "main.go"), "package main\n")
	write(t, filepath.Join(src, "target"), outside)
	blob := gitT(t, src, "hash-object", "-w", "target")
	os.Remove(filepath.Join(src, "target"))
	gitT(t, src, "add", "main.go")
	gitT(t, src, "update-index", "--add", "--cacheinfo", "120000,"+blob+",.agent-box")
	gitT(t, src, "commit", "-q", "-m", "symlinked config dir")
	sha := gitT(t, src, "rev-parse", "HEAD")
	bare := filepath.Join(ct.base, "a-Fig", "lantern.git")
	os.MkdirAll(filepath.Dir(bare), 0o755)
	gitT(t, src, "clone", "-q", "--bare", src, bare)
	gitT(t, bare, "config", "uploadpack.allowAnySHA1InWant", "true")

	id := ct.ms.EnqueueCIJob("a-Fig/lantern", sha, controller.JobClassAny, nil)
	ct.worker(controller.HostClassHome, "pc-1").loop(context.Background(), true)
	j := ct.job(t, id)
	if j.State != controller.CIFailed || exitCode(j) != nil || strings.Contains(summary(j), "OUTSIDE_SECRET") {
		t.Fatalf("job = %+v, summary %q", j, summary(j))
	}
	if len(ct.pc.startJobs) != 0 {
		t.Fatalf("PC ran %q", ct.pc.startJobs)
	}
	ct.noWorktreesLeft(t)
}

func TestCIWorkerReportsCancelledWhenStopped(t *testing.T) {
	ct := newCITest(t, true)
	ct.cloud.jobDelay = time.Hour
	sha := ciSourceRepo(t, ct.base, "a-Fig/lantern", `{"command": "go test ./..."}`)
	id := ct.ms.EnqueueCIJob("a-Fig/lantern", sha, controller.JobClassAny, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Stop the worker (as SIGTERM would) once the cloud job has started.
	ct.afterCloud = func(req *http.Request) {
		if req.Method == http.MethodPost && req.URL.Path == "/v1/jobs" {
			cancel()
		}
	}
	if code := ct.worker(controller.HostClassCloud, "modal-1").loop(ctx, false); code != 0 {
		t.Fatalf("loop = %d", code)
	}
	j := ct.job(t, id)
	if j.State != controller.CICancelled || !strings.Contains(summary(j), "stopped before the job finished") {
		t.Fatalf("job = %+v, summary %q", j, summary(j))
	}
	ct.noWorktreesLeft(t)
}
