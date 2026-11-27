package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestMCP(t *testing.T) {
	f := &fakePC{t: t, insts: []instance{{Name: "agent-box", Status: "Running", Config: map[string]string{"user.expires": "2026-11-24T00:00:00Z"}}}}
	a, _ := testApp(f)
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"box_ls","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"box_new","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"box_run","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"nope"}`,
		`not json`,
	}, "\n")
	var out bytes.Buffer
	if err := serveMCP(context.Background(), a, strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}

	type reply struct {
		ID     *int `json:"id"`
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			Tools           []tool `json:"tools"`
			Content         []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *rpcError `json:"error"`
	}
	got := map[int]reply{}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 8 { // no reply to the notification, and tools/list_changed after it
		t.Fatalf("got %d replies:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[1], `"method":"notifications/tools/list_changed"`) {
		t.Errorf("second line = %s, want tools/list_changed after initialized", lines[1])
	}
	for _, l := range lines {
		var r reply
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("bad reply %q: %v", l, err)
		}
		id := -1
		if r.ID != nil {
			id = *r.ID
		}
		got[id] = r
	}

	if v := got[1].Result.ProtocolVersion; v != "2025-03-26" {
		t.Errorf("protocolVersion = %q", v)
	}
	var names []string
	for _, tl := range got[2].Result.Tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "box_status,box_run,box_job,box_cancel,box_pull" {
		t.Errorf("tools = %v", names)
	}
	for _, id := range []int{3, 4} { // CLI only now
		if r := got[id].Result; !r.IsError || !strings.Contains(r.Content[0].Text, "unknown tool") {
			t.Errorf("removed tool %d = %+v", id, r)
		}
	}
	if !got[5].Result.IsError {
		t.Error("box_run without arguments didn't fail")
	}
	if e := got[6].Error; e == nil || e.Code != -32601 {
		t.Errorf("unknown method error = %+v", e)
	}
	if e := got[-1].Error; e == nil || e.Code != -32700 {
		t.Errorf("parse error = %+v", e)
	}
}

func TestMCPJobIDSurvivesLongLog(t *testing.T) {
	f := &fakePC{t: t, insts: []instance{{Name: "agent-box", Status: "Running"}},
		log: strings.Repeat("x", 40<<10) + "\n", exit: "4\n"}
	a, _ := testApp(f)
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"box_job","arguments":{"id":"agent-box.20261123-120000-0001"}}}`
	var out bytes.Buffer
	if err := serveMCP(context.Background(), a, strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	var r struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Structured JobResult `json:"structuredContent"`
			IsError    bool      `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	want := JobResult{Job: "agent-box.20261123-120000-0001", Box: "agent-box", State: stateDone, Code: 4}
	if r.Result.Structured != want {
		t.Errorf("structuredContent = %+v", r.Result.Structured)
	}
	first, rest, _ := strings.Cut(r.Result.Content[0].Text, "\n")
	if first != "job agent-box.20261123-120000-0001 on box agent-box: done, exit code: 4" {
		t.Errorf("first line = %q", first)
	}
	if !strings.HasPrefix(rest, "source unknown\n[earlier output cut]") {
		t.Errorf("source or truncated log missing: %.65q", rest)
	}
}

