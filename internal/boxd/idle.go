package boxd

import (
	"context"
	"crypto/rand"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/jobrun"
	"github.com/a-Fig/agent-box/internal/mode"
)

type idleThrowaway struct {
	in  Instance
	job dash.Job
}

// A timed-out request can still have reached Incus. Give it time to appear
// in the operation list before treating an unchanged Running state as failed.
const idleStopSettleAfter = time.Minute

// coolIdleThrowaways runs between polls, outside the watcher lock. It keeps
// the newest reusable box warm when pooling is enabled, and issues at most
// one new stop per pass. Existing expiry and artifact windows are unchanged.
func (w *Watcher) coolIdleThrowaways(ctx context.Context, keepWarm bool) {
	w.reconcileIdleStops(ctx)
	w.mu.Lock()
	if !w.machine.Reachable || (w.machine.Mode != string(mode.Shared) && w.machine.Mode != string(mode.Full)) {
		w.mu.Unlock()
		return
	}
	now := w.Now()
	var idle []idleThrowaway
	for _, in := range w.lastIns {
		if !w.boxBusy(in.Name, "") && !jobrun.ThrowawayClaimed(in.Config, now) {
			if j, ok := w.idleStopJob(in, now); ok {
				idle = append(idle, idleThrowaway{in: in, job: j})
			}
		}
	}
	w.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	slices.SortFunc(idle, func(a, b idleThrowaway) int {
		if n := b.job.EndedAt.Compare(*a.job.EndedAt); n != 0 {
			return n
		}
		return strings.Compare(a.in.Name, b.in.Name)
	})
	for _, candidate := range idle {
		if keepWarm && warmClaimable(candidate.in, w.Now()) {
			keepWarm = false
			continue
		}
		if ctx.Err() != nil || w.stopIdleThrowaway(ctx, candidate) {
			return
		}
	}
}

func (w *Watcher) reconcileIdleStops(ctx context.Context) {
	w.mu.Lock()
	instances, stopping, reachable := w.lastIns, w.idleStopping, w.machine.Reachable
	w.mu.Unlock()
	if !reachable {
		return
	}
	look, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	for _, in := range instances {
		if in.Name != stopping && jobrun.IdleStopReserved(in.Config) {
			w.finishIdleStop(look, in, in.Config[jobrun.ThrowawayStopKey], false)
		}
	}
}

func (w *Watcher) idleStopJob(in Instance, now time.Time) (dash.Job, bool) {
	size, known := mode.LookupSize(in.Config[mode.SizeKey])
	expires, _ := time.Parse(time.RFC3339, in.Config[jobrun.ExpiresKey])
	if in.Status != "Running" || !validBoxName.MatchString(in.Name) || !known || size.GPU || !expires.After(now) ||
		!keptThrowawayWindow(in, now) {
		return dash.Job{}, false
	}
	return w.finishedIdleJob(in)
}

func (w *Watcher) finishedIdleJob(in Instance) (dash.Job, bool) {
	if w.Hist == nil || (in.Config["volatile.uuid"] == "" && unsetTime(in.CreatedAt)) {
		return dash.Job{}, false
	}
	j, found := w.Hist.LastEnded(in)
	return j, found && j.State == dash.JobDone && j.Exit != nil && j.EndedAt != nil &&
		j.BoxUID != "" && ranOnInstance(j, in) && validJob.MatchString(j.Box+"."+j.ID)
}

