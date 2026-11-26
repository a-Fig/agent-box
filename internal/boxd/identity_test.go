package boxd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
)

func TestReplacementScanWaitsForSuccessfulVerification(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: t0.Add(2 * time.Minute)}
	old := inst("w", "Running", "small", t0)
	old.Config["volatile.uuid"] = "u1"
	const oldID, newID = "20261124-180030-ab12", "20261124-180130-ab12"
	f := &fakeIncus{ins: []Instance{old}, cfg: map[string]string{"user.agent-box.mode": "full"},
		jobs: map[string][]fakeJob{"w": {{name: "w." + oldID, state: "running", meta: metaJSON}}}}
	w := newWatcher(t, t.TempDir(), f, c)
	w.Poll(ctx)

	f.set(func() {
		f.onScan = func(string) {
			repl := inst("w", "Running", "small", t0.Add(time.Minute))
			repl.Config["volatile.uuid"] = "u2"
			f.ins = []Instance{repl}
			f.jobs["w"] = []fakeJob{{name: "w." + newID, state: "running", meta: metaJSON}}
			f.err = errors.New("verification list unavailable")
			f.onScan = nil
		}
	})
	c.add(3 * time.Second)
	w.Poll(ctx)
	jobs := w.State().Jobs
	if len(jobs) != 1 || jobs[0].ID != oldID || jobs[0].BoxUID != "u1" || jobs[0].State != dash.JobRunning {
		t.Fatalf("jobs after failed verification: %+v", jobs)
	}

	f.set(func() { f.err = nil })
	for range 2 {
		c.add(3 * time.Second)
		w.Poll(ctx)
	}
	recovered := map[string]dash.Job{}
	for _, j := range w.State().Jobs {
		recovered[j.ID] = j
	}
	if oldJob, newJob := recovered[oldID], recovered[newID]; len(recovered) != 2 ||
		oldJob.State != dash.JobLost || newJob.State != dash.JobRunning || newJob.BoxUID != "u2" {
		t.Fatalf("jobs after verification recovers: %+v", recovered)
	}
}

// A box replaced by one of its name between a poll's list and its scan: the scan read the new
// box, so its jobs aren't stamped with the old box's UID (which would mark them lost for good).
func TestScanOfAReplacedBoxIsDropped(t *testing.T) {
	c := &clock{t: t0.Add(2 * time.Minute)}
	old := inst("w", "Running", "small", t0)
	old.Config["volatile.uuid"] = "u1"
	f := &fakeIncus{ins: []Instance{old}, cfg: map[string]string{"user.agent-box.mode": "full"}, jobs: map[string][]fakeJob{}}
	w := newWatcher(t, t.TempDir(), f, c)
	w.Poll(context.Background())

	f.set(func() {
		f.onScan = func(string) {
			repl := inst("w", "Running", "small", t0.Add(time.Minute))
			repl.Config["volatile.uuid"] = "u2"
			f.ins = []Instance{repl}
			f.jobs["w"] = []fakeJob{{name: "w.20261124-180130-ab12", state: "running", log: "x", meta: metaJSON}}
			f.onScan = nil
		}
	})
	c.add(3 * time.Second)
	w.Poll(context.Background()) // listed u1, scanned u2: dropped
	c.add(3 * time.Second)
	w.Poll(context.Background())
	c.add(3 * time.Second)
	w.Poll(context.Background())
	st := w.State()
	if len(st.Jobs) != 1 || st.Jobs[0].BoxUID != "u2" || st.Jobs[0].State != dash.JobRunning {
		t.Fatalf("jobs: %+v", st.Jobs)
	}
}

// A job recorded before jobs carried their box's UID keeps boxd's old rule (holds) in boxd's own
// paths: its box may have been made minutes after it was named. The dashboard's RanOn stays tight.
func TestLegacyJobKeepsBoxdGrace(t *testing.T) {
	j := dash.Job{Box: "w", Machine: "pc", StartedAt: t0}
	b := dash.Box{Name: "w", Machine: "pc", UID: "u1", CreatedAt: t0.Add(2 * time.Minute)}
	if !ranOn(j, b) {
		t.Fatal("legacy job on a box made two minutes later: not its box")
	}
	if j.RanOn(b) {
		t.Fatal("dashboard rule loosened")
	}
	b.CreatedAt = t0.Add(dash.JobBoxGrace + time.Second)
	if ranOn(j, b) {
		t.Fatal("legacy job on a box made past the grace: its box")
	}
	j.BoxUID = "u2"
	b.CreatedAt = t0
	if ranOn(j, b) {
		t.Fatal("UID mismatch ignored")
	}
}

// A box deleted while the poll scans it keeps its scan: the job's result was read from it.
func TestScanOfADeletedBoxIsKept(t *testing.T) {
	c := &clock{t: t0.Add(2 * time.Minute)}
	f := &fakeIncus{ins: []Instance{inst("w", "Running", "small", t0)}, cfg: map[string]string{"user.agent-box.mode": "full"},
		jobs: map[string][]fakeJob{"w": {{name: "w.20261124-180100-ab12", state: "done", exit: "0", log: "ok\n", meta: metaJSON, endedAt: t0.Add(90 * time.Second).Unix()}}}}
	f.onScan = func(string) { f.ins, f.onScan = nil, nil }
	w := newWatcher(t, t.TempDir(), f, c)
	w.Poll(context.Background())
	c.add(3 * time.Second)
	w.Poll(context.Background())
	st := w.State()
	if len(st.Jobs) != 1 || st.Jobs[0].Result() != dash.ResultPass {
		t.Fatalf("jobs: %+v", st.Jobs)
	}
}
