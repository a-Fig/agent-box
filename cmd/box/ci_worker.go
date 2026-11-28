package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/a-Fig/agent-box/internal/controller"
)

// box ci worker polls the fleet controller's CI queue (docs/controller-protocol.md,
// CI endpoints), checks each claimed commit out of a local bare cache, reads the
// repo's .agent-box/ci.json and runs its command through Run, so the command
// only ever runs in a box, never on the worker's own machine.

const (
	ciDefaultPoll    = 45 * time.Second
	ciMinPoll        = 5 * time.Second
	ciMaxPoll        = time.Hour
	ciDefaultTimeout = 30 * time.Minute
	ciMaxTimeout     = 6 * time.Hour
	ciSummaryMax     = 4000
	ciTailLines      = 20
	ciConfigMax      = 64 << 10
	ciReportTimeout  = 15 * time.Second
	ciConfigPath     = ".agent-box/ci.json"
	ciDefaultRepoURL = "https://github.com"
	boxdConfigPath   = "/etc/agent-box/boxd.json"
)

// ciCommand is ci.json's command: one shell string, or argv.
type ciCommand []string

func (c *ciCommand) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*c = ciCommand{s}
		return nil
	}
	var argv []string
	if err := json.Unmarshal(b, &argv); err != nil {
		return errors.New("command must be a string or an array of strings")
	}
	*c = argv
	return nil
}

// ciConfig is a repo's .agent-box/ci.json.
type ciConfig struct {
	Command ciCommand `json:"command"`
	Size    string    `json:"size"`
	Timeout string    `json:"timeout"`
}

type ciWorker struct {
	a         *App
	client    *controller.Client
	worker    string
	hostClass string
	poll      time.Duration
	cacheDir  string
	repoBase  string // remote is repoBase/owner/name.git
}

func runCIWorker(ctx context.Context, a *App, fs *flag.FlagSet, args []string) int {
	if len(args) == 0 || args[0] != "worker" {
		fmt.Fprint(a.Err, usageText())
		return 2
	}
	ctrlURL := fs.String("url", "", "controller URL (default: boxd.json's controller.url, then controller.json's url)")
	tokenFile := fs.String("token-file", "", "file holding the controller's HOST_TOKEN (default: boxd.json's controller.token_file)")
	worker := fs.String("worker", "", "worker ID, ^[a-z0-9-]{1,40}$ (default: this machine's hostname)")
	hostClass := fs.String("host-class", controller.HostClassHome, "home (runs on this fleet, any host) or cloud (cloud hosts only; gets fork PRs)")
	poll := fs.Duration("poll", ciDefaultPoll, "how often to ask for a job when the queue is empty (5s to 1h)")
	cacheDir := fs.String("cache-dir", "", "where bare repo caches and job checkouts live (default: the user cache dir's agent-box/ci)")
	repoBase := fs.String("repo-base-url", ciDefaultRepoURL, "fetch owner/name from <this>/owner/name.git: a URL or a local directory")
	once := fs.Bool("once", false, "take at most one job, then exit")
	if fs.Parse(args[1:]) != nil || fs.NArg() != 0 {
		fmt.Fprint(a.Err, usageText())
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintln(a.Err, "box ci worker:", err)
		return 2
	}
	if *hostClass != controller.HostClassHome && *hostClass != controller.HostClassCloud {
		return fail(fmt.Errorf("--host-class must be home or cloud, not %q", *hostClass))
	}
	if *poll < ciMinPoll || *poll > ciMaxPoll {
		return fail(fmt.Errorf("--poll must be between %s and %s", ciMinPoll, ciMaxPoll))
	}
	if *worker == "" {
		h, _ := os.Hostname()
		*worker = controller.SanitizeHostID(h)
	}
	if !controller.ValidHostID(*worker) {
		return fail(fmt.Errorf("--worker %q must match ^[a-z0-9-]{1,40}$", *worker))
	}
	if *cacheDir == "" {
		d, err := os.UserCacheDir()
		if err != nil {
			return fail(fmt.Errorf("no --cache-dir and no user cache dir: %w", err))
		}
		*cacheDir = filepath.Join(d, "agent-box", "ci")
	}
	u, tok, err := ciControllerAuth(*ctrlURL, *tokenFile, boxdConfigPath)
	if err != nil {
		return fail(err)
	}
	hc := &http.Client{Timeout: controller.DefaultTimeout}
	if a.HTTP != nil {
		hc.Transport = roundTripFunc(a.HTTP)
	}
	w := &ciWorker{
		a:         a,
		client:    &controller.Client{URL: u, Token: tok, HTTP: hc},
		worker:    *worker,
		hostClass: *hostClass,
		poll:      *poll,
		cacheDir:  *cacheDir,
		repoBase:  strings.TrimRight(*repoBase, "/"),
	}
	return w.loop(ctx, *once)
}

