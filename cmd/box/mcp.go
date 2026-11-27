package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/a-Fig/agent-box/internal/mask"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/a-Fig/agent-box/internal/mode"
	"github.com/a-Fig/agent-box/internal/version"
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

var pausedHint = "state refused (exit 75) means box couldn't run it now and nothing started: the PC is busy, paused or out of reach, and the run wasn't queued. Retry later, or with a smaller size if it said no room. A running job that was paused resumes if the PC's owner switches back within " + frozenWindow + ", else it is lost; a paused or lost push says so. Exit 75 from a state done result is the command's own exit code, not a refusal."

func integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

var tools = []tool{
	{"box_status", `Check room to offload work. With a host registry, report a fleet summary and each host's reachability, mode, room by size, GPU and queue length. The advice, available and repo box fields describe the host box_run currently uses. Without a registry, the first line is that host's advice.`, obj(map[string]any{
		"dir": str("Absolute path of your git worktree, to report its box and running jobs. Optional."),
	}), nil},
	{"box_run", "Sync a local git worktree (uncommitted and untracked files included, ignored files excluded) to a box and run a shell command there. A run that ends within 30 seconds of the command starting returns its exit code and output tail. A longer one returns state running (exitCode null) with the job ID and push. push true: don't wait or poll; do other work or end your turn, and the result arrives as a turn starting \"box: job\": an exit 0 as the log's last 3 lines (a passing test run's counts first), a test failure as its failed tests and last 3 lines, any other failure as its last 10 lines; box_job gives the full result. push false (no T3 thread, or you passed push false, as a subagent should): call box_job with the job ID, which waits. If Incus returns EOF during the image lookup, box retries the launch once. With push, a run that can't start now (no room, GPU taken, My PC) is queued instead of refused: state queued, with its place in line and what it waits for; the code is snapshotted now, so keep editing, and the result is pushed when it has run. A result box_run or box_job returns starts with the job ID, box and, once ended, the exit code; a go test or vitest run's digest follows (failed tests with file:line and first assertion, then counts; structuredContent's digest), then the output tail (structuredContent's tail). A one-string command runs in bash with pipefail, so 'go test ./... | tail' exits with go test's code; '| head' can exit 141, so pipe to tail. " + pausedHint, obj(map[string]any{
		"dir":     str("Absolute path of the local git worktree."),
		"command": str(`Shell command to run in the repo's root, like "go test ./...".`),
		"reason":  str(`One or two sentences (10-300 characters) on why this run needs a box, like "Full go test -race suite for the scheduler change; it takes about 10 minutes on every core." The PC's owner sees it.`),
		"new":     map[string]any{"type": "boolean", "description": "Use a throwaway box for parallel runs. An idle CPU throwaway for the same repo and size may be reused. Pull artifacts promptly with box_pull; later runs can reuse that checkout."},
		"fresh":   map[string]any{"type": "boolean", "description": "Always launch a fresh throwaway. Use for clean-build checks or to keep a separate box for artifacts."},
		"size":    sizeArg,
		"gpu":     gpuArg,
		"timeout": str(`Kill the command in the box after this long, like "30m"; it then exits 124. Default: no limit.`),
		"push":    map[string]any{"type": "boolean", "description": "Default true: a job still running after 30 seconds is pushed into your thread when it ends. Pass false if you are a subagent (the push would wake your parent) and call box_job instead."},
		"queue":   map[string]any{"type": "boolean", "description": "Default true: with push, a run that can't start now waits in the PC's queue. Pass false to get exit 75 at once instead of waiting."},
		"env": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"},
			"description": "Environment variables for the command. Boxes are shared: never pass secrets; names containing TOKEN, SECRET, KEY or PASSWORD are refused."},
	}, "dir", "command", "reason"), jobOutput},
	{"box_job", "Get the result of a job started by box_run. With id it waits up to wait (default 10m) and returns the exit code (first line) and output tail; state running means it still runs, so call again. A throwaway box from box_run new is kept up to 30 minutes after its job ends (for box_pull), unless another run needs the room. With offset or limit it doesn't wait: it returns that piece of the log (structuredContent's log), the job's state and nextOffset, so a long log can be read in pieces. Without id it lists jobs newest first on the default and configured hosts. Fleet listings name each host and qualify job IDs outside the default. " + pausedHint, obj(map[string]any{
		"id":     str("Job ID from box_run. Omit to list jobs."),
		"wait":   str(`Stop waiting after this long. Default 10m.`),
		"offset": integer("Read the log from this byte offset, without waiting. Default 0."),
		"limit":  integer("Read at most this many bytes of the log (default 32768, at most 1048576)."),
	}), jobOutput},
	{"box_cancel", "Stop a running job's process session, or take a queued job off the queue. A cancelled job exits 130.", obj(map[string]any{
		"id": str("Job ID."),
	}, "id"), nil},
	{"box_pull", "Copy a file or directory (test reports, coverage, build output) from a box into a local directory, as <localDir>/<name>. It never replaces an existing file and drops symlinks. Pull right after the job (or run with fresh: true): if a later job replaced the repo box's checkout, pulling from the earlier job ID refuses instead of returning the wrong files.", obj(map[string]any{
		"from":       str("Job ID, or a box name."),
		"remotePath": str("Linux path in the box. A relative path starts at the job's checkout (/work/<repo>); an absolute path starts with /."),
		"localDir":   str("Absolute path of the local directory to copy into; created if missing."),
	}, "from", "remotePath", "localDir"), nil},
}

