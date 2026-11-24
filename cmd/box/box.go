package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// exitPaused tells callers the box or the PC is paused (or out of reach),
// so they should run the command locally instead. 75 is EX_TEMPFAIL.
const exitPaused = 75

// exitDetached is returned when box stops following a job that keeps running.
const exitDetached = 130

const defaultTTL = 8 * time.Hour

// workRoot holds checkouts (/work/<repo>) and job files (/work/.box) in a box.
var workRoot = "/work"

// App holds box's settings and where it talks to.
type App struct {
	R       Runner
	Remote  string // incus remote, "pc"
	Project string // incus project, "agents"
	Image   string // image alias for new boxes, "agent-base"
	Out     io.Writer
	Err     io.Writer
	Now     func() time.Time
	Rand    func() string // short random suffix for names and job IDs
}

func newApp() *App {
	return &App{
		R:       execRunner{},
		Remote:  env("BOX_REMOTE", "pc"),
		Project: env("BOX_PROJECT", "agents"),
		Image:   env("BOX_IMAGE", "agent-base"),
		Out:     os.Stdout,
		Err:     os.Stderr,
		Now:     time.Now,
		Rand:    randHex,
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func randHex() string {
	b := make([]byte, 2)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// incus runs the incus CLI in the box project.
func (a *App) incus(ctx context.Context, c Cmd) (Result, error) {
	c.Name = "incus"
	c.Args = append([]string{"--project", a.Project}, c.Args...)
	return a.R.Run(ctx, c)
}

// incusOK runs incus and turns a nonzero exit into an error.
func (a *App) incusOK(ctx context.Context, args ...string) (string, error) {
	res, err := a.incus(ctx, Cmd{Args: args})
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		return "", fmt.Errorf("incus %s: %s", args[0], strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

func (a *App) target(box string) string { return a.Remote + ":" + box }

type instance struct {
	Name   string            `json:"name"`
	Status string            `json:"status"` // Running, Stopped, Frozen, ...
	Config map[string]string `json:"config"`
}

func (a *App) list(ctx context.Context) ([]instance, error) {
	out, err := a.incusOK(ctx, "list", a.Remote+":", "--format", "json")
	if err != nil {
		return nil, err
	}
	var insts []instance
	if err := json.Unmarshal([]byte(out), &insts); err != nil {
		return nil, fmt.Errorf("incus list: %v", err)
	}
	return insts, nil
}

// mode is the tray's mode (mypc, shared, full), or "" when it can't be read.
func (a *App) mode(ctx context.Context) string {
	out, err := a.incusOK(ctx, "project", "get", a.target(a.Project), "user.agent-box.mode")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// reach lists the boxes and says why nothing can run now, if it can't.
func (a *App) reach(ctx context.Context) ([]instance, string) {
	insts, err := a.list(ctx)
	if err != nil {
		return nil, "can't reach " + a.Remote + ": " + err.Error()
	}
	if a.mode(ctx) == "mypc" {
		return nil, a.Remote + " is in My PC mode"
	}
	return insts, ""
}

func find(insts []instance, name string) *instance {
	for i := range insts {
		if insts[i].Name == name {
			return &insts[i]
		}
	}
	return nil
}

// check finds box (nil if it doesn't exist) and says why it can't run now, if it can't.
func (a *App) check(ctx context.Context, box string) (inst *instance, paused string) {
	insts, paused := a.reach(ctx)
	if paused != "" {
		return nil, paused
	}
	inst = find(insts, box)
	if inst != nil && inst.Status == "Frozen" {
		return nil, box + " is paused"
	}
	return inst, ""
}

var validBox = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// throwawayKey marks a box that box run --new created for one job; box job
// deletes it once that job has finished.
const throwawayKey = "user.box.throwaway"

// New launches a box. An empty name gets a random one; ttl > 0 sets user.expires.
func (a *App) New(ctx context.Context, name string, ttl time.Duration) (string, error) {
	if name == "" {
		name = "box-" + a.Rand()
	}
	return name, a.launch(ctx, name, ttl, false)
}

func (a *App) launch(ctx context.Context, name string, ttl time.Duration, throwaway bool) error {
	if !validBox.MatchString(name) {
		return fmt.Errorf("bad box name %q: use lowercase letters, digits and hyphens", name)
	}
	// Unqualified, the image would be looked up on the local (laptop) remote.
	args := []string{"launch", a.target(a.Image), a.target(name)}
	if ttl > 0 {
		args = append(args, "-c", "user.expires="+a.Now().Add(ttl).UTC().Format(time.RFC3339))
	}
	if throwaway {
		args = append(args, "-c", throwawayKey+"=true")
	}
	_, err := a.incusOK(ctx, args...)
	return err
}

// isConflict says whether a launch failed because the name is taken.
func isConflict(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "already exists")
}

// Rm deletes a box, running or not.
func (a *App) Rm(ctx context.Context, name string) error {
	_, err := a.incusOK(ctx, "delete", a.target(name), "--force")
	return err
}

// Ls prints the boxes in the project.
func (a *App) Ls(ctx context.Context) error {
	insts, err := a.list(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(a.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATUS\tEXPIRES")
	for _, in := range insts {
		exp := in.Config["user.expires"]
		if exp == "" {
			exp = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", in.Name, in.Status, exp)
	}
	return w.Flush()
}

// RunOpts says what box run does.
type RunOpts struct {
	Dir  string   // local git worktree
	Cmd  []string // one shell string, or argv to quote
	Box  string   // box to use; "" is the repo's own box
	New  bool     // use a throwaway box, deleted afterwards
	TTL  time.Duration
	Wait time.Duration // stop following after this long; 0 waits for the end
}

// What following a job ended with.
const (
	stateDone     = "done"     // the job's exit code was read
	stateDetached = "detached" // stopped following; the job keeps running
	statePaused   = "paused"   // the PC or the box is paused or out of reach
)

// JobResult is what Run and Job report. Code is the job's own exit code when
// State is done; otherwise it is exitDetached, exitPaused, or 1 or 2 with an
// error.
type JobResult struct {
	Job   string `json:"job,omitempty"`
	Box   string `json:"box,omitempty"`
	State string `json:"state,omitempty"`
	Code  int    `json:"exitCode"`
}

// Run syncs the worktree to a box, runs the command there and follows its
// output.
func (a *App) Run(ctx context.Context, o RunOpts) (JobResult, error) {
	if len(o.Cmd) == 0 {
		return JobResult{Code: 2}, errors.New("no command given")
	}
	rp, err := openRepo(ctx, a.R, o.Dir)
	if err != nil {
		return JobResult{Code: 1}, err
	}
	box := o.Box
	if box == "" {
		box = rp.Name
	}
	if !validBox.MatchString(box) {
		return JobResult{Code: 2}, fmt.Errorf("bad box name %q", box)
	}
	a.warnSubmodules(ctx, rp.Top)

	insts, paused := a.reach(ctx)
	var inst *instance
	if paused == "" && !o.New {
		if inst = find(insts, box); inst != nil && inst.Status == "Frozen" {
			paused = box + " is paused"
		}
	}
	if paused != "" {
		fmt.Fprintf(a.Err, "box: %s; run it locally\n", paused)
		return JobResult{State: statePaused, Code: exitPaused}, nil
	}

	id := a.Now().UTC().Format("20060102-150405") + "-" + a.Rand()
	bundle, err := snapshot(ctx, a.R, rp.Top, id)
	if err != nil {
		return JobResult{Code: 1}, err
	}
	defer os.RemoveAll(filepath.Dir(bundle))

	started := false // from here on the remote job may be running
	if o.New {
		ttl := o.TTL
		if ttl == 0 {
			ttl = defaultTTL
		}
		if box, err = a.launchThrowaway(ctx, rp.Name, insts, ttl); err != nil {
			return JobResult{Code: 1}, err
		}
		// Until its job starts, a failed run takes its own box with it.
		defer func() {
			if !started {
				a.removeQuietly(ctx, box)
			}
		}()
	} else if paused, err := a.ensureBox(ctx, box, inst); err != nil {
		return JobResult{Code: 1}, err
	} else if paused != "" {
		fmt.Fprintf(a.Err, "box: %s; run it locally\n", paused)
		return JobResult{State: statePaused, Code: exitPaused}, nil
	}

	job := box + "." + id
	r := JobResult{Job: job, Box: box, Code: 1}
	remote := workRoot + "/.box/" + job
	if _, err := a.incusOK(ctx, "file", "push", "--create-dirs", bundle, a.target(box)+remote+".bundle"); err != nil {
		return r, err
	}
	// Say the job ID before starting it, so a dropped connection can't lose it.
	fmt.Fprintf(a.Err, "box: job %s on %s\n", job, box)
	started = true
	if _, err := a.incusOK(ctx, "exec", a.target(box), "--", "bash", "-lc", startScript(job, id, rp.Name, o.Cmd)); err != nil {
		if o.New {
			fmt.Fprintf(a.Err, "box: keeping %s; box job %s removes it once the job has ended\n", box, job)
		}
		return r, err
	}

	r.State, r.Code, err = a.attach(ctx, job, o.Wait)
	switch {
	case !o.New:
	case err == nil && r.State == stateDone:
		a.cleanup(ctx, box, true)
	default:
		fmt.Fprintf(a.Err, "box: keeping %s; box job %s removes it once the job has ended\n", box, job)
	}
	return r, err
}

// launchThrowaway launches a fresh box for one job. It never adopts an
// existing box: a taken name means another try with a new suffix.
func (a *App) launchThrowaway(ctx context.Context, base string, insts []instance, ttl time.Duration) (string, error) {
	for try := 0; try < 8; try++ {
		name := base + "-" + a.Rand()
		if find(insts, name) != nil {
			continue
		}
		fmt.Fprintf(a.Err, "box: launching %s\n", name)
		err := a.launch(ctx, name, ttl, true)
		if !isConflict(err) {
			return name, err
		}
		insts = append(insts, instance{Name: name})
	}
	return "", errors.New("no free name for a throwaway box")
}

// ensureBox launches or starts a reused box. When a concurrent run launches
// or starts it first, it carries on with that one.
func (a *App) ensureBox(ctx context.Context, box string, inst *instance) (paused string, err error) {
	if inst != nil && inst.Status == "Running" {
		return "", nil
	}
	if inst == nil {
		fmt.Fprintf(a.Err, "box: launching %s\n", box)
		err = a.launch(ctx, box, 0, false)
	} else {
		_, err = a.incusOK(ctx, "start", a.target(box))
	}
	if err == nil {
		return "", nil
	}
	now, paused := a.check(ctx, box)
	switch {
	case paused != "":
		return paused, nil
	case now == nil:
		return "", err
	case now.Status == "Running":
		return "", nil
	case inst == nil: // someone else launched it but it isn't running yet
		if _, err := a.incusOK(ctx, "start", a.target(box)); err != nil {
			if now, _ := a.check(ctx, box); now != nil && now.Status == "Running" {
				return "", nil
			}
			return "", err
		}
		return "", nil
	}
	return "", err
}

func (a *App) removeQuietly(ctx context.Context, box string) {
	fmt.Fprintf(a.Err, "box: removing %s\n", box)
	if err := a.Rm(context.WithoutCancel(ctx), box); err != nil {
		fmt.Fprintf(a.Err, "box: removing %s: %v\n", box, err)
	}
}

// cleanup deletes a throwaway box once no job is running in it. ours says
// the caller created it; otherwise the box's throwaway mark decides.
func (a *App) cleanup(ctx context.Context, box string, ours bool) {
	ctx = context.WithoutCancel(ctx)
	if !ours {
		v, err := a.incusOK(ctx, "config", "get", a.target(box), throwawayKey)
		if err != nil || strings.TrimSpace(v) != "true" {
			return
		}
	}
	out, err := a.incusOK(ctx, "exec", a.target(box), "--", "bash", "-c", runningScript())
	if err != nil {
		fmt.Fprintf(a.Err, "box: keeping %s: %v\n", box, err)
		return
	}
	if running := strings.Fields(out); len(running) > 0 {
		fmt.Fprintf(a.Err, "box: keeping %s: still running %s\n", box, strings.Join(running, " "))
		return
	}
	a.removeQuietly(ctx, box)
}

// warnSubmodules says that checked-out submodules stay behind.
func (a *App) warnSubmodules(ctx context.Context, top string) {
	out, err := git(ctx, a.R, top, nil, "submodule", "status")
	if err != nil {
		return
	}
	var paths []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		if l == "" || l[0] == '-' { // "-": registered but not checked out
			continue
		}
		if f := strings.Fields(l); len(f) >= 2 {
			paths = append(paths, f[1])
		}
	}
	if len(paths) > 0 {
		fmt.Fprintf(a.Err, "box: warning: submodules aren't synced and will be empty in the box: %s\n", strings.Join(paths, " "))
	}
}

var validJob = regexp.MustCompile(`^[a-z][a-z0-9-]*\.[0-9]{8}-[0-9]{6}-[0-9a-f]+$`)

// Job reattaches to a job started by Run. Once the job has ended, it deletes
// the job's box if box run --new made it and no other job runs there.
func (a *App) Job(ctx context.Context, job string, wait time.Duration) (JobResult, error) {
	if !validJob.MatchString(job) {
		return JobResult{Code: 2}, fmt.Errorf("bad job id %q", job)
	}
	box, _, _ := strings.Cut(job, ".")
	r := JobResult{Job: job, Box: box}
	var err error
	r.State, r.Code, err = a.attach(ctx, job, wait)
	if err == nil && r.State == stateDone {
		a.cleanup(ctx, box, false)
	}
	return r, err
}

// aliveFn defines alive PID JOB: whether PID is still JOB's wrapper and not
// a later process that got the same PID after a restart.
const aliveFn = `alive() { [ -n "$1" ] && kill -0 "$1" 2>/dev/null && { [ ! -r /proc/"$1"/cmdline ] || grep -aqF "$2" /proc/"$1"/cmdline; }; }
`

// runningScript prints the jobs still running in a box.
func runningScript() string {
	return "# running jobs\n" + aliveFn + fmt.Sprintf(`cd %s/.box 2>/dev/null || exit 0
for p in *.pid; do
  [ -e "$p" ] || continue
  j=${p%%.pid}
  [ -e "$j.exit" ] && continue
  alive "$(cat "$p" 2>/dev/null)" "$j" && echo "$j"
done
exit 0
`, workRoot)
}

// followScript prints a job's log, as it grows, until the job's .exit file
// appears. It exits 3 for an unknown job and 4 for a job that died without
// writing its exit code.
func followScript(job string) string {
	f := workRoot + "/.box/" + job
	return "# follow\n" + aliveFn + fmt.Sprintf(`test -f %[1]s.log || { echo "box: no job %[2]s" >&2; exit 3; }
pid=$(cat %[1]s.pid 2>/dev/null)
off=0
while :; do
  fin=0; [ -f %[1]s.exit ] && fin=1
  size=$(( $(wc -c < %[1]s.log) ))
  if [ "$size" -gt "$off" ]; then
    tail -c +$((off + 1)) %[1]s.log | head -c $((size - off))
    off=$size
  fi
  [ $fin = 1 ] && exit 0
  if ! alive "$pid" %[2]s; then
    [ -f %[1]s.exit ] && continue
    echo "box: job %[2]s died without an exit code" >&2
    exit 4
  fi
  sleep 0.5
done
`, f, job)
}

// attach streams a job's log until the job exits and returns its exit code.
func (a *App) attach(ctx context.Context, job string, wait time.Duration) (state string, code int, err error) {
	box, _, _ := strings.Cut(job, ".")
	f := workRoot + "/.box/" + job
	follow := ctx
	if wait > 0 {
		var cancel context.CancelFunc
		follow, cancel = context.WithTimeout(ctx, wait)
		defer cancel()
	}
	res, err := a.incus(follow, Cmd{
		Args:   []string{"exec", a.target(box), "--", "bash", "-c", followScript(job)},
		Stdout: a.Out, Stderr: a.Err,
	})
	if follow.Err() != nil {
		fmt.Fprintf(a.Err, "box: job %s is still running; reattach with: box job %s\n", job, job)
		return stateDetached, exitDetached, nil
	}
	if err != nil {
		return "", 1, err
	}
	switch res.Code {
	case 0:
	case 3:
		return "", 1, fmt.Errorf("no job %s", job)
	case 4:
		return "", 1, fmt.Errorf("job %s died without an exit code", job)
	default:
		if _, paused := a.check(ctx, box); paused != "" {
			fmt.Fprintf(a.Err, "box: %s; run it locally\n", paused)
			return statePaused, exitPaused, nil
		}
		return "", 1, fmt.Errorf("lost job %s (reattach with: box job %s)", job, job)
	}
	out, err := a.incusOK(ctx, "exec", a.target(box), "--", "cat", f+".exit")
	if err != nil {
		return "", 1, fmt.Errorf("job %s ended without an exit code: %v", job, err)
	}
	code, err = strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return "", 1, fmt.Errorf("job %s: bad exit code %q", job, out)
	}
	return stateDone, code, nil
}

// startScript checks the bundle out in /work/<repo> and starts cmd detached,
// logging to /work/.box/<job>.log and writing its exit code to <job>.exit.
// ref names the snapshot in the bundle (refs/agent-box/<ref>). Jobs on the
// same repo in the same box take turns (flock), since they share the
// checkout. The payload runs without the lock's FD, so a background child it
// leaves behind can't keep holding the lock.
func startScript(job, ref, repo string, cmd []string) string {
	w := workRoot
	f := w + "/.box/" + job
	inner := fmt.Sprintf(`exec 9>%[1]s/.box/%[3]s.lock
flock 9
if git init -q %[1]s/%[3]s && cd %[1]s/%[3]s &&
   git fetch -q %[2]s.bundle refs/agent-box/%[4]s &&
   git checkout -q --force --detach FETCH_HEAD && git clean -fdq; then
  (
%[5]s
  ) 9>&-
  code=$?
else
  echo "box: checkout failed" >&2
  code=1
fi
rm -f %[2]s.bundle
echo $code > %[2]s.exit.tmp && mv %[2]s.exit.tmp %[2]s.exit
`, w, f, repo, ref, shellCommand(cmd))
	// The log is created before the job starts, so a follow that begins at
	// once never sees "no job".
	return fmt.Sprintf("mkdir -p %[1]s/.box && : > %[3]s.log && { setsid nohup bash -lc %[2]s >> %[3]s.log 2>&1 < /dev/null & }\necho $! > %[3]s.pid",
		w, shq(inner), f)
}

// shellCommand runs a single argument as a shell string ("go test ./... && x")
// and quotes several arguments word by word.
func shellCommand(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	q := make([]string, len(args))
	for i, s := range args {
		q[i] = shq(s)
	}
	return strings.Join(q, " ")
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
