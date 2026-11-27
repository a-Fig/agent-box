// Command box runs commands from a laptop git worktree in Linux boxes on the
// gaming PC, through the incus CLI. See docs/DESIGN.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/a-Fig/agent-box/internal/mode"
	"github.com/a-Fig/agent-box/internal/version"
)

func cancelText(job string, cancelled bool, code int) string {
	if cancelled {
		return fmt.Sprintf("cancelled %s (exit %d)", job, code)
	}
	return fmt.Sprintf("%s had already finished (exit %d)", job, code)
}

const usage = `usage:
  box status                                is offloading worth it now? (hosts, room, this repo's box)
  box hosts                                 list hosts and their current capacity
  box run --reason "why" [--box name | --new] [--size S | --gpu] [--env K=V]... [--timeout 30m]
          [--no-queue] -- <cmd...>          sync this worktree to a box and run cmd
  box job <id> [--offset N] [--limit N]     reattach to a job, or read a piece of its log
  box jobs [--box name]                     list fleet jobs, newest first (--box uses the default host)
  box cancel <id>                           stop a running job, or take a queued one off the queue
  box pull <id|box> <remote-path> <local-dir>
                                            copy a file or directory from a box into local-dir
  box new [name] [--ttl 2h] [--size S | --gpu] [--reason "why"]
                                            launch a box with a positive TTL
  box rm <name>                             delete a box
  box ls                                    list boxes
  box mcp                                   serve MCP on stdio: box_status, box_run, box_job, box_cancel, box_pull
  box watch [--state dir] <id>              follow a job and push its result into this session's T3 thread
                                            (box_run starts it detached; not for people)
  box hook                                  Claude Code's PostToolUse hook: this session's ended jobs,
                                            mid-turn (box join adds it; not for people)
  box join [--leave]                        register box mcp with the installed Claude Code, Codex and
                                            Antigravity, install the skill and Claude Code's hook
  box ui [--url U]                          open the PC's dashboard in the browser
  box app [--url U] [--wait]                open the dashboard in a window of its own
  box --version                             the release and commit this box was built from
  box update [--version <tag>] [--rollback] update box to the latest stable release now, install
                                            a release or nightly by tag, or swap box.old back
                                            (box mcp checks hourly; AGENT_BOX_NO_UPDATE=1 stops it)

--reason (required for run): one or two sentences, 10 to 300 characters, on
why this needs a box. The PC's owner sees it on the dashboard.

Sizes (CPUs in Shared/Full mode, RAM, disk):
  %s.
--size sets the size of a box that box new or box run launches (default
medium). A reused box of another size isn't resized: the run gets a throwaway.
--gpu is --size gpu: the PC's GPU, one GPU box at a time; on a host without
one (box status: gpu: none) it exits 75. box run --gpu uses a throwaway. The
tray stops a GPU box in My PC.
CPU throwaways reuse an idle box for the same repo and size when available.
--fresh bypasses the pool and launches a new throwaway.
If the Incus image lookup ends with EOF, box retries the launch once.

A run that can't start now (no room, the GPU taken, My PC) waits in the PC's
queue, and box run follows it there and then in its box; Ctrl-C leaves it
queued, box job follows it again, box cancel takes it off. A GPU run doesn't
wait out My PC. --no-queue exits 75 at once instead.

box run exits with the command's exit code, 124 when --timeout fired, or 75
when the box was paused or stopped, the run wasn't queued, or it left the
queue without running. A job paused mid-run resumes if the PC's owner switches back within
15 minutes (4 hours once its memory is moved to swap), else it is lost; box job
reattaches. In box pull, a relative remote path starts at the job's checkout and
an absolute path starts with / (in Git Bash, set MSYS_NO_PATHCONV=1); pulling by
job ID refuses if a later job replaced the checkout.
The dashboard is https://<host>/ of the remote; --url points them elsewhere.
Settings: BOX_REMOTE (pc), BOX_PROJECT (agents), BOX_IMAGE (agent-base).
`

// usageText is usage with the size table filled in.
func usageText() string {
	var sizes []string
	for _, s := range mode.Sizes {
		sizes = append(sizes, fmt.Sprintf("%s %d/%d, %s, %s", s.Name, s.SharedCPUs, s.FullCPUs, s.Memory, s.Disk))
	}
	return fmt.Sprintf(usage, strings.Join(sizes, "; "))
}