// sizeArg is the size property of box_new and box_run.
var sizeArg = map[string]any{"type": "string", "enum": sizeEnum(),
	"description": "Box size: small (quick unit tests), medium (default), large (big Gradle/Android builds, memory-hungry suites), gpu (see gpu). An existing repo box of another size isn't resized; the run gets a throwaway of this size."}

// gpuArg is the gpu property of box_new and box_run.
var gpuArg = map[string]any{"type": "boolean",
	"description": "Same as size gpu: the PC's GPU (CUDA, PyTorch, GPU builds); exit 75 on a host without one (box_status gpu: none). One GPU box at a time, always a throwaway; while it's taken, a run with push waits for it in the queue, otherwise exit 75. It is stopped in My PC, and a GPU run doesn't wait out My PC (exit 75)."}

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
	"host":     str("Host name for a qualified job ID."),
	"state":    str("done (exitCode is the command's), running, queued (waiting in the PC's queue), paused (exitCode 75; the job may resume), refused (exitCode 75; nothing started), or empty after an error."),
	"push":     map[string]any{"type": "boolean", "description": "box_run, state running or queued: the result will arrive as a turn in your thread."},
	"position": map[string]any{"type": "integer", "description": "State queued: place in line, from 1."},
	"why":      str("State queued: what it waits for: full (room), gpu-busy, or mypc."),
	"exitCode": map[string]any{"type": []string{"integer", "null"},
		"description": "The command's exit code when state is done; 75 when paused or refused; null while the job is still running (state running), since a running job has no exit code."},
	"warnings": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	"digest":   str("A Go test or vitest run that ended: its failed tests, each with file:line and first assertion or panic line, then the counts (at most 3KiB). In the text result it comes before the tail. Absent for other commands."),
	"tail":     str("The end of the job's output (up to 32KiB), as in the text result."),
	"error":    str("Why the call failed, when it did."),
	"finished": str("Your jobs that ended since you last looked."),
	// Only for a log page (box_job with offset or limit).
	"log":        str("This page of the log, as text."),
	"offset":     map[string]any{"type": "integer"},
	"nextOffset": map[string]any{"type": "integer", "description": "Offset of the next page."},
	"logBytes":   map[string]any{"type": "integer", "description": "Size of the log so far."},
	"more":       map[string]any{"type": "boolean", "description": "The log goes on past this page, or the job is still running."},
	"logBase64":  str("Exact log bytes when this page does not contain complete UTF-8 text."),
})

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
	call := mcpCaller(a, a.MCPExe)
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
			if req.Method == "notifications/initialized" {
				// A client holding an older box's tools (box.exe was
				// replaced under it) reloads them (#34).
				send(rpcMsg{Method: "notifications/tools/list_changed"})
			}
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
				"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}, "logging": map[string]any{}},
				"serverInfo":      map[string]any{"name": "box", "version": version.Get().String()},
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
			send(rpcMsg{ID: req.ID, Result: call(ctx, mcpCall{Method: req.Method}, func(jobNotice) {})})
		case "tools/call":
			// Registered before the goroutine starts, so a cancel that
			// follows the call on the wire always finds it.
			callCtx, cancel := context.WithCancelCause(ctx)
			key := string(req.ID)
			callsMu.Lock()
			calls[key] = cancel
			callsMu.Unlock()
			cl := client // for the job's .meta
			if cl.Name == "" {
				cl.Name = "mcp"
			}
			wg.Add(1)
			go func(req rpcMsg) {
				defer wg.Done()
				res := call(callCtx, mcpCall{Method: req.Method, Params: req.Params, Client: cl}, func(note jobNotice) {
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
	Fresh   bool              `json:"fresh"`
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
	Push    *bool             `json:"push"`
	Queue   *bool             `json:"queue"`
}

// pageResult reports a box_job log page: the summary line, then the bytes.
func pageResult(p Page) map[string]any {
	var code *int // a job that is still running has no exit code
	switch {
	case p.Code != nil:
		code = p.Code
	case p.State == jobDied:
		one := 1
		code = &one
	}
	res := textResult(p.pageHead()+"\n"+p.Text, false)
	structured := map[string]any{
		"job": p.Job, "box": p.Box, "state": p.State, "exitCode": code,
		"offset": p.Offset, "nextOffset": p.NextOffset, "logBytes": p.LogBytes, "more": p.More,
		"log": strings.ToValidUTF8(p.Text, "�"),
	}
	if !utf8.ValidString(p.Text) {
		structured["logBase64"] = base64.StdEncoding.EncodeToString([]byte(p.Text))
	}
	if p.Host != "" {
		structured["host"] = p.Host
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
	if r.State == stateDetached {
		r.State = stateRunning
	}
	head := "exit code: " + strconv.Itoa(r.Code)
	if r.State == stateRunning {
		head = "running" // no exit code yet
	} else if r.State == stateQueued {
		head = fmt.Sprintf("queued, %s, %s", place(r.Position), waitText(r.Why))
	} else if r.State != "" {
		head = r.State + ", " + head
	}
	if r.Push != nil {
		head += fmt.Sprintf(", push: %t", *r.Push)
	}
	if r.Job != "" {
		location := r.Box
		if r.Host != "" {
			location += " on " + r.Host
		}
		head = fmt.Sprintf("job %s on box %s: %s", r.Job, location, head)
	} else if r.Box != "" {
		head = fmt.Sprintf("box %s: %s", r.Box, head)
	}
	if len(warnings) > 0 {
		head += "\n" + strings.Join(warnings, "\n")
	}
	text := head + "\n" + r.Digest + log
	if err != nil {
		text += "\nerror: " + err.Error()
	}
	res := textResult(text, err != nil)
	out := struct {
		Job      string   `json:"job,omitempty"`
		Box      string   `json:"box,omitempty"`
		Host     string   `json:"host,omitempty"`
		State    string   `json:"state,omitempty"`
		ExitCode *int     `json:"exitCode"` // null while the job is running
		Push     *bool    `json:"push,omitempty"`
		Position int      `json:"position,omitempty"`
		Why      string   `json:"why,omitempty"`
		Warnings []string `json:"warnings,omitempty"`
		Digest   string   `json:"digest,omitempty"`
		// Claude Code shows structuredContent instead of the text when a
		// result has both (#33), so what the agent reads is here too.
		Tail  string `json:"tail"`
		Error string `json:"error,omitempty"`
	}{r.Job, r.Box, r.Host, r.State, &r.Code, r.Push, r.Position, r.Why, warnings, r.Digest, log, ""}
	if err != nil {
		out.Error = err.Error()
	}
	if r.State == stateRunning || r.State == stateQueued {
		out.ExitCode = nil
	}
	res["structuredContent"] = out
	return res
}

// callTool runs one tool and returns its MCP result.
func callTool(ctx context.Context, base *App, params json.RawMessage, notify func(jobNotice)) (res map[string]any) {
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
	a.KeepEnded = endedKeep // a throwaway outlives its job, for box_pull and the log
	var warnings []string
	a.Warn = func(message string) { warnings = append(warnings, message) }

	// Say what finished since the agent last looked, and restart lost
	// watchers (push.go).
	if net := a.safetyNet(args.ID); net != "" {
		defer func() {
			if c, ok := res["content"].([]map[string]any); ok && len(c) > 0 {
				c[0]["text"] = net + fmt.Sprint(c[0]["text"])
			}
			setStructured(res, "finished", strings.TrimSpace(net))
		}()
	}

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
		st, err := a.fleetStatus(ctx, args.Dir, false)
		if err != nil {
			return textResult(err.Error(), true)
		}
		var b strings.Builder
		writeStatus(&b, st)
		res := textResult(withWarnings(strings.TrimRight(b.String(), "\n")), false)
		res["structuredContent"] = st
		if len(warnings) > 0 {
			setStructured(res, "warnings", warnings)
		}
		return res
	case "box_cancel":
		removed, cancelled, c, err := a.CancelAny(ctx, args.ID)
		if err != nil {
			return textResult(err.Error(), true)
		}
		if removed {
			res := textResult("removed "+args.ID+" from the queue; it never ran", false)
			res["structuredContent"] = map[string]any{"job": args.ID, "cancelled": true, "exitCode": exitDetached, "message": "removed from the queue; it never ran"}
			return res
		}
		msg := cancelText(args.ID, cancelled, c)
		res := textResult(msg, false)
		res["structuredContent"] = map[string]any{"job": args.ID, "cancelled": cancelled, "exitCode": c, "message": msg}
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
	case "box_run":
		// A session that loaded an older box's tools still sends their shape
		// (#34): what was removed is ignored and a missing reason gets a
		// default, each with a notice, rather than failing the call.
		const stale = " Your tool list is from an older box; reload it."
		if args.Box != "" {
			a.warn("box: notice: box_run no longer takes box, so " + args.Box + " was ignored: it uses this repo's box, or new: true for a throwaway." + stale)
		}
		if args.Wait != "" {
			a.warn("box: notice: box_run no longer takes wait, so it was ignored: it waits 30 seconds, then returns running." + stale)
		}
		if args.Dir == "" || args.Command == "" {
			return textResult("dir and command are required", true)
		}
		if strings.TrimSpace(args.Reason) == "" {
			args.Reason = missingReason(args.Command)
			a.warn("box: notice: box_run needs a reason, 10-300 characters on why this needs a box, which the PC's owner sees; this run's is \"" + args.Reason + "\"." + stale)
		}
		reason, err := checkReason(args.Reason, true, "reason")
		if err != nil {
			return textResult(err.Error(), true)
		}
		timeout, err := dur(args.Timeout, 0)
		if err != nil {
			return textResult(err.Error(), true)
		}
		wait := a.InlineWait
		if wait == 0 {
			wait = inlineWait
		}
		push := args.Push == nil || *args.Push
		// Only a run whose result can be pushed is queued: nothing else
		// would hear when it starts (docs/queue.md).
		queue := push && (args.Queue == nil || *args.Queue) && a.canPush()
		r, err := a.Run(ctx, RunOpts{Dir: args.Dir, Cmd: []string{args.Command}, New: args.New, Fresh: args.Fresh, Size: args.Size, Wait: wait, Env: args.Env, Timeout: timeout, Reason: reason, Queue: queue})
		info := jobMeta{Reason: reason, Command: displayCommand([]string{args.Command}), Agent: agentInfo{Worktree: filepath.FromSlash(args.Dir)}}
		switch {
		case err != nil:
		case r.Job == "" && r.State == statePaused:
			r.State = stateRefused
		case r.State == stateQueued:
			if !a.startWatch(info, r.Job, args.Size == mode.GPUSize, r.throwaway, true) {
				// Nobody would hear when it runs: take it back, and refuse.
				if cerr := a.cancelQueued(ctx, r.Job); cerr == nil {
					job := r.Job
					r = JobResult{State: stateRefused, Code: exitPaused}
					fmt.Fprintf(out, "box: can't follow %s from here, so it was taken off the queue; nothing ran\n", job)
					break
				}
				push = false
			}
			r.Push = &push
		case r.Job != "" && (r.State == stateDetached || r.State == statePaused):
			if push {
				push = a.startWatch(info, r.Job, args.Size == mode.GPUSize, args.New || args.Fresh, false)
			}
			r.Push = &push
		}
		log := out.String()
		if r.State == stateDetached && r.Push != nil && *r.Push {
			log = dropReattachLines(log)
		}
		return jobResult(r, err, log, warnings)
	case "box_job":
		if args.ID == "" && args.Offset == nil && args.Limit == nil {
			jobs, err := a.listJobs(ctx, "")
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
				fmt.Fprint(&b, j.Job)
				if j.Host != "" {
					fmt.Fprintf(&b, " on %s", j.Host)
				}
				fmt.Fprintf(&b, " %s, started %s, log %dB\n", j.stateText(), j.Started.Format(time.RFC3339), j.LogBytes)
			}
			res := textResult(withWarnings(strings.TrimRight(b.String(), "\n")), false)
			res["structuredContent"] = map[string]any{"jobs": append([]JobInfo{}, jobs...), "warnings": append([]string{}, warnings...)}
			return res
		}
		lookup, err := a.lookupJob(args.ID)
		if err != nil {
			return textResult(err.Error(), true)
		}
		if args.Offset != nil || args.Limit != nil {
			var off, lim int64
			if args.Offset != nil {
				off = *args.Offset
			}
			if args.Limit != nil {
				lim = *args.Limit
			}
			pg, end, err := lookup.page(ctx, off, lim)
			if err != nil {
				// The box can't be read (gone, most likely): its recorded end
				// is all there is. A bad offset or limit is still an error.
				if end != nil {
					box := lookup.id.Box
					a.warn("box: can't read the log from " + box + " (" + firstLine(err.Error()) + "); this is the tail recorded when the job ended")
					return jobResult(JobResult{Job: lookup.id.ID, Box: box, Host: lookup.host, State: stateDone, Code: end.Code, Digest: end.Digest}, nil, end.Tail, warnings)
				}
				return textResult(err.Error(), true)
			}
			return pageResult(pg)
		}
		wait, err := dur(args.Wait, 10*time.Minute)
		if err != nil {
			return textResult(err.Error(), true)
		}
		if j, queued, err := lookup.queued(ctx); err == nil && queued {
			return jobResult(JobResult{Job: lookup.id.ID, Box: j.Box, Host: lookup.host, State: stateQueued, Position: j.Position, Why: j.Why}, nil, "", warnings)
		}
		r, err := lookup.job(ctx, wait)
		a.markRead(lookup.id.ID, r, err, "box_job")
		return jobResult(r, err, out.String(), warnings)
	}
	return textResult("unknown tool "+p.Name, true)
}

// missingReason is the reason recorded for a box_run sent without one by a
// session holding an older tool list, so the dashboard shows it as such.
func missingReason(command string) string {
	c := strings.Join(strings.Fields(mask.Mask(command)), " ") // masked before it is cut, so a cut can't hide a secret from the mask
	if utf8.RuneCountInString(c) > 80 {
		c = string([]rune(c)[:80]) + "…"
	}
	return "(no reason given) " + c
}

// setStructured adds key to a result's structuredContent, if it has one; a
// result without it is text only, which clients show whole.
func setStructured(res map[string]any, key string, v any) {
	sc, ok := res["structuredContent"]
	if !ok {
		return
	}
	m, ok := sc.(map[string]any)
	if !ok {
		b, err := json.Marshal(sc)
		if err != nil || json.Unmarshal(b, &m) != nil || m == nil {
			return
		}
		res["structuredContent"] = m
	}
	m[key] = v
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

// dropReattachLines takes the CLI's "reattach with: box job" and "keeping
// <box>" lines out of a running job's log once a watcher has it: the push
// brings the result and hands the box to the reaper, so those lines would
// tell the agent to do what push says not to.
func dropReattachLines(log string) string {
	lines := strings.SplitAfter(log, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(l, "box: job ") && strings.Contains(l, "is still running; reattach with: box job ") ||
			strings.HasPrefix(l, "box: keeping ") && strings.Contains(l, "hands it to the reaper once the job has ended") {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "")
}