// stopIdleThrowaway says whether it issued a stop, even if the outcome is
// ambiguous. The persistent reservation fences claims and pressure eviction
// until boxd sees a definitive outcome on the original instance.
func (w *Watcher) stopIdleThrowaway(ctx context.Context, c idleThrowaway) bool {
	box := c.in.Name
	w.mu.Lock()
	if w.idleStopping != "" || w.boxBusy(box, "") {
		w.mu.Unlock()
		return false
	}
	w.idleStopping = box
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.idleStopping = ""
		w.mu.Unlock()
	}()

	token := boxUID(c.in) + " " + w.Now().UTC().Format(time.RFC3339Nano) + " " + rand.Text()
	reserved, err := w.Incus.SetConfigIf(ctx, box, func(in Instance) map[string]string {
		now := w.Now()
		if boxUID(in) != boxUID(c.in) || jobrun.ThrowawayClaimed(in.Config, now) {
			return nil
		}
		if j, ok := w.idleStopJob(in, now); !ok || j.Key != c.job.Key {
			return nil
		}
		return map[string]string{jobrun.ThrowawayStopKey: token}
	})
	if err != nil || !reserved {
		if err != nil {
			look, cancel := context.WithTimeout(context.WithoutCancel(ctx), execTimeout)
			defer cancel()
			w.finishIdleStop(look, c.in, token, true) // the reservation write may have landed
		}
		return false
	}

	dispatch := false
	defer func() {
		look, cancel := context.WithTimeout(context.WithoutCancel(ctx), execTimeout)
		defer cancel()
		w.finishIdleStop(look, c.in, token, !dispatch)
	}()

	job := c.job.Box + "." + c.job.ID
	if _, _, err := w.Incus.FileRange(ctx, box, jobDir+"/"+job+".log", 0, 1); err != nil {
		return false
	}
	if _, err := w.Incus.Exec(ctx, box, idleStopScript(job)); err != nil {
		return false
	}
	if busy, err := w.Incus.InstanceBusy(ctx, box); err != nil || busy {
		return false
	}
	ins, err := w.Incus.Instances(ctx)
	if err != nil || ctx.Err() != nil {
		return false
	}
	for _, in := range ins {
		if in.Name != box || boxUID(in) != boxUID(c.in) || in.Config[jobrun.ThrowawayStopKey] != token {
			continue
		}
		j, ok := w.idleStopJob(in, w.Now())
		w.mu.Lock()
		busy := w.boxBusy(box, "")
		w.mu.Unlock()
		if !ok || j.Key != c.job.Key || busy {
			return false
		}
		// Incus state changes are name-bound. The fresh read and reservation
		// protect boxd and Box activity, but external Incus changes can race.
		dispatch = true
		if _, err := w.Incus.CLI(ctx, "", "stop", box, "--force"); err != nil {
			w.logf("boxd: stopping idle throwaway %s: %v", box, err)
		}
		return true
	}
	return false
}

// finishIdleStop clears its own reservation before dispatch, or replaces it
// with the stopped marker only after verifying the original stopped box.
// A later poll can finish a stop that completed after its client timed out.
func (w *Watcher) finishIdleStop(ctx context.Context, selected Instance, token string, aborted bool) {
	fields := strings.Fields(token)
	if len(fields) != 3 || fields[0] != boxUID(selected) {
		return
	}
	uid := fields[0]
	since, err := time.Parse(time.RFC3339Nano, fields[1])
	if err != nil {
		return
	}
	freeToRelease := false
	if !aborted && !w.Now().Before(since.Add(idleStopSettleAfter)) {
		busy, err := w.Incus.InstanceBusy(ctx, selected.Name)
		freeToRelease = err == nil && !busy
	}
	_, err = w.Incus.SetConfigIf(ctx, selected.Name, func(in Instance) map[string]string {
		if boxUID(in) != uid || in.Config[jobrun.ThrowawayStopKey] != token {
			return nil
		}
		if aborted {
			return map[string]string{jobrun.ThrowawayStopKey: ""}
		}
		if in.Status == "Running" && freeToRelease {
			return map[string]string{jobrun.ThrowawayStopKey: ""}
		}
		if in.Status != "Stopped" || in.Config[jobrun.ThrowawayKey] != "true" || mode.IsGPU(in.Config) {
			return nil
		}
		if _, ok := w.finishedIdleJob(in); !ok {
			return nil
		}
		return map[string]string{jobrun.ThrowawayStopKey: jobrun.ThrowawayStopped}
	})
	if err != nil {
		w.logf("boxd: settling idle stop of %s: %v", selected.Name, err)
	}
}

// Every visible job must have an exit file. A log whose wrapper has not
// published a PID yet also keeps the box running.
func idleStopScript(job string) string {
	return fmt.Sprintf(`# idle stop
cd %s || exit 3
test -f "%s.log" || exit 3
for log in *.log; do
  [ -e "$log" ] || continue
  [ -f "$log" ] && [ -f "${log%%.log}.exit" ] || exit 4
done
`, jobDir, job)
}