// envFlags collects repeated --env K=V flags.
type envFlags map[string]string

func (e envFlags) String() string { return "" }

func (e envFlags) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("want K=V, got %q", s)
	}
	e[k] = v
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, newApp(), os.Args[1:], os.Stdin))
}

func run(ctx context.Context, a *App, args []string, stdin io.Reader) int {
	if len(args) == 0 {
		fmt.Fprint(a.Err, usageText())
		return 2
	}
	if args[0] == "--version" || args[0] == "version" {
		fmt.Fprintln(a.Out, version.Text("box", version.Get()))
		return 0
	}
	fs := flag.NewFlagSet("box "+args[0], flag.ContinueOnError)
	fs.SetOutput(a.Err)
	fs.Usage = func() { fmt.Fprint(a.Err, usageText()) }
	fail := func(err error) int {
		fmt.Fprintln(a.Err, "box:", err)
		return 1
	}
	code := func(r JobResult, err error) int {
		c := r.Code
		if r.Digest != "" {
			fmt.Fprint(a.Err, "\n"+r.Digest)
		}
		if err != nil {
			fmt.Fprintln(a.Err, "box:", err)
			if c == 0 {
				c = 1
			}
		}
		return c
	}

	switch args[0] {
	case "new":
		ttl := fs.Duration("ttl", idleTTL, "lifetime, stored as user.expires; it also holds the box until then")
		size := fs.String("size", "", mode.SizeNames()+" (default "+mode.DefaultSize+")")
		gpu := fs.Bool("gpu", false, "same as --size gpu")
		reason := fs.String("reason", "", "why this needs a box (optional; 10 to 300 characters)")
		if parseInterspersed(fs, args[1:]) != nil || fs.NArg() > 1 {
			return 2
		}
		if !gpuSize(a, size, *gpu) {
			return 2
		}
		why, err := checkReason(*reason, false, "--reason")
		if err != nil {
			fmt.Fprintln(a.Err, "box:", err)
			return 2
		}
		name, err := a.New(ctx, fs.Arg(0), *ttl, *size, why)
		if isTempFail(err) {
			fmt.Fprintln(a.Err, "box:", err)
			return exitPaused
		}
		if err != nil {
			return fail(err)
		}
		fmt.Fprintln(a.Out, name)
		return 0
	case "run":
		box := fs.String("box", "", "box to use (default: the repo's own box)")
		isNew := fs.Bool("new", false, "use a throwaway box, kept up to 30 minutes after the job for box pull, unless another run needs the room")
		fresh := fs.Bool("fresh", false, "launch a new throwaway without reusing an idle box")
		ttl := fs.Duration("ttl", idleTTL, "initial lifetime of a --new box before the job starts")
		timeout := fs.Duration("timeout", 0, "kill the command after this long (exit 124)")
		size := fs.String("size", "", "size of a box this run launches: "+mode.SizeNames()+" (default "+mode.DefaultSize+")")
		gpu := fs.Bool("gpu", false, "same as --size gpu: run in a throwaway GPU box")
		envs := envFlags{}
		fs.Var(envs, "env", "K=V passed to the command (repeatable; no secrets)")
		reason := fs.String("reason", "", "required: one or two sentences (10 to 300 characters) on why this run needs a box")
		noQueue := fs.Bool("no-queue", false, "exit 75 at once when the run can't start now, instead of waiting in the PC's queue")
		if fs.Parse(args[1:]) != nil {
			return 2
		}
		if *box != "" && (*isNew || *fresh) {
			fmt.Fprintln(a.Err, "box: --box cannot be used with --new or --fresh")
			return 2
		}
		if !gpuSize(a, size, *gpu) {
			return 2
		}
		why, err := checkReason(*reason, true, "--reason")
		if err != nil {
			fmt.Fprintln(a.Err, "box:", err)
			return 2
		}
		dir, err := os.Getwd()
		if err != nil {
			return fail(err)
		}
		r, err := a.Run(ctx, RunOpts{Dir: dir, Cmd: fs.Args(), Box: *box, New: *isNew, Fresh: *fresh, TTL: *ttl, Env: envs, Timeout: *timeout, Size: *size, Reason: why, Queue: !*noQueue})
		if err == nil && r.State == stateQueued {
			// This process follows the job through the queue and into its box (#93).
			info := jobMeta{Reason: why, Command: displayCommand(fs.Args()), Agent: agentInfo{Worktree: dir}, Source: r.Source}
			a.recordQueued(info, r.Job, *size == mode.GPUSize, r.throwaway)
			r, err = a.followQueued(ctx, r.Job)
			a.markRead(r.Job, r, err, "box run")
		}
		return code(r, err)
	case "job":
		offset := fs.Int64("offset", 0, "print the log from this byte, without waiting")
		limit := fs.Int64("limit", 0, "print at most this many bytes of the log (default 32KiB)")
		if parseInterspersed(fs, args[1:]) != nil || fs.NArg() != 1 {
			fmt.Fprint(a.Err, usageText())
			return 2
		}
		paged := false
		fs.Visit(func(f *flag.Flag) { paged = paged || f.Name == "offset" || f.Name == "limit" })
		lookup, err := a.lookupJob(fs.Arg(0))
		if err != nil {
			if !paged {
				return code(JobResult{Code: 2}, err)
			}
			return fail(err)
		}
		if !paged {
			var r JobResult
			var err error
			if _, queued, queueErr := lookup.queued(ctx); queued {
				seen := lookup.id.hostTag != "" && queueErr == nil
				r, err = lookup.app.followQueuedJob(ctx, lookup.id, seen)
			} else {
				r, err = lookup.job(ctx, 0)
			}
			a.markRead(lookup.id.ID, r, err, "box job")
			return code(r, err)
		}
		p, end, err := lookup.page(ctx, *offset, *limit)
		if err != nil {
			if end != nil {
				fmt.Fprintf(a.Err, "box: can't read the log (%s); this is the tail recorded when the job ended\n", firstLine(err.Error()))
				io.WriteString(a.Out, end.Tail)
				return 0
			}
			return fail(err)
		}
		fmt.Fprintln(a.Err, "box: "+p.pageHead())
		io.WriteString(a.Out, p.Text)
		return 0
	case "status", "hosts":
		if fs.Parse(args[1:]) != nil || fs.NArg() != 0 {
			fmt.Fprint(a.Err, usageText())
			return 2
		}
		dir, _ := os.Getwd()
		listOnly := args[0] == "hosts"
		if listOnly {
			dir = ""
		}
		s, err := a.fleetStatus(ctx, dir, listOnly)
		if err != nil {
			return fail(err)
		}
		if listOnly {
			writeFleet(a.Out, s.Hosts)
			return 0
		}
		writeStatus(a.Out, s)
		if !s.Available {
			return exitPaused
		}
		return 0
	case "jobs":
		box := fs.String("box", "", "only this box")
		if fs.Parse(args[1:]) != nil || fs.NArg() != 0 {
			fmt.Fprint(a.Err, usageText())
			return 2
		}
		jobs, err := a.listJobs(ctx, *box)
		if err != nil {
			return fail(err)
		}
		if err := printJobs(a, jobs); err != nil {
			return fail(err)
		}
		return 0
	case "cancel":
		if fs.Parse(args[1:]) != nil || fs.NArg() != 1 {
			fmt.Fprint(a.Err, usageText())
			return 2
		}
		removed, cancelled, c, err := a.CancelAny(ctx, fs.Arg(0))
		if err != nil {
			return fail(err)
		}
		if removed {
			fmt.Fprintln(a.Out, "removed "+fs.Arg(0)+" from the queue; it never ran")
			return 0
		}
		fmt.Fprintln(a.Out, cancelText(fs.Arg(0), cancelled, c))
		return 0
	case "pull":
		if fs.Parse(args[1:]) != nil || fs.NArg() != 3 {
			fmt.Fprint(a.Err, usageText())
			return 2
		}
		dest, err := a.Pull(ctx, fs.Arg(0), fs.Arg(1), fs.Arg(2))
		if err != nil {
			return fail(err)
		}
		fmt.Fprintln(a.Out, dest)
		return 0
	case "join":
		leave := fs.Bool("leave", false, "unregister box and remove the skill")
		if fs.Parse(args[1:]) != nil || fs.NArg() != 0 {
			fmt.Fprint(a.Err, usageText())
			return 2
		}
		c, err := a.JoinConfig(a.Out)
		if err != nil {
			return fail(err)
		}
		if err := join(ctx, c, *leave); err != nil {
			return fail(err)
		}
		return 0
	case "rm":
		if fs.Parse(args[1:]) != nil || fs.NArg() != 1 {
			fmt.Fprint(a.Err, usageText())
			return 2
		}
		if err := a.Rm(ctx, fs.Arg(0)); err != nil {
			return fail(err)
		}
		return 0
	case "ls":
		if err := a.Ls(ctx); err != nil {
			return fail(err)
		}
		return 0
	case "mcp":
		go autoUpdate(ctx, a)
		if exe, err := os.Executable(); err == nil {
			a.MCPExe = exe
		}
		mcpCtx, stop := context.WithCancel(ctx)
		defer stop()
		go a.reconcilePushes(mcpCtx, pushReconcileInterval)
		if err := serveMCP(mcpCtx, a, stdin, a.Out); err != nil {
			return fail(err)
		}
		return 0
	case "update":
		tag := fs.String("version", "", "install this release tag, a nightly included")
		rollback := fs.Bool("rollback", false, "swap box.old back in")
		if parseInterspersed(fs, args[1:]) != nil || fs.NArg() > 0 || *rollback && *tag != "" {
			fmt.Fprint(a.Err, usageText())
			return 2
		}
		u, err := newUpdater(a, a.Out)
		if err == nil {
			if *rollback {
				err = u.Rollback(ctx)
			} else {
				err = u.Update(ctx, *tag)
			}
		}
		if err != nil {
			return fail(err)
		}
		return 0
	case "skill-sync": // from an updater (update.go): print the skill's SHA-256, or update copies of an old one
		if len(args) == 1 {
			fmt.Fprintln(a.Out, skillSHA())
			return 0
		}
		home, err := os.UserHomeDir()
		if err == nil { // not box join's config: that refuses a box under the temp folder
			err = skillSync(joinConfig{Home: home, ClaudeConfigDir: os.Getenv("CLAUDE_CONFIG_DIR"),
				CodexHome: os.Getenv("CODEX_HOME"), GeminiHome: os.Getenv("GEMINI_HOME"),
				T3Home: os.Getenv("T3CODE_HOME"), Out: a.Out}, args[1:]...)
		}
		if err != nil {
			return fail(err)
		}
		return 0
	case "mcp-call": // one request from box mcp, through the newest box.exe (mcpexe.go)
		if err := runMCPCall(ctx, stdin, a.Out, callInProcess(a)); err != nil {
			return fail(err)
		}
		return 0
	case "ui":
		return runUI(ctx, a, fs, args[1:])
	case "app":
		return runApp(ctx, a, fs, args[1:])
	case "watch":
		state := fs.String("state", a.StateDir, "directory of per-job push state")
		if parseInterspersed(fs, args[1:]) != nil || fs.NArg() != 1 {
			fmt.Fprint(a.Err, usageText())
			return 2
		}
		a.StateDir = *state
		if err := a.Watch(ctx, fs.Arg(0)); err != nil {
			return fail(err)
		}
		return 0
	case "hook":
		// Claude Code's PostToolUse hook: never an error, never a delay.
		a.Hook(stdin, a.Out)
		return 0
	case "-h", "--help", "help":
		fmt.Fprint(a.Out, usageText())
		return 0
	}
	fmt.Fprint(a.Err, usageText())
	return 2
}

// gpuSize folds --gpu into --size, refusing --gpu with another size.
func gpuSize(a *App, size *string, gpu bool) bool {
	if !gpu {
		return true
	}
	if *size != "" && *size != mode.GPUSize {
		fmt.Fprintf(a.Err, "box: --gpu and --size %s don't mix\n", *size)
		return false
	}
	*size = mode.GPUSize
	return true
}

// parseInterspersed lets flags follow positional arguments ("box new foo --ttl 2h").
func parseInterspersed(fs *flag.FlagSet, args []string) error {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	return fs.Parse(pos)
}
