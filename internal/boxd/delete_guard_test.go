package boxd

import (
	"context"
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

// When the probe cannot establish the box's state — here the box is
// replaced while it is scanned, so the scan is dropped — the unconfirmed
// delete answers a retryable error (not the warning), and deletes nothing.
func TestDeleteGuardRefusesWhenProbeCannotEstablishState(t *testing.T) {
	created := t0.Add(-time.Hour)
	r := newQueueRig(t, nil)
	r.f.set(func() { r.f.ins = []Instance{inst("w", "Running", "small", created)} })
	r.poll()
	r.f.set(func() {
		r.f.onScan = func(box string) { r.f.ins[0].CreatedAt = t0 }
	})
	code, b := r.post("/api/boxes/w/delete", deleteBody(created, false))
	if code != 502 || !strings.Contains(string(b), dash.CodeIncus) || strings.Contains(string(b), dash.CodeNeedsConfirm) {
		t.Fatalf("probe cannot establish state, unconfirmed: %d %s", code, b)
	}
	for _, c := range r.f.cli {
		if c == "delete w --force" {
			t.Fatalf("the box was deleted on a failed probe: %v", r.f.cli)
		}
	}
}

// A job that froze before boxd's last poll is not in live or the history
// either. The Frozen box is probed too: its job warns, nothing is deleted.
func TestDeleteGuardProbesFrozenBox(t *testing.T) {
	created := t0.Add(-time.Hour)
	r := newQueueRig(t, nil)
	r.f.set(func() { r.f.ins = []Instance{inst("w", "Frozen", "small", created)} })
	r.poll() // boxd sees a frozen, empty box
	r.f.set(func() {
		r.f.jobs["w"] = []fakeJob{{name: "w.20261124-175900-ab12", state: "running", log: "x", meta: metaJSON}}
	}) // it froze mid-job; no Watcher poll follows
	if code, b := r.post("/api/boxes/w/delete", deleteBody(created, false)); code != 409 || !strings.Contains(string(b), dash.CodeNeedsConfirm) {
		t.Fatalf("frozen job, unconfirmed: %d %s", code, b)
	}
	for _, c := range r.f.cli {
		if c == "delete w --force" {
			t.Fatalf("the frozen box was deleted without a warning: %v", r.f.cli)
		}
	}
}

// An old dead job whose log is still on the box is not active work: its
// wrapper is gone and it never wrote an exit. It must not warn forever —
// only live and history get to call a loss recent.
func TestDeleteGuardOldDeadLogDoesNotWarn(t *testing.T) {
	created := t0.Add(-time.Hour)
	r := newQueueRig(t, nil)
	r.f.set(func() { r.f.ins = []Instance{inst("w", "Running", "small", created)} })
	r.poll()
	old := "w.20261124-173000-ab12" // on this box, 30 minutes before now
	r.f.set(func() {
		r.f.jobs["w"] = []fakeJob{{name: old, state: "died", log: "x", meta: metaJSON}}
	})
	if code, b := r.post("/api/boxes/w/delete", deleteBody(created, false)); code != 204 {
		t.Fatalf("old dead log, unconfirmed: %d %s", code, b)
	}
	deleted := false
	for _, c := range r.f.cli {
		deleted = deleted || c == "delete w --force"
	}
	if !deleted {
		t.Fatalf("the box was not deleted past an old dead log: %v", r.f.cli)
	}
}

type scanRefusedIncus struct{ Incus }

func (s scanRefusedIncus) Exec(ctx context.Context, name, script string, args ...string) ([]byte, error) {
	if strings.HasPrefix(script, "# boxd scan") {
		return nil, &ExitError{Code: 1, Stderr: "Instance is frozen"}
	}
	return s.Incus.Exec(ctx, name, script, args...)
}

// A Frozen box Incus refuses to exec in cannot have its job state verified:
// a retryable error, no warning dialog, and no delete.
func TestDeleteGuardFrozenExecRefusedFailsClosed(t *testing.T) {
	created := t0.Add(-time.Hour)
	r := newQueueRig(t, nil)
	r.f.set(func() { r.f.ins = []Instance{inst("w", "Frozen", "small", created)} })
	r.poll()
	r.w.Incus = scanRefusedIncus{r.f}
	code, b := r.post("/api/boxes/w/delete", deleteBody(created, false))
	if code != 502 || !strings.Contains(string(b), dash.CodeIncus) || strings.Contains(string(b), dash.CodeNeedsConfirm) {
		t.Fatalf("frozen exec refused, unconfirmed: %d %s", code, b)
	}
	for _, c := range r.f.cli {
		if c == "delete w --force" {
			t.Fatalf("the box was deleted when its state could not be checked: %v", r.f.cli)
		}
	}
}

// A box in a transient or unknown state cannot be probed, so it is never
// deleted warningless: the retryable error, and no delete. A Stopped box
// starts no jobs, so it alone deletes without a probe.
func TestDeleteGuardUnknownStatusFailsClosed(t *testing.T) {
	created := t0.Add(-time.Hour)
	r := newQueueRig(t, nil)
	r.f.set(func() { r.f.ins = []Instance{inst("w", "Created", "small", created)} })
	r.poll()
	code, b := r.post("/api/boxes/w/delete", deleteBody(created, false))
	if code != 502 || !strings.Contains(string(b), dash.CodeIncus) || strings.Contains(string(b), dash.CodeNeedsConfirm) {
		t.Fatalf("unknown status, unconfirmed: %d %s", code, b)
	}
	for _, c := range r.f.cli {
		if c == "delete w --force" {
			t.Fatalf("the box was deleted in an unknown state: %v", r.f.cli)
		}
	}

	r = newQueueRig(t, nil)
	r.f.set(func() { r.f.ins = []Instance{inst("w", "Stopped", "small", created)} })
	r.poll()
	if code, b := r.post("/api/boxes/w/delete", deleteBody(created, false)); code != 204 {
		t.Fatalf("stopped, unconfirmed: %d %s", code, b)
	}
}

// A direct box run can start after boxd's last poll: neither live nor the
// history knows it yet. An unconfirmed delete must probe the running box
// fresh, refuse, and delete nothing (#251 review).
func TestDeleteGuardProbesJobStartedAfterPoll(t *testing.T) {
	created := t0.Add(-time.Hour)
	r := newQueueRig(t, nil)
	r.f.set(func() { r.f.ins = []Instance{inst("w", "Running", "small", created)} })
	r.poll() // boxd sees an empty box
	r.f.set(func() {
		r.f.jobs["w"] = []fakeJob{{name: "w.20261124-175900-ab12", state: "running", log: "x", meta: metaJSON}}
	}) // a job starts; no Watcher poll follows
	if code, b := r.post("/api/boxes/w/delete", deleteBody(created, false)); code != 409 || !strings.Contains(string(b), dash.CodeNeedsConfirm) {
		t.Fatalf("job started after the poll, unconfirmed: %d %s", code, b)
	}
	for _, c := range r.f.cli {
		if c == "delete w --force" {
			t.Fatalf("the box was deleted without a warning: %v", r.f.cli)
		}
	}
	if _, err := r.w.boxFor("w", created); err != nil {
		t.Fatalf("box after the refused delete: %v", err)
	}
	// The probe is read-only: it must not record the job in live or the
	// history, or a poll whose scan predates the job could settle it lost.
	key := dash.JobKey("w", "20261124-175900-ab12", time.Date(2026, 11, 24, 17, 59, 0, 0, time.UTC))
	if _, ok := r.w.Hist.Job(key); ok {
		t.Fatal("the probe recorded the job in the history")
	}
	r.w.mu.Lock()
	_, live := r.w.live[key]
	r.w.mu.Unlock()
	if live {
		t.Fatal("the probe recorded the job as live")
	}
}
