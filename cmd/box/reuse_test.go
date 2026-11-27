package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/jobrun"
	"github.com/a-Fig/agent-box/internal/mode"
)

func TestStoppedCPUThrowawayGivesWayToARequestThatFits(t *testing.T) {
	for _, pooled := range []bool{false, true} {
		name := "unpooled"
		if pooled {
			name = "pooled"
		}
		t.Run(name, func(t *testing.T) {
			cfg := map[string]string{
				jobrun.ThrowawayKey:     "true",
				jobrun.ThrowawayStopKey: jobrun.ThrowawayStopped,
				jobrun.ExpiresKey:       "2026-11-23T12:20:00Z",
				mode.SizeKey:            "small",
				"volatile.uuid":         "original",
			}
			if pooled {
				cfg[jobrun.ThrowawayRepoKey] = strings.Repeat("a", 64)
			}
			stopped := instance{Name: "finished", Status: "Stopped", Config: cfg}
			f := &fakePC{t: t, refuse: memoryRefusal, refuseOnce: true, kept: map[string]bool{"repo": true}, insts: []instance{
				stopped,
				{Name: "repo", Status: "Running", Config: map[string]string{jobrun.ExpiresKey: "2026-11-23T13:00:00Z"}},
				{Name: "starting", Status: "Stopped", Config: map[string]string{jobrun.ThrowawayKey: "true", jobrun.ExpiresKey: cfg[jobrun.ExpiresKey]}},
			}}
			a, out := testApp(f)
			a.dashboardURL = "https://fixture.example.com"
			evictions := 0
			action := "evict-kept"
			if pooled {
				action = "evict-idle"
			}
			a.HTTP = func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodPost || req.URL.Path != "/api/boxes/finished/"+action {
					t.Fatalf("unexpected boxd reclaim: %s %s", req.Method, req.URL)
				}
				var b struct{ UID string }
				if err := json.NewDecoder(req.Body).Decode(&b); err != nil || b.UID != "original" {
					t.Fatalf("wrong pooled eviction identity: %+v, %v", b, err)
				}
				evictions++
				f.mu.Lock()
				f.insts = f.insts[1:]
				f.mu.Unlock()
				return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
			ctx := context.Background()
			if err := a.launch(ctx, "new", time.Hour, true, mustSize(t, "small")); err != nil {
				t.Fatalf("launch: %v\n%s", err, out)
			}
			if got := f.names(); strings.Contains(got, "finished") || !strings.Contains(got, "repo") || !strings.Contains(got, "starting") {
				t.Fatalf("pressure evicted the wrong box: %s", got)
			}
			if evictions != 1 || strings.Contains(f.callList(), "delete") {
				t.Fatalf("wrong eviction path for %s: HTTP %d, Incus %s", name, evictions, f.callList())
			}
		})
	}
}

func TestStoppedCPUThrowawayRechecksIdentityAndHold(t *testing.T) {
	for _, change := range []string{"hold", "replacement"} {
		t.Run(change, func(t *testing.T) {
			const expiry = "2026-11-23T12:20:00Z"
			selected := instance{Name: "finished", Status: "Stopped", Config: map[string]string{
				jobrun.ThrowawayKey: "true", jobrun.ThrowawayStopKey: jobrun.ThrowawayStopped, jobrun.ExpiresKey: expiry, "volatile.uuid": "original",
			}}
			current := selected
			current.Config = map[string]string{
				jobrun.ThrowawayKey: "true", jobrun.ThrowawayStopKey: jobrun.ThrowawayStopped, jobrun.ExpiresKey: expiry, "volatile.uuid": "original",
			}
			if change == "hold" {
				current.Config[mode.HoldKey] = expiry
			} else {
				current.Config["volatile.uuid"] = "replacement"
			}
			a, _ := testApp(&fakePC{t: t, insts: []instance{current}})
			if a.stillIdle(context.Background(), selected, "") {
				t.Fatalf("pressure recheck accepted %s: %+v", change, current)
			}
		})
	}
}

func TestKeptEvictionRefusesUnverifiedLegacyFallback(t *testing.T) {
	a, _ := testApp(&fakePC{t: t})
	a.dashboardURL = "https://fixture.example.com"
	a.HTTP = func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody}, nil
	}
	if err := a.evictKept(context.Background(), instance{Name: "kept", Status: "Running"}); err != errNotIdle {
		t.Fatalf("generic 404 permitted unreserved eviction: %v", err)
	}
}

