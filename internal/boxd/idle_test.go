package boxd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/jobrun"
	"github.com/a-Fig/agent-box/internal/mode"
)

const idleRepoKey = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestIdleStopReadsPendingIncusOperations(t *testing.T) {
	s := &Socket{Proj: "agents", client: &http.Client{Transport: fileRangeTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/1.0/operations" || r.URL.Query().Get("project") != "agents" || r.URL.Query().Get("recursion") != "1" {
			t.Fatalf("operations request: %s", r.URL)
		}
		rec := httptest.NewRecorder()
		rec.WriteString(`{"metadata":{"running":[{"resources":{"instances":["/1.0/instances/active?project=agents"]}}],"failure":[{"resources":{"instances":["/1.0/instances/failed"]}}]}}`)
		return rec.Result(), nil
	})}}
	for name, want := range map[string]bool{"active": true, "failed": false, "other": false} {
		if got, err := s.InstanceBusy(context.Background(), name); err != nil || got != want {
			t.Fatalf("operations for %s = %t, %v", name, got, err)
		}
	}
}

func idleFinished(t *testing.T, r *queueRig, name string, ended time.Time, pooled bool) Instance {
	t.Helper()
	started, expiry, exit := ended.Add(-time.Minute), r.c.now().Add(20*time.Minute), 0
	in := inst(name, "Running", "small", started)
	in.Config["volatile.uuid"] = "uid-" + name
	in.Config[jobrun.ThrowawayKey] = "true"
	in.Config[jobrun.ExpiresKey] = expiry.Format(time.RFC3339)
	id := started.UTC().Format("20060102-150405") + "-a001"
	job := name + "." + id
	if pooled {
		in.Config[jobrun.ThrowawayRepoKey] = idleRepoKey
		in.Config[jobrun.ThrowawayOwnerKey] = job
	}
	j := dash.Job{Key: dash.JobKey(name, id, started), ID: id, Box: name, BoxUID: boxUID(in), Machine: "pc",
		State: dash.JobDone, Exit: &exit, StartedAt: started, EndedAt: &ended}
	if err := r.w.Hist.Record(record{Type: recJobEnd, At: ended, Job: &j}); err != nil {
		t.Fatal(err)
	}
	r.f.set(func() {
		r.f.ins = append(r.f.ins, in)
		r.f.jobs[name] = []fakeJob{{name: job, state: "done", exit: "0", log: "original full log\n"}}
	})
	return in
}

func TestIdleThrowawayWarmBoundPreservesFilesAndExpiry(t *testing.T) {
	for _, tc := range []struct {
		name string
		warm bool
	}{{"pool disabled", false}, {"pool enabled", true}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newQueueRig(t, map[string]string{"user.agent-box.mode": "full"})
			now := r.c.now()
			newest := idleFinished(t, r, "newest", now.Add(-time.Second), tc.warm)
			second := idleFinished(t, r, "second", now.Add(-2*time.Second), tc.warm)
			unpooled := idleFinished(t, r, "unpooled", now.Add(-3*time.Second), false)
			protected := []string{"held", "frozen", "gpu", "unknown", "starting", "active"}
			for _, name := range protected {
				idleFinished(t, r, name, now.Add(-4*time.Second), tc.warm)
			}
			r.f.set(func() {
				for i := range r.f.ins {
					in := &r.f.ins[i]
					switch in.Name {
					case "held":
						in.Config[mode.HoldKey] = now.Add(time.Hour).Format(time.RFC3339)
					case "frozen":
						in.Status = "Frozen"
					case "gpu":
						in.Config[mode.SizeKey] = mode.GPUSize
					case "unknown":
						delete(in.Config, mode.SizeKey)
					case "starting", "active":
						state := "starting"
						if in.Name == "active" {
							state = "running"
						}
						id := now.UTC().Format("20060102-150405") + "-b002"
						r.f.jobs[in.Name] = append(r.f.jobs[in.Name], fakeJob{name: in.Name + "." + id, state: state})
					}
				}
			})
			ctx := context.Background()
			for range 4 {
				r.poll()
				r.w.coolIdleThrowaways(ctx, tc.warm)
			}
			for _, initial := range []Instance{newest, second, unpooled} {
				in, ok := r.f.instance(initial.Name)
				if !ok || in.Config[jobrun.ExpiresKey] != initial.Config[jobrun.ExpiresKey] ||
					in.Config[jobrun.ThrowawayOwnerKey] != initial.Config[jobrun.ThrowawayOwnerKey] {
					t.Fatalf("idle stop changed retention or ownership for %s: %+v", initial.Name, in)
				}
				if tc.warm && in.Name == newest.Name {
					if in.Status != "Running" {
						t.Fatalf("newest eligible warm box stopped: %+v", in)
					}
					continue
				}
				if in.Status != "Stopped" || in.Config[jobrun.ThrowawayStopKey] != jobrun.ThrowawayStopped {
					t.Fatalf("surplus box was not marked stopped: %+v", in)
				}
				j, ok := r.w.Hist.LastEnded(in)
				chunk, err := readStoppedLog(ctx, r.f, j, 0, 64, true)
				if !ok || err != nil || chunk.Data != "original full log\n" || !chunk.EOF {
					t.Fatalf("stopped log for %s = %+v, %v", in.Name, chunk, err)
				}
			}
			for _, name := range protected {
				in, _ := r.f.instance(name)
				if in.Status == "Stopped" || in.Config[jobrun.ThrowawayStopKey] != "" {
					t.Fatalf("protected box %s was retired: %+v", name, in)
				}
			}
		})
	}
}

