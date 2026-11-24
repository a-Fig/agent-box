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
)

const usage = `usage:
  box new [name] [--ttl 8h]                 launch a box (--ttl 0: no expiry)
  box run [--box name | --new] -- <cmd...>  sync this worktree to a box and run cmd
  box job <id>                              reattach to a job
  box rm <name>                             delete a box
  box ls                                    list boxes
  box mcp                                   serve MCP on stdio

box run exits with the command's exit code, or 75 when the PC or box is paused.
Settings: BOX_REMOTE (pc), BOX_PROJECT (agents), BOX_IMAGE (agent-base).
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, newApp(), os.Args[1:], os.Stdin))
}

func run(ctx context.Context, a *App, args []string, stdin io.Reader) int {
	if len(args) == 0 {
		fmt.Fprint(a.Err, usage)
		return 2
	}
	fs := flag.NewFlagSet("box "+args[0], flag.ContinueOnError)
	fs.SetOutput(a.Err)
	fs.Usage = func() { fmt.Fprint(a.Err, usage) }
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
		ttl := fs.Duration("ttl", defaultTTL, "lifetime, stored as user.expires")
		if parseInterspersed(fs, args[1:]) != nil || fs.NArg() > 1 {
			return 2
		}
		name, err := a.New(ctx, fs.Arg(0), *ttl)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintln(a.Out, name)
		return 0
	case "run":
		box := fs.String("box", "", "box to use (default: the repo's own box)")
		isNew := fs.Bool("new", false, "use a throwaway box, deleted afterwards")
		ttl := fs.Duration("ttl", defaultTTL, "lifetime of a --new box")
		if fs.Parse(args[1:]) != nil {
			return 2
		}
		if *box != "" && *isNew {
			fmt.Fprintln(a.Err, "box: --box and --new don't mix")
			return 2
		}
		dir, err := os.Getwd()
		if err != nil {
			return fail(err)
		}
		return code(a.Run(ctx, RunOpts{Dir: dir, Cmd: fs.Args(), Box: *box, New: *isNew, TTL: *ttl}))
	case "job":
		if fs.Parse(args[1:]) != nil || fs.NArg() != 1 {
			fmt.Fprint(a.Err, usage)
			return 2
		}
		return code(a.Job(ctx, fs.Arg(0), 0))
	case "rm":
		if fs.Parse(args[1:]) != nil || fs.NArg() != 1 {
			fmt.Fprint(a.Err, usage)
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
	case "-h", "--help", "help":
		fmt.Fprint(a.Out, usage)
		return 0
	}
	fmt.Fprint(a.Err, usage)
	return 2
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
