package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	CodexHome       string // $CODEX_HOME, if set
	GeminiHome      string // $GEMINI_HOME, if set
	T3Home          string // $T3CODE_HOME, if set (else ~/.t3)
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
		Home: home, ClaudeConfigDir: os.Getenv("CLAUDE_CONFIG_DIR"), CodexHome: os.Getenv("CODEX_HOME"),
		GeminiHome: os.Getenv("GEMINI_HOME"), T3Home: os.Getenv("T3CODE_HOME"),
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

// claudeDirs are Claude Code's config directories: ~/.claude and, when set,
// $CLAUDE_CONFIG_DIR.
func (c joinConfig) claudeDirs() []string {
	dirs := []string{filepath.Join(c.Home, ".claude")}
	if c.ClaudeConfigDir != "" && !strings.EqualFold(filepath.Clean(c.ClaudeConfigDir), filepath.Clean(dirs[0])) {
		dirs = append(dirs, filepath.Clean(c.ClaudeConfigDir))
	}
	return dirs
}

// codexHome is Codex's home: $CODEX_HOME, else ~/.codex.
func (c joinConfig) codexHome() string {
	if c.CodexHome != "" {
		return c.CodexHome
	}
	return filepath.Join(c.Home, ".codex")
}

// skillPaths returns every user skill location for Claude Code, Codex and
// Antigravity, installed or not: leave and skill-sync look in all of them.
func (c joinConfig) skillPaths() []string {
	var paths []string
	for _, d := range c.claudeDirs() {
		paths = append(paths, filepath.Join(d, "skills", "box", "SKILL.md"))
	}
	paths = append(paths, filepath.Join(c.codexHome(), "skills", "box", "SKILL.md"))
	for _, h := range c.antigravityHomes() {
		paths = append(paths, filepath.Join(h, "config", "skills", "box", "SKILL.md"))
	}
	return paths
}

// installed is what box join sets up: the agents on this machine. As in mur
// join, an agent is there when its CLI is on PATH or its config directory
// exists, so join never makes a folder for an agent nobody installed.
type installed struct {
	cli        map[string]string // agent CLI name to its path, for those on PATH
	claudeDirs []string
	codexHome  string // "" without Codex
	agHomes    []string
}

func (c joinConfig) findAgents() installed {
	f := installed{cli: map[string]string{}}
	if c.LookPath != nil {
		for _, a := range agentCLIs(c.Exe) {
			if bin, err := c.LookPath(a.name); err == nil {
				f.cli[a.name] = bin
			}
		}
	}
	// The CLI's own folder counts even before it exists: Claude Code reads
	// $CLAUDE_CONFIG_DIR when set, else ~/.claude.
	inUse := filepath.Join(c.Home, ".claude")
	if c.ClaudeConfigDir != "" {
		inUse = filepath.Clean(c.ClaudeConfigDir)
	}
	for _, d := range c.claudeDirs() {
		if isDir(d) || f.cli["claude"] != "" && strings.EqualFold(d, inUse) {
			f.claudeDirs = append(f.claudeDirs, d)
		}
	}
	if h := c.codexHome(); isDir(h) || f.cli["codex"] != "" {
		f.codexHome = h
	}
	f.agHomes = c.antigravityHomes()
	return f
}

// skillPaths are the skill files join writes: one per agent home it found.
func (f installed) skillPaths() []string {
	var paths []string
	for _, d := range f.claudeDirs {
		paths = append(paths, filepath.Join(d, "skills", "box", "SKILL.md"))
	}
	if f.codexHome != "" {
		paths = append(paths, filepath.Join(f.codexHome, "skills", "box", "SKILL.md"))
	}
	for _, h := range f.agHomes {
		paths = append(paths, filepath.Join(h, "config", "skills", "box", "SKILL.md"))
	}
	return paths
}

// manualMCP is the server entry to add by hand to an agent join doesn't set up.
func manualMCP(exe string) string {
	cmd, _ := json.Marshal(exe)
	return "{\n  \"mcpServers\": {\n    \"box\": { \"command\": " + string(cmd) + ", \"args\": [\"mcp\"] }\n  }\n}\n"
}