func TestLaunchReclaimsUnpooledThrowawayWhenBoxdIsUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		stopping bool
	}{
		{name: "offline"},
		{name: "forbidden", status: http.StatusForbidden},
		{name: "bad gateway", status: http.StatusBadGateway},
		{name: "service unavailable", status: http.StatusServiceUnavailable},
		{name: "gateway timeout", status: http.StatusGatewayTimeout},
		{name: "stop reserved during fallback", status: http.StatusBadGateway, stopping: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const name = "kept"
			cfg := map[string]string{
				jobrun.ThrowawayKey: "true",
				jobrun.ExpiresKey:   "2026-11-23T12:20:00Z",
				mode.SizeKey:        "small",
			}
			f := &fakePC{t: t, refuse: memoryRefusal, kept: map[string]bool{name: true},
				insts: []instance{{Name: name, Status: "Running", Config: cfg}}}
			a, out := testApp(f)
			a.dashboardURL = "https://fixture.example.com"
			a.HTTP = func(*http.Request) (*http.Response, error) {
				if tc.status != 0 {
					return &http.Response{StatusCode: tc.status, Body: http.NoBody}, nil
				}
				return nil, errors.New("boxd unavailable")
			}
			a.R = lookupRunner(func(ctx context.Context, cmd Cmd) (Result, error) {
				args := strings.Join(cmd.Args, " ")
				if tc.stopping && strings.Contains(args, "config set pc:"+name+" "+jobrun.DirectEvictKey+"=") {
					cfg[jobrun.ThrowawayStopKey] = "stop token"
				}
				if strings.Contains(args, "delete pc:"+name+" ") {
					if cfg[jobrun.DirectEvictKey] == "" {
						t.Fatal("direct delete ran without a reservation")
					}
					f.refuse = ""
				}
				return f.Run(ctx, cmd)
			})
			err := a.launch(context.Background(), "new", time.Hour, true, mustSize(t, "small"))
			if tc.stopping {
				if !isNoRoom(err) || f.names() != name || strings.Contains(f.callList(), "delete") {
					t.Fatalf("evicted while cooling: %v, %s\n%s", err, f.names(), out)
				}
			} else if err != nil || f.names() != "new" {
				t.Fatalf("launch did not reclaim %s: %v, %s\n%s", name, err, f.names(), out)
			}
		})
	}
}

func TestRmRefusesActiveIdleStop(t *testing.T) {
	cfg := map[string]string{jobrun.ThrowawayStopKey: "stop token"}
	f := &fakePC{t: t, insts: []instance{{Name: "kept", Status: "Running", Config: cfg}}}
	a, _ := testApp(f)
	if err := a.Rm(context.Background(), "kept"); err == nil || strings.Contains(f.callList(), "delete") {
		t.Fatalf("rm dispatched during cooling: %v, %s", err, f.callList())
	}
	cfg[jobrun.ThrowawayStopKey] = jobrun.ThrowawayStopped
	if err := a.Rm(context.Background(), "kept"); err != nil || f.names() != "" {
		t.Fatalf("rm of a cooled box: %v, boxes %s", err, f.names())
	}
}

func TestMarkIdleKeepsThrowawayExpiryDuringCooling(t *testing.T) {
	for _, when := range []string{"before write", "after write"} {
		t.Run(when, func(t *testing.T) {
			const box = "kept"
			f := &fakePC{t: t}
			a, _ := testApp(f)
			cfg := map[string]string{
				jobrun.ThrowawayKey: "true",
				jobrun.ExpiresKey:   a.Now().Add(endedKeep).Format(time.RFC3339),
			}
			f.insts = []instance{{Name: box, Status: "Running", Config: cfg}}
			wrote := false
			a.R = lookupRunner(func(ctx context.Context, cmd Cmd) (Result, error) {
				args := strings.Join(cmd.Args, " ")
				if when == "before write" && strings.Contains(args, "config get pc:"+box+" "+jobrun.ThrowawayStopKey) {
					cfg[jobrun.ThrowawayStopKey] = "stop token"
				}
				res, err := f.Run(ctx, cmd)
				if strings.Contains(args, "config set pc:"+box+" "+jobrun.ExpiresKey+"=") {
					wrote = true
					f.insts[0].Status = "Stopped"
					cfg[jobrun.ThrowawayStopKey] = jobrun.ThrowawayStopped
				}
				return res, err
			})
			a.markIdle(context.Background(), box, box+".20261123-120000-0001")
			if cfg[jobrun.ExpiresKey] != a.Now().Add(endedKeep).Format(time.RFC3339) {
				t.Fatalf("markIdle extended the retrieval window: %v", cfg)
			}
			if wrote != (when == "after write") {
				t.Fatalf("unexpected expiry write: %s", f.callList())
			}
		})
	}
}

