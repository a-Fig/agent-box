package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/a-Fig/agent-box/internal/mode"
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

var pausedHint = "Exit code 75 means the PC or box is paused or busy: run the command locally. A running job that was paused resumes if the PC's owner switches back within " + frozenWindow + ", else it is lost; box_job reattaches later."

func integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

var tools = []tool{
	{"box_status", `Say in one call whether offloading to the PC is worth it now. The first line is advice like "available: room for 1 large, or 2 medium, or 4 small" or "unavailable: PC is in My PC mode: run locally"; details and structuredContent follow: reachability, mode, gaming yield, box slots, CPUs and RAM per size, whether the GPU is free, and this repo's box with its size and running jobs.`, obj(map[string]any{
		"dir": str("Absolute path of your git worktree, to report its box and running jobs. Optional."),
	}), nil},
	{"box_new", "Launch a Linux box on the PC. Returns its name.", obj(map[string]any{
		"name":   str("Box name (lowercase letters, digits, hyphens). Omit for a random one."),
		"ttl":    str(`How long the box is meant to live, like "2h". Default 2h.`),
		"size":   sizeArg,
		"gpu":    gpuArg,
		"reason": str("Optional: why you need this box, as for box_run."),
	}), nil},
	{"box_run", "Sync a local git worktree (uncommitted and untracked files included, ignored files excluded) to a box and run a shell command there. The first line of the result gives the job ID, box and exit code; the output tail follows. " + pausedHint, obj(map[string]any{
		"dir":     str("Absolute path of the local git worktree."),
		"command": str(`Shell command to run in the repo's root, like "go test ./...".`),
		"reason":  str(`One or two sentences (10-300 characters) on why this run needs a box, like "Full go test -race suite for the scheduler change; too heavy for the laptop." The PC's owner sees it.`),
		"box":     str("Reusable box to use. Default: the repo's own box."),
		"new":     map[string]any{"type": "boolean", "description": "Use a throwaway box, deleted after the run."},
		"size":    sizeArg,
		"gpu":     gpuArg,
		"wait":    str(`Stop waiting after this long, like "10m" (the job keeps running; use box_job). Default 10m.`),
		"timeout": str(`Kill the command in the box after this long, like "30m"; it then exits 124. Default: no limit.`),
		"env": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"},
			"description": "Environment variables for the command. Boxes are shared: never pass secrets; names containing TOKEN, SECRET, KEY or PASSWORD are refused."},
	}, "dir", "command", "reason"), jobOutput},
	{"box_job", "Wait for a job started by box_run and return its exit code (first line) and output tail. A throwaway box from box_run new is deleted once its job has ended. With offset or limit it doesn't wait: it returns that piece of the log, the job's state and nextOffset, so a long log can be read in pieces. " + pausedHint, obj(map[string]any{
		"id":     str("Job ID printed by box_run."),
		"wait":   str(`Stop waiting after this long. Default 10m.`),
		"offset": integer("Read the log from this byte offset, without waiting. Default 0."),
		"limit":  integer("Read at most this many bytes of the log (default 32768, at most 1048576)."),
	}, "id"), jobOutput},
	{"box_jobs", "List jobs, newest first, with state (running, done with exit code, died), start time and log size. Use it to find a job ID you lost.", obj(map[string]any{
		"box": str("Only this box. Default: every running box."),
	}), nil},
	{"box_cancel", "Stop a running job's process session. A cancelled job exits 130.", obj(map[string]any{
		"id": str("Job ID."),
	}, "id"), nil},
	{"box_pull", "Copy a file or directory (test reports, coverage, build output) from a box into a local directory, as <localDir>/<name>. It never replaces an existing file and drops symlinks. Pull right after the job: the next job on the same repo replaces the checkout.", obj(map[string]any{
		"from":       str("Job ID, or a box name."),
		"remotePath": str("Path in the box. A relative path starts at the job's checkout (/work/<repo>)."),
		"localDir":   str("Absolute path of the local directory to copy into; created if missing."),
	}, "from", "remotePath", "localDir"), nil},
	{"box_rm", "Delete a box.", obj(map[string]any{"name": str("Box name.")}, "name"), nil},
	{"box_ls", "List boxes with their status and expiry.", obj(map[string]any{}), nil},
}

// sizeArg is the size property of box_new and box_run.
var sizeArg = map[string]any{"type": "string", "enum": sizeEnum(),
	"description": "Box size: small (quick unit tests), medium (default), large (big Gradle/Android builds, memory-hungry suites), gpu (see gpu). An existing repo box of another size isn't resized; the run gets a throwaway of this size."}

// gpuArg is the gpu property of box_new and box_run.
var gpuArg = map[string]any{"type": "boolean",
	"description": "Same as size gpu: the PC's RTX 3070 (CUDA, PyTorch, GPU builds). One GPU box at a time, always a throwaway; exit 75 if it's taken. It is stopped if the PC's owner starts a game."}

