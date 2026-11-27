package boxd

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/a-Fig/agent-box/internal/jobrun"
	"github.com/a-Fig/agent-box/internal/mode"
)

// The timer needs job state and end times, without the watcher's log tails,
// history or limit on how many jobs it reports.
const expiryScan = "# expiry scan\n" + jobrun.AliveFn + jobStateFn + `set -e
if [ -e /work/.box ]; then
  cd /work/.box
  for file in *.log *.pid *.exit; do
    [ -e "$file" ] || [ -L "$file" ] || continue
    job=${file%.*}
    for f in "$job.log" "$job.pid" "$job.exit"; do
      if { [ -e "$f" ] || [ -L "$f" ]; } && [ ! -f "$f" ]; then exit 4; fi
    done
    read -r state code <<< "$(jobstate "$job")"
    case "$state" in
      running|starting) echo "busy $job";;
      done)
        [[ $code =~ ^[0-9]+$ ]] || exit 4
        stamp=$(stat -c %Y -- "$job.exit")
        echo "done $job $stamp";;
      died) ;;
      unknown) echo "could not read PID for $job" >&2; exit 4;;
      *) exit 4;;
    esac
  done
fi
echo checked
`

// ReapExpired runs once from the distro timer. All reservations and deadlines
// live in Incus, so a restart can retry with the same evidence.
func (w *Watcher) ReapExpired(ctx context.Context, dryRun bool) error {
	ins, err := w.Incus.Instances(ctx)
	if err != nil {
		return err
	}
	for _, in := range ins {
		ctx, cancel := context.WithTimeout(ctx, jobrun.EvictTTL/2)
		if err := w.reapInstance(ctx, in, dryRun); err != nil {
			w.logf("reaper: defer %s: %v", in.Name, err)
		}
		cancel()
	}
	return ctx.Err()
}

