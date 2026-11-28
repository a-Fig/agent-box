package boxd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/a-Fig/agent-box/internal/dash"
)

func TestCloudUsageAndPause(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "full"})
	_, ch, cancel := r.w.Subscribe()
	defer cancel()
	next := func() dash.CloudState {
		t.Helper()
		select {
		case e := <-ch:
			if e.Name != dash.EventCloud {
				t.Fatalf("event %s, want cloud", e.Name)
			}
			return e.Data.(dash.CloudState)
		default:
			t.Fatal("no cloud event")
		}
		return dash.CloudState{}
	}

	body := `{"host":{"name":"modal","kind":"modal","mode":"function","running":1,"maxConcurrent":4,
		"budget":{"month":"2026-11","spentUSD":3.12,"runningUSD":0.04,"capUSD":25,"creditUSD":30,"warnAt":0.75},
		"reportedBy":"spoofed"},"controller":{"day":"2026-11-27","requests":2412,"limit":100000}}`
	if code, b := r.post("/api/cloud/usage", body); code != http.StatusNoContent {
		t.Fatalf("usage: %d %s", code, b)
	}
	ev := next()
	if len(ev.Cloud) != 1 || ev.Cloud[0].Running != 1 || ev.Cloud[0].Budget.CapUSD != 25 || ev.Controller.Requests != 2412 {
		t.Fatalf("event %+v", ev)
	}
	st := r.w.State()
	if len(st.Cloud) != 1 || st.Cloud[0].ReportedAt != t0 || st.Cloud[0].ReportedBy != "owner@example.com" || st.CloudPaused {
		t.Fatalf("state %+v", st.Cloud)
	}
	// A later report replaces the host's, and a controller-only report keeps the hosts.
	r.post("/api/cloud/usage", `{"host":{"name":"modal","kind":"modal","running":3,"maxConcurrent":4}}`)
	next()
	r.post("/api/cloud/usage", `{"controller":{"day":"2026-11-27","requests":2500,"limit":100000}}`)
	ev = next()
	if len(ev.Cloud) != 1 || ev.Cloud[0].Running != 3 || ev.Cloud[0].Budget != nil || ev.Controller.Requests != 2500 {
		t.Fatalf("after replace %+v", ev)
	}

	for _, bad := range []string{
		`{}`,
		`not json`,
		`{"host":{"kind":"modal"}}`,
		`{"host":{"name":"x","kind":"modal","mode":"vm"}}`,
		`{"host":{"name":"x","kind":"modal","running":-1}}`,
		`{"host":{"name":"x","kind":"modal","budget":{"spentUSD":-2}}}`,
		`{"host":{"name":"x","kind":"modal","budget":{"warnAt":1.5}}}`,
		`{"controller":{"requests":-1}}`,
		`{"host":{"name":"` + strings.Repeat("x", 20<<10) + `","kind":"modal"}}`,
	} {
		if code, b := r.post("/api/cloud/usage", bad); code != http.StatusBadRequest {
			t.Fatalf("%.40s: %d %s", bad, code, b)
		}
	}
	// No control header, no control.
	req, _ := http.NewRequest("POST", r.srv.URL+"/api/cloud/pause", strings.NewReader(`{"paused":true}`))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("pause without the header: %v %v", resp, err)
	}

	code, b := r.post("/api/cloud/pause", `{"paused":true}`)
	var got dash.CloudState
	json.Unmarshal(b, &got)
	if code != 200 || !got.CloudPaused || len(got.Cloud) != 1 {
		t.Fatalf("pause: %d %s", code, b)
	}
	if !next().CloudPaused || !r.w.State().CloudPaused {
		t.Fatal("pause not announced")
	}
	// Pausing again changes nothing, so nothing is announced or recorded.
	r.post("/api/cloud/pause", `{"paused":true}`)
	select {
	case e := <-ch:
		t.Fatalf("event %s for no change", e.Name)
	default:
	}
	r.post("/api/cloud/pause", `{"paused":false}`)
	if next().CloudPaused || r.w.State().CloudPaused {
		t.Fatal("resume not announced")
	}
	var acts []string
	for _, rec := range readEvents(t, r.dir+"/history") {
		if rec.Type == recControl {
			acts = append(acts, rec.Control.Action)
		}
	}
	if strings.Join(acts, ",") != "cloud pause,cloud resume" {
		t.Fatalf("controls recorded: %v", acts)
	}
}

func TestCloudReportsAreBounded(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "full"})
	for i := range dash.MaxCloudHosts {
		if code, b := r.post("/api/cloud/usage", fmt.Sprintf(`{"host":{"name":"c%d","kind":"modal"}}`, i)); code != http.StatusNoContent {
			t.Fatalf("host %d: %d %s", i, code, b)
		}
	}
	if code, _ := r.post("/api/cloud/usage", `{"host":{"name":"one-more","kind":"modal"}}`); code != http.StatusBadRequest {
		t.Fatalf("past the bound: %d", code)
	}
	if code, _ := r.post("/api/cloud/usage", `{"host":{"name":"c0","kind":"modal","running":1}}`); code != http.StatusNoContent {
		t.Fatalf("a known host at the bound: %d", code)
	}
	if st := r.w.State(); len(st.Cloud) != dash.MaxCloudHosts || st.Cloud[0].Name != "c0" {
		t.Fatalf("hosts %d", len(st.Cloud))
	}
}

// Reports, pauses and state reads at once, for -race.
func TestCloudConcurrent(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "full"})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			r.w.ReportCloud(dash.CloudUsageBody{Host: &dash.CloudHost{Name: fmt.Sprint("c", i%3), Kind: "modal", Budget: &dash.CloudBudget{SpentUSD: float64(i)}}}, "t")
			r.w.PauseCloud(i%2 == 0, "t")
			st := r.w.State()
			for _, c := range st.Cloud {
				if c.Budget != nil {
					c.Budget.SpentUSD = -1 // a copy: this mustn't reach boxd's own
				}
			}
		})
	}
	wg.Wait()
	for _, c := range r.w.State().Cloud {
		if c.Budget.SpentUSD < 0 {
			t.Fatal("state shares the kept budget")
		}
	}
}
