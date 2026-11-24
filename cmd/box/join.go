package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/a-Fig/agent-box/skills"
)

// joinConfig is everything box join touches, so tests can point it at a
// temporary home and fake agent CLIs.
type joinConfig struct {
	R               Runner
	LookPath        func(string) (string, error)
	Home            string // the user's home directory
	ClaudeConfigDir string // $CLAUDE_CONFIG_DIR, if set
	Exe             string // absolute path of this box binary
	Out             io.Writer
}

func defaultJoinConfig(out io.Writer) (joinConfig, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return joinConfig{}, err
	}
	exe, err := os.Executable()
	if err != nil {
		return joinConfig{}, err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	// go run builds into the temp folder and deletes the binary afterwards.
	if tmp := os.TempDir(); tmp != "" && strings.HasPrefix(strings.ToLower(exe), strings.ToLower(filepath.Clean(tmp)+string(filepath.Separator))) {
		return joinConfig{}, fmt.Errorf("%s is a temporary build (go run?); build box with go build and run join from that binary", exe)
	}
	return joinConfig{
		R: execRunner{}, LookPath: exec.LookPath,
		Home: home, ClaudeConfigDir: os.Getenv("CLAUDE_CONFIG_DIR"),
		Exe: exe, Out: out,
	}, nil
}

// agentCLI is how one coding agent's CLI adds and removes a user-scope MCP server.
type agentCLI struct {
	name        string
	add, remove []string
}

func agentCLIs(exe string) []agentCLI {
	return []agentCLI{
		{"claude", []string{"mcp", "add", "--scope", "user", "box", "--", exe, "mcp"}, []string{"mcp", "remove", "--scope", "user", "box"}},
		{"codex", []string{"mcp", "add", "box", "--", exe, "mcp"}, []string{"mcp", "remove", "box"}},
	}
}

// skillPaths are where the box skill goes: ~/.claude/skills, and the
// CLAUDE_CONFIG_DIR one when that is set somewhere else.
func (c joinConfig) skillPaths() []string {
	paths := []string{filepath.Join(c.Home, ".claude", "skills", "box", "SKILL.md")}
	if c.ClaudeConfigDir != "" {
		p := filepath.Join(c.ClaudeConfigDir, "skills", "box", "SKILL.md")
		if !strings.EqualFold(filepath.Clean(p), filepath.Clean(paths[0])) {
			paths = append(paths, p)
		}
	}
	return paths
}

// join registers box mcp as a user-scope MCP server named box with each
// agent CLI it finds and installs the box skill; leave undoes both.
func join(ctx context.Context, c joinConfig, leave bool) error {
	if !leave && !filepath.IsAbs(c.Exe) {
		return fmt.Errorf("box binary path %q isn't absolute", c.Exe)
	}
	var errs []error
	for _, a := range agentCLIs(c.Exe) {
		bin, err := c.LookPath(a.name)
		if err != nil {
			fmt.Fprintf(c.Out, "%s: not installed, skipped\n", a.name)
			continue
		}
		// Removing first makes join repeatable: add refuses an existing name.
		rm, err := c.R.Run(ctx, Cmd{Name: bin, Args: a.remove})
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.name, err))
			continue
		}
		if leave {
			if rm.Code == 0 {
				fmt.Fprintf(c.Out, "%s: removed MCP server box\n", a.name)
			} else {
				fmt.Fprintf(c.Out, "%s: MCP server box wasn't registered\n", a.name)
			}
			continue
		}
		res, err := c.R.Run(ctx, Cmd{Name: bin, Args: a.add})
		if err == nil && res.Code != 0 {
			err = fmt.Errorf("%s %s: %s", a.name, strings.Join(a.add[:2], " "), firstLine(res.Stderr+res.Stdout))
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.name, err))
			continue
		}
		fmt.Fprintf(c.Out, "%s: registered MCP server box (user scope): %s mcp\n", a.name, c.Exe)
	}
	for _, p := range c.skillPaths() {
		if leave {
			err := os.Remove(p)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
			os.Remove(filepath.Dir(p)) // the box folder, if now empty
			fmt.Fprintf(c.Out, "skill: removed %s\n", p)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.WriteFile(p, []byte(skills.Box), 0o644); err != nil {
			errs = append(errs, err)
			continue
		}
		fmt.Fprintf(c.Out, "skill: installed %s\n", p)
	}
	return errors.Join(errs...)
}
