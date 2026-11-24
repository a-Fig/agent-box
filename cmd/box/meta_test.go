package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/mode"
)

const testReason = "Run the tests in a box for this change."

const testRemotes = `{"local":{"Addrs":["unix://"],"Protocol":"incus"},"pc":{"Addrs":["https://agent-box.example.ts.net:8443"],"Protocol":"incus"}}`

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// posted records refusals App.Post was asked to send.
type posted struct {
	urls   []string
	bodies []refusal
	raw    []string
	err    error
}

func (p *posted) post(_ context.Context, url string, body []byte) error {
	p.urls = append(p.urls, url)
	var r refusal
	json.Unmarshal(body, &r)
	p.bodies = append(p.bodies, r)
	p.raw = append(p.raw, string(body))
	return p.err
}

func TestCheckReason(t *testing.T) {
	long := strings.Repeat("é", reasonMax) // runes, not bytes
	for _, c := range []struct {
		in       string
		required bool
		want     string
		err      string
	}{
		{"", true, "", "reason is required"},
		{"   ", true, "", "reason is required"},
		{"", false, "", ""},
		{"too short", true, "", "is 9 characters"},
		{"  Full -race suite.  ", true, "Full -race suite.", ""},
		{long, true, long, ""},
		{long + "x", false, "", "is 301 characters"},
	} {
		got, err := checkReason(c.in, c.required, "reason")
		if got != c.want || (err == nil) != (c.err == "") || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("checkReason(%q, %v) = %q, %v; want %q, %q", c.in, c.required, got, err, c.want, c.err)
		}
	}
	if utf8.RuneCountInString(long) != reasonMax {
		t.Fatal("bad test")
	}
}

func TestRunReasonRequired(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--", "true"},
		{"run", "--reason", "short", "--", "true"},
		{"new", "--reason", "short"},
	} {
		f := &fakePC{t: t}
		a, out := testApp(f)
		if code := run(context.Background(), a, args, nil); code != 2 || !strings.Contains(out.String(), "one or two sentences") {
			t.Errorf("%q = %d\n%s", args, code, out)
		}
		if c := f.callList(); c != "" {
			t.Errorf("%q made Incus calls %q", args, c)
		}
	}

	f := &fakePC{t: t}
	a, _ := testApp(f)
	dir := jsonString(newRepo(t))
	for _, args := range []string{
		`{"dir":` + dir + `,"command":"true"}`,
		`{"dir":` + dir + `,"command":"true","reason":"short"}`,
	} {
		res := callTool(context.Background(), a, json.RawMessage(`{"name":"box_run","arguments":`+args+`}`), nil)
		text := res["content"].([]map[string]any)[0]["text"].(string)
		if res["isError"] != true || !strings.Contains(text, "reason") || !strings.Contains(text, "one or two sentences") {
			t.Errorf("box_run %s = %v", args, res)
		}
	}
	if code := run(context.Background(), a, []string{"new", "--reason", "short"}, nil); code != 2 || f.callList() != "" {
		t.Errorf("box new with a short reason = %d; calls %q", code, f.callList())
	}

	var sawReason bool
	for _, tl := range tools {
		if tl.Name != "box_run" {
			continue
		}
		for _, r := range tl.InputSchema["required"].([]string) {
			sawReason = sawReason || r == "reason"
		}
	}
	if !sawReason {
		t.Error("box_run's schema doesn't require reason")
	}
}

func TestNewStoresReason(t *testing.T) {
	f := &fakePC{t: t}
	a, _ := testApp(f)
	if code := run(context.Background(), a, []string{"new", "kept", "--reason", "  Keep a warm box for the Gradle builds.  "}, nil); code != 0 {
		t.Fatalf("box new = %d", code)
	}
	if got := f.inst("kept").Config[reasonKey]; got != "Keep a warm box for the Gradle builds." {
		t.Errorf("%s = %q", reasonKey, got)
	}
	if _, err := a.New(context.Background(), "plain", idleTTL, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.inst("plain").Config[reasonKey]; ok {
		t.Error("New without a reason set one")
	}
}

// metaOf returns the one .meta pushed.
func metaOf(t *testing.T, f *fakePC) (jobMeta, string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.files) != 1 {
		t.Fatalf("pushed .meta files: %v", f.files)
	}
	for target, body := range f.files {
		var m jobMeta
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatalf("%s: %v\n%s", target, err, body)
		}
		return m, target + "\n" + body
	}
	return jobMeta{}, ""
}