func TestIdleStopLosesToConcurrentPoolClaim(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "full"})
	in := idleFinished(t, r, "reused", r.c.now().Add(-time.Second), true)
	r.poll()
	newOwner := in.Name + ".20261124-180000-bbbb"
	r.f.set(func() {
		r.f.between = func(cfg map[string]string) {
			cfg[jobrun.ThrowawayOwnerKey] = newOwner
			cfg[jobrun.ThrowawayClaimKey] = r.c.now().Add(jobrun.ThrowawayClaimTTL).Format(time.RFC3339)
		}
	})
	r.w.coolIdleThrowaways(context.Background(), false)
	current, _ := r.f.instance(in.Name)
	if current.Status != "Running" || current.Config[jobrun.ThrowawayOwnerKey] != newOwner ||
		current.Config[jobrun.ThrowawayStopKey] != "" || len(r.f.cli) != 0 {
		t.Fatalf("new owner did not keep the box: %+v, calls=%v", current, r.f.cli)
	}
}

func TestIdleStopRechecksAfterFileProof(t *testing.T) {
	for _, scenario := range []string{"new hold", "new job", "name replacement", "file read failed"} {
		t.Run(scenario, func(t *testing.T) {
			r := newQueueRig(t, map[string]string{"user.agent-box.mode": "full"})
			in := idleFinished(t, r, "candidate", r.c.now().Add(-time.Second), false)
			r.poll()
			intercept := interceptFileRange{Incus: r.f}
			if scenario == "file read failed" {
				intercept.err = errors.New("no ranged file access")
			}
			intercept.afterRead = func() {
				r.f.set(func() {
					switch scenario {
					case "new hold":
						r.f.ins[0].Config[mode.HoldKey] = r.c.now().Add(time.Hour).Format(time.RFC3339)
					case "new job":
						r.f.jobs[in.Name] = append(r.f.jobs[in.Name], fakeJob{name: in.Name + ".20261124-180000-bbbb", state: "starting"})
					case "name replacement":
						r.f.ins[0].Config["volatile.uuid"] = "replacement"
						r.f.ins[0].Config[jobrun.ThrowawayStopKey] = "another reservation"
					}
				})
			}
			r.w.Incus = intercept
			r.w.coolIdleThrowaways(context.Background(), false)
			current, _ := r.f.instance(in.Name)
			want := ""
			if scenario == "name replacement" {
				want = "another reservation"
			}
			if current.Status != "Running" || current.Config[jobrun.ThrowawayStopKey] != want || len(r.f.cli) != 0 {
				t.Fatalf("unproven box stop: %+v, calls=%v", current, r.f.cli)
			}
		})
	}
}

