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

// check finds box (nil if it doesn't exist) and says why it can't run now, if it can't.
func (a *App) check(ctx context.Context, box string) (inst *instance, paused string) {
	insts, err := a.list(ctx)
	if err != nil {
		return nil, "can't reach " + a.Remote + ": " + err.Error()
	}
	if a.mode(ctx) == "mypc" {
		return nil, a.Remote + " is in My PC mode"
	}
	for i := range insts {
		if insts[i].Name == box {
			if insts[i].Status == "Frozen" {
				return nil, box + " is paused"
			}
			return &insts[i], ""
		}
	}
	return nil, ""
}

var validBox = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// New launches a box. An empty name gets a random one; ttl > 0 sets user.expires.
func (a *App) New(ctx context.Context, name string, ttl time.Duration) (string, error) {
	if name == "" {
		name = "box-" + a.Rand()
	}
	if !validBox.MatchString(name) {
		return "", fmt.Errorf("bad box name %q: use lowercase letters, digits and hyphens", name)
	}
	// Unqualified, the image would be looked up on the local (laptop) remote.
	args := []string{"launch", a.target(a.Image), a.target(name)}
	if ttl > 0 {
		args = append(args, "-c", "user.expires="+a.Now().Add(ttl).UTC().Format(time.RFC3339))
	}
	_, err := a.incusOK(ctx, args...)
	return name, err
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

// Run syncs the worktree to a box, runs the command there and follows its
// output. It returns the remote exit code, exitPaused, or exitDetached.
func (a *App) Run(ctx context.Context, o RunOpts) (int, error) {
	if len(o.Cmd) == 0 {
		return 2, errors.New("no command given")
	}
	rp, err := openRepo(ctx, a.R, o.Dir)
	if err != nil {
		return 1, err
	}
	box := o.Box
	switch {
	case o.New:
		box = rp.Name + "-" + a.Rand()
	case box == "":
		box = rp.Name
	}
	if !validBox.MatchString(box) {
		return 2, fmt.Errorf("bad box name %q", box)
	}

	inst, paused := a.check(ctx, box)
	if paused != "" {
		fmt.Fprintf(a.Err, "box: %s; run it locally\n", paused)
		return exitPaused, nil
	}

	job := box + "." + a.Now().UTC().Format("20060102-150405") + "-" + a.Rand()
	bundle, err := snapshot(ctx, a.R, rp.Top, job)
	if err != nil {
		return 1, err
	}
	defer os.RemoveAll(filepath.Dir(bundle))

	switch {
	case inst == nil:
		ttl := time.Duration(0)
		if o.New {
			ttl = o.TTL
			if ttl == 0 {
				ttl = defaultTTL
			}
		}
		fmt.Fprintf(a.Err, "box: launching %s\n", box)
		if _, err := a.New(ctx, box, ttl); err != nil {
			return 1, err
		}
	case inst.Status != "Running":
		if _, err := a.incusOK(ctx, "start", a.target(box)); err != nil {
			return 1, err
		}
	}

	remote := workRoot + "/.box/" + job
	if _, err := a.incusOK(ctx, "file", "push", "--create-dirs", bundle, a.target(box)+remote+".bundle"); err != nil {
		return 1, err
	}
	if _, err := a.incusOK(ctx, "exec", a.target(box), "--", "bash", "-lc", startScript(job, rp.Name, o.Cmd)); err != nil {
		return 1, err
	}
	fmt.Fprintf(a.Err, "box: job %s on %s\n", job, box)

	code, err := a.attach(ctx, job, o.Wait)
	if o.New && code != exitDetached && err == nil {
		if err := a.Rm(context.WithoutCancel(ctx), box); err != nil {
			fmt.Fprintf(a.Err, "box: removing %s: %v\n", box, err)
		}
	}
	return code, err
}

var validJob = regexp.MustCompile(`^[a-z][a-z0-9-]*\.[0-9]{8}-[0-9]{6}-[0-9a-f]+$`)

// Job reattaches to a job started by Run.
func (a *App) Job(ctx context.Context, job string, wait time.Duration) (int, error) {
	if !validJob.MatchString(job) {
		return 2, fmt.Errorf("bad job id %q", job)
	}
	return a.attach(ctx, job, wait)
}

// attach streams a job's log until the job exits and returns its exit code.
func (a *App) attach(ctx context.Context, job string, wait time.Duration) (int, error) {
	box, _, _ := strings.Cut(job, ".")
	f := workRoot + "/.box/" + job
	follow := ctx
	if wait > 0 {
		var cancel context.CancelFunc
		follow, cancel = context.WithTimeout(ctx, wait)
		defer cancel()
	}
	script := fmt.Sprintf(`test -f %[1]s.log || { echo "box: no job %[2]s" >&2; exit 3; }
tail -n +1 -f --pid="$(cat %[1]s.pid)" %[1]s.log`, f, job)
	res, err := a.incus(follow, Cmd{
		Args:   []string{"exec", a.target(box), "--", "bash", "-c", script},
		Stdout: a.Out, Stderr: a.Err,
	})
	if follow.Err() != nil {
		fmt.Fprintf(a.Err, "box: job %s is still running; reattach with: box job %s\n", job, job)
		return exitDetached, nil
	}
	if err != nil {
		return 1, err
	}
	if res.Code == 3 {
		return 1, fmt.Errorf("no job %s", job)
	}
	if res.Code != 0 {
		if _, paused := a.check(ctx, box); paused != "" {
			fmt.Fprintf(a.Err, "box: %s; run it locally\n", paused)
			return exitPaused, nil
		}
		return 1, fmt.Errorf("lost job %s (reattach with: box job %s)", job, job)
	}
	out, err := a.incusOK(ctx, "exec", a.target(box), "--", "cat", f+".exit")
	if err != nil {
		return 1, fmt.Errorf("job %s ended without an exit code: %v", job, err)
	}
	code, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 1, fmt.Errorf("job %s: bad exit code %q", job, out)
	}
	return code, nil
}

// startScript checks the bundle out in /work/<repo> and starts cmd detached,
// logging to /work/.box/<job>.log and writing its exit code to <job>.exit.
// Jobs on the same repo in the same box take turns (flock), since they share
// the checkout.
func startScript(job, repo string, cmd []string) string {
	w := workRoot
	f := w + "/.box/" + job
	inner := fmt.Sprintf(`exec 9>%[1]s/.box/%[3]s.lock
flock 9
if git init -q %[1]s/%[3]s && cd %[1]s/%[3]s &&
   git fetch -q %[2]s.bundle refs/agent-box/%[4]s &&
   git checkout -q --force --detach FETCH_HEAD && git clean -fdq; then
  (
%[5]s
  )
  code=$?
else
  echo "box: checkout failed" >&2
  code=1
fi
rm -f %[2]s.bundle
echo $code > %[2]s.exit.tmp && mv %[2]s.exit.tmp %[2]s.exit
`, w, f, repo, job, shellCommand(cmd))
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