// antigravityHomes returns each Antigravity home directory that is configured
// or installed: $GEMINI_HOME (if present), standalone ~/.gemini (if present or `agy` is on
// PATH), and every initialized T3 Code Antigravity provider directory under
// ~/.t3/userdata/providers/antigravity/ (or $T3CODE_HOME).
func (c joinConfig) antigravityHomes() []string {
	var homes []string
	add := func(dir string) {
		if dir == "" {
			return
		}
		clean := filepath.Clean(dir)
		for _, h := range homes {
			if strings.EqualFold(h, clean) {
				return
			}
		}
		homes = append(homes, clean)
	}
	if c.GeminiHome != "" && isDir(c.GeminiHome) {
		add(c.GeminiHome)
	}
	if c.Home != "" {
		standalone := filepath.Join(c.Home, ".gemini")
		hasAgy := false
		if c.LookPath != nil {
			_, err := c.LookPath("agy")
			hasAgy = err == nil
		}
		if hasAgy || isDir(filepath.Join(standalone, "antigravity")) ||
			isDir(filepath.Join(standalone, "antigravity-cli")) ||
			isDir(filepath.Join(standalone, "config")) {
			add(standalone)
		}
	}
	t3Home := c.T3Home
	if t3Home == "" && c.Home != "" {
		t3Home = filepath.Join(c.Home, ".t3")
	}
	if t3Home != "" {
		providers := filepath.Join(t3Home, "userdata", "providers", "antigravity")
		if ents, err := os.ReadDir(providers); err == nil {
			for _, e := range ents {
				p := filepath.Join(providers, e.Name())
				if isDir(p) {
					add(p)
				}
			}
		}
	}
	return homes
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// setAntigravityMCP adds box mcp to an Antigravity config/mcp_config.json
// file, or with leave removes it. Other servers, key order and a UTF-8 BOM are
// kept. A symlinked file is written through the link.
func setAntigravityMCP(path, exe string, leave bool) (string, error) {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		path = p
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if leave {
			return "MCP server box wasn't registered (" + path + ")", nil
		}
	}
	bom := bytes.HasPrefix(b, utf8BOM)
	b = bytes.TrimPrefix(b, utf8BOM)
	root := &object{}
	if len(bytes.TrimSpace(b)) > 0 {
		v, err := decodeOrdered(b)
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		o, ok := v.(*object)
		if !ok {
			return "", fmt.Errorf("%s: not a JSON object", path)
		}
		root = o
	}
	servers, _ := root.get("mcpServers").(*object)
	if servers == nil && root.has("mcpServers") {
		return "", fmt.Errorf("%s: mcpServers isn't an object", path)
	}
	if servers == nil {
		servers = &object{}
	}
	if leave {
		if !servers.has("box") {
			return "MCP server box wasn't registered (" + path + ")", nil
		}
		servers.del("box")
		if len(servers.keys) == 0 {
			root.del("mcpServers")
		}
		return "removed MCP server box (" + path + ")", writeOrdered(path, root, bom)
	}
	cur, ok := servers.get("box").(*object)
	if ok && sameAntigravityEntry(cur, exe) {
		return "MCP server box already registered (" + path + ")", nil
	}
	entry := cur
	if entry == nil {
		entry = &object{}
	}
	entry.set("command", exe)
	entry.set("args", []any{"mcp"})
	servers.set("box", entry)
	root.set("mcpServers", servers)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return fmt.Sprintf("registered MCP server box: %s mcp (%s)", exe, path), writeOrdered(path, root, bom)
}

func sameAntigravityEntry(o *object, exe string) bool {
	cmd, ok := o.get("command").(string)
	if !ok || cmd != exe {
		return false
	}
	args, ok := o.get("args").([]any)
	if !ok || len(args) != 1 {
		return false
	}
	s, ok := args[0].(string)
	return ok && s == "mcp"
}

