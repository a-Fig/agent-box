package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/a-Fig/agent-box/internal/jobrun"
	"github.com/a-Fig/agent-box/internal/mode"
)

// exitPaused tells callers the box or the PC is paused (or out of reach),
// so they should run the command locally instead. 75 is EX_TEMPFAIL.
const exitPaused = 75

// exitDetached is returned when box stops following a job that keeps running.
const exitDetached = 130

const (
	idleTTL    = 2 * time.Hour
	jobTTL     = 24 * time.Hour
	expiresKey = "user.expires"
)

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
	Warn    func(string)    // optional destination for warnings instead of Err
	Notify  func(jobNotice) // optional destination for job and launch notices
	Now     func() time.Time
	Rand    func() string // short random suffix for names and job IDs
	// PausePoll is how often attach checks whether the box was frozen; zero
	// is pausePoll.
	PausePoll time.Duration
	// PollTimeout bounds each of those checks (zero is pollTimeout), and
	// ProbeTimeout bounds each probe after a job's result is known (zero is
	// probeTimeout), so a freeze there can't hold the result back.
	PollTimeout, ProbeTimeout time.Duration
	// JoinConfig says where box join registers and installs; tests replace it.
	JoinConfig func(out io.Writer) (joinConfig, error)
	// Client is the MCP client box mcp serves (empty for the CLI), and
	// Thread a T3 thread a tool call named in _meta; both go in a job's .meta.
	Client clientInfo
	Thread string
	// Getenv reads the agent's environment (nil is os.Getenv).
	Getenv func(string) string
	// Post sends a refusal to boxd; nil sends none. Errors are ignored.
	Post func(ctx context.Context, url string, body []byte) error
	// Browse opens an address in the person's browser; tests replace it.
	Browse func(url string) error
	// HTTP sends requests to boxd for the queue (queue.go); nil is
	// http.DefaultClient. Tests replace it.
	HTTP func(*http.Request) (*http.Response, error)
	// Pushing a job's result to the agent's T3 thread (push.go). Push sends
	// into a thread, Spawn starts a detached `box watch`, StateDir holds the
	// per-job files. Any of them nil or empty means no push.
	Push     Pusher
	Spawn    func(stateDir, job string) error
	StateDir string
	// InlineWait is how long box_run follows a job (zero is inlineWait);
	// WatchPoll replaces the watcher's waits, for tests.
	InlineWait time.Duration
	WatchPoll  time.Duration
	// KeepEnded, when set, keeps a throwaway box for this long after its
	// job ended (its user.expires moves to now plus this, and the reaper
	// deletes it) instead of deleting it, so box_pull and the full log still
	// work after the result was pushed.
	KeepEnded time.Duration
	// MCPExe, when set, is the box.exe that box mcp hands each tools/list and tools/call to,
	// so a long-lived server runs the newest code (mcpexe.go). Empty answers in process.
	MCPExe string
}

func newApp() *App {
	a := &App{
		R:       execRunner{},
		Remote:  env("BOX_REMOTE", "pc"),
		Project: env("BOX_PROJECT", "agents"),
		Image:   env("BOX_IMAGE", "agent-base"),
		Out:     os.Stdout,
		Err:     os.Stderr,
		Now:     time.Now,
		Rand:    randHex,

		JoinConfig: defaultJoinConfig,
		Post:       postJSON,
		Browse:     browse,
		Spawn:      spawnWatcher,
		StateDir:   defaultStateDir(),
	}
	if p := findT3(os.Getenv); p != "" {
		a.Push = t3CLI{path: p}
	}
	return a
}

func (a *App) warn(message string) {
	if a.Warn != nil {
		a.Warn(message)
		return
	}
	fmt.Fprintln(a.Err, message)
}

type jobNotice struct {
	Event string `json:"event"`
	Box   string `json:"box"`
	Job   string `json:"job,omitempty"`
}