func sizeEnum() []string {
	var n []string
	for _, s := range mode.Sizes {
		n = append(n, s.Name)
	}
	return n
}

// jobOutput is the structuredContent of box_run and box_job (a JobResult).
var jobOutput = obj(map[string]any{
	"job":      str("Job ID, for box_job."),
	"box":      str("Box the job runs in."),
	"state":    str("done (exitCode is the command's), detached (still running), paused (exitCode 75), or empty after an error."),
	"exitCode": map[string]any{"type": "integer"},
	"warnings": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	// Only for a log page (box_job with offset or limit).
	"offset":     map[string]any{"type": "integer"},
	"nextOffset": map[string]any{"type": "integer", "description": "Offset of the next page."},
	"logBytes":   map[string]any{"type": "integer", "description": "Size of the log so far."},
	"more":       map[string]any{"type": "boolean", "description": "The log goes on past this page, or the job is still running."},
	"logBase64":  str("Exact log bytes when this page does not contain complete UTF-8 text."),
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
	var suppressInfo atomic.Bool
	var client clientInfo // from initialize; read and written only in this loop
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
				ProtocolVersion string     `json:"protocolVersion"`
				ClientInfo      clientInfo `json:"clientInfo"`
			}
			json.Unmarshal(req.Params, &p)
			client = p.ClientInfo
			if p.ProtocolVersion == "" {
				p.ProtocolVersion = "2025-06-18"
			}
			send(rpcMsg{ID: req.ID, Result: map[string]any{
				"protocolVersion": p.ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}, "logging": map[string]any{}},
				"serverInfo":      map[string]any{"name": "box", "version": "0.1.0"},
			}})
		case "ping":
			send(rpcMsg{ID: req.ID, Result: map[string]any{}})
		case "logging/setLevel":
			var p struct{ Level string }
			json.Unmarshal(req.Params, &p)
			switch p.Level {
			case "debug", "info":
				suppressInfo.Store(false)
			case "notice", "warning", "error", "critical", "alert", "emergency":
				suppressInfo.Store(true)
			default:
				send(rpcMsg{ID: req.ID, Error: &rpcError{-32602, "invalid log level"}})
				continue
			}
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
			ca := *a // with the client, for the job's .meta
			ca.Client = client
			if ca.Client.Name == "" {
				ca.Client.Name = "mcp"
			}
			wg.Add(1)
			go func(req rpcMsg) {
				defer wg.Done()
				res := callTool(callCtx, &ca, req.Params, func(note jobNotice) {
					if suppressInfo.Load() || callCtx.Err() != nil {
						return
					}
					data := struct {
						jobNotice
						RequestID json.RawMessage `json:"requestId"`
					}{note, req.ID}
					params, _ := json.Marshal(map[string]any{"level": "info", "logger": "box", "data": data})
					send(rpcMsg{Method: "notifications/message", Params: params})
				})
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
	Name    string            `json:"name"`
	TTL     string            `json:"ttl"`
	Dir     string            `json:"dir"`
	Command string            `json:"command"`
	Box     string            `json:"box"`
	New     bool              `json:"new"`
	Size    string            `json:"size"`
	GPU     bool              `json:"gpu"`
	ID      string            `json:"id"`
	Wait    string            `json:"wait"`
	Timeout string            `json:"timeout"`
	Env     map[string]string `json:"env"`
	Offset  *int64            `json:"offset"`
	Limit   *int64            `json:"limit"`
	From    string            `json:"from"`
	Remote  string            `json:"remotePath"`
	Local   string            `json:"localDir"`
	Reason  string            `json:"reason"`
}

