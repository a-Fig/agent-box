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
)

func cancelText(job string, cancelled bool, code int) string {
	if cancelled {
		return fmt.Sprintf("cancelled %s (exit %d)", job, code)
	}
	return fmt.Sprintf("%s had already finished (exit %d)", job, code)
}

const usage = `usage:
  box status                                is offloading worth it now? (PC, mode, room, this repo's box)
  box run --reason "why" [--box name | --new] [--size S | --gpu] [--env K=V]... [--timeout 30m] -- <cmd...>
                                            sync this worktree to a box and run cmd
  box job <id> [--offset N] [--limit N]     reattach to a job, or read a piece of its log
  box jobs [--box name]                     list jobs, newest first
  box cancel <id>                           stop a running job
  box pull <id|box> <remote-path> <local-dir>
                                            copy a file or directory from a box into local-dir
  box new [name] [--ttl 2h] [--size S | --gpu] [--reason "why"]
                                            launch a box with a positive TTL
  box rm <name>                             delete a box
  box ls                                    list boxes
  box mcp                                   serve MCP on stdio
  box join [--leave]                        register box mcp with Claude Code and Codex, install the skill
  box ui [--url U]                          open the PC's dashboard in the browser
  box app [--url U] [--wait]                open the dashboard in a window of its own

--reason (required for run): one or two sentences, 10 to 300 characters, on
why this needs a box. The PC's owner sees it on the dashboard.

Sizes (CPUs in Shared/Full mode, RAM, disk):
  %s.
--size sets the size of a box that box new or box run launches (default
medium). A reused box of another size isn't resized: the run gets a throwaway.
--gpu is --size gpu: the PC's RTX 3070, one GPU box at a time. box run --gpu
uses a throwaway. The tray stops a GPU box when a game starts or in My PC.

box run exits with the command's exit code, 124 when --timeout fired, or 75
when the PC or box is paused, there is no room, or the GPU is taken or was
stopped. A job paused mid-run resumes if the PC's owner switches back within
15 minutes (4 hours once its memory is moved to swap), else it is lost; box job
reattaches. A relative remote path in box pull starts at the job's checkout.
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
	fs := flag.NewFlagSet("box "+args[0], flag.ContinueOnError)
	fs.SetOutput(a.Err)
	fs.Usage = func() { fmt.Fprint(a.Err, usageText()) }
	fail := func(err error) int {
		fmt.Fprintln(a.Err, "box:", err)
		return 1
	}
	code := func(r JobResult, err error) int {
		c := r.Code
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
		ttl := fs.Duration("ttl", idleTTL, "lifetime, stored as user.expires")
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
		isNew := fs.Bool("new", false, "use a throwaway box, deleted afterwards")
		ttl := fs.Duration("ttl", idleTTL, "initial lifetime of a --new box before the job starts")
		timeout := fs.Duration("timeout", 0, "kill the command after this long (exit 124)")
		size := fs.String("size", "", "size of a box this run launches: "+mode.SizeNames()+" (default "+mode.DefaultSize+")")
		gpu := fs.Bool("gpu", false, "same as --size gpu: run in a throwaway GPU box")
		envs := envFlags{}
		fs.Var(envs, "env", "K=V passed to the command (repeatable; no secrets)")
		reason := fs.String("reason", "", "required: one or two sentences (10 to 300 characters) on why this run needs a box")
		if fs.Parse(args[1:]) != nil {
			return 2
		}
		if *box != "" && *isNew {
			fmt.Fprintln(a.Err, "box: --box and --new don't mix")
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
		return code(a.Run(ctx, RunOpts{Dir: dir, Cmd: fs.Args(), Box: *box, New: *isNew, TTL: *ttl, Env: envs, Timeout: *timeout, Size: *size, Reason: why}))
	case "job":
		offset := fs.Int64("offset", 0, "print the log from this byte, without waiting")
		limit := fs.Int64("limit", 0, "print at most this many bytes of the log (default 32KiB)")
		if parseInterspersed(fs, args[1:]) != nil || fs.NArg() != 1 {
			fmt.Fprint(a.Err, usageText())
			return 2
		}
		paged := false
		fs.Visit(func(f *flag.Flag) { paged = paged || f.Name == "offset" || f.Name == "limit" })
		if !paged {
			return code(a.Job(ctx, fs.Arg(0), 0))
		}
		p, err := a.LogPage(ctx, fs.Arg(0), *offset, *limit)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintln(a.Err, "box: "+p.pageHead())
		io.WriteString(a.Out, p.Text)
		return 0
	case "status":
		if fs.Parse(args[1:]) != nil || fs.NArg() != 0 {
			fmt.Fprint(a.Err, usageText())
			return 2
		}
		dir, _ := os.Getwd()
		s := a.Status(ctx, dir)
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
		jobs, err := a.Jobs(ctx, *box)
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
		cancelled, c, err := a.Cancel(ctx, fs.Arg(0))
		if err != nil {
			return fail(err)
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
		if err := serveMCP(ctx, a, stdin, a.Out); err != nil {
			return fail(err)
		}
		return 0
	case "ui":
		return runUI(ctx, a, fs, args[1:])
	case "app":
		return runApp(ctx, a, fs, args[1:])
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
