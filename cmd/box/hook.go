package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The hook: `box hook` runs after each of a Claude Code agent's tool calls
// (PostToolUse). T3 gives a busy thread a pushed message only once its turn
// ends ("A busy thread gets it as its next turn; nothing is interrupted", t3
// thread send --help, 2026-11-25), which left a working agent up to ten
// minutes behind its own jobs. The hook tells it mid-turn instead: each job of
// this session that has ended and not reached the agent comes back as
// additionalContext, and is marked notified so nothing tells it again.
//
// It reads only the state files the watcher writes: no network, and any error
// is silence, so it never fails or slows the tool call it follows.
//
// The watcher and the hook take turns. A hook that ran for this thread lately
// leaves <thread>.hookseen; the watcher then waits hookGrace after a job ends
// before it sends, so the hook can deliver mid-turn. If the hook does, the
// watcher sends nothing, and the agent gets no second turn for it. If not (the
// agent sits in one long tool call, or is idle), the watcher sends and marks
// the job notified, and the hook stays quiet.

const (
	hookGrace  = 8 * time.Second  // how long the watcher leaves the job to the hook
	hookFresh  = 10 * time.Minute // a hook seen this recently is taken to be installed
	hookBudget = 3 * time.Second  // past this the hook stays quiet: join gives it 5s
)

var validThread = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (a *App) hookSeenPath(thread string) string {
	return filepath.Join(a.StateDir, thread+".hookseen")
}

// hookActive says whether this thread's hook has run lately, so it will see
// an ended job at the agent's next tool call.
func (a *App) hookActive(thread string) bool {
	if !validThread.MatchString(thread) {
		return false
	}
	st, err := os.Stat(a.hookSeenPath(thread))
	return err == nil && a.Now().Sub(st.ModTime()) < hookFresh
}

// Hook is `box hook`: it writes Claude Code's PostToolUse output for the
// ended jobs of this session, or nothing.
func (a *App) Hook(stdin io.Reader, out io.Writer) {
	// The event. The thread comes from the environment, which a subagent and
	// a claude started from the agent's shell inherit too: a subagent's event
	// carries agent_id, and a child claude has its own session_id. Either
	// would take the notice from the agent it is for.
	var ev struct {
		SessionID string `json:"session_id"`
		AgentID   string `json:"agent_id"`
	}
	began := time.Now()
	if stdin != nil {
		// The whole event, however large (a tool's output can be megabytes):
		// one cut short would read as no agent_id. An event it can't read
		// leaves the end to the watcher.
		if json.NewDecoder(stdin).Decode(&ev) != nil {
			return
		}
		io.Copy(io.Discard, stdin)
	}
	thread := a.thread()
	if !validThread.MatchString(thread) || a.StateDir == "" || ev.AgentID != "" {
		return
	}
	if _, err := os.Stat(a.StateDir); err != nil {
		return // box has never pushed on this laptop
	}
	now := a.Now()
	if os.Chtimes(a.hookSeenPath(thread), now, now) != nil && os.WriteFile(a.hookSeenPath(thread), nil, 0o644) == nil {
		os.Chtimes(a.hookSeenPath(thread), now, now)
	}
	ents, err := os.ReadDir(a.StateDir)
	if err != nil {
		return
	}
	have := map[string]bool{}
	for _, e := range ents {
		have[e.Name()] = true
	}
	var msgs, jobs []string
	baseDir := a.t3BaseDir()
	for _, e := range ents {
		job, ok := strings.CutSuffix(e.Name(), ".ended")
		if !ok || have[job+".notified"] || !have[job+".json"] {
			continue
		}
		rec, err := a.readWatchRecord(job)
		var end jobEnd
		if err != nil || rec.Thread != thread || !rec.inProfile(baseDir) || readJSON(a.statePath(job, ".ended"), &end) != nil ||
			rec.Session != "" && ev.SessionID != "" && rec.Session != ev.SessionID {
			continue
		}
		msgs, jobs = append(msgs, endMessage(rec, end)), append(jobs, job)
	}
	// Claude Code drops a hook's output at its timeout (5s, set by join). A
	// hook this slow says nothing, so it can't mark an end it may not deliver.
	if len(msgs) == 0 || time.Since(began) > hookBudget {
		return
	}
	b, err := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":     "PostToolUse",
		"additionalContext": strings.Join(msgs, "\n\n"),
	}})
	if err != nil {
		return
	}
	if _, err := out.Write(append(b, '\n')); err != nil {
		return
	}
	// Marked only once the output is written: a hook killed before this
	// leaves the end owed, to be sent again, rather than lost. Two hooks at
	// once (parallel tool calls) may both tell it, which is fine.
	for _, job := range jobs {
		a.claimBy(job, ".notified", "hook")
	}
}

// waitForHook gives the hook hookGrace to deliver an ended job, when this
// thread's hook is running. It says whether the job reached the agent.
func (a *App) waitForHook(ctx context.Context, rec jobRecord) bool {
	if !a.hookActive(rec.Thread) {
		return false
	}
	const step = 250 * time.Millisecond
	for range int(hookGrace / step) {
		if a.exists(rec.Job, ".notified") {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(a.watchWait(step)):
		}
	}
	return a.exists(rec.Job, ".notified")
}