func TestRunWritesMeta(t *testing.T) {
	dir := newRepo(t)
	gitT(t, dir, "checkout", "-q", "-b", "feat/x")
	gitT(t, dir, "remote", "add", "origin", "https://x-access-token:ghp_sekrit@github.com/owner/repo.git")
	head := gitT(t, dir, "rev-parse", "HEAD")
	f := &fakePC{t: t, insts: []instance{{Name: "repo", Status: "Running"}}, exit: "0"}
	a, out := testApp(f)
	a.Getenv = envMap(map[string]string{"T3CODE_THREAD_ID": "thread-1", "CLAUDE_CODE_SESSION_ID": "claude-1"})
	r, err := a.Run(context.Background(), RunOpts{Dir: dir, Cmd: []string{"go", "test", "./..."}, Reason: testReason,
		Env: map[string]string{"FOO": "hidden-value", "BAR": "x"}})
	if err != nil || r.Code != 0 {
		t.Fatalf("Run = %+v, %v\n%s", r, err, out)
	}
	m, raw := metaOf(t, f)
	if !strings.HasPrefix(raw, "pc:repo"+workRoot+"/.box/"+r.Job+".meta\n") {
		t.Errorf("pushed to %q", strings.SplitN(raw, "\n", 2)[0])
	}
	if strings.Contains(raw, "ghp_sekrit") || strings.Contains(raw, "x-access-token") || strings.Contains(raw, "hidden-value") {
		t.Errorf("meta leaks a secret:\n%s", raw)
	}
	host, _ := os.Hostname()
	want := jobMeta{V: 1, Reason: testReason, Command: "go test ./...", Repo: "https://github.com/owner/repo.git",
		Branch: "feat/x", Commit: head, Size: "medium", EnvNames: []string{"BAR", "FOO"}, StartedAt: "2026-11-23T12:00:00Z",
		Agent: agentInfo{Client: "cli", Session: "thread-1", SessionSource: "t3", Host: host, Worktree: m.Agent.Worktree, Display: "My_Repo"}}
	if got, _ := json.Marshal(m); string(got) != string(must(json.Marshal(want))) {
		t.Errorf("meta =\n%s\nwant\n%s", got, must(json.Marshal(want)))
	}
	if !filepath.IsAbs(m.Agent.Worktree) || filepath.Base(m.Agent.Worktree) != "My_Repo" {
		t.Errorf("worktree = %q", m.Agent.Worktree)
	}
	// Every key boxd reads is there, even when empty.
	for _, k := range []string{`"v":1`, `"envNames":[`, `"gpu":false`, `"clientVersion":""`} {
		if !strings.Contains(raw, k) {
			t.Errorf("meta lacks %s:\n%s", k, raw)
		}
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

func TestMetaFromMCP(t *testing.T) {
	f := &fakePC{t: t, insts: []instance{{Name: "my-repo", Status: "Running"}}, exit: "0"}
	a, _ := testApp(f)
	a.Getenv = envMap(map[string]string{"CODEX_THREAD_ID": "codex-1"})
	dir := jsonString(newRepo(t))
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"claude-code","version":"2.1.0"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"box_run","arguments":{"dir":` + dir + `,"command":"true","reason":"` + testReason + `","env":{"CI":"1"}}}}`,
	}, "\n")
	var out bytes.Buffer
	if err := serveMCP(context.Background(), a, strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	m, raw := metaOf(t, f)
	if ag := m.Agent; ag.Client != "claude-code" || ag.ClientVersion != "2.1.0" || ag.Session != "codex-1" || ag.SessionSource != "codex" {
		t.Errorf("agent = %+v\n%s", ag, raw)
	}
	if m.Command != "true" || m.Reason != testReason || strings.Join(m.EnvNames, ",") != "CI" || m.Repo != "" || m.Branch == "" {
		t.Errorf("meta = %s", raw)
	}

	// T3 names the thread in _meta for Codex, whose MCP servers get no T3 variables.
	f = &fakePC{t: t, insts: []instance{{Name: "my-repo", Status: "Running"}}, exit: "0"}
	a, _ = testApp(f)
	a.Getenv = envMap(map[string]string{"CODEX_THREAD_ID": "codex-1"})
	res := callTool(context.Background(), a, json.RawMessage(`{"name":"box_run","_meta":{"threadId":"t3-thread"},"arguments":{"dir":`+dir+`,"command":"true","reason":"`+testReason+`"}}`), nil)
	if res["isError"] == true {
		t.Fatalf("box_run = %v", res)
	}
	if m, raw := metaOf(t, f); m.Agent.Session != "t3-thread" || m.Agent.SessionSource != "t3" || m.Agent.Client != "cli" {
		t.Errorf("agent = %s", raw)
	}
}

func TestStripCredentials(t *testing.T) {
	for in, want := range map[string]string{
		"":                                       "",
		"https://github.com/owner/repo":          "https://github.com/owner/repo",
		"https://tok:x-oauth@github.com/o/r.git": "https://github.com/o/r.git",
		"https://ghp_abc@github.com/o/r":         "https://github.com/o/r",
		"ssh://git@github.com/o/r.git":           "ssh://github.com/o/r.git",
		"git@github.com:o/r.git":                 "git@github.com:o/r.git",
		"ghp_abc@github.com:o/r.git":             "github.com:o/r.git",
		"/c/Users/someone/src/repo":              "/c/Users/someone/src/repo",
		"https://[bad":                           "",
	} {
		if got := stripCredentials(in); got != want {
			t.Errorf("stripCredentials(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDisplayCommand(t *testing.T) {
	for want, args := range map[string][]string{
		"go test ./... && echo ok":      {"go test ./... && echo ok"},
		"go test -run 'Test A' ./...":   {"go", "test", "-run", "Test A", "./..."},
		"npx vitest run --maxWorkers=2": {"npx", "vitest", "run", "--maxWorkers=2"},
	} {
		if got := displayCommand(args); got != want {
			t.Errorf("displayCommand(%q) = %q, want %q", args, got, want)
		}
	}
}

func TestRefusalPosted(t *testing.T) {
	dir := newRepo(t)
	gitT(t, dir, "remote", "add", "origin", "https://user:pw@github.com/owner/my-repo")
	f := &fakePC{t: t, mode: "mypc", remotes: testRemotes}
	a, out := testApp(f)
	var p posted
	a.Post = p.post
	r, err := a.Run(context.Background(), RunOpts{Dir: dir, Cmd: []string{"true"}, Reason: testReason, Size: "large"})
	if err != nil || r.Code != exitPaused {
		t.Fatalf("Run = %+v, %v\n%s", r, err, out)
	}
	if !strings.Contains(out.String(), "box: pc is in My PC mode; run it locally\n") {
		t.Errorf("output = %q", out)
	}
	if len(p.urls) != 1 || p.urls[0] != "https://agent-box.example.ts.net/api/refusals" {
		t.Fatalf("posted to %q", p.urls)
	}
	want := `{"why":"mypc","reason":"` + testReason + `","size":"large","gpu":false,"repo":"https://github.com/owner/my-repo","agent":"My_Repo"}`
	if p.raw[0] != want {
		t.Errorf("refusal =\n%s\nwant\n%s", p.raw[0], want)
	}
	if f.pushed != 0 {
		t.Error("pushed to a paused PC")
	}

	for name, c := range map[string]struct {
		f   *fakePC
		why string // "" posts nothing
	}{
		"yielding": {&fakePC{mode: "mypc", project: map[string]string{mode.ProjectReasonKey: "gaming: steam:570"}}, refusedYielding},
		"full":     {&fakePC{refuse: memoryRefusal}, refusedFull},
		"gpu busy": {&fakePC{mode: "shared", running: "other-1234.20261123-110000-beef",
			insts: []instance{{Name: "other-1234", Status: "Running", Config: gpuConfig(throwawayKey, "true")}}}, refusedGPUBusy},
		"unreachable":        {&fakePC{listErr: true}, ""},
		"frozen box, shared": {&fakePC{mode: "shared", insts: []instance{{Name: "my-repo", Status: "Frozen"}}}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			c.f.t, c.f.remotes = t, testRemotes
			a, out := testApp(c.f)
			p := posted{}
			a.Post = p.post
			// From MCP; the GPU case asks for the GPU.
			res := callTool(context.Background(), a, json.RawMessage(`{"name":"box_run","arguments":{"dir":`+jsonString(dir)+
				`,"command":"true","gpu":`+fmt.Sprint(c.why == refusedGPUBusy)+`,"reason":"`+testReason+`"}}`), nil)
			var sc struct {
				State    string
				ExitCode int
			}
			raw, _ := json.Marshal(res["structuredContent"])
			if json.Unmarshal(raw, &sc); sc.ExitCode != exitPaused || sc.State != stateRefused {
				t.Fatalf("box_run = %v\n%s", res, out)
			}
			switch {
			case c.why == "" && len(p.bodies) != 0:
				t.Errorf("posted %+v", p.bodies)
			case c.why != "" && (len(p.bodies) != 1 || p.bodies[0].Why != c.why):
				t.Errorf("posted %+v, want why %s", p.bodies, c.why)
			case c.why == refusedGPUBusy && (!p.bodies[0].GPU || p.bodies[0].Size != "gpu"):
				t.Errorf("GPU refusal = %+v", p.bodies[0])
			}
		})
	}
}

func TestRefusalErrorsIgnored(t *testing.T) {
	for name, remotes := range map[string]string{"post fails": testRemotes, "no remote": "", "bad json": "nope"} {
		t.Run(name, func(t *testing.T) {
			f := &fakePC{t: t, mode: "mypc", remotes: remotes}
			a, out := testApp(f)
			p := posted{err: errors.New("dial tcp: connection refused")}
			a.Post = p.post
			r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, Reason: testReason})
			if err != nil || r.Code != exitPaused || r.State != statePaused {
				t.Fatalf("Run = %+v, %v", r, err)
			}
			if strings.Contains(out.String(), "refused") || strings.Count(out.String(), "box:") != 1 {
				t.Errorf("output = %q", out)
			}
			if remotes != testRemotes && len(p.urls) != 0 {
				t.Errorf("posted without a host: %q", p.urls)
			}
		})
	}
}

// A run that ends sends no refusal, only how it ended, so boxd learns the
// result of a throwaway that is deleted before it looks.
func TestRunReportsItsEnd(t *testing.T) {
	f := &fakePC{t: t, insts: []instance{{Name: "my-repo", Status: "Running"}}, exit: "3", remotes: testRemotes}
	a, out := testApp(f)
	var p posted
	a.Post = p.post
	r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, Reason: testReason})
	if err != nil || r.Code != 3 {
		t.Fatalf("Run = %+v, %v\n%s", r, err, out)
	}
	if len(p.urls) != 1 || p.urls[0] != "https://agent-box.example.ts.net/api/jobs/ended" {
		t.Fatalf("posted to %q", p.urls)
	}
	var e dash.JobEndedBody
	if err := json.Unmarshal([]byte(p.raw[0]), &e); err != nil || e.Box != "my-repo" || e.Exit != 3 ||
		r.Job != e.Box+"."+e.ID || e.EndedAt.IsZero() {
		t.Errorf("ended = %s (job %s)", p.raw[0], r.Job)
	}

	// A throwaway is reported before it is deleted.
	f = &fakePC{t: t, exit: "0", remotes: testRemotes}
	a, out = testApp(f)
	p = posted{}
	a.Post = p.post
	if r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, Reason: testReason, New: true}); err != nil || r.Code != 0 {
		t.Fatalf("Run --new = %+v, %v\n%s", r, err, out)
	}
	calls := f.callList()
	if len(p.urls) != 1 || !strings.Contains(calls, "delete") || strings.Index(calls, "remote") > strings.LastIndex(calls, "delete") {
		t.Errorf("posted %q; calls %s", p.urls, calls)
	}
}

func TestTailWriterKeepsTheEnd(t *testing.T) {
	var sink bytes.Buffer
	tw := &tailWriter{w: &sink}
	tw.Write([]byte(strings.Repeat("x", 3000)))
	// The last TailBytes start inside the é, and hold a stray invalid byte.
	tw.Write([]byte("é" + strings.Repeat("y", dash.TailBytes-3) + "\xff" + "z"))
	got := tw.String()
	if sink.Len() != 3000+dash.TailBytes+1 || got != strings.Repeat("y", dash.TailBytes-3)+"z" {
		t.Fatalf("tail %d bytes, valid %v: %q...", len(got), utf8.ValidString(got), got[:min(len(got), 10)])
	}
}

func TestPCHost(t *testing.T) {
	for remotes, want := range map[string]string{
		testRemotes: "agent-box.example.ts.net",
		`{"pc":{"Addr":"https://old.example.ts.net:8443"}}`: "old.example.ts.net",
		`{"pc":{"Addrs":["unix://"]}}`:                      "",
		`{"other":{"Addrs":["https://x.example:8443"]}}`:    "",
	} {
		a, _ := testApp(&fakePC{t: t, remotes: remotes})
		if got := a.pcHost(context.Background()); got != want {
			t.Errorf("pcHost(%s) = %q, want %q", remotes, got, want)
		}
	}
}

func TestPostJSON(t *testing.T) {
	var got *http.Request
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b := new(bytes.Buffer)
		b.ReadFrom(r.Body)
		body = b.String()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := postJSON(context.Background(), srv.URL+"/api/refusals", []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodPost || got.URL.Path != "/api/refusals" || got.Header.Get("Content-Type") != "application/json" ||
		got.Header.Get("X-Agent-Box") != "1" || body != `{"v":1}` {
		t.Errorf("request = %s %s %v %q", got.Method, got.URL, got.Header, body)
	}
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) })
	if err := postJSON(context.Background(), srv.URL+"/api/refusals", nil); err == nil {
		t.Error("403 wasn't an error")
	}
}
