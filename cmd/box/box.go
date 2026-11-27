package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/digest"
	"github.com/a-Fig/agent-box/internal/jobrun"
	"github.com/a-Fig/agent-box/internal/mode"
)

// exitPaused means box couldn't run the command now and nothing started: the
// box or the PC is paused, out of reach or full. 75 is EX_TEMPFAIL.
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
	// dashboardURL overrides the derived boxd address for a configured job host.
	dashboardURL string
	jobHostName  string // display name for a qualified job destination
	// boxdDisabled keeps a qualified lookup outside boxd's fixed project on Incus.
	boxdDisabled bool
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
		// The CLI keeps a throwaway as box mcp does, so box pull can fetch
		// a build's output after the run (#78).
		KeepEnded: endedKeep,
		Rand:      randHex,

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
	Host  string `json:"host,omitempty"`
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
		if !a.hasGPU(ctx) {
			return "", errNoGPU{}
		}
		insts, err := a.list(ctx)
		if err != nil {
			return "", err
		}
		if err := a.claimGPU(ctx, insts, ""); err != nil {
			return "", err
		}
	}
	// box new's TTL is the person's own ask, so it holds the box (mode.HoldKey).
	extra := []string{"-c", mode.HoldKey + "=" + a.Now().Add(ttl).UTC().Format(time.RFC3339)}
	if reason != "" {
		extra = append(extra, "-c", reasonKey+"="+reason)
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
// treat it like a paused PC: nothing starts (exit 75).
type errNoRoom struct {
	size, resource, reason string
}

func (e *errNoRoom) Error() string {
	return fmt.Sprintf("no room on the PC for a %s box (%s)", e.size, e.reason)
}

func isNoRoom(err error) bool {
	var e *errNoRoom
	return errors.As(err, &e)
}

// errGPUBusy is a GPU box asked for while another one holds the GPU.
// Callers exit 75, like a paused PC.
type errGPUBusy struct{ holder string }

func (e *errGPUBusy) Error() string {
	return fmt.Sprintf("the GPU box is in use (%s)", e.holder)
}

func invalidGPUBox(box string) error {
	return fmt.Errorf("--box %s isn't a GPU box; drop --box to run in a throwaway GPU box", box)
}

// errNoGPU is a GPU box asked for on a host without one (hasGPU). Callers
// exit 75 as for a refusal, though waiting won't help.
type errNoGPU struct{}

func (errNoGPU) Error() string {
	return "this host has no GPU for boxes"
}

// gpuKey is the project key that tells a GPU host from one without:
// setup-host.sh sets it to allow on WSL, for the GPU's /dev/dxg, and to block
// elsewhere, where a GPU box can't start.
const gpuKey = "restricted.devices.unix-char"

// hasGPU says whether the host can run a GPU box. block says no, and so does
// an unset key on a restricted project, where Incus defaults it to block. An
// unreadable key counts as a GPU host, so a failed read never turns the GPU
// away, and a launch that can't get the devices fails on its own.
func (a *App) hasGPU(ctx context.Context) bool {
	switch a.projectKey(ctx, gpuKey) {
	case "block":
		return false
	case "":
		return !incusTrue(a.projectKey(ctx, "restricted"))
	}
	return true
}

// incusTrue reads an Incus boolean as Incus does (shared/util/boolean.go): true, 1, yes or on,
// in any case.
func incusTrue(v string) bool {
	switch strings.ToLower(v) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// isTempFail says whether err means "not now": no room, the GPU is taken, or
// there is none.
func isTempFail(err error) bool {
	var g *errGPUBusy
	var n errNoGPU
	return isNoRoom(err) || errors.As(err, &g) || errors.As(err, &n)
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

type gpuBoxState int

const (
	gpuBoxFree gpuBoxState = iota
	gpuBoxReclaimable
	gpuBoxBusy
)

// inspectGPUBox classifies one GPU instance using the same read-only rules
// claimGPU applies: unheld throwaways that are stopped past startup (or
// tray-stopped) or running idle past startup are reclaimable; held, starting,
// paused, busy, non-throwaway, or unverified boxes still hold the GPU.
func (a *App) inspectGPUBox(ctx context.Context, in instance) (gpuBoxState, string) {
	throwaway := in.Config[throwawayKey] == "true" && !mode.Held(in.Config, a.Now()).After(a.Now())
	if in.Status == "Stopped" {
		switch {
		case a.beingCreated(in):
			return gpuBoxBusy, "box " + in.Name + ", starting"
		case throwaway:
			return gpuBoxReclaimable, ""
		default:
			return gpuBoxFree, ""
		}
	}
	holder, idle := a.gpuHolder(ctx, in)
	if idle && throwaway && !a.starting(in) {
		return gpuBoxReclaimable, ""
	}
	return gpuBoxBusy, holder
}

// claimGPU checks that no box but except holds the GPU, so only one GPU box
// runs at a time. Throwaway GPU boxes that are done are deleted on the way,
// since they hold 8GiB of the project's memory and would block the next one:
// stopped ones the tray marked (reclaimable) or that are past startup, and
// running ones past startup that run no job. Otherwise a running, paused or
// starting GPU box makes it return errGPUBusy. A throwaway a person extended
// (mode.Held) isn't deleted.
func (a *App) claimGPU(ctx context.Context, insts []instance, except string) error {
	for _, in := range insts {
		if in.Name == except || !mode.IsGPU(in.Config) {
			continue
		}
		switch state, holder := a.inspectGPUBox(ctx, in); state {
		case gpuBoxReclaimable:
			a.removeQuietly(ctx, in.Name)
		case gpuBoxBusy:
			return &errGPUBusy{holder: holder}
		}
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
// (the tray stops GPU boxes in My PC), or "" when box isn't a GPU box or is
// running.
func (a *App) gpuStopped(ctx context.Context, box string) string {
	insts, err := a.list(ctx)
	if err != nil {
		return ""
	}
	in := find(insts, box)
	if in == nil || !mode.IsGPU(in.Config) {
		return ""
	}
	// The tray marks the box just before it stops it, so the mark can come
	// while incus still lists the box Running (#118).
	if in.Config[mode.StoppedKey] == string(mode.MyPC) {
		return "the GPU box was stopped because the PC's owner switched it to My PC; rerun later or on CPU"
	}
	if in.Status == "Running" {
		return ""
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

// reclaimable says whether in is a throwaway GPU box the tray stopped in My
// PC. It is kept for GPUStoppedTTL so box job can say why its job
// died, but it still holds a GPU box's memory and disk against the project's
// caps, so any launch that would not fit deletes it first, unless a person
// extended it (mode.Held at now).
// Only the tray's mark counts: Incus creates a box stopped before starting
// it, so an unmarked stopped box may be one a run is launching.
func reclaimable(in instance, now time.Time) bool {
	return !mode.Held(in.Config, now).After(now) && in.Status == "Stopped" && mode.IsGPU(in.Config) && in.Config[throwawayKey] == "true" && in.Config[mode.StoppedKey] != ""
}

// reclaim deletes every reclaimable box and says whether there was one.
func (a *App) reclaim(ctx context.Context) bool {
	insts, err := a.list(ctx)
	if err != nil {
		return false
	}
	any := false
	for _, in := range insts {
		if reclaimable(in, a.Now()) {
			a.removeQuietly(ctx, in.Name)
			any = true
		}
	}
	return any
}

// keptThrowaways lists the throwaway boxes kept after their job ended
// (endedKeep: box run and boxd move a throwaway's expiry to then, for box
// pull and the full log), the first to expire first. Their ends stay in
// boxd's history, which box job reads once they're gone (#90). They hold their room
// until the reaper deletes them, so a launch short of room deletes them, as
// claimGPU deletes an idle GPU throwaway and boxd's queue does the same. A
// kept box is a throwaway whose expiry is within the keep window and whose
// last job ended. Running boxes must pass a guest check, and stopped CPU
// boxes must carry boxd's finished-stop marker. Holds and claims exclude it.
func (a *App) keptThrowaways(ctx context.Context, insts []instance) []instance {
	var out []instance
	for _, in := range insts {
		now := a.Now()
		if stoppedIdleThrowaway(in, now) || (keptThrowawayConfig(in, now) && a.hasEndedJob(ctx, in.Name)) {
			out = append(out, in)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Config[expiresKey] < out[j].Config[expiresKey] })
	return out
}

func keptThrowawayConfig(in instance, now time.Time) bool {
	return in.Status == "Running" && keptThrowawayWindow(in, now)
}

func stoppedIdleThrowaway(in instance, now time.Time) bool {
	return in.Status == "Stopped" && !mode.IsGPU(in.Config) &&
		in.Config[jobrun.ThrowawayStopKey] == jobrun.ThrowawayStopped && keptThrowawayWindow(in, now)
}

func keptThrowawayWindow(in instance, now time.Time) bool {
	exp, err := time.Parse(time.RFC3339, in.Config[expiresKey])
	return in.Config[throwawayKey] == "true" && err == nil &&
		!exp.After(now.Add(endedKeep+time.Minute)) && !mode.Held(in.Config, now).After(now) &&
		!jobrun.ThrowawayClaimed(in.Config, now)
}

func (a *App) hasEndedJob(ctx context.Context, box string) bool {
	look, cancel := a.probeCtx(ctx)
	defer cancel()
	res, err := a.incus(look, Cmd{Args: []string{"exec", a.target(box), "--", "bash", "-c", jobrun.KeptScript(workRoot)}})
	return err == nil && res.Code == 0 && strings.TrimSpace(res.Stdout) == "kept"
}

// idleRepoBoxes lists the repo boxes (not throwaways) idle since their last
// job, the longest idle first (#187). A repo box is a cache: deleting it costs
// the repo's next run a fresh checkout and a cold build cache, so a launch
// short of room takes idle ones after the kept throwaways. Idle is what
// keptThrowaways checks (keptScript: no job running there, and one ended),
// on a running box whose expiry is still the idle one set when its last job
// ended (idleTTL), so not one whose next job has set its 24h. Never a held
// one (mode.Held): a person's extend, or box new --ttl.
func (a *App) idleRepoBoxes(ctx context.Context, insts []instance) []instance {
	var out []instance
	for _, in := range insts {
		if idleRepoConfig(in, a.Now()) && a.hasEndedJob(ctx, in.Name) {
			out = append(out, in)
		}
	}
	// The expiry is the last job's end plus idleTTL: the earliest is idle longest.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Config[expiresKey] < out[j].Config[expiresKey] })
	return out
}

// idleRepoConfig checks whether the config allows pressure eviction.
func idleRepoConfig(in instance, now time.Time) bool {
	exp, err := time.Parse(time.RFC3339, in.Config[expiresKey])
	_, setup := jobrun.SetupOwner(in.Config)
	return in.Config[throwawayKey] != "true" && in.Status == "Running" && err == nil &&
		!exp.After(now.Add(idleTTL+time.Minute)) && !mode.Held(in.Config, now).After(now) &&
		!setup.After(now) && !jobrun.DeleteReserved(in.Config, now)
}

// stillIdle reads the config after checking the guest, so a setup that
// reserves the box during the probe is visible before the delete.
func (a *App) stillIdle(ctx context.Context, selected instance, lease string) bool {
	throwaway := selected.Config[throwawayKey] == "true"
	if selected.Status != "Stopped" && !a.hasEndedJob(ctx, selected.Name) {
		return false
	}
	insts, err := a.list(ctx)
	if err != nil {
		return false
	}
	for _, in := range insts {
		if in.Name == selected.Name {
			if in.Config["volatile.uuid"] != selected.Config["volatile.uuid"] || !in.CreatedAt.Equal(selected.CreatedAt) {
				return false
			}
			if throwaway {
				if lease != "" {
					if in.Config[jobrun.DirectEvictKey] != lease || in.Config[jobrun.ThrowawayRepoKey] != "" ||
						in.Config[jobrun.ThrowawayStopKey] != "" {
						return false
					}
					delete(in.Config, jobrun.DirectEvictKey) // ignore this request's own reservation
				}
				if selected.Status == "Stopped" {
					return stoppedIdleThrowaway(in, a.Now())
				}
				return keptThrowawayConfig(in, a.Now())
			}
			return idleRepoConfig(in, a.Now())
		}
	}
	return false
}

// spareBoxes is what a launch short of room may delete, in the order it
// deletes them: kept throwaways, then idle repo boxes.
func (a *App) spareBoxes(ctx context.Context, insts []instance) []instance {
	return append(a.keptThrowaways(ctx, insts), a.idleRepoBoxes(ctx, insts)...)
}

// launch creates box name. Refused for the project's caps, it deletes
// reclaimable boxes and tries once more, then deletes kept throwaways and
// then idle repo boxes (spareBoxes) one at a time, trying again after each.
// Each candidate is read again just before its delete (stillIdle).
// extra are more launch flags, like box new's reason.
func (a *App) launch(ctx context.Context, name string, ttl time.Duration, throwaway bool, sz mode.Size, extra ...string) error {
	err := a.launchOnce(ctx, name, ttl, throwaway, sz, extra)
	if isNoRoom(err) && a.reclaim(ctx) {
		err = a.launchOnce(ctx, name, ttl, throwaway, sz, extra)
	}
	// Each pass looks again, since other launches may be taking the same room
	// (#87): it deletes the first spare box only if deleting them all would
	// fit this box, so none is deleted for a launch that fails anyway.
	rejected := map[string]bool{}
	for isNoRoom(err) {
		insts, lerr := a.list(ctx)
		if lerr != nil {
			break
		}
		spare := slices.DeleteFunc(a.spareBoxes(ctx, insts), func(in instance) bool { return rejected[in.Name] })
		if len(spare) == 0 {
			break
		}
		if !a.fitsWithout(ctx, insts, spare, sz) {
			fmt.Fprintf(a.Err, "box: deleting the %d kept throwaways and idle boxes wouldn't make room for a %s box; leaving them\n", len(spare), sz.Name)
			break
		}
		gone := spare[0]
		// A claim or hold may have landed since the selection or the last try.
		var rerr error
		pooled := gone.Config[jobrun.ThrowawayRepoKey] != ""
		useBoxd := !pooled && gone.Config[throwawayKey] == "true" && !mode.IsGPU(gone.Config) && a.Project == mode.Project
		if pooled {
			rerr = a.evictPooled(ctx, gone)
		} else if useBoxd {
			rerr = a.evictKept(ctx, gone)
			if errors.Is(rerr, errDirectEviction) {
				rerr = a.evictKeptDirect(context.WithoutCancel(ctx), gone)
			}
		}
		if !pooled && !useBoxd {
			rerr = jobrun.RetryETag(context.WithoutCancel(ctx), func() error {
				if !a.stillIdle(ctx, gone, "") {
					return errNotIdle
				}
				_, err := a.incusOK(context.WithoutCancel(ctx), "delete", a.target(gone.Name), "--force")
				return err
			})
		}
		if errors.Is(rerr, errNotIdle) {
			if pooled || useBoxd {
				rejected[gone.Name] = true
			}
			continue // the next pass looks again, without it
		}
		if rerr != nil && !isGone(rerr) {
			a.warn(fmt.Sprintf("box: warning: removing %s: %v", gone.Name, rerr))
			break // it would be listed again
		}
		if gone.Config[throwawayKey] == "true" {
			fmt.Fprintf(a.Err, "box: deleted %s, kept after its job, to make room\n", gone.Name)
		} else {
			fmt.Fprintf(a.Err, "box: deleted idle box %s to make room\n", gone.Name)
		}
		err = a.launchOnce(ctx, name, ttl, throwaway, sz, extra)
	}
	return err
}

// fitsWithout says whether a box of size sz would fit once the boxes in gone
// are deleted, as box status counts it.
func (a *App) fitsWithout(ctx context.Context, insts, gone []instance, sz mode.Size) bool {
	del := map[string]bool{}
	for _, in := range gone {
		del[in.Name] = true
	}
	now := mode.Mode(a.mode(ctx))
	if now == "" {
		now = mode.Shared
	}
	maxBoxes := -1
	if n, err := strconv.Atoi(a.projectKey(ctx, "limits.instances")); err == nil {
		maxBoxes = n
	}
	sizes, _ := a.room(ctx, insts, now, maxBoxes, func(in instance) bool { return del[in.Name] })
	for _, s := range sizes {
		if s.Name == sz.Name {
			return s.Fits != 0
		}
	}
	return true
}

// errNotIdle is a repo box that took a job or a hold before its delete.
var errNotIdle = errors.New("no longer idle")

// isGone says whether a delete failed because another delete got there
// first: the box is gone, or already stopped by that delete (#87).
func isGone(err error) bool {
	s := err.Error()
	return strings.Contains(s, "Instance not found") || strings.Contains(s, "instance is already stopped")
}

// Incus looks up the image before submitting the instance create request.
var imageLookupEOF = regexp.MustCompile(`^Error: Get "https?://[^/"]+/1[.]0/images/[^/?"]+(\?[^"]*)?": EOF$`)

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
	run := func() (Result, error) {
		var stdin io.Reader
		if devices != "" {
			stdin = strings.NewReader(devices)
		}
		return a.incus(ctx, Cmd{Args: args, Stdin: stdin})
	}
	res, err := run()
	if err == nil && res.Code != 0 && ctx.Err() == nil && imageLookupEOF.MatchString(strings.TrimSpace(res.Stderr)) {
		res, err = run()
	}
	if err == nil && res.Code != 0 {
		err = fmt.Errorf("incus launch: %s", strings.TrimSpace(res.Stderr))
	}
	if err != nil {
		if match := projectLimitRefusal.FindStringSubmatch(err.Error()); match != nil {
			resource := match[1]
			if resource == "" {
				switch strings.ToLower(match[2]) {
				case "":
					resource = "instances"
				case "container":
					resource = "containers"
				case "virtual-machine":
					resource = "virtual-machines"
				default:
					resource = "unknown"
				}
			}
			return &errNoRoom{size: sz.Name, resource: resource, reason: firstLine(strings.TrimPrefix(err.Error(), "incus launch: "))}
		}
	}
	return err
}

// setExpiry sets the box's expiry to now plus ttl, or to its hold if that is
// later (mode.Held): a person's extend outlives a job's start and end.
// It retries a lost write race (jobrun.RetryETag): the tray sets a box's CPU
// limits whenever they differ from the mode's, within a tick of a launch in
// Full, just when box sets the job's expiry, or on any mode change.
func (a *App) setExpiry(ctx context.Context, box string, ttl time.Duration) error {
	return jobrun.RetryETag(ctx, func() error {
		until := a.heldExpiry(ctx, box, a.Now().Add(ttl))
		_, err := a.incusOK(ctx, "config", "set", a.target(box), expiresKey+"="+until)
		return err
	})
}

func (a *App) heldExpiry(ctx context.Context, box string, until time.Time) string {
	hold, _ := a.incusOK(ctx, "config", "get", a.target(box), mode.HoldKey)
	return mode.Held(map[string]string{mode.HoldKey: strings.TrimSpace(hold)}, until).UTC().Format(time.RFC3339)
}

// Publish the setup owner before pushing files. The second reservation read
// also catches a cleanup that reserved the box while config set was starting.
func (a *App) reserveSetup(ctx context.Context, box, job string, pooled bool) error {
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := a.incusOK(ctx, "config", "get", a.target(box), jobrun.DirectEvictKey)
		if err != nil {
			return err
		}
		if jobrun.DeleteReserved(map[string]string{jobrun.DirectEvictKey: strings.TrimSpace(raw)}, a.Now()) {
			return fmt.Errorf("box %s is being deleted", box)
		}
		return nil
	}
	err := jobrun.RetryETag(ctx, func() error {
		if err := check(); err != nil {
			return err
		}
		now := a.Now()
		expiry := jobrun.ProtectedExpiry
		if a.Project != mode.Project {
			expiry = a.heldExpiry(ctx, box, now.Add(jobTTL))
		}
		args := []string{"config", "set", a.target(box), expiresKey + "=" + expiry,
			jobrun.SetupKey + "=" + jobrun.SetupValue(job, now)}
		if pooled {
			args = append(args, jobrun.ThrowawayClaimKey+"=")
		}
		_, err := a.incusOK(ctx, args...)
		return err
	})
	if err != nil {
		return err
	}
	return check()
}

// A legacy client can shorten the deadline while the bundle is still being
// uploaded. Once the wrapper is visible, check the write and repair it if
// this job is still running.
func (a *App) confirmProtection(ctx context.Context, box, job string) error {
	ctx, cancel := a.probeCtx(ctx)
	defer cancel()
	if a.Project != mode.Project {
		return a.setExpiry(ctx, box, jobTTL)
	}
	return jobrun.RetryETag(ctx, func() error {
		raw, err := a.incusOK(ctx, "config", "get", a.target(box), expiresKey)
		if err != nil || mode.ProtectedDeadline(strings.TrimSpace(raw)) {
			return err
		}
		out, err := a.incusOK(ctx, "exec", a.target(box), "--", "bash", "-c", runningScript())
		if err != nil || !slices.Contains(strings.Fields(out), job) {
			return err
		}
		_, err = a.incusOK(ctx, "config", "set", a.target(box), expiresKey+"="+jobrun.ProtectedExpiry)
		return err
	})
}

func (a *App) launchSetup(job string) []string {
	if a.Project != mode.Project {
		return nil
	}
	return []string{"-c", jobrun.SetupKey + "=" + jobrun.SetupValue(job, a.Now())}
}

// Only agents has a host reaper to reconcile a protected deadline.
func (a *App) hostManagesExpiry(ctx context.Context, box string) (bool, error) {
	if a.Project != mode.Project {
		return false, nil
	}
	owner, err := a.incusOK(ctx, "config", "get", a.target(box), jobrun.SetupKey)
	return strings.TrimSpace(owner) != "", err
}

// isConflict says whether a launch failed because the name is taken.
func isConflict(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "already exists")
}

// Rm deletes a box, running or not, waiting out another operation on it
// such as the tray's limit write (#71).
func (a *App) Rm(ctx context.Context, name string) error {
	return jobrun.RetryETag(ctx, func() error {
		stop, err := a.incusOK(ctx, "config", "get", a.target(name), jobrun.ThrowawayStopKey)
		if err != nil {
			return err
		}
		if stop = strings.TrimSpace(stop); stop != "" && stop != jobrun.ThrowawayStopped {
			return fmt.Errorf("box %s is being stopped; try again", name)
		}
		_, err = a.incusOK(ctx, "delete", a.target(name), "--force")
		return err
	})
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
		if mode.ProtectedDeadline(exp) {
			exp = "job protected"
		} else if exp == "" {
			exp = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", in.Name, in.Status, exp)
	}
	return w.Flush()
}

// RunOpts says what box run does.
type RunOpts struct {
	Dir   string   // local git worktree
	Cmd   []string // one shell string, or argv to quote
	Box   string   // box to use; "" is the repo's own box
	Host  string   // CLI --host pin (debugging override); empty lets fleet pick
	New   bool     // use a throwaway box, kept KeepEnded after the job, else deleted
	Fresh bool     // launch a new throwaway even when a pooled box is idle
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
			return fmt.Errorf("env %s looks like a secret; boxes are shared, so box won't pass it. Use a non-secret value", k)
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
	Host  string `json:"host,omitempty"`
	State string `json:"state,omitempty"`
	Code  int    `json:"exitCode"`
	// Push is set on a box_run that returned a running or queued job:
	// whether its result will arrive as a turn in the agent's T3 thread.
	Push *bool `json:"push,omitempty"`
	// A queued job's place in line, from 1, and what it waits for.
	Position int             `json:"position,omitempty"`
	Why      string          `json:"why,omitempty"`
	Capacity *capacityDetail `json:"capacity,omitempty"`
	// Digest leads a finished Go test or vitest run's result: the failed
	// tests with file:line and first message, then the counts
	// (internal/digest). "" for other runs.
	Digest string `json:"digest,omitempty"`

	throwaway bool       // a queued job's box is a throwaway, kept after the job
	fleetHost *fleetHost // exact fleet destination when dispatched via fleet routing
}

// Run syncs the worktree to a box, runs the command there and follows its
// output.
func (a *App) Run(ctx context.Context, o RunOpts) (JobResult, error) {
	o.New = o.New || o.Fresh
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
	return a.runDispatch(ctx, o, sz)
}

func (a *App) runOnHost(ctx context.Context, o RunOpts, sz mode.Size, targetHost *fleetHost, prepared *fleetRun) (JobResult, *turnAwayInfo, error) {
	if sz.GPU && !a.hasGPU(ctx) { // refused, not queued: waiting won't bring a GPU
		limits := []string{"gpu"}
		if targetHost != nil {
			return JobResult{State: statePaused, Code: exitPaused}, &turnAwayInfo{msg: errNoGPU{}.Error(), queue: queueRun{limits: limits}}, nil
		}
		fmt.Fprintln(a.Err, "box: refused:", errNoGPU{}.Error()+"; nothing started")
		detail := &capacityDetail{Limits: limits}
		a.printCapacity(detail)
		return JobResult{State: statePaused, Code: exitPaused, Capacity: detail}, nil, nil
	}
	var rp repo
	var info jobMeta
	var id, bundle string
	if prepared != nil {
		rp, info, id, bundle = prepared.repo, prepared.info, prepared.ref, prepared.bundle
	} else {
		var err error
		rp, err = openRepo(ctx, a.R, o.Dir)
		if err != nil {
			return JobResult{Code: 1}, nil, err
		}
		a.warnSubmodules(ctx, rp.Top)
		info = a.runInfo(ctx, rp, o, sz)
	}
	box := o.Box
	if box == "" {
		box = rp.Name
	}
	if !validBox.MatchString(box) {
		return JobResult{Code: 2}, nil, fmt.Errorf("bad box name %q", box)
	}
	// qr is the run as the queue takes it, from where Run has got to. A run
	// turned away by a pause gets a throwaway: the checks below on the repo's
	// box (its size, that it isn't a throwaway) haven't run, and the tray may
	// stop a frozen box, which the queue doesn't start again.
	qr := func(paused bool) queueRun {
		q := queueRun{o: o, rp: rp, box: box, new: o.New, sz: sz, info: info, bundle: bundle, ref: id, host: targetHost}
		if o.New || sz.GPU || paused {
			q.box, q.new = rp.Name, true
		}
		return q
	}
	handleTurnAway := func(paused bool, err error) (JobResult, *turnAwayInfo, error) {
		q := qr(paused)
		msg, cause := err.Error(), errCause(err)
		if paused {
			cause = a.pausedCause(msg)
			msg += "; nothing started"
		} else {
			q.limits = launchLimits(err)
		}
		if targetHost != nil {
			cctx, cancel := context.WithTimeout(ctx, refusalTimeout)
			why := cause(cctx)
			cancel()
			return JobResult{State: statePaused, Code: exitPaused}, &turnAwayInfo{why: why, msg: msg, queue: q}, nil
		}
		return a.turnAway(ctx, q, msg, cause), nil, nil
	}

	insts, paused := a.reach(ctx)
	var inst *instance
	if sz.GPU && paused == "" {
		// GPU boxes are throwaways, so an idle box never holds the GPU. The
		// one exception is a GPU box someone made with box new --gpu.
		if o.Box == "" {
			o.New = true
		} else if inst = find(insts, box); inst == nil || !mode.IsGPU(inst.Config) {
			return JobResult{Box: box, Code: 2}, nil, invalidGPUBox(box)
		}
		except := ""
		if !o.New {
			except = box
		}
		if err := a.claimGPU(ctx, insts, except); err != nil {
			return handleTurnAway(false, err)
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
				return JobResult{Box: box, Code: 1}, nil, err
			}
		}
	}
	if paused != "" {
		return handleTurnAway(true, errors.New(paused))
	}

	if bundle == "" {
		id = a.Now().UTC().Format("20060102-150405") + "-" + a.Rand()
		var err error
		bundle, err = snapshot(ctx, a.R, rp.Top, id)
		if err != nil {
			return JobResult{Code: 1}, nil, err
		}
		defer os.RemoveAll(filepath.Dir(bundle))
	}

	setupFailed := func(r JobResult, err error) (JobResult, *turnAwayInfo, error) {
		if targetHost != nil && ctx.Err() == nil {
			return r, &turnAwayInfo{msg: err.Error()}, err
		}
		return r, nil, err
	}
	started := false // from here on the remote job may be running
	setupCtx, cancelSetup := context.WithTimeout(ctx, jobrun.SetupTTL)
	defer cancelSetup()
	reused := false
	repoKey := ""
	if !o.New {
		actual, paused, err := a.ensureBox(setupCtx, box, box+"."+id, inst, sz, o.Size)
		var have existingBoxSize
		switch {
		case errors.As(err, &have):
			fmt.Fprintf(a.Err, "box: %s is %s, so this run uses a throwaway %s box\n", box, string(have), sz.Name)
			o.New = true
		case isTempFail(err):
			return handleTurnAway(false, err)
		case err != nil:
			return setupFailed(JobResult{Code: 1}, err)
		case paused != "":
			return handleTurnAway(true, errors.New(paused))
		default:
			sz = actual
			info.Size, info.GPU = sz.Name, sz.GPU
		}
	}
	if o.New {
		ttl := o.TTL
		if ttl == 0 {
			ttl = idleTTL
		}
		if ttl == idleTTL && !o.Fresh {
			repoKey = throwawayRepoKey(info.Repo, sz)
			if repoKey != "" && !a.poolEnabled(ctx) {
				repoKey = ""
			}
		}
		claimCtx, cancel := context.WithTimeout(ctx, jobrun.ThrowawayClaimTTL)
		defer cancel()
		if idle := a.claimIdleThrowaway(claimCtx, insts, repoKey, id, sz); idle != "" {
			box, setupCtx, reused = idle, claimCtx, true
			fmt.Fprintf(a.Err, "box: reusing idle throwaway %s\n", box)
		} else {
			var err error
			if box, err = a.launchThrowaway(setupCtx, rp.Name, insts, ttl, sz, repoKey, id); err != nil {
				if isTempFail(err) {
					return handleTurnAway(false, err)
				}
				return JobResult{Box: box, Code: 1}, nil, err
			}
			// A failed setup takes only the box it launched.
			defer func() {
				if !started {
					a.removeQuietly(ctx, box)
				}
			}()
		}
	}

	job := box + "." + id
	pubID, err := runJobID(targetHost, job, box)
	if err != nil {
		return JobResult{Box: box, Code: 1}, nil, err
	}
	hostName := a.jobHostName
	r := JobResult{Job: pubID.ID, Box: box, Host: hostName, Code: 1, throwaway: o.New, fleetHost: targetHost}
	if err := a.reserveSetup(setupCtx, box, job, reused); err != nil {
		return setupFailed(r, fmt.Errorf("reserve job setup: %w", err))
	}
	remote := workRoot + "/.box/" + job
	if _, err := a.incusOK(setupCtx, "file", "push", "--create-dirs", bundle, a.target(box)+remote+".bundle"); err != nil {
		return setupFailed(r, err)
	}
	if err := a.pushMeta(setupCtx, box, remote, filepath.Dir(bundle), info); err != nil {
		a.warn(fmt.Sprintf("box: warning: recording %s.meta: %v", job, err))
	}
	if err := setupCtx.Err(); err != nil {
		return setupFailed(r, err)
	}
	// Say the job ID before starting it, so a dropped connection can't lose it.
	fmt.Fprintf(a.Err, "box: job %s on %s%s\n", pubID.ID, box, onHost(hostName))
	a.notify(jobNotice{Event: "starting_job", Box: box, Job: pubID.ID, Host: hostName})
	started = true
	_, err = a.incusOK(setupCtx, "exec", a.target(box), "--", "bash", "-lc", startScript(job, id, rp.Name, payload(o)))
	if err == nil || a.Project == mode.Project {
		if protectErr := a.confirmProtection(ctx, box, job); protectErr != nil && err == nil {
			a.warn(fmt.Sprintf("box: warning: checking job protection on %s: %v", box, protectErr))
		}
	}
	if err != nil {
		// The tray stops GPU boxes in My PC, and another run may then
		// delete one. Only then does the job surely not run: a frozen box's
		// job may resume, so it keeps its job ID below.
		if insts, lerr := a.list(ctx); lerr == nil {
			if in := find(insts, box); in != nil && in.Status == "Frozen" {
				if strings.Contains(err.Error(), "Instance is frozen") {
					// Incus refused the exec, so the start script never ran
					// and there is no job to resume: refuse (or queue) it
					// like a box that was already frozen before the run.
					_, paused := a.check(ctx, box)
					if paused == "" {
						paused = box + " is paused"
					}
					// No job started, so a throwaway has nothing to keep
					// it for: let the launch defer remove it, rather than
					// leaving it holding a slot until cleanup checks it.
					started = false
					return handleTurnAway(true, errors.New(paused))
				}
				// Any other failure may have happened after the script
				// started the job, so keep the job ID: it may resume.
				if sz.GPU {
					fmt.Fprintln(a.Err, pausedGPU(pubID.ID))
				} else {
					fmt.Fprintln(a.Err, pausedMidRun(pubID.ID))
				}
				r.State, r.Code = statePaused, exitPaused
				return r, nil, nil
			} else if sz.GPU && (in == nil || in.Status == "Stopped") {
				fmt.Fprintf(a.Err, "box: the GPU box was stopped (the PC's owner needed it); rerun later or on CPU\n")
				r.State, r.Code = statePaused, exitPaused
				return r, nil, nil
			}
		}
		if o.New {
			fmt.Fprintf(a.Err, "box: keeping %s; box job %s hands it to the reaper once the job has ended\n", box, pubID.ID)
		}
		return r, nil, err
	}
	cancelSetup()

	r.State, r.Code, r.Digest, err = a.attach(ctx, pubID, o.Wait)
	recoveredEnd := false
	if err != nil && ctx.Err() == nil && o.New && !sz.GPU && a.Project == mode.Project {
		look, cancel := context.WithTimeout(ctx, refusalTimeout)
		end, ok := a.endedElsewhere(look, pubID)
		cancel()
		if ok {
			fmt.Fprintln(a.Err, "box: using the recorded result after the log stream ended")
			r.State, r.Code, r.Digest, err = stateDone, end.Code, end.Digest, nil
			recoveredEnd = true
		}
	}
	switch {
	case !o.New:
	case recoveredEnd:
	case err == nil && r.State == stateDone:
		a.cleanup(ctx, box, true, repoKey != "")
	case sz.GPU && r.State == statePaused && a.gpuStopped(ctx, box) != "":
		// attach said the GPU box was stopped: there is nothing to reattach (#118).
	default:
		fmt.Fprintf(a.Err, "box: keeping %s; box job %s hands it to the reaper once the job has ended\n", box, pubID.ID)
	}
	return r, nil, err
}

// launchThrowaway launches a fresh box for one job. It never adopts an
// existing box: a taken name means another try with a new suffix. It also
// retries once under a fresh suffix on Incus's btrfs first-launch metadata
// error (jobrun.IsDisallowedMetadata), which warms the image cache on the
// failed try but leaves the old name's empty container dir poisoned on disk.
func (a *App) launchThrowaway(ctx context.Context, base string, insts []instance, ttl time.Duration, sz mode.Size, repoKey, ref string) (string, error) {
	retriedMeta := false
	// Unnamed: if there's no room, boxd's queue names a box of its own (#143).
	fmt.Fprintln(a.Err, "box: launching a throwaway box")
	for try := 0; try < 8; try++ {
		name := base + "-" + a.Rand()
		if find(insts, name) != nil {
			continue
		}
		a.notify(jobNotice{Event: "launching_box", Box: name, Host: a.jobHostName})
		job := name + "." + ref
		config := a.launchSetup(job)
		if repoKey != "" {
			config = append(config, "-c", jobrun.ThrowawayRepoKey+"="+repoKey, "-c", jobrun.ThrowawayOwnerKey+"="+job)
		}
		err := a.launch(ctx, name, ttl, true, sz, config...)
		if err == nil {
			return name, a.confirmGPU(ctx, name, sz)
		}
		if jobrun.IsDisallowedMetadata(err) && !retriedMeta && try < 7 {
			// Guard try < 7 so a metadata error on the final slot returns the
			// real Incus error rather than "no free name for a throwaway box".
			retriedMeta = true
			fmt.Fprintf(a.Err, "box: %s hit Incus's first-launch metadata error; retrying once with a fresh box\n", name)
			insts = append(insts, instance{Name: name})
			continue
		}
		if !isConflict(err) {
			if !isTempFail(err) {
				// It may have been made before the launch failed.
				fmt.Fprintf(a.Err, "box: launching %s failed; if box ls lists it, box rm %s\n", name, name)
			}
			return name, err
		}
		insts = append(insts, instance{Name: name})
	}
	return "", errors.New("no free name for a throwaway box")
}

// ensureBox launches or starts a reused box and returns the size of the box it
// actually used. When a concurrent run launches or starts it first, it carries
// on with that one.
func (a *App) ensureBox(ctx context.Context, box, job string, inst *instance, sz mode.Size, requestedSize string) (actual mode.Size, paused string, err error) {
	if inst != nil && inst.Status == "Running" {
		return mode.SizeOf(inst.Config), "", nil
	}
	if inst == nil {
		fmt.Fprintf(a.Err, "box: launching %s\n", box)
		err = a.launch(ctx, box, idleTTL, false, sz, a.launchSetup(job)...)
	} else {
		if err := a.reserveSetup(ctx, box, job, false); err != nil {
			return mode.Size{}, "", err
		}
		_, err = a.incusOK(ctx, "start", a.target(box))
	}
	if err == nil {
		if inst != nil {
			return mode.SizeOf(inst.Config), "", nil
		}
		return sz, "", nil
	}
	now, paused := a.check(ctx, box)
	if paused != "" {
		return mode.Size{}, paused, nil
	}
	if now == nil {
		if jobrun.IsDisallowedMetadata(err) {
			return mode.Size{}, "", fmt.Errorf("%w (this or an earlier failed launch left an empty dir on the PC: /var/lib/incus/storage-pools/default/containers/%s_%s; use a new box (--new / new: true) until it is removed)", err, a.Project, box)
		}
		return mode.Size{}, "", err
	}
	if err := reusableBox(now, requestedSize); err != nil {
		return mode.Size{}, "", err
	}
	switch {
	case now.Status == "Running":
		return mode.SizeOf(now.Config), "", nil
	case inst == nil: // someone else launched it but it isn't running yet
		if err := a.reserveSetup(ctx, box, job, false); err != nil {
			return mode.Size{}, "", err
		}
		if _, err := a.incusOK(ctx, "start", a.target(box)); err != nil {
			if now, paused := a.check(ctx, box); paused != "" {
				return mode.Size{}, paused, nil
			} else if now != nil && now.Status == "Running" {
				return mode.SizeOf(now.Config), "", reusableBox(now, requestedSize)
			}
			return mode.Size{}, "", err
		}
		return mode.SizeOf(now.Config), "", nil
	}
	return mode.Size{}, "", err
}

func (a *App) removeQuietly(ctx context.Context, box string) {
	fmt.Fprintf(a.Err, "box: removing %s\n", box)
	if err := a.Rm(context.WithoutCancel(ctx), box); err != nil {
		a.warn(fmt.Sprintf("box: warning: removing %s: %v", box, err))
	}
}

// cleanup deletes a throwaway box once no job is running in it. ours says
// the caller created it; otherwise the box's throwaway mark decides.
func (a *App) cleanup(ctx context.Context, box string, ours, pooled bool) {
	ctx, cancel := a.probeCtx(ctx)
	defer cancel()
	if !ours {
		v, err := a.incusOK(ctx, "config", "get", a.target(box), throwawayKey)
		if err != nil || strings.TrimSpace(v) != "true" {
			return
		}
		key, err := a.incusOK(ctx, "config", "get", a.target(box), jobrun.ThrowawayRepoKey)
		if err != nil {
			return
		}
		pooled = strings.TrimSpace(key) != ""
	}
	if pooled {
		return
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
		managed, err := a.hostManagesExpiry(ctx, box)
		if err != nil {
			return
		}
		if !managed {
			if err := a.setExpiry(ctx, box, a.KeepEnded); err != nil {
				a.warn(fmt.Sprintf("box: warning: setting expiry on %s: %v", box, err))
				return
			}
		}
		fmt.Fprintf(a.Err, "box: kept %s for box pull for up to %d minutes, unless a queued job or another launch needs the room; box rm %s deletes it now\n", box, int(a.KeepEnded.Minutes()), box)
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

// endedElsewhere is a job's end when its box can't say: the one recorded
// here when box read its exit, else boxd's record, which holds every job's
// end, including one nobody here read (a run that exited on a pause, a push
// that never came) whose box a launch then took (#90).
func (a *App) endedElsewhere(ctx context.Context, id jobID) (jobEnd, bool) {
	if end, ok := a.recordedEnd(id.ID); ok {
		return end, true
	}
	j, found, err := a.queuedJob(ctx, id.Bare)
	if err != nil || !found || j.State != dash.JobDone || j.Exit == nil {
		return jobEnd{}, false
	}
	return jobEnd{Code: *j.Exit, Tail: j.Tail}, true
}

// evictedFor names the queued job whose room took job's kept throwaway box,
// from boxd's record, or "" (#143).
func (a *App) evictedFor(ctx context.Context, job string) string {
	j, found, err := a.queuedJob(ctx, job)
	if err != nil || !found {
		return ""
	}
	return j.Evicted
}

func evictedText(box, by string) string {
	return fmt.Sprintf("%s was deleted to make room for queued job %s, so its files and full log are gone; rerun the job to get them", box, by)
}

// Job reattaches to a job started by Run. Once the job has ended, it hands
// a throwaway box made for it to the reaper (KeepEnded) or deletes it, if no
// other job runs there. A job whose box can't say gets endedElsewhere's end.
func (a *App) Job(ctx context.Context, job string, wait time.Duration) (JobResult, error) {
	if !validJob.MatchString(job) {
		return JobResult{Code: 2}, fmt.Errorf("bad job id %q", job)
	}
	box, _, _ := strings.Cut(job, ".")
	return a.job(ctx, jobID{ID: job, Bare: job, Box: box}, wait)
}

func (a *App) job(ctx context.Context, id jobID, wait time.Duration) (JobResult, error) {
	if wait < 0 {
		return JobResult{Code: 2}, errors.New("wait can't be negative")
	}
	job, box := id.ID, id.Box
	r := JobResult{Job: job, Box: box}
	var err error
	r.State, r.Code, r.Digest, err = a.attach(ctx, id, wait)
	if err != nil {
		// A job whose exit was read once has ended: when its box has gone
		// since (a throwaway's 30 minutes ran out, or a launch short of room
		// took it), its recorded end is the result, not "lost".
		if end, ok := a.endedElsewhere(ctx, id); ok {
			if by := a.evictedFor(ctx, id.Bare); by != "" {
				fmt.Fprintf(a.Err, "box: %s; this is the result recorded when it ended\n", evictedText(box, by))
			} else {
				fmt.Fprintf(a.Err, "box: can't read %s from its box (%s); this is the result recorded when it ended. If the box is gone, so are its full log and files.\n", job, firstLine(err.Error()))
			}
			fmt.Fprint(a.Out, end.Tail)
			return JobResult{Job: job, Box: box, State: stateDone, Code: end.Code, Digest: end.Digest}, nil
		}
	}
	if err == nil && r.State == stateDone {
		a.cleanup(ctx, box, false, false)
	}
	return r, err
}

const (
	aliveFn  = jobrun.AliveFn
	jobPIDFn = jobrun.JobPIDFn
)

// runningScript prints the jobs still running in a box.
func runningScript() string { return jobrun.RunningScript(workRoot) }

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
    completion=%[6]s
    [ -f %[1]s.cancelled ] && completion=%[7]s
    printf '%%s%%s %%s\n' %[3]s "$(cat %[1]s.exit)" "$completion" >&2
    d=$(timeout -k 2 10 bash -c %[4]s 2>/dev/null | base64 -w0)
    [ -n "$d" ] && printf '%%s%%s\n' %[5]s "$d" >&2
    exit 0
  fi
  if ! alive "$pid" %[2]s; then
    [ -f %[1]s.exit ] && continue
    echo "box: job %[2]s died without an exit code" >&2
    exit 4
  fi
  sleep 0.5
done
`, f, job, shq(exitMarker), shq(digest.Script(f)), shq(digestMarker), completionExited, completionCancelled)
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
// ended, followed by its exit code and completion kind. attach reads these
// from one exec so a freeze cannot interrupt a separate cancellation check.
const exitMarker = "\x01box-exit "

// digestMarker starts the line followScript prints on stderr after the exit
// line when the job left a test report: the cut-down report (digest.Script),
// base64. It rides on the follow, so the digest costs no exec of its own, and
// comes after the exit, within 10 seconds, so it can't hold the result back:
// a follow cut off before it just has no digest.
const digestMarker = "\x01box-digest "

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
	w      io.Writer
	mu     sync.Mutex
	buf    []byte
	code   string
	seen   bool
	report string // digestMarker's base64, when there was one
	// notRunning: incus said the box isn't running, a line held back
	// because a stopped GPU box has a better explanation (#118).
	notRunning bool
}

// notRunning is incus's error for an exec into a box that isn't running.
const notRunning = "Error: Instance is not running"

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
		} else if rest, ok := bytes.CutPrefix(line, []byte(digestMarker)); ok {
			e.report = strings.TrimSpace(string(rest))
		} else if string(line) == notRunning+"\n" {
			e.notRunning = true // attach says why, or prints it
		} else if _, err := e.w.Write(line); err != nil {
			return len(p), err
		}
		e.buf = e.buf[i+1:]
	}
}

// flush writes a trailing partial line, unless it is a marker's, cut off.
func (e *exitFilter) flush() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if bytes.HasPrefix(e.buf, []byte(digestMarker)) || bytes.HasPrefix(e.buf, []byte(exitMarker)) {
		e.buf = nil
	}
	if len(e.buf) > 0 {
		e.w.Write(e.buf)
		e.buf = nil
	}
}

// digest is the failure digest of the report the follow printed, or "".
func (e *exitFilter) digest() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, err := base64.StdEncoding.DecodeString(e.report)
	if err != nil {
		return ""
	}
	return digest.Parse(string(b))
}

func (e *exitFilter) exit() (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.code, e.seen
}

// pausedMidRun says what is true of a job whose box was frozen: it resumes
// only if the PC's owner switches back before the tray stops the box.
func pausedMidRun(job string) string {
	return fmt.Sprintf("box: job %s is paused (the PC's owner is using it). It resumes if they switch back within %s; after that the box is stopped and the job lost. Reattach later with: box job %s", job, frozenWindow, job)
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
func (a *App) attach(ctx context.Context, id jobID, wait time.Duration) (state string, code int, dg string, err error) {
	job, box := id.ID, id.Box
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
		Args:   []string{"exec", a.target(box), "--", "bash", "-c", followScript(id.Bare)},
		Stdout: tail, Stderr: ef,
	})
	stop()
	<-watched
	ef.flush()
	if text, ok := ef.exit(); ok {
		codeText, completion, hasCompletion := strings.Cut(text, " ")
		code, cerr := strconv.Atoi(codeText)
		if cerr != nil || (hasCompletion && completion != completionExited && completion != completionCancelled) {
			return "", 1, "", fmt.Errorf("job %s: bad exit code %q", job, text)
		}
		ended := a.Now()
		a.markIdle(ctx, box, id.Bare)
		a.reportEnded(ctx, id.Bare, code, ended, tail.String())
		dg = ef.digest()
		a.recordEnd(job, jobEnd{Host: a.jobHostName, Code: code, Took: int64(ended.Sub(jobStart(id.Bare)) / time.Second), Tail: keepTail(kept.lastLines(1 << 20)), Digest: dg, Completion: completion})
		return stateDone, code, dg, nil
	}
	if hit.Load() && ctx.Err() == nil {
		if gpu.Load() {
			fmt.Fprintln(a.Err, pausedGPU(job))
		} else {
			fmt.Fprintln(a.Err, pausedMidRun(job))
		}
		return statePaused, exitPaused, "", nil
	}
	if follow.Err() != nil {
		fmt.Fprintf(a.Err, "box: job %s is still running; reattach with: box job %s\n", job, job)
		return stateDetached, exitDetached, "", nil
	}
	if err != nil {
		return "", 1, "", err
	}
	// The tray stops a GPU box in My PC. Its job may look dead (4) or the
	// exec may just end; either way the agent should hear why.
	if res.Code != 0 && res.Code != 3 {
		if msg := a.gpuStopped(ctx, box); msg != "" {
			fmt.Fprintf(a.Err, "box: %s\n", msg)
			return statePaused, exitPaused, "", nil
		}
	}
	if ef.notRunning {
		fmt.Fprintln(a.Err, notRunning)
	}
	switch res.Code {
	case 0:
		return "", 1, "", fmt.Errorf("job %s ended without an exit code", job)
	case 3:
		return "", 1, "", fmt.Errorf("no job %s", job)
	case 4:
		return "", 1, "", fmt.Errorf("job %s died without an exit code", job)
	default:
		if _, paused := a.check(ctx, box); paused != "" {
			if strings.HasSuffix(paused, " is paused") {
				fmt.Fprintln(a.Err, pausedMidRun(job))
			} else {
				fmt.Fprintf(a.Err, "box: %s; stopped following job %s, reattach with: box job %s\n", paused, job, job)
			}
			return statePaused, exitPaused, "", nil
		}
		return "", 1, "", fmt.Errorf("lost job %s (reattach with: box job %s)", job, job)
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
func (a *App) markIdle(ctx context.Context, box, job string) {
	ctx, cancel := a.probeCtx(ctx)
	defer cancel()
	managed, err := a.hostManagesExpiry(ctx, box)
	if err != nil || managed {
		return // the host reconciles managed deadlines from the job files
	}
	repoKey, err := a.incusOK(ctx, "config", "get", a.target(box), jobrun.ThrowawayRepoKey)
	if err != nil {
		return
	}
	if strings.TrimSpace(repoKey) != "" {
		a.poolExpiry(ctx, box, job, true)
		return
	}
	throwaway, err := a.incusOK(ctx, "config", "get", a.target(box), throwawayKey)
	if err != nil {
		return
	}
	out, err := a.incusOK(ctx, "exec", a.target(box), "--", "bash", "-c", runningScript())
	if err != nil || len(strings.Fields(out)) > 0 {
		return
	}
	unpooled := strings.TrimSpace(throwaway) == "true"
	ttl := idleTTL
	if unpooled {
		stop, err := a.incusOK(ctx, "config", "get", a.target(box), jobrun.ThrowawayStopKey)
		if err != nil || strings.TrimSpace(stop) != "" {
			return
		}
		ttl = endedKeep
	}
	if err := a.setExpiry(ctx, box, ttl); err != nil {
		a.warn(fmt.Sprintf("box: warning: setting idle expiry on %s: %v", box, err))
		return
	}
	// An unpooled throwaway cannot take another job. A failed probe during
	// cooling must never replace its retrieval deadline with a job deadline.
	if unpooled {
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

func shq(s string) string { return jobrun.Shq(s) }