func TestRunRecoversExitWhenCoolingInterruptsFollow(t *testing.T) {
	f := &fakePC{t: t}
	a, out := testApp(f)
	a.dashboardURL = "https://fixture.example.com"
	followStopped := false
	a.R = lookupRunner(func(ctx context.Context, cmd Cmd) (Result, error) {
		if isScript(cmd, "# follow") {
			// Boxd has already read the exit file. The guest stops before
			// the stream can report the terminal marker.
			followStopped = true
			return Result{}, nil
		}
		return f.Run(ctx, cmd)
	})
	a.HTTP = func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/api/throwaway-pool" {
			return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody}, nil
		}
		if !followStopped || !strings.HasPrefix(req.URL.Path, "/api/queue/") {
			t.Fatalf("unexpected result lookup: %s", req.URL)
		}
		return &http.Response{StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(`{"state":"done","exit":0}`))}, nil
	}
	result, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, New: true})
	if err != nil || !followStopped || result.State != stateDone || result.Code != 0 || strings.Contains(f.callList(), "delete") {
		t.Fatalf("interrupted follow returned %+v, %v\n%s", result, err, out)
	}
}

func TestRunClaimsMatchingThrowawayOrLaunches(t *testing.T) {
	const origin = "https://example.test/team/my-repo.git"
	for _, tt := range []struct {
		name   string
		status int
		fresh  bool
		cancel bool
	}{
		{name: "warm", status: http.StatusNoContent},
		{name: "busy", status: http.StatusConflict},
		{name: "old host", status: http.StatusNotFound},
		{name: "fresh", fresh: true},
		{name: "cancelled setup", status: http.StatusNoContent, cancel: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := newRepo(t)
			gitT(t, dir, "remote", "add", "origin", origin)
			f := &fakePC{t: t, exit: "0", remotes: testRemotes, pushHang: tt.cancel}
			a, out := testApp(f)
			a.KeepEnded = jobrun.EndedKeep
			now := a.Now()
			sz := mustSize(t, "small")
			repoKey := throwawayRepoKey(origin, sz)
			candidate := "my-repo-9999"
			idleUntil := now.Add(20 * time.Minute).Format(time.RFC3339)
			f.insts = []instance{{Name: candidate, Status: "Running", Config: map[string]string{
				jobrun.ThrowawayKey:     "true",
				jobrun.ThrowawayRepoKey: repoKey,
				jobrun.ExpiresKey:       idleUntil,
				mode.SizeKey:            sz.Name,
			}}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			claims := 0
			a.HTTP = func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/api/throwaway-pool" && r.Method == http.MethodGet {
					status := http.StatusNoContent
					if tt.status == http.StatusNotFound {
						status = http.StatusNotFound
					}
					return &http.Response{StatusCode: status, Body: http.NoBody}, nil
				}
				if strings.HasSuffix(r.URL.Path, "/pool-expiry") {
					var req dash.PoolExpiryBody
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Job == "" {
						t.Fatalf("bad pool expiry request %+v: %v", req, err)
					}
					if tt.status == http.StatusNoContent {
						f.insts[0].Config[jobrun.ThrowawayClaimKey] = ""
					}
					return &http.Response{StatusCode: tt.status, Body: http.NoBody}, nil
				}
				claims++
				var req dash.ClaimThrowawayBody
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RepoKey != repoKey || req.Size != sz.Name ||
					req.Job != candidate+".20261123-120000-0001" ||
					r.URL.Path != "/api/boxes/"+candidate+"/claim" || r.Method != http.MethodPost {
					t.Fatalf("claim request %s %s, body %+v, decode %v", r.Method, r.URL.Path, req, err)
				}
				if tt.status == http.StatusNoContent {
					f.insts[0].Config[jobrun.ThrowawayClaimKey] = now.Add(jobrun.ThrowawayClaimTTL).Format(time.RFC3339)
				}
				if tt.cancel {
					cancel()
				}
				return &http.Response{StatusCode: tt.status, Body: http.NoBody}, nil
			}
			r, err := a.Run(ctx, RunOpts{Dir: dir, Cmd: []string{"true"}, New: !tt.fresh, Fresh: tt.fresh, Size: sz.Name})
			if tt.cancel {
				if err == nil || r.Box != candidate || claims != 1 || f.insts[0].Config[jobrun.ExpiresKey] != idleUntil {
					t.Fatalf("cancelled run = %+v, %v, expiry = %s", r, err, f.insts[0].Config[jobrun.ExpiresKey])
				}
				return
			}
			if err != nil || r.Code != 0 || (claims == 0) != (tt.fresh || tt.status == http.StatusNotFound) || claims > 1 {
				t.Fatalf("run = %+v, %v; %d claims\n%s", r, err, claims, out)
			}
			if tt.status == http.StatusNoContent {
				if len(f.launches) != 0 || r.Box != candidate || f.insts[0].Config[jobrun.ThrowawayClaimKey] != "" ||
					!strings.Contains(out.String(), "reusing idle throwaway") {
					t.Fatalf("reuse result = %+v, launches = %d, config = %v\n%s", r, len(f.launches), f.insts[0].Config, out)
				}
			} else if len(f.launches) != 1 || r.Box == candidate {
				t.Fatalf("fallback result = %+v, launches = %d\n%s", r, len(f.launches), out)
			}
			if tt.status == http.StatusNotFound && f.insts[1].Config[jobrun.ThrowawayRepoKey] != "" {
				t.Fatal("left pool metadata on a host without pooling")
			}
		})
	}
}

