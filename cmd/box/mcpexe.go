package main

// A box mcp server lives as long as its agent session, which can be days, and Windows lets
// box.exe be replaced under it. So on 2026-11-25 sessions were still running a server from
// before #37, without output tails, after several releases (#70). The server now hands each
// tools/list and tools/call to the box.exe on disk (`box mcp-call`), so a release reaches every
// session at its next call. If that box.exe can't be run, the server answers in process.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// mcpCall is one request for `box mcp-call`, the first line on its stdin. Stdin then stays
// open; closing it cancels the call, as notifications/cancelled does in process.
type mcpCall struct {
	Method string          `json:"method"` // tools/list or tools/call
	Params json.RawMessage `json:"params,omitempty"`
	Client clientInfo      `json:"client"`
}

// mcpCallLine is one line `box mcp-call` writes: a notice while the call runs, then the result.
type mcpCallLine struct {
	Notice *jobNotice     `json:"notice,omitempty"`
	Result map[string]any `json:"result,omitempty"`
}

// runMCPCall is `box mcp-call`: it answers one request from in on out with call.
func runMCPCall(ctx context.Context, in io.Reader, out io.Writer,
	call func(ctx context.Context, req mcpCall, notify func(jobNotice)) map[string]any) error {
	r := bufio.NewReader(in)
	line, err := r.ReadBytes('\n')
	var req mcpCall
	if err := json.Unmarshal(line, &req); err != nil {
		return fmt.Errorf("mcp-call: bad request: %w", err)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if err == nil { // the parent closes stdin to cancel
		go func() {
			io.Copy(io.Discard, r)
			cancel(errClientCancelled)
		}()
	}
	enc := json.NewEncoder(out) // one goroutine writes: notices come from the call's own goroutines
	notices := make(chan jobNotice, 16)
	done := make(chan map[string]any)
	go func() {
		done <- call(ctx, req, func(n jobNotice) {
			select {
			case notices <- n:
			case <-ctx.Done():
			}
		})
	}()
	for {
		select {
		case n := <-notices:
			enc.Encode(mcpCallLine{Notice: &n})
		case res := <-done:
			for len(notices) > 0 { // a notice sent just before the call returned
				n := <-notices
				enc.Encode(mcpCallLine{Notice: &n})
			}
			return enc.Encode(mcpCallLine{Result: res})
		}
	}
}

// callInProcess answers a request with this binary's own code.
func callInProcess(a *App) func(ctx context.Context, req mcpCall, notify func(jobNotice)) map[string]any {
	return func(ctx context.Context, req mcpCall, notify func(jobNotice)) map[string]any {
		if req.Method == "tools/list" {
			return map[string]any{"tools": tools}
		}
		ca := *a
		ca.Client = req.Client
		return callTool(ctx, &ca, req.Params, notify)
	}
}

// errNoResult says the box.exe on disk ran but answered nothing, so the call may have done
// its work: it isn't run again in process.
var errNoResult = errors.New("box mcp-call ended without a result")

// callExe runs req through `exe mcp-call`. An error before the request was written means it
// never ran, and the caller may answer in process.
func callExe(ctx context.Context, exe string, req mcpCall, notify func(jobNotice)) (map[string]any, error) {
	cmd := exec.Command(exe, "mcp-call")
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	b, _ := json.Marshal(req)
	if _, err := stdin.Write(append(b, '\n')); err != nil {
		stdin.Close()
		cmd.Wait()
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { stdin.Close() })
	defer stop()
	var res map[string]any
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var l mcpCallLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if l.Notice != nil {
			notify(*l.Notice)
		}
		if l.Result != nil {
			res = l.Result
		}
	}
	stdin.Close()
	werr := cmd.Wait()
	if res == nil {
		return nil, fmt.Errorf("%w (%v)", errNoResult, werr)
	}
	return res, nil
}

// mcpCaller is how serveMCP answers tools/list and tools/call: through the box.exe on disk when
// exe is set, else, or when that can't start, in process.
func mcpCaller(a *App, exe string) func(ctx context.Context, req mcpCall, notify func(jobNotice)) map[string]any {
	local := callInProcess(a)
	if exe == "" {
		return local
	}
	return func(ctx context.Context, req mcpCall, notify func(jobNotice)) map[string]any {
		res, err := callExe(ctx, exe, req, notify)
		switch {
		case err == nil:
			return res
		case req.Method == "tools/list": // no side effects, and a client needs its tools
			return local(ctx, req, notify)
		case errors.Is(err, errNoResult):
			return textResult("box: "+err.Error(), true)
		default: // it never ran: mid-update, or the file is gone
			return local(ctx, req, notify)
		}
	}
}
