package boxd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/jobrun"
	"github.com/a-Fig/agent-box/internal/mode"
)

const expiryJob = "work.20261124-180000-cafe"

func expiryBox(status, expires string) Instance {
	in := inst("work", status, "small", t0)
	in.Config["volatile.uuid"] = "original"
	in.Config[jobrun.ExpiresKey] = expires
	return in
}

func reapAt(t *testing.T, inc Incus, now time.Time) {
	t.Helper()
	w := Watcher{Incus: inc, Now: func() time.Time { return now }, Logf: t.Logf}
	if err := w.ReapExpired(context.Background(), false); err != nil {
		t.Fatal(err)
	}
}

func TestExpirySurvivesClientDisconnectAndCleanupRestart(t *testing.T) {
	for _, raw := range []string{jobrun.ProtectedExpiry, t0.Add(jobTTL).Format(time.RFC3339)} {
		in := expiryBox("Running", raw)
		if raw == jobrun.ProtectedExpiry {
			in.Config[jobrun.SetupKey] = jobrun.SetupValue(expiryJob, t0)
		}
		f := &fakeIncus{ins: []Instance{in}, jobs: map[string][]fakeJob{
			in.Name: {{name: expiryJob, state: "running"}},
		}}
		now := t0.Add(25 * time.Hour)
		reapAt(t, f, now)
		reapAt(t, f, now.Add(time.Minute)) // a new cleanup process
		got, ok := f.instance(in.Name)
		if !ok || got.Config[jobrun.ExpiresKey] != raw || len(f.expirySets) != 0 {
			t.Fatalf("running box changed: %+v", got)
		}
		f.jobs[in.Name][0] = fakeJob{name: expiryJob, state: "done", endedAt: now.Unix()}
		reapAt(t, f, now.Add(5*time.Minute))
		got, ok = f.instance(in.Name)
		if !ok || got.Config[jobrun.ExpiresKey] != now.Add(idleTTL).Format(time.RFC3339) {
			t.Fatalf("missed completion deadline = %+v", got)
		}
		reapAt(t, f, now.Add(idleTTL))
		if _, ok := f.instance(in.Name); !ok {
			t.Fatal("deleted at the deadline")
		}
		reapAt(t, f, now.Add(idleTTL+time.Minute))
		if _, ok := f.instance(in.Name); ok {
			t.Fatal("kept past retention")
		}
	}
}

func TestExpiryOfAbandonedSetupWaitsForIdleCheck(t *testing.T) {
	in := expiryBox("Running", jobrun.ProtectedExpiry)
	in.Config[jobrun.SetupKey] = jobrun.SetupValue(expiryJob, t0)
	in.Config[jobrun.ThrowawayKey] = "true"
	f := &fakeIncus{ins: []Instance{in}, jobs: map[string][]fakeJob{
		in.Name: {{name: expiryJob, state: "starting"}},
	}}
	reapAt(t, f, t0.Add(time.Minute))
	reapAt(t, f, t0.Add(jobrun.SetupTTL+time.Second))
	if got, _ := f.instance(in.Name); got.Config[jobrun.ExpiresKey] != jobrun.ProtectedExpiry {
		t.Fatal("incomplete launch lost protection")
	}
	f.jobs[in.Name][0].state = "died"
	now := t0.Add(jobrun.SetupTTL + time.Minute)
	reapAt(t, f, now)
	if got, _ := f.instance(in.Name); got.Config[jobrun.ExpiresKey] != now.Add(jobrun.EndedKeep).Format(time.RFC3339) {
		t.Fatalf("abandoned box deadline = %v", got.Config)
	}
	reapAt(t, f, now.Add(jobrun.EndedKeep+time.Minute))
	if _, ok := f.instance(in.Name); ok {
		t.Fatal("abandoned box kept past retention")
	}
}

type expiryProbeFailure struct{ *fakeIncus }

func (expiryProbeFailure) Exec(context.Context, string, string, ...string) ([]byte, error) {
	return nil, errors.New("exec probe unavailable")
}