// pageResult reports a box_job log page: the summary line, then the bytes.
func pageResult(p Page) map[string]any {
	code := exitDetached
	switch {
	case p.Code != nil:
		code = *p.Code
	case p.State == jobDied:
		code = 1
	}
	res := textResult(p.pageHead()+"\n"+p.Text, false)
	structured := map[string]any{
		"job": p.Job, "box": p.Box, "state": p.State, "exitCode": code,
		"offset": p.Offset, "nextOffset": p.NextOffset, "logBytes": p.LogBytes, "more": p.More,
	}
	if !utf8.ValidString(p.Text) {
		structured["logBase64"] = base64.StdEncoding.EncodeToString([]byte(p.Text))
	}
	res["structuredContent"] = structured
	return res
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
func jobResult(r JobResult, err error, log string, warnings []string) map[string]any {
	head := "exit code: " + strconv.Itoa(r.Code)
	if r.State != "" {
		head = r.State + ", " + head
	}
	if r.Job != "" {
		head = fmt.Sprintf("job %s on box %s: %s", r.Job, r.Box, head)
	} else if r.Box != "" {
		head = fmt.Sprintf("box %s: %s", r.Box, head)
	}
	if len(warnings) > 0 {
		head += "\n" + strings.Join(warnings, "\n")
	}
	text := head + "\n" + log
	if err != nil {
		text += "\nerror: " + err.Error()
	}
	res := textResult(text, err != nil)
	res["structuredContent"] = struct {
		JobResult
		Warnings []string `json:"warnings,omitempty"`
	}{r, warnings}
	return res
}

// callTool runs one tool and returns its MCP result.
func callTool(ctx context.Context, base *App, params json.RawMessage, notify func(jobNotice)) map[string]any {
	var p struct {
		Name      string   `json:"name"`
		Arguments toolArgs `json:"arguments"`
		// T3 names the thread here for Codex, whose MCP servers get a bare
		// environment (see App.session).
		Meta struct {
			ThreadID string `json:"threadId"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return textResult("bad arguments: "+err.Error(), true)
	}
	args := p.Arguments
	if args.GPU {
		if args.Size != "" && args.Size != mode.GPUSize {
			return textResult("gpu and size "+args.Size+" don't mix", true)
		}
		args.Size = mode.GPUSize
	}
	out := &tailBuffer{max: 32 << 10}
	a := *base
	a.Out, a.Err = out, out
	if p.Meta.ThreadID != "" {
		a.Thread = p.Meta.ThreadID
	}
	a.Notify = notify
	var warnings []string
	a.Warn = func(message string) { warnings = append(warnings, message) }

	dur := func(s string, def time.Duration) (time.Duration, error) {
		if s == "" {
			return def, nil
		}
		return time.ParseDuration(s)
	}

	withWarnings := func(text string) string {
		if len(warnings) > 0 {
			text += "\n" + strings.Join(warnings, "\n")
		}
		return text
	}

	switch p.Name {
	case "box_status":
		st := a.Status(ctx, args.Dir)
		var b strings.Builder
		writeStatus(&b, st)
		res := textResult(withWarnings(strings.TrimRight(b.String(), "\n")), false)
		res["structuredContent"] = st
		return res
	case "box_jobs":
		jobs, err := a.Jobs(ctx, args.Box)
		if err != nil {
			return textResult(withWarnings(err.Error()), true)
		}
		running := 0
		for _, j := range jobs {
			if j.State == jobRunning {
				running++
			}
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d job(s), %d running\n", len(jobs), running)
		for _, j := range jobs {
			fmt.Fprintf(&b, "%s %s, started %s, log %dB\n", j.Job, j.stateText(), j.Started.Format(time.RFC3339), j.LogBytes)
		}
		res := textResult(withWarnings(strings.TrimRight(b.String(), "\n")), false)
		res["structuredContent"] = map[string]any{"jobs": append([]JobInfo{}, jobs...), "warnings": warnings}
		return res
	case "box_cancel":
		cancelled, c, err := a.Cancel(ctx, args.ID)
		if err != nil {
			return textResult(err.Error(), true)
		}
		res := textResult(cancelText(args.ID, cancelled, c), false)
		res["structuredContent"] = map[string]any{"job": args.ID, "cancelled": cancelled, "exitCode": c}
		return res
	case "box_pull":
		if !filepath.IsAbs(args.Local) {
			return textResult("localDir must be an absolute path", true)
		}
		dest, err := a.Pull(ctx, args.From, args.Remote, args.Local)
		if err != nil {
			return textResult(withWarnings(err.Error()), true)
		}
		return textResult(withWarnings("pulled to "+dest), false)
	case "box_new":
		ttl, err := dur(args.TTL, idleTTL)
		if err != nil {
			return textResult(err.Error(), true)
		}
		reason, err := checkReason(args.Reason, false, "reason")
		if err != nil {
			return textResult(err.Error(), true)
		}
		name, err := a.New(ctx, args.Name, ttl, args.Size, reason)
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
		reason, err := checkReason(args.Reason, true, "reason")
		if err != nil {
			return textResult(err.Error(), true)
		}
		timeout, err := dur(args.Timeout, 0)
		if err != nil {
			return textResult(err.Error(), true)
		}
		r, err := a.Run(ctx, RunOpts{Dir: args.Dir, Cmd: []string{args.Command}, Box: args.Box, New: args.New, Size: args.Size, Wait: wait, Env: args.Env, Timeout: timeout, Reason: reason})
		return jobResult(r, err, out.String(), warnings)
	case "box_job":
		if args.Offset != nil || args.Limit != nil {
			var off, lim int64
			if args.Offset != nil {
				off = *args.Offset
			}
			if args.Limit != nil {
				lim = *args.Limit
			}
			pg, err := a.LogPage(ctx, args.ID, off, lim)
			if err != nil {
				return textResult(err.Error(), true)
			}
			return pageResult(pg)
		}
		wait, err := dur(args.Wait, 10*time.Minute)
		if err != nil {
			return textResult(err.Error(), true)
		}
		r, err := a.Job(ctx, args.ID, wait)
		return jobResult(r, err, out.String(), warnings)
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
