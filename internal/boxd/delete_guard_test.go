package boxd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
)

func deleteBody(created time.Time, confirmed bool) string {
	b, _ := json.Marshal(dash.DeleteBody{CreatedAt: created, Confirmed: confirmed})
	return string(b)
}

func seedEnded(t *testing.T, r *queueRig, box dash.Box, id string, ended time.Time) {
	t.Helper()
	exit := 0
	j := dash.Job{Key: dash.JobKey(box.Name, id, ended.Add(-time.Minute)), ID: id, Box: box.Name, BoxUID: box.UID, Machine: box.Machine,
		State: dash.JobDone, Exit: &exit, StartedAt: ended.Add(-time.Minute), EndedAt: &ended}
	if err := r.w.Hist.Record(record{Type: recJobEnd, Job: &j}); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteGuardEmptyActiveRecentExpiredStale(t *testing.T) {
	created := t0.Add(-time.Hour)

	// Empty: no jobs, unconfirmed deletes on the first click.
	r := newQueueRig(t, nil)
	r.f.set(func() { r.f.ins = []Instance{inst("w", "Running", "small", created)} })
	r.poll()
	box, err := r.w.boxFor("w", created)
	if err != nil {
		t.Fatal(err)
	}
	if code, b := r.post("/api/boxes/w/delete", deleteBody(created, false)); code != 204 {
		t.Fatalf("empty unconfirmed: %d %s", code, b)
	}

	// Active: running job refuses unconfirmed, confirmed deletes.
	r = newQueueRig(t, nil)
	job := "w.20261124-175900-ab12"
	r.f.set(func() {
		r.f.ins = []Instance{inst("w", "Running", "small", created)}
		r.f.jobs["w"] = []fakeJob{{name: job, state: "running", log: "x", meta: metaJSON}}
	})
	r.poll()
	if code, b := r.post("/api/boxes/w/delete", deleteBody(created, false)); code != 409 || !strings.Contains(string(b), dash.CodeNeedsConfirm) {
		t.Fatalf("active unconfirmed: %d %s", code, b)
	}
	if code, b := r.post("/api/boxes/w/delete", deleteBody(created, true)); code != 204 {
		t.Fatalf("active confirmed: %d %s", code, b)
	}

	// Recent: ended a minute ago refuses unconfirmed.
	r = newQueueRig(t, nil)
	r.f.set(func() { r.f.ins = []Instance{inst("w", "Running", "small", created)} })
	r.poll()
	box, _ = r.w.boxFor("w", created)
	seedEnded(t, r, box, "20261124-175800-ab12", t0.Add(-time.Minute))
	if code, b := r.post("/api/boxes/w/delete", deleteBody(created, false)); code != 409 || !strings.Contains(string(b), dash.CodeNeedsConfirm) {
		t.Fatalf("recent unconfirmed: %d %s", code, b)
	}

	// Expired: ended 3 minutes ago deletes unconfirmed.
	r = newQueueRig(t, nil)
	r.f.set(func() { r.f.ins = []Instance{inst("w", "Running", "small", created)} })
	r.poll()
	box, _ = r.w.boxFor("w", created)
	seedEnded(t, r, box, "20261124-175700-ab12", t0.Add(-3*time.Minute))
	if code, b := r.post("/api/boxes/w/delete", deleteBody(created, false)); code != 204 {
		t.Fatalf("expired unconfirmed: %d %s", code, b)
	}

	// Stale UI: the page last saw an empty box, but a job has since been
	// recorded on it. The unconfirmed request must refuse, not delete.
	r = newQueueRig(t, nil)
	r.f.set(func() { r.f.ins = []Instance{inst("w", "Running", "small", created)} })
	r.poll()
	box, _ = r.w.boxFor("w", created)
	seedEnded(t, r, box, "20261124-175900-ab12", t0.Add(-30*time.Second))
	if code, b := r.post("/api/boxes/w/delete", deleteBody(created, false)); code != 409 {
		t.Fatalf("stale UI unconfirmed: %d %s", code, b)
	}
	if last := r.f.cli; len(last) > 0 && last[len(last)-1] == "delete w --force" {
		t.Fatalf("stale UI deleted the box: %v", last)
	}
	_ = box
}