// Claude Code's settings: box join adds the hook to them and leave removes
// it. The file is edited in place, its other keys kept in their order.

// hookCommand is the command line Claude Code runs.
func hookCommand(exe string) string { return `"` + filepath.ToSlash(exe) + `" hook` }

// isBoxHook matches a command that box join added, from any box binary.
var isBoxHook = regexp.MustCompile(`(?i)(^|[/\\"])box(\.exe)?"?\s+hook\s*$`)

// claudeSettingsPaths are the settings files of Claude Code's config
// directories, installed or not.
func (c joinConfig) claudeSettingsPaths() []string {
	var paths []string
	for _, d := range c.claudeDirs() {
		paths = append(paths, filepath.Join(d, "settings.json"))
	}
	return paths
}

// setHook adds box's PostToolUse hook to a settings file, or with leave
// removes it. It says what it did, "" for nothing. A file it changes keeps its
// keys, their order and a BOM, but is written back indented by 2 spaces with
// LF line ends: leave restores the settings, not always the file's bytes. A
// symlinked file is written through the link.
func setHook(path, exe string, leave bool) (string, error) {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		path = p
	}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	bom := bytes.HasPrefix(b, utf8BOM)
	b = bytes.TrimPrefix(b, utf8BOM)
	write := func(v any) error { return writeOrdered(path, v, bom) }
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
	hooks, _ := root.get("hooks").(*object)
	if hooks == nil && root.has("hooks") {
		return "", fmt.Errorf("%s: hooks isn't an object", path)
	}
	if hooks == nil {
		hooks = &object{}
	}
	groups, _ := hooks.get("PostToolUse").([]any)
	if groups == nil && hooks.has("PostToolUse") {
		return "", fmt.Errorf("%s: hooks.PostToolUse isn't a list", path)
	}
	// Take out every box hook, so join is repeatable and a moved binary is
	// replaced rather than added twice.
	removed, same := 0, false
	var kept []any
	for _, g := range groups {
		grp, ok := g.(*object)
		if !ok {
			kept = append(kept, g)
			continue
		}
		hs, _ := grp.get("hooks").([]any)
		var keep []any
		for _, h := range hs {
			if ho, ok := h.(*object); ok {
				if cmd, _ := ho.get("command").(string); isBoxHook.MatchString(cmd) {
					removed++
					same = cmd == hookCommand(exe)
					continue
				}
			}
			keep = append(keep, h)
		}
		if len(hs) > 0 && len(keep) == 0 {
			continue // the group held only box's hook
		}
		if len(keep) != len(hs) {
			grp.set("hooks", keep)
		}
		kept = append(kept, g)
	}
	if leave {
		if removed == 0 {
			return "", nil
		}
		switch {
		case len(kept) > 0:
			hooks.set("PostToolUse", kept)
		default:
			hooks.del("PostToolUse")
		}
		if len(hooks.keys) == 0 {
			root.del("hooks")
		}
		return "removed the hook from " + path, write(root)
	}
	if removed == 1 && same {
		return "hook already in " + path, nil // as it is: no rewrite
	}
	entry := &object{}
	entry.set("type", "command")
	entry.set("command", hookCommand(exe))
	entry.set("timeout", json.Number("5"))
	group := &object{}
	group.set("matcher", "*")
	group.set("hooks", []any{entry})
	hooks.set("PostToolUse", append(kept, group))
	root.set("hooks", hooks)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if removed > 0 {
		return "replaced the hook in " + path, write(root)
	}
	return "added the hook to " + path, write(root)
}

// object is a JSON object that keeps its keys' order.
type object struct {
	keys []string
	vals map[string]any
}

func (o *object) has(k string) bool { _, ok := o.vals[k]; return ok }
func (o *object) get(k string) any  { return o.vals[k] }

func (o *object) set(k string, v any) {
	if o.vals == nil {
		o.vals = map[string]any{}
	}
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *object) del(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, x := range o.keys {
		if x == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

func (o *object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := marshalRaw(k)
		vb, err := marshalRaw(o.vals[k])
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func decodeOrdered(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	v, err := decodeValue(d)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the JSON value")
	}
	return v, nil
}

func decodeValue(d *json.Decoder) (any, error) {
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		o := &object{}
		for d.More() {
			kt, err := d.Token()
			if err != nil {
				return nil, err
			}
			v, err := decodeValue(d)
			if err != nil {
				return nil, err
			}
			o.set(kt.(string), v)
		}
		_, err := d.Token() // }
		return o, err
	case json.Delim('['):
		l := []any{}
		for d.More() {
			v, err := decodeValue(d)
			if err != nil {
				return nil, err
			}
			l = append(l, v)
		}
		_, err := d.Token() // ]
		return l, err
	}
	return t, nil
}

// marshalRaw is json.Marshal without HTML escaping, so a command's && and >
// stay as the user wrote them.
func marshalRaw(v any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

var utf8BOM = []byte{0xef, 0xbb, 0xbf}

func writeOrdered(path string, v any, bom bool) error {
	var b bytes.Buffer
	if bom {
		b.Write(utf8BOM)
	}
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	if err := e.Encode(v); err != nil {
		return err
	}
	return writeFile(path, b.Bytes())
}
