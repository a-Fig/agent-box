package boxd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/jobrun"
)

func TestClaimThrowawayChecksAndReservesIdleBox(t *testing.T) {
	key := strings.Repeat("a", 64)
	for _, scenario := range []string{"idle", "wrong repo", "wrong size", "starting", "no finished job", "hold during claim"} {
		t.Run(scenario, func(t *testing.T) {
			r := newQueueRig(t, nil)
			srv := httptest.NewServer((&Server{W: r.w, ReuseThrowaways: true}).Handler())
			t.Cleanup(srv.Close)
			in := inst("widget-1234", "Running", "small", t0)
			in.Config[jobrun.ThrowawayKey] = "true"
			in.Config[jobrun.ThrowawayRepoKey] = key
			in.Config[jobrun.ExpiresKey] = t0.Add(5 * time.Minute).Format(time.RFC3339)
			r.f.ins = []Instance{in}
			r.f.jobs[in.Name] = []fakeJob{{name: in.Name + ".old", state: "done", exit: "0"}}
			req := dash.ClaimThrowawayBody{RepoKey: key, Size: "small", Job: in.Name + ".20261124-180000-1111"}
			switch scenario {
			case "wrong repo":
				req.RepoKey = strings.Repeat("b", 64)
			case "wrong size":
				req.Size = "medium"
			case "starting":
				r.f.jobs[in.Name] = append(r.f.jobs[in.Name], fakeJob{name: in.Name + ".next", state: "starting"})
			case "no finished job":
				delete(r.f.jobs, in.Name)
			case "hold during claim":
				r.f.between = func(c map[string]string) {
					c["user.box.hold-until"] = t0.Add(time.Hour).Format(time.RFC3339)
				}
			}
			claim := func() int {
				t.Helper()
				body, _ := json.Marshal(req)
				request, err := http.NewRequest(http.MethodPost, srv.URL+"/api/boxes/"+in.Name+"/claim", bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set(dash.ControlHeader, "1")
				res, err := http.DefaultClient.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				res.Body.Close()
				return res.StatusCode
			}
			if scenario != "idle" {
				if got := claim(); got != http.StatusConflict {
					t.Fatalf("claim returned %d, want conflict", got)
				}
				return
			}
			if got := claim(); got != http.StatusNoContent {
				t.Fatalf("claim returned %d, want no content", got)
			}
			claimed, _ := r.f.instance(in.Name)
			until := t0.Add(jobrun.ThrowawayClaimTTL).Format(time.RFC3339)
			if claimed.Config[jobrun.ExpiresKey] != until || claimed.Config[jobrun.ThrowawayClaimKey] != until ||
				claimed.Config[jobrun.ThrowawayOwnerKey] != req.Job {
				t.Fatalf("claim config = %v, want claim and expiry through %s", claimed.Config, until)
			}
			if got := claim(); got != http.StatusConflict {
				t.Fatalf("second claim returned %d, want conflict", got)
			}
			if r.w.stillIdle(context.Background(), in) {
				t.Fatal("eviction accepted a box claimed after the poll")
			}
		})
	}
}

func TestPooledEvictionAndOldCleanupLoseToClaim(t *testing.T) {
	for _, action := range []string{"eviction", "cleanup"} {
		t.Run(action, func(t *testing.T) {
			r := newQueueRig(t, nil)
			in := inst("widget-1234", "Running", "small", t0)
			old, next := in.Name+".20261124-180000-1111", in.Name+".20261124-180000-2222"
			in.Config[jobrun.ThrowawayKey] = "true"
			in.Config[jobrun.ThrowawayRepoKey] = strings.Repeat("a", 64)
			in.Config[jobrun.ThrowawayOwnerKey] = old
			deadline := t0.Add(jobrun.EndedKeep).Format(time.RFC3339)
			if action == "cleanup" {
				deadline = t0.Add(jobTTL).Format(time.RFC3339)
			}
			in.Config[jobrun.ExpiresKey] = deadline
			r.f.ins = []Instance{in}
			r.f.jobs[in.Name] = []fakeJob{{name: old, state: "done", exit: "0"}}
			r.f.between = func(cfg map[string]string) {
				cfg[jobrun.ThrowawayClaimKey] = t0.Add(jobrun.ThrowawayClaimTTL).Format(time.RFC3339)
				cfg[jobrun.ThrowawayOwnerKey] = next
			}
			ctx := context.Background()
			var changed bool
			var err error
			if action == "eviction" {
				changed, err = r.w.evictPooled(ctx, in.Name, boxUID(in))
			} else {
				changed, err = r.w.setPoolExpiry(ctx, in.Name, old, true)
			}
			if changed || err != nil || len(r.f.cli) != 0 {
				t.Fatalf("stale %s changed=%v err=%v cli=%v", action, changed, err, r.f.cli)
			}
			current, _ := r.f.instance(in.Name)
			if current.Config[jobrun.ThrowawayOwnerKey] != next || current.Config[jobrun.ExpiresKey] != deadline {
				t.Fatalf("stale %s changed the owner's config: %v", action, current.Config)
			}
		})
	}
}

func TestThrowawayPoolIsDisabledByDefault(t *testing.T) {
	srv := (&Server{}).Handler()
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/throwaway-pool"},
		{http.MethodPost, "/api/boxes/widget-1234/claim"},
	} {
		req := httptest.NewRequest(route.method, route.path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(dash.ControlHeader, "1")
		res := httptest.NewRecorder()
		srv.ServeHTTP(res, req)
		if res.Code != http.StatusNotFound {
			t.Fatalf("%s %s returned %d, want disabled", route.method, route.path, res.Code)
		}
	}
}