func (w *Watcher) reapInstance(ctx context.Context, selected Instance, dryRun bool) error {
	box := selected.Name
	if !validBoxName.MatchString(box) || (selected.Config["volatile.uuid"] == "" && unsetTime(selected.CreatedAt)) {
		return fmt.Errorf("missing or invalid instance identity")
	}
	now := w.Now().UTC()
	raw := selected.Config[jobrun.ExpiresKey]
	if raw == "" {
		return nil
	}
	expires, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return fmt.Errorf("invalid %s %q", jobrun.ExpiresKey, raw)
	}
	managed := mode.ProtectedDeadline(raw)
	claim := selected.Config[jobrun.ThrowawayClaimKey] != "" && selected.Config[jobrun.ThrowawayOwnerKey] != ""
	if jobrun.ThrowawayClaimed(selected.Config, now) || (!managed && !claim && !mode.Held(selected.Config, expires).Before(now)) {
		return nil
	}
	owner, setup := jobrun.SetupOwner(selected.Config)
	if claim {
		owner = selected.Config[jobrun.ThrowawayOwnerKey]
		setup, _ = time.Parse(time.RFC3339, selected.Config[jobrun.ThrowawayClaimKey])
	}
	if setup.After(now) && selected.Config[jobrun.ThrowawayKey] != "true" {
		return nil // parallel repo jobs can be uploading without a guest marker yet
	}
	if selected.Status != "Running" && selected.Status != "Stopped" {
		return fmt.Errorf("instance is %s; job state unavailable", selected.Status)
	}
	if busy, err := w.Incus.InstanceBusy(ctx, box); err != nil || busy {
		if err != nil {
			return err
		}
		return fmt.Errorf("Incus operation in progress")
	}
	busy, end, ownerEnded, err := w.expiryJobs(ctx, selected, owner)
	if err != nil || busy || (setup.After(now) && !ownerEnded) {
		return err
	}

	ttl := idleTTL
	if selected.Config[jobrun.ThrowawayKey] == "true" {
		ttl = jobrun.EndedKeep
	}
	// Use a recorded end when it covers the last setup. A leftover claim or
	// an absent wrapper without a terminal record starts retention now.
	if (managed || claim) && (end.IsZero() || (owner != "" && !ownerEnded)) {
		end = now
	}
	deadline := mode.Held(selected.Config, end.Add(ttl))
	update := managed || claim || (!end.IsZero() && deadline.After(expires))
	if dryRun {
		if update {
			w.logf("reaper: would set %s expiry to %s", box, deadline.Format(time.RFC3339))
		} else {
			w.logf("reaper: would delete %s (expired %s)", box, raw)
		}
		return nil
	}

	lease := now.Add(jobrun.EvictTTL).Format(time.RFC3339Nano)
	wrote, err := w.Incus.SetConfigIf(ctx, box, func(in Instance) map[string]string {
		if !sameExpiryInstance(selected, in) {
			return nil
		}
		if update {
			changes := map[string]string{
				jobrun.ExpiresKey: deadline.Format(time.RFC3339Nano), jobrun.ThrowawayClaimKey: "",
			}
			if mode.ProtectedDeadline(in.Config[mode.HoldKey]) {
				changes[mode.HoldKey] = ""
			}
			return changes
		}
		return map[string]string{jobrun.DirectEvictKey: lease}
	})
	if err != nil || !wrote || update {
		return err
	}
	// Recheck live evidence under the reservation and read the instance
	// again before Incus's name-bound delete (#217).
	if busy, _, _, err := w.expiryJobs(ctx, selected, owner); err != nil || busy {
		return err
	}
	if busy, err := w.Incus.InstanceBusy(ctx, box); err != nil || busy {
		return err
	}
	ins, err := w.Incus.Instances(ctx)
	if err != nil {
		return err
	}
	selected.Config = maps.Clone(selected.Config)
	selected.Config[jobrun.DirectEvictKey] = lease
	for _, in := range ins {
		if in.Name != box || !sameExpiryInstance(selected, in) {
			continue
		}
		if ctx.Err() != nil || !jobrun.DeleteReserved(in.Config, w.Now()) {
			return nil
		}
		w.logf("reaper: deleting %s (expired %s)", box, raw)
		_, err := w.Incus.CLI(ctx, "", "delete", box, "--force")
		return err
	}
	return nil
}

func sameExpiryInstance(a, b Instance) bool {
	if boxUID(a) != boxUID(b) || a.Status != b.Status {
		return false
	}
	for _, key := range []string{jobrun.ExpiresKey, jobrun.SetupKey, mode.HoldKey, jobrun.ThrowawayKey,
		jobrun.ThrowawayOwnerKey, jobrun.ThrowawayClaimKey, jobrun.ThrowawayStopKey, jobrun.DirectEvictKey} {
		if a.Config[key] != b.Config[key] {
			return false
		}
	}
	return true
}

func (w *Watcher) expiryJobs(ctx context.Context, in Instance, owner string) (busy bool, ended time.Time, ownerEnded bool, err error) {
	if in.Status == "Stopped" {
		return false, time.Time{}, false, nil
	}
	out, err := w.Incus.Exec(ctx, in.Name, expiryScan)
	if err != nil {
		return false, time.Time{}, false, err
	}
	body, checked := strings.CutSuffix(string(out), "checked\n")
	if !checked {
		return false, time.Time{}, false, fmt.Errorf("incomplete job state")
	}
	for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch {
		case len(f) == 2 && f[0] == "busy":
			busy = true
		case len(f) == 3 && f[0] == "done":
			stamp, err := strconv.ParseInt(f[2], 10, 64)
			if err != nil || stamp <= 0 {
				return false, time.Time{}, false, fmt.Errorf("invalid job end time")
			}
			if t := time.Unix(stamp, 0).UTC(); t.After(ended) {
				ended = t
			}
			ownerEnded = ownerEnded || f[1] == owner
		default:
			return false, time.Time{}, false, fmt.Errorf("invalid job state")
		}
	}
	return busy, ended, ownerEnded, nil
}
