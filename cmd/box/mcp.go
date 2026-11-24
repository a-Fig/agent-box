package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// A hand-written MCP server: JSON-RPC 2.0, one message per line on stdio.

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

const pausedHint = "Exit code 75 means the PC or the box is paused: run the command locally instead."

var tools = []tool{
	{"box_new", "Launch a Linux box on the PC. Returns its name.", obj(map[string]any{
		"name": str("Box name (lowercase letters, digits, hyphens). Omit for a random one."),
		"ttl":  str(`How long the box is meant to live, like "8h". Default 8h.`),
	})},
	{"box_run", "Sync a local git worktree (uncommitted and untracked files included, ignored files excluded) to a box and run a shell command there. Returns the output tail and the exit code. " + pausedHint, obj(map[string]any{
		"dir":     str("Absolute path of the local git worktree."),
		"command": str(`Shell command to run in the repo's root, like "go test ./...".`),
		"box":     str("Box to use. Default: the repo's own reused box."),
		"new":     map[string]any{"type": "boolean", "description": "Use a throwaway box, deleted after the run."},
		"wait":    str(`Stop waiting after this long, like "10m" (the job keeps running; use box_job). Default 10m.`),
	}, "dir", "command")},
	{"box_job", "Wait for a job started by box_run and return its output tail and exit code. " + pausedHint, obj(map[string]any{
		"id":   str("Job ID printed by box_run."),
		"wait": str(`Stop waiting after this long. Default 10m.`),
	}, "id")},
	{"box_rm", "Delete a box.", obj(map[string]any{"name": str("Box name.")}, "name")},
	{"box_ls", "List boxes with their status and expiry.", obj(map[string]any{})},
}

// serveMCP answers requests from in on out until in closes. Tool calls run
// concurrently so a long box_run doesn't block pings.
func serveMCP(ctx context.Context, a *App, in io.Reader, out io.Writer) error {
	var mu sync.Mutex
	var wg sync.WaitGroup
	enc := json.NewEncoder(out)
	send := func(m rpcMsg) {
		m.JSONRPC = "2.0"
		mu.Lock()
		defer mu.Unlock()
		enc.Encode(m)
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req rpcMsg
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			send(rpcMsg{ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
			continue
		}
		if len(req.ID) == 0 { // notification: no reply
			continue
		}
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			json.Unmarshal(req.Params, &p)
			if p.ProtocolVersion == "" {
				p.ProtocolVersion = "2025-06-18"
			}
			send(rpcMsg{ID: req.ID, Result: map[string]any{
				"protocolVersion": p.ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "box", "version": "0.1.0"},
			}})
		case "ping":
			send(rpcMsg{ID: req.ID, Result: map[string]any{}})
		case "tools/list":
			send(rpcMsg{ID: req.ID, Result: map[string]any{"tools": tools}})
		case "tools/call":
			wg.Add(1)
			go func(req rpcMsg) {
				defer wg.Done()
				text, isErr := callTool(ctx, a, req.Params)
				send(rpcMsg{ID: req.ID, Result: map[string]any{
					"content": []map[string]any{{"type": "text", "text": text}},
					"isError": isErr,
				}})
			}(req)
		default:
			send(rpcMsg{ID: req.ID, Error: &rpcError{-32601, "method not found: " + req.Method}})
		}
	}
	wg.Wait()
	return sc.Err()
}

type toolArgs struct {
	Name    string `json:"name"`
	TTL     string `json:"ttl"`
	Dir     string `json:"dir"`
	Command string `json:"command"`
	Box     string `json:"box"`
	New     bool   `json:"new"`
	ID      string `json:"id"`
	Wait    string `json:"wait"`
}

// callTool runs one tool and returns its text and whether it failed.
func callTool(ctx context.Context, base *App, params json.RawMessage) (string, bool) {
	var p struct {
		Name      string   `json:"name"`
		Arguments toolArgs `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "bad arguments: " + err.Error(), true
	}
	args := p.Arguments
	out := &tailBuffer{max: 32 << 10}
	a := *base
	a.Out, a.Err = out, out

	dur := func(s string, def time.Duration) (time.Duration, error) {
		if s == "" {
			return def, nil
		}
		return time.ParseDuration(s)
	}
	result := func(code int, err error) (string, bool) {
		if err != nil {
			return out.String() + "\nerror: " + err.Error(), true
		}
		return fmt.Sprintf("%s\nexit code: %d", out.String(), code), false
	}

	switch p.Name {
	case "box_new":
		ttl, err := dur(args.TTL, defaultTTL)
		if err != nil {
			return err.Error(), true
		}
		name, err := a.New(ctx, args.Name, ttl)
		if err != nil {
			return err.Error(), true
		}
		return name, false
	case "box_run":
		wait, err := dur(args.Wait, 10*time.Minute)
		if err != nil {
			return err.Error(), true
		}
		if args.Dir == "" || args.Command == "" {
			return "dir and command are required", true
		}
		return result(a.Run(ctx, RunOpts{Dir: args.Dir, Cmd: []string{args.Command}, Box: args.Box, New: args.New, Wait: wait}))
	case "box_job":
		wait, err := dur(args.Wait, 10*time.Minute)
		if err != nil {
			return err.Error(), true
		}
		return result(a.Job(ctx, args.ID, wait))
	case "box_rm":
		if err := a.Rm(ctx, args.Name); err != nil {
			return err.Error(), true
		}
		return "removed " + args.Name, false
	case "box_ls":
		if err := a.Ls(ctx); err != nil {
			return err.Error(), true
		}
		return out.String(), false
	}
	return "unknown tool " + p.Name, true
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu   sync.Mutex
	max  int
	buf  []byte
	lost bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
		t.lost = true
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lost {
		return "[earlier output cut]\n" + string(t.buf)
	}
	return string(t.buf)
}
