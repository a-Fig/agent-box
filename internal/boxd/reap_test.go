package boxd

import (
	"context"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/jobrun"
)

func TestEndedExpiry(t *testing.T) {
	started := t0                              // the job's start
	own := started.Add(jobTTL + 5*time.Second) // what box run set
	now := started.Add(time.Hour)              // the job ended an hour in
	box := func(throwaway bool, expires time.Time, created time.Time) Instance {
		in := inst("w", "Running", "small", created)
		in.Config["user.expires"] = expires.Format(time.RFC3339)
		if throwaway {
			in.Config[jobrun.ThrowawayKey] = "true"
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
		"already short (box read it)": {box(true, now.Add(30*time.Minute), started), time.Time{}},
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
// box's 30 minutes instead of the job's 24 hours (#53, #90). A job that starts there
// keeps its own 24 hours, whether it shows in the box's running jobs (even
// within jobExpirySlack of the first job's start) or its write comes
// between boxd's read and its write.
func TestReapEndedUnreadJob(t *testing.T) {
	for _, tc := range []string{"alone", "write between", "a second job running"} {
		r := newQueueRig(t, map[string]string{"user.agent-box.mode": "full", "limits.memory": "24GiB"})
		job, second := "w-0001.20261124-180000-ab12", "w-0001.20261124-180500-cd34"
		r.f.set(func() {
			in := inst("w-0001", "Running", "small", t0)
			in.Config[jobrun.ThrowawayKey] = "true"
			in.Config["user.expires"] = t0.Add(jobTTL).Format(time.RFC3339)
			r.f.ins = []Instance{in}
			r.f.jobs["w-0001"] = []fakeJob{{name: job, state: "running", log: "x", meta: metaJSON}}
		})
		r.poll()
		r.c.add(time.Hour)
		next := t0.Add(5*time.Minute + jobTTL).UTC().Format(time.RFC3339) // inside the slack
		r.f.set(func() {
			r.f.jobs["w-0001"][0].state, r.f.jobs["w-0001"][0].exit = "done", "0"
			switch tc {
			case "write between":
				r.f.between = func(cfg map[string]string) { cfg["user.expires"] = r.c.now().Add(jobTTL).UTC().Format(time.RFC3339) }
			case "a second job running": // started after boxd's scan, so not yet live
				r.f.ins[0].Config["user.expires"] = next
				r.f.running = map[string][]string{"w-0001": {second}}
			}
		})
		r.poll()
		var sets []string
		var expires string
		for i := 0; i < 50; i++ {
			time.Sleep(10 * time.Millisecond)
			r.f.mu.Lock()
			sets, expires = append([]string(nil), r.f.expirySets...), r.f.ins[0].Config["user.expires"]
			r.f.mu.Unlock()
			if len(sets) > 0 {
				break
			}
		}
		want := r.c.now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
		switch {
		case tc == "alone" && (len(sets) != 1 || expires != want):
			t.Errorf("%s: sets %v, expires %s, want %s", tc, sets, expires, want)
		case tc != "alone" && len(sets) != 0:
			t.Errorf("%s: cut a running job's expiry: sets %v, expires %s", tc, sets, expires)
		}
	}
}

// A job that ended while boxd's reapEnded didn't get to write (a restart cut
// it short; here another job looked to be running) leaves its throwaway the
// job's 24 hours. A boxd started again sweeps it to 30 minutes, once nothing
// runs there (#156).
func TestSweepEndedAfterRestart(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "full", "limits.memory": "24GiB"})
	job := "w-0001.20261124-180000-ab12"
	own := t0.Add(jobTTL).Format(time.RFC3339)
	r.f.set(func() {
		in := inst("w-0001", "Running", "small", t0)
		in.Config[jobrun.ThrowawayKey] = "true"
		in.Config["user.expires"] = own
		r.f.ins = []Instance{in}
		r.f.jobs["w-0001"] = []fakeJob{{name: job, state: "running", log: "x", meta: metaJSON}}
	})
	r.poll()
	r.c.add(time.Hour)
	r.f.set(func() {
		r.f.jobs["w-0001"][0].state, r.f.jobs["w-0001"][0].exit = "done", "0"
		r.f.running = map[string][]string{"w-0001": {"w-0001.20261124-185000-cd34"}}
	})
	r.poll()
	expires := func() string {
		r.f.mu.Lock()
		defer r.f.mu.Unlock()
		return r.f.ins[0].Config["user.expires"]
	}
	time.Sleep(100 * time.Millisecond) // the declined reap
	if got := expires(); got != own {
		t.Fatalf("reaped while another job ran: %s", got)
	}

	r.f.set(func() { r.f.running = nil })
	r.c.add(time.Hour)
	w := r.restart()
	w.Poll(context.Background())
	want := r.c.now().Add(jobrun.EndedKeep).UTC().Format(time.RFC3339)
	for i := 0; i < 50 && expires() != want; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if got := expires(); got != want {
		t.Fatalf("after restart: expires %s, want %s", got, want)
	}
	sets := func() []string {
		r.f.mu.Lock()
		defer r.f.mu.Unlock()
		return append([]string(nil), r.f.expirySets...)
	}
	n := len(sets())
	w.Poll(context.Background()) // shortened already: no second write
	time.Sleep(50 * time.Millisecond)
	if got := sets(); len(got) != n {
		t.Errorf("wrote again: %v", got)
	}
}