// ciControllerAuth finds the controller URL and HOST_TOKEN: flags first, then
// boxd.json's controller block (the host token boxd publishes with), and for
// the URL only, controller.json (whose token is the client token).
func ciControllerAuth(ctrlURL, tokenFile, boxdPath string) (string, string, error) {
	var boxdCfg struct {
		Controller *struct {
			URL       string `json:"url"`
			TokenFile string `json:"token_file"`
		} `json:"controller"`
	}
	if data, err := os.ReadFile(boxdPath); err == nil {
		if err := json.Unmarshal(data, &boxdCfg); err != nil {
			return "", "", fmt.Errorf("parse %s: %w", boxdPath, err)
		}
	}
	if c := boxdCfg.Controller; c != nil {
		if ctrlURL == "" {
			ctrlURL = strings.TrimSpace(c.URL)
		}
		if tokenFile == "" {
			tokenFile = strings.TrimSpace(c.TokenFile)
		}
	}
	if ctrlURL == "" {
		if dir, err := os.UserConfigDir(); err == nil {
			if data, err := os.ReadFile(filepath.Join(dir, "agent-box", "controller.json")); err == nil {
				var cc controllerConfig
				if json.Unmarshal(data, &cc) == nil {
					ctrlURL = strings.TrimSpace(cc.URL)
				}
			}
		}
	}
	if ctrlURL == "" {
		return "", "", errors.New("no controller URL: pass --url, or set controller.url in boxd.json")
	}
	if !controller.ValidControllerURL(ctrlURL) {
		return "", "", fmt.Errorf("controller URL %q must be https:// (or http:// on loopback)", ctrlURL)
	}
	if tokenFile == "" {
		return "", "", errors.New("no host token: pass --token-file with the controller's HOST_TOKEN, or set controller.token_file in boxd.json")
	}
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		return "", "", fmt.Errorf("read token file: %w", err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", "", fmt.Errorf("token file %s is empty", tokenFile)
	}
	return ctrlURL, tok, nil
}

func (w *ciWorker) logf(format string, args ...any) {
	fmt.Fprintf(w.a.Err, "box ci worker: "+format+"\n", args...)
}

// loop polls until ctx ends. With once it handles at most one job, and exits 1
// only when the controller couldn't be asked.
func (w *ciWorker) loop(ctx context.Context, once bool) int {
	w.logf("%s (%s) polling %s every %s", w.worker, w.hostClass, w.client.URL, w.poll)
	for {
		job, err := w.client.NextCIJob(ctx, w.worker, w.hostClass)
		switch {
		case ctx.Err() != nil:
			return 0
		case err != nil:
			w.logf("ask for a job: %v", err)
			if once {
				return 1
			}
		case job != nil:
			w.handle(ctx, job)
			if once || ctx.Err() != nil {
				return 0
			}
			continue // ask again at once: more may be queued
		case once:
			w.logf("no job queued")
			return 0
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(w.poll):
		}
	}
}

// handle runs one claimed job and reports how it ended. Every path reports.
func (w *ciWorker) handle(ctx context.Context, job *controller.CIJob) {
	w.logf("job %s: %s@%s (%s)", job.ID, job.Repo, shortSHA(job.SHA), job.HostClass)
	state, code, summary := w.execute(ctx, job)
	if ctx.Err() != nil && state != controller.CIDone {
		state, summary = controller.CICancelled, "agent-box: the CI worker stopped before the job finished. "+summary
	}
	patch := controller.CIJobPatch{Worker: w.worker, State: state, ExitCode: code, Summary: boundSummary(summary)}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ciReportTimeout)
	defer cancel()
	if err := w.client.PatchCIJob(rctx, job.ID, patch); err != nil {
		w.logf("job %s: report %s: %v", job.ID, state, err)
		return
	}
	w.logf("job %s: %s", job.ID, state)
}

