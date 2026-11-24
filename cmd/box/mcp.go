package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
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
	// OutputSchema describes structuredContent, for the job tools.
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
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
	}), nil},
	{"box_run", "Sync a local git worktree (uncommitted and untracked files included, ignored files excluded) to a box and run a shell command there. The first line of the result gives the job ID, box and exit code; the output tail follows. " + pausedHint, obj(map[string]any{
		"dir":     str("Absolute path of the local git worktree."),
		"command": str(`Shell command to run in the repo's root, like "go test ./...".`),
		"box":     str("Box to use. Default: the repo's own reused box."),
		"new":     map[string]any{"type": "boolean", "description": "Use a throwaway box, deleted after the run."},
		"wait":    str(`Stop waiting after this long, like "10m" (the job keeps running; use box_job). Default 10m.`),
	}, "dir", "command"), jobOutput},
	{"box_job", "Wait for a job started by box_run and return its exit code (first line) and output tail. A throwaway box from box_run new is deleted once its job has ended. " + pausedHint, obj(map[string]any{
		"id":   str("Job ID printed by box_run."),
		"wait": str(`Stop waiting after this long. Default 10m.`),
	}, "id"), jobOutput},
	{"box_rm", "Delete a box.", obj(map[string]any{"name": str("Box name.")}, "name"), nil},
	{"box_ls", "List boxes with their status and expiry.", obj(map[string]any{}), nil},
}

// jobOutput is the structuredContent of box_run and box_job (a JobResult).
var jobOutput = obj(map[string]any{
	"job":      str("Job ID, for box_job."),
	"box":      str("Box the job runs in."),
	"state":    str("done (exitCode is the command's), detached (still running), paused (exitCode 75), or empty after an error."),
	"exitCode": map[string]any{"type": "integer"},
}, "exitCode")

// errClientCancelled is the cause of a tool call's context that the client
// cancelled with notifications/cancelled.
var errClientCancelled = errors.New("cancelled by the client")

// serveMCP answers requests from in on out until in closes. Tool calls run
// concurrently so a long box_run doesn't block pings, each with its own
// context that notifications/cancelled cancels.
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
	var callsMu sync.Mutex
	calls := map[string]context.CancelCauseFunc{} // by request id
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
			if req.Method == "notifications/cancelled" {
				var p struct {
					RequestID json.RawMessage `json:"requestId"`
				}
				json.Unmarshal(req.Params, &p)
				callsMu.Lock()
				if cancel := calls[string(p.RequestID)]; cancel != nil {
					cancel(errClientCancelled)
				}
				callsMu.Unlock()
			}
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
			// Registered before the goroutine starts, so a cancel that
			// follows the call on the wire always finds it.
			callCtx, cancel := context.WithCancelCause(ctx)
			key := string(req.ID)
			callsMu.Lock()
			calls[key] = cancel
			callsMu.Unlock()
			wg.Add(1)
			go func(req rpcMsg) {
				defer wg.Done()
				res := callTool(callCtx, a, req.Params)
				callsMu.Lock()
				delete(calls, key)
				callsMu.Unlock()
				cancelled := context.Cause(callCtx) == errClientCancelled
				cancel(nil)
				if cancelled { // the client has given up on this request
					return
				}
				send(rpcMsg{ID: req.ID, Result: res})
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

func textResult(text string, isErr bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	}
}

// jobResult reports a box_run or box_job. The job ID, box and exit code go
// in structuredContent and on the first text line, never in the log tail
// that follows, so cutting a long log can't lose them.
func jobResult(r JobResult, err error, log string) map[string]any {
	head := "exit code: " + strconv.Itoa(r.Code)
	if r.State != "" {
		head = r.State + ", " + head
	}
	if r.Job != "" {
		head = fmt.Sprintf("job %s on box %s: %s", r.Job, r.Box, head)
	}
	text := head + "\n" + log
	if err != nil {
		text += "\nerror: " + err.Error()
	}
	res := textResult(text, err != nil)
	res["structuredContent"] = r
	return res
}

// callTool runs one tool and returns its MCP result.
func callTool(ctx context.Context, base *App, params json.RawMessage) map[string]any {
	var p struct {
		Name      string   `json:"name"`
		Arguments toolArgs `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return textResult("bad arguments: "+err.Error(), true)
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

	switch p.Name {
	case "box_new":
		ttl, err := dur(args.TTL, defaultTTL)
		if err != nil {
			return textResult(err.Error(), true)
		}
		name, err := a.New(ctx, args.Name, ttl)
		if err != nil {
			return textResult(err.Error(), true)
		}
		return textResult(name, false)
	case "box_run":
		wait, err := dur(args.Wait, 10*time.Minute)
		if err != nil {
			return textResult(err.Error(), true)
		}
		if args.Dir == "" || args.Command == "" {
			return textResult("dir and command are required", true)
		}
		r, err := a.Run(ctx, RunOpts{Dir: args.Dir, Cmd: []string{args.Command}, Box: args.Box, New: args.New, Wait: wait})
		return jobResult(r, err, out.String())
	case "box_job":
		wait, err := dur(args.Wait, 10*time.Minute)
		if err != nil {
			return textResult(err.Error(), true)
		}
		r, err := a.Job(ctx, args.ID, wait)
		return jobResult(r, err, out.String())
	case "box_rm":
		if err := a.Rm(ctx, args.Name); err != nil {
			return textResult(err.Error(), true)
		}
		return textResult("removed "+args.Name, false)
	case "box_ls":
		if err := a.Ls(ctx); err != nil {
			return textResult(err.Error(), true)
		}
		return textResult(out.String(), false)
	}
	return textResult("unknown tool "+p.Name, true)
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