func TestIdleStopKeepsReservationUntilAmbiguousStopIsVerified(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "full"})
	in := idleFinished(t, r, "candidate", r.c.now().Add(-time.Second), true)
	r.poll()
	r.f.set(func() {
		r.f.cliErr = func(args []string) error {
			r.f.busyOperation = true
			return context.DeadlineExceeded
		}
	})
	ctx := context.Background()
	r.w.coolIdleThrowaways(ctx, false)
	current, _ := r.f.instance(in.Name)
	token := current.Config[jobrun.ThrowawayStopKey]
	if current.Status != "Running" || !jobrun.IdleStopReserved(current.Config) || current.Config[jobrun.ExpiresKey] != in.Config[jobrun.ExpiresKey] {
		t.Fatalf("ambiguous stop released the reservation or changed expiry: %+v", current)
	}
	if claimed, err := r.w.claimThrowaway(ctx, in.Name, idleRepoKey, "small", in.Name+".20261124-180000-bbbb"); err != nil || claimed {
		t.Fatalf("claim during an ambiguous stop = %t, %v", claimed, err)
	}
	if _, err := r.w.ExtendBox(ctx, in.Name, in.CreatedAt, 1, "fixture"); err == nil {
		t.Fatal("dashboard extended a box while an idle stop was outstanding")
	} else if e := new(controlError); !errors.As(err, &e) || e.code != dash.CodeStopping {
		t.Fatalf("extension during an ambiguous stop: %v", err)
	}
	r.c.add(jobrun.ThrowawayClaimTTL + time.Second)
	r.poll()
	r.w.coolIdleThrowaways(ctx, false)
	current, _ = r.f.instance(in.Name)
	if current.Config[jobrun.ThrowawayStopKey] != token || len(r.f.cli) != 1 {
		t.Fatalf("ambiguous stop was retried or unfenced after a claim TTL: %+v, calls=%v", current, r.f.cli)
	}
	r.f.set(func() {
		r.f.cliErr = nil
		r.f.busyOperation = false
		r.f.ins[0].Status = "Stopped"
	})
	r.poll()
	r.w.coolIdleThrowaways(ctx, false)
	current, _ = r.f.instance(in.Name)
	if current.Config[jobrun.ThrowawayStopKey] != jobrun.ThrowawayStopped ||
		current.Config[jobrun.ExpiresKey] != in.Config[jobrun.ExpiresKey] ||
		!strings.HasPrefix(r.f.cli[0], "stop candidate ") {
		t.Fatalf("verified stop wasn't settled: %+v, calls=%v", current, r.f.cli)
	}
}

func TestIdleStopReleasesStaleReservationWithNoOperation(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "full"})
	in := idleFinished(t, r, "candidate", r.c.now().Add(-time.Second), false)
	r.f.set(func() {
		r.f.ins[0].Config[jobrun.ThrowawayStopKey] = boxUID(in) + " " + r.c.now().UTC().Format(time.RFC3339Nano) + " nonce"
	})
	r.poll()
	r.w.reconcileIdleStops(context.Background())
	got, _ := r.f.instance(in.Name)
	if !jobrun.IdleStopReserved(got.Config) {
		t.Fatal("released a recent stop before Incus could report it")
	}
	r.c.add(idleStopSettleAfter)
	r.poll()
	r.w.reconcileIdleStops(context.Background())
	got, _ = r.f.instance(in.Name)
	if got.Config[jobrun.ThrowawayStopKey] != "" || got.Status != "Running" {
		t.Fatalf("abandoned reservation = %+v", got)
	}
}

func TestIdleStopKeepsAClaimableWarmBox(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "full"})
	now := r.c.now()
	newest := idleFinished(t, r, "near-expiry", now.Add(-time.Second), true)
	older := idleFinished(t, r, "claimable", now.Add(-2*time.Second), true)
	r.f.set(func() {
		r.f.ins[0].Config[jobrun.ExpiresKey] = now.Add(10 * time.Second).Format(time.RFC3339)
	})
	r.poll()
	r.w.coolIdleThrowaways(context.Background(), true)
	cold, _ := r.f.instance(newest.Name)
	warm, _ := r.f.instance(older.Name)
	if cold.Status != "Stopped" || warm.Status != "Running" {
		t.Fatalf("warm slot went to the unclaimable box: %s=%s, %s=%s", cold.Name, cold.Status, warm.Name, warm.Status)
	}
}