func (a *App) notify(notice jobNotice) {
	if a.Notify != nil {
		a.Notify(notice)
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
	Name           string            `json:"name"`
	Status         string            `json:"status"` // Running, Stopped, Frozen, ...
	Config         map[string]string `json:"config"`
	ExpandedConfig map[string]string `json:"expanded_config"`
	CreatedAt      time.Time         `json:"created_at"`
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

// New launches a box of the given size ("" is medium) with a TTL. An empty
// name gets a random one. A reason, if given, is stored as user.box.reason.
func (a *App) New(ctx context.Context, name string, ttl time.Duration, size, reason string) (string, error) {
	sz, err := lookupSize(size)
	if err != nil {
		return "", err
	}
	if name == "" {
		name = "box-" + a.Rand()
	}
	if sz.GPU {
		insts, err := a.list(ctx)
		if err != nil {
			return "", err
		}
		if err := a.claimGPU(ctx, insts, ""); err != nil {
			return "", err
		}
	}
	var extra []string
	if reason != "" {
		extra = []string{"-c", reasonKey + "=" + reason}
	}
	if err := a.launch(ctx, name, ttl, false, sz, extra...); err != nil {
		return name, err
	}
	return name, a.confirmGPU(ctx, name, sz)
}

// lookupSize resolves a --size value; "" is the default size.
func lookupSize(name string) (mode.Size, error) {
	if name == "" {
		name = mode.DefaultSize
	}
	sz, ok := mode.LookupSize(name)
	if !ok {
		return sz, fmt.Errorf("unknown size %q: use %s", name, mode.SizeNames())
	}
	return sz, nil
}

// errNoRoom is a launch that Incus refused for the project's limits. Callers
// treat it like a paused PC: run locally (exit 75).
type errNoRoom struct {
	size   string
	reason string
}

func (e *errNoRoom) Error() string {
	return fmt.Sprintf("no room on the PC for a %s box (%s); run it locally, or ask for a smaller size", e.size, e.reason)
}

func isNoRoom(err error) bool {
	var e *errNoRoom
	return errors.As(err, &e)
}

// errGPUBusy is a GPU box asked for while another one holds the GPU.
// Callers exit 75, like a paused PC.
type errGPUBusy struct{ holder string }

func (e *errGPUBusy) Error() string {
	return fmt.Sprintf("the GPU box is in use (%s); run on CPU or wait", e.holder)
}

// isTempFail says whether err means "not now": no room, or the GPU is taken.
func isTempFail(err error) bool {
	var g *errGPUBusy
	return isNoRoom(err) || errors.As(err, &g)
}

// gpuHolder says what holds the GPU through box in: "job X" for its running
// jobs, or "box X" when none runs (a box new --gpu box, or a paused one).
// idle is true only for a running box that was checked and runs no job.
func (a *App) gpuHolder(ctx context.Context, in instance) (holder string, idle bool) {
	if in.Status != "Running" {
		return "box " + in.Name + ", " + strings.ToLower(in.Status), false
	}
	out, err := a.incusOK(ctx, "exec", a.target(in.Name), "--", "bash", "-c", runningScript())
	if jobs := strings.Fields(out); err == nil && len(jobs) > 0 {
		return "job " + strings.Join(jobs, ", job "), false
	}
	return "box " + in.Name, err == nil
}

// gpuStartGrace is how long a new GPU box counts as starting up. Between
// launch and its job's PID file, a box runs no job (and Incus creates it
// stopped before starting it), so until then no other run may take it for
// dead and delete it.
const gpuStartGrace = 15 * time.Minute

// starting says whether in is younger than gpuStartGrace.
func (a *App) starting(in instance) bool {
	return !in.CreatedAt.IsZero() && a.Now().Sub(in.CreatedAt) < gpuStartGrace
}

// beingCreated says whether stopped GPU box in may be one a run is launching
// right now: young, and not marked by the tray. It holds the GPU.
func (a *App) beingCreated(in instance) bool {
	return in.Status == "Stopped" && in.Config[mode.StoppedKey] == "" && a.starting(in)
}

// claimGPU checks that no box but except holds the GPU, so only one GPU box
// runs at a time. Throwaway GPU boxes that are done are deleted on the way,
// since they hold 8GiB of the project's memory and would block the next one:
// stopped ones the tray marked (reclaimable) or that are past startup, and
// running ones past startup that run no job. Otherwise a running, paused or
// starting GPU box makes it return errGPUBusy.
func (a *App) claimGPU(ctx context.Context, insts []instance, except string) error {
	for _, in := range insts {
		if in.Name == except || !mode.IsGPU(in.Config) {
			continue
		}
		throwaway := in.Config[throwawayKey] == "true"
		if in.Status == "Stopped" {
			switch {
			case a.beingCreated(in):
				return &errGPUBusy{holder: "box " + in.Name + ", starting"}
			case throwaway:
				a.removeQuietly(ctx, in.Name)
			}
			continue
		}
		holder, idle := a.gpuHolder(ctx, in)
		if idle && throwaway && !a.starting(in) {
			a.removeQuietly(ctx, in.Name)
			continue
		}
		return &errGPUBusy{holder: holder}
	}
	return nil
}

// confirmGPU closes the race between two launches that both passed
// claimGPU: once GPU box name exists, if another GPU box is running or
// paused, name is deleted and the caller backs off with errGPUBusy. Both may
// back off, but the GPU never has two boxes.
func (a *App) confirmGPU(ctx context.Context, name string, sz mode.Size) error {
	if !sz.GPU {
		return nil
	}
	insts, err := a.list(ctx)
	if err != nil {
		return nil // the launch worked; the next claim sees both boxes
	}
	if find(insts, name) == nil { // another run took ours for dead
		return &errGPUBusy{holder: "a box that replaced " + name}
	}
	for _, in := range insts {
		if in.Name == name || !mode.IsGPU(in.Config) {
			continue
		}
		// A stopped box still being created counts: it is about to run.
		if in.Status != "Stopped" || a.beingCreated(in) {
			a.removeQuietly(ctx, name)
			return &errGPUBusy{holder: "box " + in.Name}
		}
	}
	return nil
}

// gpuStopped is what to tell an agent whose job's GPU box isn't running
// (the tray stops GPU boxes for games and My PC), or "" when box isn't a GPU
// box or is running.
func (a *App) gpuStopped(ctx context.Context, box string) string {
	insts, err := a.list(ctx)
	if err != nil {
		return ""
	}
	in := find(insts, box)
	if in == nil || !mode.IsGPU(in.Config) || in.Status == "Running" {
		return ""
	}
	why := in.Config[mode.StoppedKey]
	switch {
	case strings.HasPrefix(why, "gaming"):
		return "the GPU box was stopped because the PC's owner started a game; rerun later or on CPU"
	case why == string(mode.MyPC):
		return "the GPU box was stopped because the PC's owner switched it to My PC; rerun later or on CPU"
	}
	return "the GPU box " + box + " is " + strings.ToLower(in.Status) + "; rerun later or on CPU"
}

type existingBoxSize string

func (s existingBoxSize) Error() string { return "existing box has size " + string(s) }

func reusableBox(in *instance, requestedSize string) error {
	if in.Config[throwawayKey] == "true" {
		return fmt.Errorf("box %s is a throwaway and cannot be reused", in.Name)
	}
	if have := mode.SizeOf(in.Config).Name; requestedSize != "" && have != requestedSize {
		return existingBoxSize(have)
	}
	return nil
}

// projectLimitRefusal matches Incus's refusals for project limits, like
// `Reached maximum aggregate value "12GiB" for "limits.memory" in project
// "agents"` or `Reached maximum number of instances in project "agents"`.
var projectLimitRefusal = jobrun.LimitRefusal

// reclaimable says whether in is a throwaway GPU box the tray stopped for a
// game or My PC. It is kept for GPUStoppedTTL so box job can say why its job
// died, but it still holds a GPU box's memory and disk against the project's
// caps, so any launch that would not fit deletes it first.
// Only the tray's mark counts: Incus creates a box stopped before starting
// it, so an unmarked stopped box may be one a run is launching.
func reclaimable(in instance) bool {
	return in.Status == "Stopped" && mode.IsGPU(in.Config) && in.Config[throwawayKey] == "true" && in.Config[mode.StoppedKey] != ""
}

// reclaim deletes every reclaimable box and says whether there was one.
func (a *App) reclaim(ctx context.Context) bool {
	insts, err := a.list(ctx)
	if err != nil {
		return false
	}
	any := false
	for _, in := range insts {
		if reclaimable(in) {
			a.removeQuietly(ctx, in.Name)
			any = true
		}
	}
	return any
}

// keptScript prints "kept" when a box runs no job and a job has ended there.
func keptScript() string {
	return "# kept jobs\n" + aliveFn + fmt.Sprintf(`cd %s/.box 2>/dev/null || exit 0
ended=
for p in *.pid; do
  [ -e "$p" ] || continue
  j=${p%%.pid}
  if [ -e "$j.exit" ]; then ended=1; continue; fi
  alive "$(cat "$p" 2>/dev/null)" "$j" && exit 0
done
[ -n "$ended" ] && echo kept
exit 0
`, workRoot)
}

// keptThrowaways lists the throwaway boxes kept after their job ended
// (endedKeep: box run and boxd move a throwaway's expiry to then, for box
// pull and the full log), the first to expire first. They hold their room
// until the reaper deletes them, so a launch short of room deletes them, as
// claimGPU deletes an idle GPU throwaway and boxd's queue does the same. A
// kept box is a running throwaway whose expiry is within the keep window,
// with no job running and one that ended; a throwaway still being set up
// for its job has none ended.
func (a *App) keptThrowaways(ctx context.Context, insts []instance) []instance {
	var out []instance
	for _, in := range insts {
		exp, err := time.Parse(time.RFC3339, in.Config[expiresKey])
		if in.Config[throwawayKey] != "true" || in.Status != "Running" || err != nil ||
			exp.After(a.Now().Add(endedKeep+time.Minute)) {
			continue
		}
		look, cancel := a.probeCtx(ctx) // a box frozen since the list would hold the exec
		res, err := a.incus(look, Cmd{Args: []string{"exec", a.target(in.Name), "--", "bash", "-c", keptScript()}})
		cancel()
		if err == nil && res.Code == 0 && strings.TrimSpace(res.Stdout) == "kept" {
			out = append(out, in)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Config[expiresKey] < out[j].Config[expiresKey] })
	return out
}

// launch creates box name. Refused for the project's caps, it deletes
// reclaimable boxes and tries once more, then deletes kept throwaways one at
// a time, trying again after each.
// extra are more launch flags, like box new's reason.
func (a *App) launch(ctx context.Context, name string, ttl time.Duration, throwaway bool, sz mode.Size, extra ...string) error {
	err := a.launchOnce(ctx, name, ttl, throwaway, sz, extra)
	if isNoRoom(err) && a.reclaim(ctx) {
		err = a.launchOnce(ctx, name, ttl, throwaway, sz, extra)
	}
	if isNoRoom(err) {
		if insts, lerr := a.list(ctx); lerr == nil {
			for _, in := range a.keptThrowaways(ctx, insts) {
				fmt.Fprintf(a.Err, "box: deleting %s, kept after its job, for room\n", in.Name)
				a.removeQuietly(ctx, in.Name)
				if err = a.launchOnce(ctx, name, ttl, throwaway, sz, extra); !isNoRoom(err) {
					break
				}
			}
		}
	}
	return err
}

func (a *App) launchOnce(ctx context.Context, name string, ttl time.Duration, throwaway bool, sz mode.Size, extra []string) error {
	if !validBox.MatchString(name) {
		return fmt.Errorf("bad box name %q: use lowercase letters, digits and hyphens", name)
	}
	if ttl <= 0 {
		return errors.New("box TTL must be positive")
	}
	// Unqualified, the image would be looked up on the local (laptop) remote.
	flags, devices := jobrun.LaunchFlags(sz, a.Now().Add(ttl), throwaway, extra...)
	args := append([]string{"launch", a.target(a.Image), a.target(name)}, flags...)
	var stdin io.Reader
	if devices != "" {
		stdin = strings.NewReader(devices)
	}
	res, err := a.incus(ctx, Cmd{Args: args, Stdin: stdin})
	if err == nil && res.Code != 0 {
		err = fmt.Errorf("incus launch: %s", strings.TrimSpace(res.Stderr))
	}
	if err != nil && projectLimitRefusal.MatchString(err.Error()) {
		return &errNoRoom{size: sz.Name, reason: firstLine(strings.TrimPrefix(err.Error(), "incus launch: "))}
	}
	return err
}

func (a *App) expiryConfig(ttl time.Duration) string {
	return expiresKey + "=" + a.Now().Add(ttl).UTC().Format(time.RFC3339)
}

// It retries a lost write race (jobrun.RetryETag): the tray sets a box's CPU
// limits whenever they differ from the mode's, within a tick of a launch in
// Full, just when box sets the job's expiry, or on any mode change.
func (a *App) setExpiry(ctx context.Context, box string, ttl time.Duration) error {
	return jobrun.RetryETag(ctx, func() error {
		_, err := a.incusOK(ctx, "config", "set", a.target(box), a.expiryConfig(ttl))
		return err
	})
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
		exp := in.Config[expiresKey]
		if exp == "" {
			exp = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", in.Name, in.Status, exp)
	}
	return w.Flush()
}

// RunOpts says what box run does.
type RunOpts struct {
	Dir string   // local git worktree
	Cmd []string // one shell string, or argv to quote
	Box string   // box to use; "" is the repo's own box
	New bool     // use a throwaway box, deleted afterwards
	// Size of a box launched for this run: "" is medium for a new box and
	// any size for an existing one.
	Size string
	TTL  time.Duration
	Wait time.Duration     // stop following after this long; 0 waits for the end
	Env  map[string]string // passed to the command; no secrets (see checkEnv)
	// Timeout makes the box kill the command after this long (exit 124).
	Timeout time.Duration
	// Reason is the agent's why, for the .meta and refusals. The CLI and MCP
	// require it; Run doesn't check it.
	Reason string
	// Queue hands a run that can't start now to boxd's queue instead of
	// refusing it (queue.go). box_run sets it when the result can be pushed.
	Queue bool
}

// exitTimeout is the exit code of a command the box killed at its timeout,
// as coreutils timeout reports it.
const exitTimeout = 124

var (
	validEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	secretName   = regexp.MustCompile(`TOKEN|SECRET|KEY|PASSWORD|PASSWD|CREDENTIAL`)
)

// checkEnv refuses variable names that aren't names or that look like
// secrets: boxes are shared, and the values show up in the box's process
// list.
func checkEnv(env map[string]string) error {
	for k, value := range env {
		if !validEnvName.MatchString(k) {
			return fmt.Errorf("bad env name %q", k)
		}
		if secretName.MatchString(strings.ToUpper(k)) {
			return fmt.Errorf("env %s looks like a secret; boxes are shared, so box won't pass it. Use a non-secret value or run locally", k)
		}
		if strings.ContainsRune(value, 0) {
			return fmt.Errorf("env %s contains NUL", k)
		}
	}
	return nil
}

// What following a job ended with.
const (
	stateDone     = "done"     // the job's exit code was read
	stateDetached = "detached" // stopped following; the job keeps running
	statePaused   = "paused"   // the PC or the box is paused or out of reach
)

// States the MCP tools report instead of detached and a paused refusal.
const (
	stateRunning = "running"
	stateRefused = "refused"
)

// JobResult is what Run and Job report. Code is the job's own exit code when
// State is done; otherwise it is exitDetached, exitPaused, or 1 or 2 with an
// error.
type JobResult struct {
	Job   string `json:"job,omitempty"`
	Box   string `json:"box,omitempty"`
	State string `json:"state,omitempty"`
	Code  int    `json:"exitCode"`
	// Push is set on a box_run that returned a running or queued job:
	// whether its result will arrive as a turn in the agent's T3 thread.
	Push *bool `json:"push,omitempty"`
	// A queued job's place in line, from 1, and what it waits for.
	Position int    `json:"position,omitempty"`
	Why      string `json:"why,omitempty"`

	throwaway bool // a queued job's box is a throwaway, kept after the job
}

// Run syncs the worktree to a box, runs the command there and follows its
// output.
func (a *App) Run(ctx context.Context, o RunOpts) (JobResult, error) {
	if len(o.Cmd) == 0 {
		return JobResult{Code: 2}, errors.New("no command given")
	}
	if o.New && o.Box != "" {
		return JobResult{Code: 2}, errors.New("--box and --new don't mix")
	}
	if o.Wait < 0 {
		return JobResult{Code: 2}, errors.New("wait can't be negative")
	}
	if err := checkEnv(o.Env); err != nil {
		return JobResult{Code: 2}, err
	}
	sz, err := lookupSize(o.Size)
	if err != nil {
		return JobResult{Code: 2}, err
	}
	if o.Timeout < 0 {
		return JobResult{Code: 2}, errors.New("timeout must be positive")
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
	info := a.runInfo(ctx, rp, o, sz)
	var id, bundle string
	// qr is the run as the queue takes it, from where Run has got to. A run
	// turned away by a pause gets a throwaway: the checks below on the repo's
	// box (its size, that it isn't a throwaway) haven't run, and the tray may
	// stop a frozen box, which the queue doesn't start again.
	qr := func(paused bool) queueRun {
		q := queueRun{o: o, rp: rp, box: box, new: o.New, sz: sz, info: info, bundle: bundle, ref: id}
		if o.New || sz.GPU || paused {
			q.box, q.new = rp.Name, true
		}
		return q
	}

	insts, paused := a.reach(ctx)
	var inst *instance
	if sz.GPU && paused == "" {
		// GPU boxes are throwaways, so an idle box never holds the GPU. The
		// one exception is a GPU box someone made with box new --gpu.
		if o.Box == "" {
			o.New = true
		} else if inst = find(insts, box); inst == nil || !mode.IsGPU(inst.Config) {
			return JobResult{Box: box, Code: 2}, fmt.Errorf("--box %s isn't a GPU box; drop --box to run in a throwaway GPU box", box)
		}
		except := ""
		if !o.New {
			except = box
		}
		if err := a.claimGPU(ctx, insts, except); err != nil {
			return a.turnAway(ctx, qr(false), err.Error(), errCause(err)), nil
		}
	}
	if paused == "" && !o.New {
		if inst = find(insts, box); inst != nil {
			switch err := reusableBox(inst, o.Size); have := err.(type) {
			case existingBoxSize:
				fmt.Fprintf(a.Err, "box: %s is %s, so this run uses a throwaway %s box\n", box, string(have), sz.Name)
				o.New = true
			case nil:
				if inst.Status == "Frozen" {
					paused = box + " is paused"
				}
			default:
				return JobResult{Box: box, Code: 1}, err
			}
		}
	}
	if paused != "" {
		return a.turnAway(ctx, qr(true), paused+"; run it locally", a.pausedCause(paused)), nil
	}

	id = a.Now().UTC().Format("20060102-150405") + "-" + a.Rand()
	bundle, err = snapshot(ctx, a.R, rp.Top, id)
	if err != nil {
		return JobResult{Code: 1}, err
	}
	defer os.RemoveAll(filepath.Dir(bundle))

	started := false // from here on the remote job may be running
	if !o.New {
		paused, err := a.ensureBox(ctx, box, inst, sz, o.Size)
		var have existingBoxSize
		switch {
		case errors.As(err, &have):
			fmt.Fprintf(a.Err, "box: %s is %s, so this run uses a throwaway %s box\n", box, string(have), sz.Name)
			o.New = true
		case isTempFail(err):
			return a.turnAway(ctx, qr(false), err.Error(), errCause(err)), nil
		case err != nil:
			return JobResult{Code: 1}, err
		case paused != "":
			return a.turnAway(ctx, qr(true), paused+"; run it locally", a.pausedCause(paused)), nil
		}
	}
	if o.New {
		ttl := o.TTL
		if ttl == 0 {
			ttl = idleTTL
		}
		if box, err = a.launchThrowaway(ctx, rp.Name, insts, ttl, sz); err != nil {
			if isTempFail(err) {
				return a.turnAway(ctx, qr(false), err.Error(), errCause(err)), nil
			}
			return JobResult{Box: box, Code: 1}, err
		}
		// Until its job starts, a failed run takes its own box with it.
		defer func() {
			if !started {
				a.removeQuietly(ctx, box)
			}
		}()
	}

	job := box + "." + id
	r := JobResult{Job: job, Box: box, Code: 1}
	remote := workRoot + "/.box/" + job
	if _, err := a.incusOK(ctx, "file", "push", "--create-dirs", bundle, a.target(box)+remote+".bundle"); err != nil {
		return r, err
	}
	if err := a.pushMeta(ctx, box, remote, filepath.Dir(bundle), info); err != nil {
		a.warn(fmt.Sprintf("box: warning: recording %s.meta: %v", job, err))
	}
	if err := a.setExpiry(ctx, box, jobTTL); err != nil {
		return r, fmt.Errorf("set job expiry: %w", err)
	}
	// Say the job ID before starting it, so a dropped connection can't lose it.
	fmt.Fprintf(a.Err, "box: job %s on %s\n", job, box)
	a.notify(jobNotice{Event: "starting_job", Box: box, Job: job})
	if err := ctx.Err(); err != nil {
		return r, err
	}
	started = true
	if _, err := a.incusOK(ctx, "exec", a.target(box), "--", "bash", "-lc", startScript(job, id, rp.Name, payload(o))); err != nil {
		// The tray stops GPU boxes for games, and another run may then
		// delete one. Only then does the job surely not run: a frozen box's
		// job may resume, so it keeps its job ID below.
		if sz.GPU {
			if insts, lerr := a.list(ctx); lerr == nil {
				if in := find(insts, box); in == nil || in.Status == "Stopped" {
					fmt.Fprintf(a.Err, "box: the GPU box was stopped (the PC's owner needed it); rerun later or on CPU\n")
					r.State, r.Code = statePaused, exitPaused
					return r, nil
				}
			}
		}
		if o.New {
			fmt.Fprintf(a.Err, "box: keeping %s; box job %s removes it once the job has ended\n", box, job)
		}
		return r, err
	}
	// Again now that the job runs: another job finishing in between may have
	// shortened the expiry before this one was visible to its running check.
	if err := a.setExpiry(context.WithoutCancel(ctx), box, jobTTL); err != nil {
		a.warn(fmt.Sprintf("box: warning: setting job expiry on %s: %v", box, err))
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
func (a *App) launchThrowaway(ctx context.Context, base string, insts []instance, ttl time.Duration, sz mode.Size) (string, error) {
	for try := 0; try < 8; try++ {
		name := base + "-" + a.Rand()
		if find(insts, name) != nil {
			continue
		}
		fmt.Fprintf(a.Err, "box: launching %s\n", name)
		a.notify(jobNotice{Event: "launching_box", Box: name})
		err := a.launch(ctx, name, ttl, true, sz)
		if err == nil {
			return name, a.confirmGPU(ctx, name, sz)
		}
		if !isConflict(err) {
			return name, err
		}
		insts = append(insts, instance{Name: name})
	}
	return "", errors.New("no free name for a throwaway box")
}

// ensureBox launches or starts a reused box. When a concurrent run launches
// or starts it first, it carries on with that one.
func (a *App) ensureBox(ctx context.Context, box string, inst *instance, sz mode.Size, requestedSize string) (paused string, err error) {
	if inst != nil && inst.Status == "Running" {
		return "", nil
	}
	if inst == nil {
		fmt.Fprintf(a.Err, "box: launching %s\n", box)
		err = a.launch(ctx, box, idleTTL, false, sz)
	} else {
		_, err = a.incusOK(ctx, "start", a.target(box))
	}
	if err == nil {
		return "", nil
	}
	now, paused := a.check(ctx, box)
	if paused != "" {
		return paused, nil
	}
	if now == nil {
		return "", err
	}
	if err := reusableBox(now, requestedSize); err != nil {
		return "", err
	}
	switch {
	case now.Status == "Running":
		return "", nil
	case inst == nil: // someone else launched it but it isn't running yet
		if _, err := a.incusOK(ctx, "start", a.target(box)); err != nil {
			if now, paused := a.check(ctx, box); paused != "" {
				return paused, nil
			} else if now != nil && now.Status == "Running" {
				return "", reusableBox(now, requestedSize)
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
		a.warn(fmt.Sprintf("box: warning: removing %s: %v", box, err))
	}
}

// cleanup deletes a throwaway box once no job is running in it. ours says
// the caller created it; otherwise the box's throwaway mark decides.
func (a *App) cleanup(ctx context.Context, box string, ours bool) {
	ctx, cancel := a.probeCtx(ctx)
	defer cancel()
	if !ours {
		v, err := a.incusOK(ctx, "config", "get", a.target(box), throwawayKey)
		if err != nil || strings.TrimSpace(v) != "true" {
			return
		}
	}
	out, err := a.incusOK(ctx, "exec", a.target(box), "--", "bash", "-c", runningScript())
	if err != nil {
		a.warn(fmt.Sprintf("box: warning: keeping %s: %v", box, err))
		return
	}
	if running := strings.Fields(out); len(running) > 0 {
		fmt.Fprintf(a.Err, "box: keeping %s: still running %s\n", box, strings.Join(running, " "))
		return
	}
	if a.KeepEnded > 0 {
		if err := a.setExpiry(ctx, box, a.KeepEnded); err != nil {
			a.warn(fmt.Sprintf("box: warning: setting expiry on %s: %v", box, err))
		}
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
		a.warn("box: warning: submodules aren't synced and will be empty in the box: " + strings.Join(paths, " "))
	}
}

var validJob = regexp.MustCompile(`^[a-z][a-z0-9-]*\.[0-9]{8}-[0-9]{6}-[0-9a-f]+$`)

// Job reattaches to a job started by Run. Once the job has ended, it deletes
// the job's box if box run --new made it and no other job runs there.
func (a *App) Job(ctx context.Context, job string, wait time.Duration) (JobResult, error) {
	if !validJob.MatchString(job) {
		return JobResult{Code: 2}, fmt.Errorf("bad job id %q", job)
	}
	if wait < 0 {
		return JobResult{Code: 2}, errors.New("wait can't be negative")
	}
	box, _, _ := strings.Cut(job, ".")
	r := JobResult{Job: job, Box: box}
	var err error
	r.State, r.Code, err = a.attach(ctx, job, wait)
	if err != nil {
		// A job whose exit was read once has ended: when its box has gone
		// since (a throwaway's 30 minutes ran out, or a launch short of room
		// took it), its recorded end is the result, not "lost".
		if end, ok := a.recordedEnd(job); ok {
			fmt.Fprintf(a.Err, "box: can't read %s from its box (%s); this is the result recorded when it ended. If the box is gone, so are its full log and files.\n", job, firstLine(err.Error()))
			fmt.Fprint(a.Out, end.Tail)
			return JobResult{Job: job, Box: box, State: stateDone, Code: end.Code}, nil
		}
	}
	if err == nil && r.State == stateDone {
		a.cleanup(ctx, box, false)
	}
	return r, err
}

const (
	aliveFn  = jobrun.AliveFn
	jobPIDFn = jobrun.JobPIDFn
)

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
	return "# follow\n" + aliveFn + jobPIDFn + fmt.Sprintf(`test -f %[1]s.log || { echo "box: no job %[2]s" >&2; exit 3; }
pid=$(jobpid %[1]s)
off=0
while :; do
  fin=0; [ -f %[1]s.exit ] && fin=1
  size=$(( $(wc -c < %[1]s.log) ))
  if [ "$size" -gt "$off" ]; then
    tail -c +$((off + 1)) %[1]s.log | head -c $((size - off))
    off=$size
  fi
  if [ $fin = 1 ]; then
    printf '%%s%%s\n' %[3]s "$(cat %[1]s.exit)" >&2
    exit 0
  fi
  if ! alive "$pid" %[2]s; then
    [ -f %[1]s.exit ] && continue
    echo "box: job %[2]s died without an exit code" >&2
    exit 4
  fi
  sleep 0.5
done
`, f, job, shq(exitMarker))
}

// pausePoll is how often attach looks for a box frozen mid-run, pollTimeout
// how long one look may take, and probeTimeout how long a probe after the
// job's result is known may take.
const (
	pausePoll    = 5 * time.Second
	pollTimeout  = 3 * time.Second
	probeTimeout = 10 * time.Second
)

// exitMarker starts the line followScript prints on stderr when the job has
// ended, followed by its exit code. attach reads it from that exec, so it
// needs no second exec that a freeze could cut off.
const exitMarker = "\x01box-exit "

// lockedWriter writes to w under mu.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// exitFilter passes writes through except lines that start with exitMarker,
// which it keeps.
type exitFilter struct {
	w    io.Writer
	mu   sync.Mutex
	buf  []byte
	code string
	seen bool
}

func (e *exitFilter) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.buf = append(e.buf, p...)
	for {
		i := bytes.IndexByte(e.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := e.buf[:i+1]
		if rest, ok := bytes.CutPrefix(line, []byte(exitMarker)); ok {
			e.code, e.seen = strings.TrimSpace(string(rest)), true
		} else if _, err := e.w.Write(line); err != nil {
			return len(p), err
		}
		e.buf = e.buf[i+1:]
	}
}

// flush writes a trailing partial line.
func (e *exitFilter) flush() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.buf) > 0 {
		e.w.Write(e.buf)
		e.buf = nil
	}
}

func (e *exitFilter) exit() (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.code, e.seen
}

// pausedMidRun says what is true of a job whose box was frozen: it resumes
// only if the PC's owner switches back before the tray stops the box.
func pausedMidRun(job string) string {
	return fmt.Sprintf("box: job %s is paused (the PC's owner is using it). It resumes if they switch back within %s; after that the box is stopped and the job lost. Reattach later with: box job %s, or run it locally", job, frozenWindow, job)
}

// frozenWindow is how long a paused job can still resume, in words.
var frozenWindow = fmt.Sprintf("%d minutes (%d hours once its memory is moved to swap)",
	int(mode.FrozenStop.Minutes()), int(mode.FrozenSwappedStop.Hours()))

// pausedGPU is the same for a GPU box, which the tray normally stops, not freezes.
func pausedGPU(job string) string {
	return fmt.Sprintf("box: job %s is paused (the PC's owner is using the GPU box). The tray may still stop it, which loses the job. Reattach later with: box job %s, or rerun later or on CPU", job, job)
}

// watchPause polls until ctx ends. When a fresh look finds the box Frozen it
// records that (and whether it is a GPU box) and calls stop. A frozen box
// freezes the follow process too, so nothing else would notice. My PC mode
// alone proves nothing: the tray writes it before it freezes, and a freeze
// can fail. A look that errors or stalls past PollTimeout waits for the next
// tick.
func (a *App) watchPause(ctx context.Context, box string, stop context.CancelFunc, hit, gpu *atomic.Bool) {
	every := a.PausePoll
	if every <= 0 {
		every = pausePoll
	}
	limit := a.PollTimeout
	if limit <= 0 {
		limit = pollTimeout
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		look, cancel := context.WithTimeout(ctx, limit)
		insts, err := a.list(look)
		cancel()
		if err != nil {
			continue
		}
		in := find(insts, box)
		if in == nil {
			stop() // the box was deleted: the follow can't end by itself
			return
		}
		if in.Status == "Frozen" {
			gpu.Store(mode.IsGPU(in.Config))
			hit.Store(true)
			stop()
			return
		}
	}
}

// attach streams a job's log until the job exits and returns its exit code.
// If the box is frozen meanwhile it returns statePaused promptly. The
// follow itself reports the job's exit code, and a code it reported wins
// over any cancellation after it.
func (a *App) attach(ctx context.Context, job string, wait time.Duration) (state string, code int, err error) {
	box, _, _ := strings.Cut(job, ".")
	follow := ctx
	if wait > 0 {
		var cancel context.CancelFunc
		follow, cancel = context.WithTimeout(ctx, wait)
		defer cancel()
	}
	run, stop := context.WithCancel(follow)
	defer stop()
	var hit, gpu atomic.Bool
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		a.watchPause(run, box, stop, &hit, &gpu)
	}()
	// Out and Err may be one buffer, and exec now copies each from its own
	// pipe, so serialize the writes.
	var wmu sync.Mutex
	ef := &exitFilter{w: &lockedWriter{&wmu, a.Err}}
	kept := &lineTail{}
	tail := &tailWriter{w: &lockedWriter{&wmu, io.MultiWriter(a.Out, kept)}}
	res, err := a.incus(run, Cmd{
		Args:   []string{"exec", a.target(box), "--", "bash", "-c", followScript(job)},
		Stdout: tail, Stderr: ef,
	})
	stop()
	<-watched
	ef.flush()
	if text, ok := ef.exit(); ok {
		code, cerr := strconv.Atoi(text)
		if cerr != nil {
			return "", 1, fmt.Errorf("job %s: bad exit code %q", job, text)
		}
		ended := a.Now()
		a.markIdle(ctx, box)
		a.reportEnded(ctx, job, code, ended, tail.String())
		a.recordEnd(job, jobEnd{Code: code, Took: int64(ended.Sub(jobStart(job)) / time.Second), Tail: keepTail(kept.lastLines(1 << 20))})
		return stateDone, code, nil
	}
	if hit.Load() && ctx.Err() == nil {
		if gpu.Load() {
			fmt.Fprintln(a.Err, pausedGPU(job))
		} else {
			fmt.Fprintln(a.Err, pausedMidRun(job))
		}
		return statePaused, exitPaused, nil
	}
	if follow.Err() != nil {
		fmt.Fprintf(a.Err, "box: job %s is still running; reattach with: box job %s\n", job, job)
		return stateDetached, exitDetached, nil
	}
	if err != nil {
		return "", 1, err
	}
	// The tray stops a GPU box for games. Its job may look dead (4) or the
	// exec may just end; either way the agent should hear why.
	if res.Code != 0 && res.Code != 3 {
		if msg := a.gpuStopped(ctx, box); msg != "" {
			fmt.Fprintf(a.Err, "box: %s\n", msg)
			return statePaused, exitPaused, nil
		}
	}
	switch res.Code {
	case 0:
		return "", 1, fmt.Errorf("job %s ended without an exit code", job)
	case 3:
		return "", 1, fmt.Errorf("no job %s", job)
	case 4:
		return "", 1, fmt.Errorf("job %s died without an exit code", job)
	default:
		if _, paused := a.check(ctx, box); paused != "" {
			if strings.HasSuffix(paused, " is paused") {
				fmt.Fprintln(a.Err, pausedMidRun(job))
			} else {
				fmt.Fprintf(a.Err, "box: %s; run it locally\n", paused)
			}
			return statePaused, exitPaused, nil
		}
		return "", 1, fmt.Errorf("lost job %s (reattach with: box job %s)", job, job)
	}
}

// probeCtx bounds the probes that follow a known result: a freeze there
// must not hold the result back. It outlives ctx's cancellation.
func (a *App) probeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	d := a.ProbeTimeout
	if d <= 0 {
		d = probeTimeout
	}
	return context.WithTimeout(context.WithoutCancel(ctx), d)
}

// markIdle shortens the box's expiry to idleTTL once a job has ended, unless
// another job is still running there (it would lose its jobTTL window).
func (a *App) markIdle(ctx context.Context, box string) {
	ctx, cancel := a.probeCtx(ctx)
	defer cancel()
	out, err := a.incusOK(ctx, "exec", a.target(box), "--", "bash", "-c", runningScript())
	if err != nil || len(strings.Fields(out)) > 0 {
		return
	}
	if err := a.setExpiry(ctx, box, idleTTL); err != nil {
		a.warn(fmt.Sprintf("box: warning: setting idle expiry on %s: %v", box, err))
		return
	}
	// A job can publish its PID and refresh the deadline after the first
	// scan but before our idle write. Restore the job deadline in that case.
	// A job that starts after this scan refreshes its own deadline.
	out, err = a.incusOK(ctx, "exec", a.target(box), "--", "bash", "-c", runningScript())
	if err != nil {
		a.warn(fmt.Sprintf("box: warning: rechecking active jobs on %s: %v", box, err))
	} else if len(strings.Fields(out)) == 0 {
		return
	}
	if err := a.setExpiry(ctx, box, jobTTL); err != nil {
		a.warn(fmt.Sprintf("box: warning: restoring job expiry on %s: %v", box, err))
	}
}

// payload is the shell code a job runs in its checkout (jobrun.Payload).
func payload(o RunOpts) string { return jobrun.Payload(o.Env, o.Cmd, o.Timeout) }

// startScript checks the bundle out and starts the job (jobrun.StartScript).
func startScript(job, ref, repo, payload string) string {
	return jobrun.StartScript(workRoot, job, ref, repo, payload)
}

func shellCommand(args []string) string { return jobrun.ShellCommand(args) }

func shq(s string) string { return jobrun.Shq(s) }
