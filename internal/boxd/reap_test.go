package boxd

import (
	"testing"
	"time"
)

func TestEndedExpiry(t *testing.T) {
	started := t0                              // the job's start
	own := started.Add(jobTTL + 5*time.Second) // what box run set
	now := started.Add(time.Hour)              // the job ended an hour in
	box := func(throwaway bool, expires time.Time, created time.Time) Instance {
		in := inst("w", "Running", "small", created)
		in.Config["user.expires"] = expires.Format(time.RFC3339)
		if throwaway {
			in.Config[keyThrowaway] = "true"
		}
		return in
	}
	for name, tc := range map[string]struct {
		in   Instance
		want time.Time // zero: no write
	}{
		"throwaway, its job's 24h":    {box(true, own, started), now.Add(30 * time.Minute)},
		"repo box, its job's 24h":     {box(false, own, started.Add(-24*time.Hour)), now.Add(2 * time.Hour)},
		"a later job's 24h":           {box(true, now.Add(jobTTL), started), time.Time{}},
		"already short (box read it)": {box(true, now.Add(20*time.Minute), started), time.Time{}},
		"extended from the page":      {box(true, own.Add(4*time.Hour), started), time.Time{}},
		"another box of that name":    {box(true, own, started.Add(time.Hour)), time.Time{}},
	} {
		got, ok := endedExpiry(tc.in, started, now)
		if ok != !tc.want.IsZero() || (ok && !got.Equal(tc.want)) {
			t.Errorf("%s: %v %v, want %v", name, got, ok, tc.want)
		}
	}
	bad := box(true, own, started)
	bad.Config["user.expires"] = "soon"
	if _, ok := endedExpiry(bad, started, now); ok {
		t.Error("wrote over an expiry it can't read")
	}
}

// A job box run started and nobody followed ends: boxd gives its throwaway
// 30 minutes instead of the job's 24 hours (#53). A job that starts there
// just as boxd writes keeps its own 24 hours.
func TestReapEndedUnreadJob(t *testing.T) {
	for _, race := range []bool{false, true} {
		r := newQueueRig(t, map[string]string{"user.agent-box.mode": "full", "limits.memory": "24GiB"})
		job := "w-0001.20261124-180000-ab12"
		r.f.set(func() {
			in := inst("w-0001", "Running", "small", t0)
			in.Config[keyThrowaway] = "true"
			in.Config["user.expires"] = t0.Add(jobTTL).Format(time.RFC3339)
			r.f.ins = []Instance{in}
			r.f.jobs["w-0001"] = []fakeJob{{name: job, state: "running", log: "x", meta: metaJSON}}
		})
		r.poll()
		r.c.add(time.Hour)
		next := r.c.now().Add(jobTTL).UTC().Format(time.RFC3339)
		r.f.set(func() {
			r.f.jobs["w-0001"][0].state, r.f.jobs["w-0001"][0].exit = "done", "0"
			if race {
				r.f.between = func(in *Instance) { in.Config["user.expires"] = next } // box run starting the next job
			}
		})
		r.poll()
		var sets []string
		var expires string
		for i := 0; i < 100; i++ {
			r.f.mu.Lock()
			sets, expires = append([]string(nil), r.f.expirySets...), r.f.ins[0].Config["user.expires"]
			raced := r.f.between == nil
			r.f.mu.Unlock()
			if len(sets) > 0 || (race && raced) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		switch want := r.c.now().Add(30 * time.Minute).UTC().Format(time.RFC3339); {
		case !race && (len(sets) != 1 || expires != want):
			t.Errorf("no race: sets %v, expires %s, want %s", sets, expires, want)
		case race && (len(sets) != 0 || expires != next):
			t.Errorf("race: sets %v, expires %s, want the next job's %s", sets, expires, next)
		}
	}
}
