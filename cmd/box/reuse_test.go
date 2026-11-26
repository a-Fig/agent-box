package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/jobrun"
	"github.com/a-Fig/agent-box/internal/mode"
)

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