// join registers box mcp as a user-scope MCP server named box with each
// agent it finds installed (Claude Code, Codex and Antigravity), installs the
// box skill, and adds box hook to Claude Code's settings (hook.go); leave
// undoes all three wherever they are.
func join(ctx context.Context, c joinConfig, leave bool) error {
	if !leave && !filepath.IsAbs(c.Exe) {
		return fmt.Errorf("box binary path %q isn't absolute", c.Exe)
	}
	f := c.findAgents()
	if !leave && len(f.claudeDirs) == 0 && f.codexHome == "" && len(f.agHomes) == 0 {
		fmt.Fprintf(c.Out, "box join found no Claude Code, Codex or Antigravity here (no CLI on PATH, no config folder), so it changed nothing.\n"+
			"For another agent that speaks MCP, add this server to its MCP config by hand:\n\n%s", manualMCP(c.Exe))
		return errors.New("no coding agent found")
	}
	var errs []error
	var setUp, skipped []string
	for _, a := range agentCLIs(c.Exe) {
		bin, onPath := f.cli[a.name]
		present := onPath || a.name == "claude" && len(f.claudeDirs) > 0 || a.name == "codex" && f.codexHome != ""
		if !present {
			fmt.Fprintf(c.Out, "%s: not installed, skipped\n", a.name)
			skipped = append(skipped, a.name)
			continue
		}
		if !onPath {
			setUp = append(setUp, a.name+" (skill only)")
			if !leave {
				fmt.Fprintf(c.Out, "%s: %s isn't on PATH, so the MCP server isn't registered; with it on PATH, run box join again or %s %s\n",
					a.name, a.name, a.name, strings.Join(a.add, " "))
			}
			continue
		}
		setUp = append(setUp, a.name)
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
	if len(f.agHomes) == 0 {
		fmt.Fprintf(c.Out, "antigravity: not installed, skipped\n")
		skipped = append(skipped, "antigravity")
	} else {
		setUp = append(setUp, "antigravity")
	}
	for _, h := range f.agHomes {
		p := filepath.Join(h, "config", "mcp_config.json")
		did, err := setAntigravityMCP(p, c.Exe, leave)
		if err != nil {
			errs = append(errs, fmt.Errorf("antigravity: %w", err))
			continue
		}
		if did != "" {
			fmt.Fprintf(c.Out, "antigravity: %s\n", did)
		}
	}
	// The hook goes in each Claude Code config directory join found; leave
	// takes it out wherever it is.
	hookAt := c.claudeSettingsPaths()
	skillAt := c.skillPaths()
	if !leave {
		hookAt = nil
		for _, d := range f.claudeDirs {
			hookAt = append(hookAt, filepath.Join(d, "settings.json"))
		}
		skillAt = f.skillPaths()
	}
	for _, p := range hookAt {
		if _, err := os.Stat(p); leave && err != nil {
			continue
		}
		did, err := setHook(p, c.Exe, leave)
		if err != nil {
			errs = append(errs, fmt.Errorf("claude hook: %w", err))
			continue
		}
		if did != "" {
			fmt.Fprintf(c.Out, "claude: %s\n", did)
		}
	}
	for _, p := range skillAt {
		existing, err := os.ReadFile(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		if err == nil && string(existing) != skills.Box {
			errs = append(errs, fmt.Errorf("existing skill at %s differs from the embedded box skill", p))
			continue
		}
		if leave {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
				continue
			}
			os.Remove(filepath.Dir(p)) // the box folder, if now empty
			fmt.Fprintf(c.Out, "skill: removed %s\n", p)
			continue
		}
		if err == nil {
			fmt.Fprintf(c.Out, "skill: already installed %s\n", p)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			errs = append(errs, err)
			continue
		}
		file, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		_, writeErr := io.WriteString(file, skills.Box)
		if err := errors.Join(writeErr, file.Close()); err != nil {
			os.Remove(p)
			errs = append(errs, err)
			continue
		}
		fmt.Fprintf(c.Out, "skill: installed %s\n", p)
	}
	if !leave {
		fmt.Fprintf(c.Out, "set up: %s", strings.Join(setUp, ", "))
		if len(skipped) > 0 {
			fmt.Fprintf(c.Out, "; skipped, not installed: %s", strings.Join(skipped, ", "))
		}
		fmt.Fprintln(c.Out)
	}
	return errors.Join(errs...)
}
