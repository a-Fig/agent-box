package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/controller"
	"github.com/a-Fig/agent-box/internal/dash"
)

func setTestConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("APPDATA", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(configDir, "agent-box")
}

func writeControllerConfig(t *testing.T, url, token string) string {
	t.Helper()
	dir := setTestConfigDir(t)
	return writeControllerConfigInDir(t, dir, url, token)
}

func writeControllerConfigInDir(t *testing.T, dir, url, token string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	tokPath := filepath.Join(dir, "controller.token")
	if err := os.WriteFile(tokPath, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "controller.json")
	data, err := json.Marshal(controllerConfig{URL: url, TokenFile: tokPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

type countedController struct {
	mu         sync.Mutex
	hostsCalls int
	usageCalls int
	handler    http.Handler
}

func (c *countedController) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	switch r.URL.Path {
	case "/v1/hosts":
		c.hostsCalls++
	case "/v1/usage":
		c.usageCalls++
	}
	c.mu.Unlock()
	c.handler.ServeHTTP(w, r)
}

func (c *countedController) counts() (hosts, usage int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hostsCalls, c.usageCalls
}

func TestConfiguredControllerValidation(t *testing.T) {
	// Missing file -> nil, "", nil.
	dir := setTestConfigDir(t)
	cfg, tok, err := configuredController()
	if err != nil || cfg != nil || tok != "" {
		t.Fatalf("missing config = %+v, %q, %v", cfg, tok, err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	tokPath := filepath.Join(dir, "client.token")
	if err := os.WriteFile(tokPath, []byte("client-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "controller.json")

	// Loopback HTTP allowed for tests; HTTPS allowed.
	for _, validURL := range []string{"https://ctrl.example.com", "http://127.0.0.1:8787", "http://localhost:8787", "http://[::1]:8787"} {
		b, _ := json.Marshal(controllerConfig{URL: validURL, TokenFile: tokPath})
		os.WriteFile(cfgPath, b, 0o600)
		cfg, tok, err := configuredController()
		if err != nil || cfg == nil || tok != "client-secret" {
			t.Fatalf("valid URL %q failed: cfg=%+v tok=%q err=%v", validURL, cfg, tok, err)
		}
	}

	// Non-loopback HTTP, bad scheme, or bad token_file rejected.
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"non-loopback http", `{"url":"http://ctrl.example.com","token_file":"` + filepath.ToSlash(tokPath) + `"}`},
		{"ftp scheme", `{"url":"ftp://ctrl.example.com","token_file":"` + filepath.ToSlash(tokPath) + `"}`},
		{"credentials in url", `{"url":"https://user:pass@ctrl.example.com","token_file":"` + filepath.ToSlash(tokPath) + `"}`},
		{"missing token_file", `{"url":"https://ctrl.example.com"}`},
		{"missing token target", `{"url":"https://ctrl.example.com","token_file":"` + filepath.ToSlash(filepath.Join(dir, "missing.tok")) + `"}`},
		{"malformed json", `{`},
	} {
		os.WriteFile(cfgPath, []byte(tc.raw), 0o600)
		if _, _, err := configuredController(); err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
	}
}

func TestControllerReachableMakesZeroControllerCalls(t *testing.T) {
	now := time.Date(2026, 11, 27, 15, 50, 0, 0, time.UTC)
	cc := &countedController{
		handler: &controller.MemServer{
			HostToken:   "host-tok",
			ClientToken: "client-tok",
			Now:         func() time.Time { return now },
		},
	}
	srv := httptest.NewServer(cc)
	defer srv.Close()

	writeControllerConfig(t, srv.URL, "client-tok\n")

	a, _ := testApp(&fakePC{t: t, mode: "shared"})
	s, err := a.fleetStatus(context.Background(), "", false)
	if err != nil || !s.Reachable || s.ControllerUsage != nil {
		t.Fatalf("fleetStatus() = %+v, %v", s, err)
	}
	hostsCalls, usageCalls := cc.counts()
	if hostsCalls != 0 || usageCalls != 0 {
		t.Fatalf("controller calls = hosts:%d usage:%d, want hosts:0 usage:0", hostsCalls, usageCalls)
	}
	var out strings.Builder
	writeStatus(&out, s)
	if strings.Contains(out.String(), "controller:") {
		t.Fatalf("unexpected controller usage line when all hosts reachable:\n%s", out.String())
	}
}

func TestControllerFetchesHostsAndUsageConcurrentlyWhenUnreachable(t *testing.T) {
	now := time.Date(2026, 11, 27, 15, 45, 0, 0, time.UTC)
	ms := &controller.MemServer{
		HostToken:   "host-tok",
		ClientToken: "client-tok",
		Now:         func() time.Time { return now },
	}

	// Barrier proves /v1/hosts and /v1/usage execute concurrently: sequential
	// calls would block on the barrier until context timeout.
	bothArrived := make(chan struct{})
	var arrivedMu sync.Mutex
	seen := map[string]bool{}
	barrierHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hosts" || r.URL.Path == "/v1/usage" {
			arrivedMu.Lock()
			seen[r.URL.Path] = true
			if seen["/v1/hosts"] && seen["/v1/usage"] {
				select {
				case <-bothArrived:
				default:
					close(bothArrived)
				}
			}
			arrivedMu.Unlock()
			select {
			case <-bothArrived:
			case <-r.Context().Done():
				return
			}
		}
		ms.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(barrierHandler)
	defer srv.Close()

	hostClient := &controller.Client{URL: srv.URL, Token: "host-tok"}
	if err := hostClient.PutState(context.Background(), "pc", controller.PutStateRequest{
		ID:     "pc",
		Name:   "PC",
		Seq:    1,
		SentAt: now.Add(-5 * time.Minute),
		State: dash.State{
			Machines: []dash.Machine{{ID: "pc", Mode: dash.ModeShared}},
			Now:      now.Add(-5 * time.Minute),
		},
	}); err != nil {
		t.Fatal(err)
	}

	writeControllerConfig(t, srv.URL, "client-tok")

	a := newApp()
	a.R = fleetTestRunner(func(_ context.Context, _ Cmd) (Result, error) {
		return Result{Code: 1, Stderr: "connection refused"}, nil
	})
	s, err := a.fleetStatus(context.Background(), "", false)
	if err != nil || s.Reachable || s.LastSeen == nil || s.ControllerUsage == nil {
		t.Fatalf("concurrent unreachable fallback = %+v, err=%v", s, err)
	}
	var out strings.Builder
	writeStatus(&out, s)
	if !strings.Contains(out.String(), "requests today") {
		t.Fatalf("missing controller usage line:\n%s", out.String())
	}
}

func TestControllerBrokenConfigWarnsAndContinues(t *testing.T) {
	dir := setTestConfigDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "controller.json"), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}

	var warnings []string
	a, _ := testApp(&fakePC{t: t, mode: "shared"})
	a.Warn = func(msg string) { warnings = append(warnings, msg) }
	s, err := a.fleetStatus(context.Background(), "", false)
	if err != nil || !s.Reachable {
		t.Fatalf("fleetStatus() with broken controller.json = %+v, %v", s, err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "controller config") {
		t.Fatalf("warnings = %v, want controller config warning", warnings)
	}
}

func TestControllerSingleHostUnreachableFallbackAndStaleIndicator(t *testing.T) {
	now := time.Date(2026, 11, 27, 15, 36, 0, 0, time.UTC)
	ms := &controller.MemServer{
		HostToken:   "host-tok",
		ClientToken: "client-tok",
		Now:         func() time.Time { return now },
	}
	cc := &countedController{handler: ms}
	srv := httptest.NewServer(cc)
	defer srv.Close()

	// Seed controller state for "pc" at 15:36:00Z.
	hostClient := &controller.Client{URL: srv.URL, Token: "host-tok"}
	err := hostClient.PutState(context.Background(), "pc", controller.PutStateRequest{
		ID:     "pc",
		Name:   "PC",
		Seq:    1,
		SentAt: now,
		State: dash.State{
			Machines: []dash.Machine{{ID: "pc", Name: "PC", Mode: dash.ModeShared}},
			Boxes:    []dash.Box{{Name: "b1"}, {Name: "b2"}},
			Jobs: []dash.Job{
				{Key: "b1/j1/t", State: dash.JobRunning},
				{Key: "b2/j2/t", State: dash.JobDone},
			},
			Now: now,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	writeControllerConfig(t, srv.URL, "client-tok")

	// Unreachable PC 2 minutes later -> not stale yet.
	now = now.Add(2 * time.Minute)
	a := newApp()
	a.R = fleetTestRunner(func(_ context.Context, _ Cmd) (Result, error) {
		return Result{Code: 1, Stderr: "connection refused"}, nil
	})
	s, err := a.fleetStatus(context.Background(), "", false)
	if err != nil || s.Reachable || s.LastSeen == nil || s.LastState == nil || s.LastStateStale {
		t.Fatalf("fresh cached fallback = %+v, err=%v", s, err)
	}
	var out strings.Builder
	writeStatus(&out, s)
	if !strings.Contains(out.String(), "pc: unreachable, last seen 2m ago: Shared · 2 boxes · 1 running") {
		t.Fatalf("unexpected non-stale output:\n%s", out.String())
	}

	// 14 minutes after PutState -> stale indicator appears.
	now = now.Add(12 * time.Minute)
	s, err = a.fleetStatus(context.Background(), "", false)
	if err != nil || !s.LastStateStale {
		t.Fatalf("stale cached fallback = %+v, err=%v", s, err)
	}
	out.Reset()
	writeStatus(&out, s)
	if !strings.Contains(out.String(), "pc: unreachable, last seen 14m ago (stale): Shared · 2 boxes · 1 running") {
		t.Fatalf("unexpected stale output:\n%s", out.String())
	}

	// Verify MCP box_status surfaces the cached fallback and usage too.
	res := callTool(context.Background(), a, json.RawMessage(`{"name":"box_status","arguments":{}}`), nil)
	content := res["content"].([]map[string]any)
	text := content[0]["text"].(string)
	if !strings.Contains(text, "pc: unreachable, last seen 14m ago (stale): Shared · 2 boxes · 1 running") ||
		!strings.Contains(text, "requests today") {
		t.Fatalf("MCP box_status text = %q", text)
	}
}

func TestControllerFleetAtMostOneHostsCallAndControllerID(t *testing.T) {
	now := time.Date(2026, 11, 27, 15, 50, 0, 0, time.UTC)
	ms := &controller.MemServer{
		HostToken:   "host-tok",
		ClientToken: "client-tok",
		Now:         func() time.Time { return now },
	}
	cc := &countedController{handler: ms}
	srv := httptest.NewServer(cc)
	defer srv.Close()

	hostClient := &controller.Client{URL: srv.URL, Token: "host-tok"}
	for _, rec := range []struct {
		id, mode string
		boxes    int
		running  int
	}{
		{"primary", dash.ModeFull, 1, 1},
		{"lab-linux", dash.ModeShared, 2, 0},
	} {
		var boxes []dash.Box
		for i := 0; i < rec.boxes; i++ {
			boxes = append(boxes, dash.Box{Name: "b"})
		}
		var jobs []dash.Job
		for i := 0; i < rec.running; i++ {
			jobs = append(jobs, dash.Job{Key: "k", State: dash.JobRunning})
		}
		if err := hostClient.PutState(context.Background(), rec.id, controller.PutStateRequest{
			ID:     rec.id,
			Name:   rec.id,
			Seq:    1,
			SentAt: now,
			State: dash.State{
				Machines: []dash.Machine{{ID: rec.id, Mode: rec.mode}},
				Boxes:    boxes,
				Jobs:     jobs,
				Now:      now,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	now = now.Add(10 * time.Minute)
	dir := writeControllerConfig(t, srv.URL, "client-tok")
	hostsJSON := `{"hosts":[{"name":"primary","remote":"pc"},{"name":"lab","remote":"lab-remote","controller_id":"lab-linux"}]}`
	if err := os.WriteFile(filepath.Join(dir, "hosts.json"), []byte(hostsJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	a := newApp()
	a.R = fleetTestRunner(func(_ context.Context, _ Cmd) (Result, error) {
		return Result{Code: 1, Stderr: "timeout"}, nil
	})
	s, err := a.fleetStatus(context.Background(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	hostsCalls, usageCalls := cc.counts()
	if hostsCalls != 1 || usageCalls != 1 {
		t.Fatalf("controller calls for 2 unreachable hosts = hosts:%d usage:%d, want 1/1", hostsCalls, usageCalls)
	}

	var out strings.Builder
	writeStatus(&out, s)
	got := out.String()
	if !strings.Contains(got, "primary (pc/agents): unreachable, last seen 10m ago (stale): Full · 1 box · 1 running") ||
		!strings.Contains(got, "lab (lab-remote/agents): unreachable, last seen 10m ago (stale): Shared · 2 boxes · 0 running") {
		t.Fatalf("fleet output missing enriched hosts:\n%s", got)
	}
}

func TestControllerOutageDegradesGracefully(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	writeControllerConfig(t, srv.URL, "client-tok")

	// Reachable PC still succeeds without controller usage.
	a, _ := testApp(&fakePC{t: t, mode: "full"})
	s, err := a.fleetStatus(context.Background(), "", false)
	if err != nil || !s.Reachable || !s.Available || s.ControllerUsage != nil {
		t.Fatalf("reachable during controller outage = %+v, %v", s, err)
	}

	// Unreachable PC still produces standard unreachable status without error.
	downApp := newApp()
	downApp.R = fleetTestRunner(func(_ context.Context, _ Cmd) (Result, error) {
		return Result{Code: 1, Stderr: "connection refused"}, nil
	})
	s2, err := downApp.fleetStatus(context.Background(), "", false)
	if err != nil || s2.Reachable || s2.LastSeen != nil || s2.ControllerUsage != nil || !strings.Contains(s2.Advice, "can't reach pc") {
		t.Fatalf("unreachable during controller outage = %+v, %v", s2, err)
	}
}

func TestFormatControllerUsageCommas(t *testing.T) {
	got := formatControllerUsage(controller.UsageResponse{Day: "2026-11-27", Requests: 2412, Limit: 100000})
	if got != "controller: 2,412 of 100,000 requests today" {
		t.Fatalf("got %q", got)
	}
	if gotZero := formatControllerUsage(controller.UsageResponse{Requests: 0, Limit: 100000}); gotZero != "controller: 0 of 100,000 requests today" {
		t.Fatalf("got %q", gotZero)
	}
}