func TestMCPCancelDetaches(t *testing.T) {
	f := &fakePC{t: t, insts: []instance{{Name: "agent-box", Status: "Running"}}, script: "running - 0\n", block: true}
	a, _ := testApp(f)
	pr, pw := io.Pipe()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- serveMCP(context.Background(), a, pr, &out) }()
	io.WriteString(pw, `{"jsonrpc":"2.0","id":"r7","method":"tools/call","params":{"name":"box_job","arguments":{"id":"agent-box.20261123-120000-0001"}}}`+"\n")
	io.WriteString(pw, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"r7","reason":"user"}}`+"\n")
	pw.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled call kept running")
	}
	if out.Len() != 0 {
		t.Errorf("replied to a cancelled request: %s", out.String())
	}
	if c := f.callList(); c != "exec exec" {
		t.Errorf("incus calls = %q, want only the log probe and follow", c)
	}
}

func readJobNotice(t *testing.T, dec *json.Decoder, event string) jobNotice {
	t.Helper()
	var msg struct {
		Method string
		Params struct {
			Level string
			Data  struct {
				jobNotice
				RequestID json.RawMessage `json:"requestId"`
			}
		}
	}
	if err := dec.Decode(&msg); err != nil {
		t.Fatal(err)
	}
	if msg.Method != "notifications/message" || msg.Params.Level != "info" || msg.Params.Data.Event != event || len(msg.Params.Data.RequestID) == 0 {
		t.Fatalf("expected %s notice, got %+v", event, msg)
	}
	return msg.Params.Data.jobNotice
}

func TestMCPRunNoticesDeliveredBeforeCancel(t *testing.T) {
	f := &fakePC{t: t, block: true}
	a, _ := testApp(f)
	repoJSON, err := json.Marshal(newRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	input, request := io.Pipe()
	output, response := io.Pipe()
	defer request.Close()
	defer output.Close()
	go func() { response.CloseWithError(serveMCP(ctx, a, input, response)) }()
	if _, err := io.WriteString(request, `{"jsonrpc":"2.0","id":"run","method":"tools/call","params":{"name":"box_run","arguments":{"dir":`+string(repoJSON)+`,"command":"true","new":true,"reason":"Run the tests in a box for this change."}}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(output)
	launch := readJobNotice(t, dec, "launching_box")
	start := readJobNotice(t, dec, "starting_job")
	if launch.Box != "my-repo-0002" || launch.Job != "" || start.Box != launch.Box || start.Job != "my-repo-0002.20261123-120000-0001" {
		t.Fatalf("launch = %+v; start = %+v", launch, start)
	}
	for !strings.Contains(f.callList(), "file file exec config exec") {
		select {
		case <-ctx.Done():
			t.Fatalf("never followed job: %s", f.callList())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err := io.WriteString(request, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"run"}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	request.Close()
	var next rpcMsg
	if err := dec.Decode(&next); err != io.EOF {
		t.Fatalf("expected no reply after cancellation, got %+v, %v", next, err)
	}
	if f.names() != launch.Box {
		t.Error("cancellation deleted the box")
	}
}

func TestMCPSubmoduleWarningSurvivesLongLog(t *testing.T) {
	repo := newRepo(t)
	gitT(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", newRepo(t), "dependency")
	f := &fakePC{t: t, insts: []instance{{Name: "my-repo", Status: "Running"}}, log: strings.Repeat("x", 40<<10), exit: "0"}
	a, _ := testApp(f)
	repoJSON, err := json.Marshal(repo)
	if err != nil {
		t.Fatal(err)
	}
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"box_run","arguments":{"dir":` + string(repoJSON) + `,"command":"true","reason":"Run the tests in a box for this change."}}}`
	var out bytes.Buffer
	if err := serveMCP(context.Background(), a, strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(&out)
	readJobNotice(t, dec, "starting_job")
	var reply struct {
		Result struct {
			Content           []struct{ Text string }
			StructuredContent struct{ Warnings []string }
		}
	}
	if err := dec.Decode(&reply); err != nil {
		t.Fatal(err)
	}
	const warning = "box: warning: submodules aren't synced and will be empty in the box: dependency"
	if got := reply.Result.StructuredContent.Warnings; len(got) != 1 || got[0] != warning {
		t.Errorf("warnings = %q", got)
	}
	if got := strings.SplitN(reply.Result.Content[0].Text, "\n", 5); len(got) < 4 ||
		got[1] != warning || !strings.HasPrefix(got[2], "source ") || got[3] != "[earlier output cut]" {
		t.Error("warning and source were not shown ahead of the truncated log")
	}
}

// Claude Code shows structuredContent, not the text, when a result has both
// (#33): the log tail must be there too.
func TestMCPStructuredCarriesTail(t *testing.T) {
	f := &fakePC{t: t, insts: []instance{{Name: "agent-box", Status: "Running"}}, log: "hello\n", exit: "3\n"}
	a, _ := testApp(f)
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"box_job","arguments":{"id":"agent-box.20261123-120000-0001"}}}`
	var out bytes.Buffer
	if err := serveMCP(context.Background(), a, strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	var r struct {
		Result struct {
			Structured map[string]any `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if s := r.Result.Structured; s["exitCode"] != 3.0 || !strings.Contains(fmt.Sprint(s["tail"]), "hello") {
		t.Errorf("structuredContent = %v", s)
	}
}

// A session holding an older box's tool list (#34) sends box_run with wait
// and box and without reason: it runs, with notices, and the reason recorded
// says none was given.
func TestMCPOldShapedRun(t *testing.T) {
	f := &fakePC{t: t, insts: []instance{{Name: "my-repo", Status: "Running"}}, log: "ok\n", exit: "0"}
	a, _ := testApp(f)
	repoJSON, err := json.Marshal(newRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"box_run","arguments":{"dir":` + string(repoJSON) + `,"command":"go test ./...","timeout":"40m","wait":"2m","box":"other"}}}`
	var out bytes.Buffer
	if err := serveMCP(context.Background(), a, strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(&out)
	readJobNotice(t, dec, "starting_job")
	var reply struct {
		Result struct {
			IsError           bool
			StructuredContent struct {
				State    string
				Warnings []string
				Tail     string
			}
		}
	}
	if err := dec.Decode(&reply); err != nil {
		t.Fatal(err)
	}
	s := reply.Result.StructuredContent
	if reply.Result.IsError || s.State != stateDone || len(s.Warnings) != 3 || !strings.Contains(s.Tail, "ok") {
		t.Fatalf("old-shaped box_run = %+v", reply.Result)
	}
	for i, want := range []string{"takes box", "takes wait", "needs a reason"} {
		if !strings.Contains(s.Warnings[i], want) {
			t.Errorf("notice %d = %q, want %q", i, s.Warnings[i], want)
		}
	}
	if m, _ := metaOf(t, f); m.Reason != "(no reason given) go test ./..." {
		t.Errorf("recorded reason = %q", m.Reason)
	}
}

func TestMissingReason(t *testing.T) {
	long := missingReason(strings.Repeat("x", 100))
	if n := utf8.RuneCountInString(long); n != len("(no reason given) ")+81 || n > reasonMax {
		t.Errorf("long command reason = %q", long)
	}
	if _, err := checkReason(missingReason("go"), true, "reason"); err != nil {
		t.Errorf("a short command's default reason is refused: %v", err)
	}
}

func TestTailBuffer(t *testing.T) {
	b := &tailBuffer{max: 4}
	b.Write([]byte("abc"))
	b.Write([]byte("def"))
	if s := b.String(); s != "[earlier output cut]\ncdef" {
		t.Errorf("tail = %q", s)
	}
}