func TestExpiryDefersWhenStateChangesOrIsUnavailable(t *testing.T) {
	now := t0.Add(time.Hour)
	for _, name := range []string{"paused", "held", "concurrent claim", "replaced", "failed guest probe", "failed list", "cooling"} {
		t.Run(name, func(t *testing.T) {
			in := expiryBox("Running", t0.Format(time.RFC3339))
			f := &fakeIncus{ins: []Instance{in}}
			var inc Incus = f
			switch name {
			case "paused":
				f.ins[0].Status = "Frozen"
			case "held":
				in.Config[mode.HoldKey] = now.Add(time.Hour).Format(time.RFC3339)
			case "concurrent claim":
				f.between = func(c map[string]string) {
					c[jobrun.SetupKey] = jobrun.SetupValue(expiryJob, now)
					c[jobrun.ExpiresKey] = jobrun.ProtectedExpiry
				}
			case "replaced":
				f.between = func(c map[string]string) { c["volatile.uuid"] = "replacement" }
			case "failed guest probe":
				inc = expiryProbeFailure{f}
			case "failed list":
				f.err = errors.New("Incus unavailable")
			case "cooling":
				in.Config[jobrun.ThrowawayStopKey] = "stop in progress"
			}
			w := Watcher{Incus: inc, Now: func() time.Time { return now }}
			reported := false
			w.Logf = func(string, ...any) { reported = true }
			err := w.ReapExpired(context.Background(), false)
			if (err != nil) != (name == "failed list") {
				t.Fatalf("reap: %v", err)
			}
			if (name == "failed guest probe" || name == "paused") && !reported {
				t.Fatal("cleanup gave no reason for deferral")
			}
			if len(f.cli) != 0 {
				t.Fatalf("cleanup called Incus: %v", f.cli)
			}
		})
	}
}

func TestExpiryRetriesFailedDeleteAfterReservation(t *testing.T) {
	in := expiryBox("Stopped", t0.Format(time.RFC3339))
	f := &fakeIncus{ins: []Instance{in}, cliErr: func([]string) error { return errors.New("delete failed") }}
	now := t0.Add(time.Hour)
	reapAt(t, f, now)
	reapAt(t, f, now.Add(time.Second))
	if len(f.cli) != 1 {
		t.Fatalf("commands during delete reservation = %v", f.cli)
	}
	f.cliErr = nil
	reapAt(t, f, now.Add(time.Minute))
	if _, ok := f.instance(in.Name); ok {
		t.Fatal("cleanup did not retry the failed delete")
	}
}

func TestExpiryLeavesStoppedThrowawayUntilRetentionEnds(t *testing.T) {
	deadline := t0.Add(jobrun.EndedKeep)
	in := expiryBox("Stopped", deadline.Format(time.RFC3339))
	in.Config[jobrun.ThrowawayKey] = "true"
	in.Config[jobrun.ThrowawayStopKey] = jobrun.ThrowawayStopped
	f := &fakeIncus{ins: []Instance{in}}
	reapAt(t, f, deadline.Add(-time.Minute))
	got, ok := f.instance(in.Name)
	if !ok || !keptThrowaway(got, deadline.Add(-time.Minute)) || f.execs != 0 {
		t.Fatalf("cooled box no longer retained and eligible for pressure eviction: %+v", got)
	}
	reapAt(t, f, deadline.Add(time.Minute))
	if _, ok := f.instance(in.Name); ok {
		t.Fatal("cooled box kept past its retrieval deadline")
	}
}

func TestExpiryScanChecksPIDOwnershipAndTerminalState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("guest script runs on Linux")
	}
	dir := t.TempDir()
	const stale, done = "stale", "done"
	if err := os.WriteFile(filepath.Join(dir, done+".exit"), []byte("124\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `printf '%s\n' "$$" > "$2/$1.pid"
printf '%s\n' "$$" > "$2/stale.pid"
printf '%s\n' "$$" > "$2/done.pid"
` + strings.ReplaceAll(expiryScan, "/work/.box", jobrun.Shq(dir))
	cmd := exec.Command("bash", "-s", "--", expiryJob, dir)
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "busy "+expiryJob+"\n") ||
		!strings.Contains(string(out), "done "+done+" ") || strings.Contains(string(out), "busy "+stale) ||
		strings.Contains(string(out), "busy "+done) {
		t.Fatalf("scan: %v\n%s", err, out)
	}
}