func TestIdleStopProtectsAnUnresolvedQueueStart(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "mypc"})
	_, rep, _ := r.enqueue(small("protected", "fixture"), "B")
	box, _, _ := strings.Cut(rep.Job, ".")
	in := idleFinished(t, r, box, r.c.now().Add(-time.Second), false)
	it, _ := r.w.Queue.get(rep.Job)
	if err := r.w.Queue.markStarting(it, r.c.now()); err != nil {
		t.Fatal(err)
	}
	q, err := OpenQueue(r.w.Queue.dir)
	if err != nil {
		t.Fatal(err)
	}
	r.w.Queue = q
	r.f.set(func() { r.f.cfg["user.agent-box.mode"] = "full" })
	// The scheduler's one slot is occupied by a different queue item.
	r.w.starting = "different.20261124-180000-a001"
	r.w.Poll(context.Background())
	r.w.coolIdleThrowaways(context.Background(), false)
	got, _ := r.f.instance(in.Name)
	if got.Status != "Running" || len(r.f.cli) != 0 {
		t.Fatalf("unresolved queue start was cooled: %+v, CLI %v", got, r.f.cli)
	}
}

func TestIdleStopAndUnpooledEvictionShareReservation(t *testing.T) {
	for _, key := range []string{jobrun.ThrowawayClaimKey, jobrun.DirectEvictKey} {
		t.Run(key, func(t *testing.T) {
			r := newQueueRig(t, map[string]string{"user.agent-box.mode": "full"})
			in := idleFinished(t, r, "candidate", r.c.now().Add(-time.Second), false)
			r.poll()
			ctx := context.Background()
			r.f.set(func() {
				// Simulate pressure eviction winning the conditional write.
				r.f.between = func(cfg map[string]string) {
					cfg[key] = r.c.now().Add(jobrun.EvictTTL).Format(time.RFC3339)
				}
			})
			r.w.coolIdleThrowaways(ctx, false)
			got, _ := r.f.instance(in.Name)
			if got.Status != "Running" || len(r.f.cli) != 0 {
				t.Fatalf("stop raced an eviction: %+v", got)
			}
			r.f.set(func() {
				delete(r.f.ins[0].Config, key)
				r.f.ins[0].Config[jobrun.ThrowawayStopKey] = boxUID(in) + " pending"
			})
			if evicted, err := r.w.evictThrowaway(ctx, in.Name, boxUID(in), false); evicted || err != nil || len(r.f.cli) != 0 {
				t.Fatalf("unpooled eviction passed a stop reservation: %t, %v, %v", evicted, err, r.f.cli)
			}
		})
	}
}

func TestQueueEvictsCooledBoxWhenOnlyMemoryIsFull(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "full", "limits.instances": "10", "limits.memory": "2GiB"})
	in := idleFinished(t, r, "candidate", r.c.now().Add(-time.Second), false)
	r.f.set(func() {
		r.f.ins[0].Config["limits.memory"] = "2GiB"
	})
	r.poll()
	r.w.coolIdleThrowaways(context.Background(), false)
	current, _ := r.f.instance(in.Name)
	if current.Status != "Stopped" {
		t.Fatalf("fixture did not stop: %+v", current)
	}
	r.poll()
	if room := r.w.State().Machines[0].Room; room.Small != 0 {
		t.Fatalf("stopped allocation is missing from room: %+v", room)
	}
	_, rep, _ := r.enqueue(small("next", "fixture"), "B")
	r.poll()
	if _, exists := r.f.instance(in.Name); exists || len(r.w.Queued()) != 0 {
		t.Fatalf("queue did not make room for %s: %v", rep.Job, r.f.cli)
	}
	if prev := strings.Join(r.f.cli, "\n"); !strings.Contains(prev, "delete candidate --force\nlaunch") {
		t.Fatalf("expected eviction before launch: %s", prev)
	}
}
