package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestMCP(t *testing.T) {
	f := &fakePC{t: t, insts: []instance{{Name: "agent-box", Status: "Running", Config: map[string]string{"user.expires": "2026-11-24T00:00:00Z"}}}}
	a, _ := testApp(f)
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"box_ls","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"box_rm","arguments":{"name":"old-box"}}}`,
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
	if len(lines) != 7 { // no reply to the notification
		t.Fatalf("got %d replies:\n%s", len(lines), out.String())
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
	if strings.Join(names, ",") != "box_new,box_run,box_job,box_rm,box_ls" {
		t.Errorf("tools = %v", names)
	}
	if txt := got[3].Result.Content[0].Text; !strings.Contains(txt, "agent-box") || !strings.Contains(txt, "2026-11-24T00:00:00Z") {
		t.Errorf("box_ls = %q", txt)
	}
	if r := got[4].Result; r.IsError || r.Content[0].Text != "removed old-box" {
		t.Errorf("box_rm = %+v", r)
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

func TestTailBuffer(t *testing.T) {
	b := &tailBuffer{max: 4}
	b.Write([]byte("abc"))
	b.Write([]byte("def"))
	if s := b.String(); s != "[earlier output cut]\ncdef" {
		t.Errorf("tail = %q", s)
	}
}