func TestMarkIdleUsesOwnerCheckedExpiryForPooledBox(t *testing.T) {
	const box, job = "my-repo-9999", "my-repo-9999.20261123-120000-0001"
	f := &fakePC{t: t, remotes: testRemotes}
	a, _ := testApp(f)
	deadline := a.Now().Add(jobTTL).Format(time.RFC3339)
	f.insts = []instance{{Name: box, Status: "Running", Config: map[string]string{
		jobrun.ThrowawayRepoKey: "pool",
		jobrun.ExpiresKey:       deadline,
	}}}
	post := false
	a.HTTP = func(r *http.Request) (*http.Response, error) {
		var got dash.PoolExpiryBody
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil || got.Job != job || !got.Ended {
			t.Fatalf("expiry request = %+v, %v", got, err)
		}
		post = r.URL.Path == "/api/boxes/"+box+"/pool-expiry"
		return &http.Response{StatusCode: http.StatusConflict, Body: http.NoBody}, nil
	}
	a.markIdle(context.Background(), box, job)
	if !post || f.insts[0].Config[jobrun.ExpiresKey] != deadline {
		t.Fatalf("older job changed expiry after boxd refused it: %v", f.insts[0].Config)
	}
}

func TestLaunchTriesAnotherPooledBoxAfterEvictionConflict(t *testing.T) {
	const first, second = "widget-0001", "widget-0002"
	f := &fakePC{t: t, remotes: testRemotes, refuse: memoryRefusal,
		kept:    map[string]bool{first: true, second: true},
		project: map[string]string{"limits.instances": "2"},
	}
	a, out := testApp(f)
	sz := mustSize(t, "small")
	for i, name := range []string{first, second} {
		f.insts = append(f.insts, instance{Name: name, Status: "Running", Config: map[string]string{
			jobrun.ThrowawayKey:     "true",
			jobrun.ThrowawayRepoKey: "pool",
			jobrun.ExpiresKey:       a.Now().Add(time.Duration(20+i) * time.Minute).Format(time.RFC3339),
			mode.SizeKey:            sz.Name,
		}})
	}
	var tried []string
	a.HTTP = func(r *http.Request) (*http.Response, error) {
		tried = append(tried, r.URL.Path)
		switch r.URL.Path {
		case "/api/boxes/" + first + "/evict-idle":
			return &http.Response{StatusCode: http.StatusConflict, Body: http.NoBody}, nil
		case "/api/boxes/" + second + "/evict-idle":
			f.insts = f.insts[:1]
			f.refuse = ""
			return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
			return nil, nil
		}
	}
	if err := a.launch(context.Background(), "new-1", time.Hour, true, sz); err != nil {
		t.Fatalf("launch: %v\n%s", err, out)
	}
	if len(tried) != 2 || !strings.Contains(f.names(), first) || strings.Contains(f.names(), second) {
		t.Fatalf("evictions = %v, boxes = %s", tried, f.names())
	}
}
