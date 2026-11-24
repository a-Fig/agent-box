package boxd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
)

func (r *queueRig) post(path, body string) (int, []byte) {
	req, _ := http.NewRequest("POST", r.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(dash.ControlHeader, "1")
	req.Header.Set(LoginHeader, "owner@example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	var b []byte
	buf := make([]byte, 64<<10)
	n, _ := resp.Body.Read(buf)
	b = buf[:n]
	return resp.StatusCode, b
}

func TestControls(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "full", "limits.memory": "24GiB"})
	created := t0.Add(-time.Hour)
	job := "w.20261124-175900-ab12"
	r.f.set(func() {
		r.f.ins = []Instance{inst("w", "Running", "small", created)}
		r.f.jobs["w"] = []fakeJob{{name: job, state: "running", log: "x", meta: metaJSON}}
	})
	r.poll()

	// Mode: written as a request for the tray, never as the mode.
	if code, b := r.post("/api/mode", `{"mode":"bored"}`); code != 400 {
		t.Fatalf("bad mode: %d %s", code, b)
	}
	code, b := r.post("/api/mode", `{"mode":"shared"}`)
	var m dash.Machine
	json.Unmarshal(b, &m)
	if code != 202 || m.ModeRequested == nil || m.ModeRequested.Mode != "shared" || m.ModeRequested.By != "owner@example.com" || m.Mode != "full" {
		t.Fatalf("mode: %d %s", code, b)
	}
	if last := r.f.cli[len(r.f.cli)-1]; !strings.HasPrefix(last, `project set agents user.agent-box.mode-request={"mode":"shared","at":`) {
		t.Fatalf("mode request: %s", last)
	}

	// Extend: from the later of now and the expiry (the fake's is 2026-11-25T00:00Z).
	if code, b := r.post("/api/boxes/w/extend", `{"hours":3,"createdAt":"`+created.Format(time.RFC3339)+`"}`); code != 400 {
		t.Fatalf("3h: %d %s", code, b)
	}
	if code, b := r.post("/api/boxes/w/extend", `{"hours":4,"createdAt":"2026-11-24T10:00:00Z"}`); code != 409 || !strings.Contains(string(b), `"gone"`) {
		t.Fatalf("another box of the name: %d %s", code, b)
	}
	code, b = r.post("/api/boxes/w/extend", `{"hours":4,"createdAt":"`+created.Format(time.RFC3339)+`"}`)
	var box dash.Box
	json.Unmarshal(b, &box)
	if code != 200 || box.ExpiresAt == nil || box.ExpiresAt.Format(time.RFC3339) != "2026-11-25T04:00:00Z" {
		t.Fatalf("extend: %d %s", code, b)
	}

	// Cancel: a frozen box can't run the kill.
	key := url.PathEscape(dash.JobKey("w", "20261124-175900-ab12", t0.Add(-time.Minute)))
	r.f.set(func() { r.f.ins[0].Status = "Frozen"; r.f.cfg["user.agent-box.reason"] = "gaming: x" })
	r.poll()
	if code, b := r.post("/api/jobs/"+key+"/cancel", `{}`); code != 409 || !strings.Contains(string(b), `"yielding"`) {
		t.Fatalf("cancel frozen: %d %s", code, b)
	}
	r.f.set(func() { r.f.ins[0].Status = "Running"; r.f.cfg["user.agent-box.reason"] = "" })
	r.poll()
	code, b = r.post("/api/jobs/"+key+"/cancel", `{}`)
	var j dash.Job
	json.Unmarshal(b, &j)
	if code != 200 || j.Result() != dash.ResultCancelled {
		t.Fatalf("cancel: %d %s", code, b)
	}
	if code, b := r.post("/api/jobs/"+key+"/cancel", `{}`); code != 409 || !strings.Contains(string(b), `"not-running"`) {
		t.Fatalf("cancel again: %d %s", code, b)
	}

	// Delete: only the box the page saw.
	if code, _ := r.post("/api/boxes/w/delete", `{"createdAt":"2026-11-24T10:00:00Z"}`); code != 409 {
		t.Fatalf("delete another: %d", code)
	}
	if code, b := r.post("/api/boxes/w/delete", `{"createdAt":"`+created.Format(time.RFC3339)+`"}`); code != 204 {
		t.Fatalf("delete: %d %s", code, b)
	}
	if last := r.f.cli[len(r.f.cli)-1]; last != "delete w --force" {
		t.Fatalf("delete ran %s", last)
	}
	if code, _ := r.post("/api/boxes/nope/delete", `{"createdAt":"`+created.Format(time.RFC3339)+`"}`); code != 404 {
		t.Fatalf("delete missing: %d", code)
	}
	// The controls are in the history, with who asked.
	rs := readEvents(t, r.dir+"/history")
	var acts []string
	for _, rec := range rs {
		if rec.Type == recControl {
			acts = append(acts, rec.Control.Action+":"+rec.Control.By)
		}
	}
	if strings.Join(acts, ",") != "mode shared:owner@example.com,extend 4h:owner@example.com,cancel:owner@example.com,delete:owner@example.com" {
		t.Fatalf("controls recorded: %v", acts)
	}
	_ = context.Background
}