// execute returns the job's final state, exit code (nil when nothing ran) and summary.
func (w *ciWorker) execute(ctx context.Context, job *controller.CIJob) (string, *int, string) {
	refuse := func(why string) (string, *int, string) {
		return controller.CIFailed, nil, "agent-box refused this job; nothing ran: " + why
	}
	// Fork PRs never run on home hardware: the controller only hands them to
	// cloud workers, and a home worker refuses one anyway.
	switch job.HostClass {
	case controller.JobClassAny:
	case controller.JobClassCloud:
		if w.hostClass != controller.HostClassCloud {
			return refuse("a cloud-only job (a fork PR) reached a home worker, and home hardware never runs those.")
		}
	default:
		return refuse(fmt.Sprintf("unknown host class %q.", job.HostClass))
	}
	if !controller.ValidRepo(job.Repo) {
		return refuse("the repo name is not owner/name.")
	}
	if !controller.ValidSHA(job.SHA) {
		return refuse("the commit is not a full hex SHA.")
	}

	dir, cleanup, err := w.checkout(ctx, job.Repo, job.SHA)
	defer cleanup()
	if err != nil {
		w.logf("job %s: checkout: %v", job.ID, err)
		return controller.CIFailed, nil, fmt.Sprintf("agent-box couldn't check out %s at %s; nothing ran.", job.Repo, shortSHA(job.SHA))
	}
	cfg, opts, err := readCIConfig(dir)
	if err != nil {
		return controller.CIFailed, nil, fmt.Sprintf("agent-box needs a valid %s at %s; nothing ran: %v", ciConfigPath, shortSHA(job.SHA), err)
	}
	opts.Dir = dir
	opts.New = true
	opts.Reason = fmt.Sprintf("GitHub CI for %s at %s (controller job %s).", job.Repo, shortSHA(job.SHA), job.ID)
	opts.Env = map[string]string{"CI": "true", "AGENT_BOX_CI": "1", "GITHUB_REPOSITORY": job.Repo, "GITHUB_SHA": job.SHA}
	// A cloud worker runs on cloud hosts only, whatever the job's class.
	opts.CloudOnly = w.hostClass == controller.HostClassCloud
	opts.Queue = !opts.CloudOnly

	tail := &tailBuffer{max: 16 << 10}
	r, err := w.runTee(ctx, opts, tail)
	head := fmt.Sprintf("`%s` on %s", displayCommand(cfg.Command), orDefault(r.Host, "the default host"))
	if r.Job != "" {
		head += ", box job " + r.Job
	}
	code := r.Code
	body := strings.TrimSpace(r.Digest)
	if body == "" {
		body = strings.TrimSpace(lastLines(tail.String(), ciTailLines))
	}
	if body != "" {
		body = "\n\n```\n" + body + "\n```"
	}
	switch {
	case err != nil:
		exit := &code
		if code == 0 { // nothing ran, so there is no exit code to report
			exit = nil
		}
		return controller.CIFailed, exit, fmt.Sprintf("%s failed to run: %v%s", head, err, body)
	case r.State == statePaused || r.State == stateQueued:
		return controller.CIFailed, &code, fmt.Sprintf("%s didn't run (no room on the fleet; exit %d).%s", head, code, body)
	case code == 0:
		return controller.CIDone, &code, fmt.Sprintf("%s passed.%s", head, body)
	default:
		return controller.CIFailed, &code, fmt.Sprintf("%s exited %d.%s", head, code, body)
	}
}

// runTee runs o through Run (following it through the queue if it waits
// there), copying its output to the worker's own output and to tail.
func (w *ciWorker) runTee(ctx context.Context, o RunOpts, tail *tailBuffer) (JobResult, error) {
	a := w.a
	out, errOut := a.Out, a.Err
	a.Out, a.Err = io.MultiWriter(out, tail), io.MultiWriter(errOut, tail)
	defer func() { a.Out, a.Err = out, errOut }()
	r, err := a.Run(ctx, o)
	if err == nil && r.State == stateQueued {
		r, err = a.followQueuedResult(ctx, r)
	}
	return r, err
}

