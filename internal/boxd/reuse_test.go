package boxd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
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
			in := inst("widget-1234", "Running", "small", t0)
			in.Config[jobrun.ThrowawayKey] = "true"
			in.Config[jobrun.ThrowawayRepoKey] = key
			in.Config[jobrun.ExpiresKey] = t0.Add(5 * time.Minute).Format(time.RFC3339)
			r.f.ins = []Instance{in}
			r.f.jobs[in.Name] = []fakeJob{{name: in.Name + ".old", state: "done", exit: "0"}}
			req := dash.ClaimThrowawayBody{RepoKey: key, Size: "small"}
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
				request, err := http.NewRequest(http.MethodPost, r.srv.URL+"/api/boxes/"+in.Name+"/claim", bytes.NewReader(body))
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
			if claimed.Config[jobrun.ExpiresKey] != until || claimed.Config[jobrun.ThrowawayClaimKey] != until {
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
