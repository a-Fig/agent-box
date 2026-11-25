package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// With BOX_TEST_MCPCALL set, this test binary stands in for a newer box.exe: `mcp-call` answers
// with a fake call, so callExe is tested across a real process boundary.
func TestMain(m *testing.M) {
	if how := os.Getenv("BOX_TEST_MCPCALL"); how != "" && len(os.Args) > 1 && os.Args[1] == "mcp-call" {
		if how == "die" {
			os.Exit(3)
		}
		err := runMCPCall(context.Background(), os.Stdin, os.Stdout, func(ctx context.Context, req mcpCall, notify func(jobNotice)) map[string]any {
			notify(jobNotice{Event: "launching", Box: "b1"})
			if strings.Contains(string(req.Params), "block") {
				<-ctx.Done()
				return textResult("cause: "+context.Cause(ctx).Error(), false)
			}
			return textResult("child "+req.Method+" for "+req.Client.Name, false)
		})
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func text(res map[string]any) string {
	b, _ := json.Marshal(res["content"])
	var c []struct{ Text string }
	json.Unmarshal(b, &c)
	if len(c) == 0 {
		return ""
	}
	return c[0].Text
}

func TestCallExeRunsTheBinaryOnDisk(t *testing.T) {
	t.Setenv("BOX_TEST_MCPCALL", "1")
	var notes []jobNotice
	res, err := callExe(context.Background(), os.Args[0], mcpCall{Method: "tools/call", Params: json.RawMessage(`{"name":"box_status"}`),
		Client: clientInfo{Name: "claude-code"}}, func(n jobNotice) { notes = append(notes, n) })
	if err != nil || text(res) != "child tools/call for claude-code" {
		t.Fatalf("res=%v err=%v", res, err)
	}
	if len(notes) != 1 || notes[0].Event != "launching" || notes[0].Box != "b1" {
		t.Fatalf("notices %+v", notes)
	}
}

// A client's cancel closes the child's stdin, and the child's call sees the client's cause,
// as an in-process call does.
func TestCallExeCancel(t *testing.T) {
	t.Setenv("BOX_TEST_MCPCALL", "1")
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan string)
	go func() {
		res, err := callExe(ctx, os.Args[0], mcpCall{Method: "tools/call", Params: json.RawMessage(`{"name":"block"}`)}, func(jobNotice) { cancel() })
		if err != nil {
			got <- err.Error()
			return
		}
		got <- text(res)
	}()
	select {
	case s := <-got:
		if s != "cause: "+errClientCancelled.Error() {
			t.Fatalf("got %q", s)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the cancelled call never ended")
	}
}

func TestMCPCallerFallsBack(t *testing.T) {
	a, _ := testApp(&fakePC{})
	// A box.exe that can't start (mid-update, or gone) is answered in process.
	call := mcpCaller(a, filepath.Join(t.TempDir(), "missing.exe"))
	if res := call(context.Background(), mcpCall{Method: "tools/list"}, nil); res["tools"] == nil {
		t.Fatalf("no in-process tools: %v", res)
	}
	// One that ran but answered nothing may have done the work, so it isn't run again.
	t.Setenv("BOX_TEST_MCPCALL", "die")
	res := mcpCaller(a, os.Args[0])(context.Background(), mcpCall{Method: "tools/list"}, nil)
	if res["isError"] != true || !strings.Contains(text(res), "without a result") {
		t.Fatalf("res %v", res)
	}
}

func TestRunMCPCallBadRequest(t *testing.T) {
	var out bytes.Buffer
	err := runMCPCall(context.Background(), strings.NewReader("not json\n"), &out, nil)
	if err == nil || out.Len() != 0 {
		t.Fatalf("err=%v out=%q", err, out.String())
	}
	// A request with no newline (stdin closed at once) still runs, uncancelled.
	err = runMCPCall(context.Background(), strings.NewReader(`{"method":"tools/list"}`), &out,
		func(ctx context.Context, req mcpCall, notify func(jobNotice)) map[string]any {
			return map[string]any{"cancelled": ctx.Err() != nil}
		})
	var l mcpCallLine
	if err != nil || json.Unmarshal(out.Bytes(), &l) != nil || l.Result["cancelled"] != false {
		t.Fatalf("err=%v out=%q", err, out.String())
	}
}