// checkout fetches sha into the bare cache <cacheDir>/<owner>/<name>.git and
// adds a detached worktree of it in a fresh temporary directory. cleanup
// removes the worktree; it is safe to call when checkout failed.
func (w *ciWorker) checkout(ctx context.Context, repoName, sha string) (string, func(), error) {
	owner, name, _ := strings.Cut(repoName, "/")
	bare := filepath.Join(w.cacheDir, owner, name+".git")
	noop := func() {}
	r := w.a.R
	if _, err := os.Stat(filepath.Join(bare, "HEAD")); err != nil {
		if err := os.MkdirAll(bare, 0o755); err != nil {
			return "", noop, err
		}
		if _, err := git(ctx, r, bare, nil, "init", "--bare", "-q"); err != nil {
			return "", noop, err
		}
	}
	remote := w.repoBase + "/" + owner + "/" + name + ".git"
	env := []string{"GIT_TERMINAL_PROMPT=0"}
	if _, err := git(ctx, r, bare, env, "fetch", "-q", "--no-tags", remote, sha); err != nil {
		return "", noop, err
	}
	workRoot := filepath.Join(w.cacheDir, "work")
	if err := os.MkdirAll(workRoot, 0o755); err != nil {
		return "", noop, err
	}
	tmp, err := os.MkdirTemp(workRoot, "job-")
	if err != nil {
		return "", noop, err
	}
	// The folder is named for the repo, so the box is too.
	dir := filepath.Join(tmp, name)
	cleanup := func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ciReportTimeout)
		defer cancel()
		git(cctx, r, bare, nil, "worktree", "remove", "--force", dir)
		os.RemoveAll(tmp)
		git(cctx, r, bare, nil, "worktree", "prune")
	}
	if _, err := git(ctx, r, bare, nil, "worktree", "add", "-q", "--detach", dir, sha); err != nil {
		return "", cleanup, err
	}
	return dir, cleanup, nil
}

// readCIConfig reads and checks <dir>/.agent-box/ci.json.
func readCIConfig(dir string) (ciConfig, RunOpts, error) {
	var cfg ciConfig
	// The checkout is untrusted (a fork PR's), so the file is opened through
	// os.Root: a symlinked .agent-box or ci.json can't reach outside it.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return cfg, RunOpts{}, errors.New("the checkout can't be opened")
	}
	defer root.Close()
	f, err := root.Open(ciConfigPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, RunOpts{}, errors.New("the file is missing")
		}
		return cfg, RunOpts{}, errors.New("the file can't be read inside the checkout")
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return cfg, RunOpts{}, errors.New("it must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, ciConfigMax+1))
	if err != nil {
		return cfg, RunOpts{}, errors.New("the file can't be read")
	}
	if len(data) > ciConfigMax {
		return cfg, RunOpts{}, fmt.Errorf("it is over %d bytes", ciConfigMax)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, RunOpts{}, fmt.Errorf("bad JSON: %v", err)
	}
	if len(cfg.Command) == 0 || strings.TrimSpace(cfg.Command[0]) == "" {
		return cfg, RunOpts{}, errors.New("command is empty")
	}
	if _, err := lookupSize(cfg.Size); err != nil {
		return cfg, RunOpts{}, err
	}
	timeout := ciDefaultTimeout
	if cfg.Timeout != "" {
		timeout, err = time.ParseDuration(cfg.Timeout)
		if err != nil || timeout <= 0 || timeout > ciMaxTimeout {
			return cfg, RunOpts{}, fmt.Errorf("timeout must be a duration from 1s to %s, like \"10m\"", ciMaxTimeout)
		}
	}
	return cfg, RunOpts{Cmd: cfg.Command, Size: cfg.Size, Timeout: timeout}, nil
}

// boundSummary keeps a summary under ciSummaryMax bytes, keeping its start
// and its end (where a failure usually is).
func boundSummary(s string) string {
	s = strings.ToValidUTF8(s, "?")
	if len(s) <= ciSummaryMax {
		return s
	}
	const mark = "\n…\n"
	half := (ciSummaryMax - len(mark)) / 2
	return strings.ToValidUTF8(s[:half]+mark+s[len(s)-half:], "?")
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
