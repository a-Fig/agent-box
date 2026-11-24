package mode

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Reclaimer pushes a frozen box's memory out to swap, so the WSL VM can give
// the RAM back to Windows while a game runs.
type Reclaimer interface {
	// Reclaim pushes frozen instance name, whose init process is pid, out to
	// swap and returns how many bytes it still holds in RAM. It ends within
	// ReclaimTimeout and always closes the swap it opened.
	Reclaim(ctx context.Context, name string, pid int64) (left int64, err error)
	// CloseSwap sets memory.swap.max back to 0 on the named boxes and on
	// every box in the project that isn't frozen, in case a reclaim was cut
	// off with swap open.
	CloseSwap(ctx context.Context, names ...string) error
}

const (
	// ReclaimTimeout bounds one reclaim attempt inside the distro.
	ReclaimTimeout = 60 * time.Second
	// ReclaimLeft is how much RAM a box may still hold after a reclaim that
	// worked; measured leftovers were about 11MB.
	ReclaimLeft = 256 << 20
	// ReclaimRetry is the wait before the next attempt, times the attempts so far.
	ReclaimRetry = 15 * time.Second
	// ReclaimTries is how many attempts a frozen box gets. After that it is
	// stopped, because a game needs the RAM more than the box needs its job.
	ReclaimTries = 2
)

// reclaimJob is the reclaim state of one frozen box. A new one is made on
// every freeze, so a box resumed and paused again is reclaimed again.
type reclaimJob struct {
	gen    int
	tries  int
	next   time.Time          // no attempt starts before this
	cancel context.CancelFunc // set while an attempt runs
	done   bool
	stop   bool // out of tries: stop the box
}

type reclaimResult struct {
	name string
	gen  int
	left int64
	err  error
}

// reclaims runs reclaim attempts in the background, so a slow or stuck one
// never holds up the tick that pauses the next box, stops long-frozen ones,
// or reaps. Only Tick touches the maps; attempts report through results.
type reclaims struct {
	jobs    map[string]*reclaimJob
	touched map[string]bool // a reclaim ran: close swap before the box runs again
	gen     int
	swept   bool // the start sweep ran (a tray killed mid-reclaim may have left swap open)
	sweep   bool // an attempt failed: close swap on boxes that run

	mu      sync.Mutex
	results []reclaimResult
	wg      sync.WaitGroup
}

func (r *reclaims) init() {
	if r.jobs == nil {
		r.jobs, r.touched = map[string]*reclaimJob{}, map[string]bool{}
	}
}

// forget drops a box's job, cancelling an attempt that runs.
func (r *reclaims) forget(name string) {
	if j := r.jobs[name]; j != nil && j.cancel != nil {
		j.cancel()
	}
	delete(r.jobs, name)
}

func (r *reclaims) forgetAll() {
	for name := range r.jobs {
		r.forget(name)
	}
}

// wait blocks until every attempt has reported; tests use it.
func (r *reclaims) wait() { r.wg.Wait() }

// harvest applies the attempts that ended since the last tick.
func (r *reclaims) harvest(now time.Time) (errs []error) {
	r.mu.Lock()
	res := r.results
	r.results = nil
	r.mu.Unlock()
	for _, x := range res {
		if x.err != nil {
			r.sweep = true
		}
		j := r.jobs[x.name]
		if j == nil || j.gen != x.gen {
			continue // the box was thawed, stopped or frozen again meanwhile
		}
		j.cancel = nil
		if x.err == nil && x.left <= ReclaimLeft {
			j.done = true
			continue
		}
		j.tries++
		err := x.err
		if err == nil {
			err = fmt.Errorf("reclaim %s: still holds %s in RAM", x.name, FormatBytes(x.left))
		}
		if j.tries >= ReclaimTries {
			j.stop = true
			err = fmt.Errorf("%w (attempt %d of %d; stopping it)", err, j.tries, ReclaimTries)
		} else {
			j.next = now.Add(ReclaimRetry * time.Duration(j.tries))
		}
		errs = append(errs, err)
	}
	return errs
}

// start runs one attempt in the background. ctx is the tray's, not the
// tick's: the attempt outlives the tick.
func (r *reclaims) start(ctx context.Context, rc Reclaimer, in Instance) {
	j := r.jobs[in.Name]
	var pid int64
	if in.State != nil {
		pid = in.State.Pid
	}
	actx, cancel := context.WithTimeout(ctx, ReclaimTimeout+15*time.Second)
	j.cancel = cancel
	r.touched[in.Name] = true
	r.wg.Add(1)
	go func(name string, gen int) {
		defer r.wg.Done()
		defer cancel()
		left, err := int64(0), error(nil)
		if pid <= 0 {
			err = fmt.Errorf("reclaim %s: no init PID", name)
		} else {
			left, err = rc.Reclaim(actx, name, pid)
		}
		r.mu.Lock()
		r.results = append(r.results, reclaimResult{name, gen, left, err})
		r.mu.Unlock()
	}(in.Name, j.gen)
}

// reclaimFrozen is the reclaim step for a CPU box in My PC, after freeze.
// prev is its state before freeze ran. It returns the state the box ends in.
func (m *Manager) reclaimFrozen(ctx context.Context, now time.Time, in Instance, prev, state string) (string, error) {
	r := &m.rc
	if state != "frozen" {
		r.forget(in.Name)
		return state, nil
	}
	if prev != "frozen" || r.jobs[in.Name] == nil { // a new freeze
		r.forget(in.Name)
		r.gen++
		r.jobs[in.Name] = &reclaimJob{gen: r.gen}
	}
	j := r.jobs[in.Name]
	switch {
	case j.stop:
		if err := m.Incus.Stop(ctx, in.Name); err != nil {
			return state, err
		}
		r.forget(in.Name)
		delete(m.frozenSince, in.Name)
		return "stopped", nil
	case !j.done && j.cancel == nil && !now.Before(j.next):
		r.start(ctx, m.Reclaim, in)
	}
	return state, nil
}

// closeSwapBeforeRun closes the swap a reclaim opened on a box about to run
// again (or already running). Until that works the box stays frozen.
func (m *Manager) closeSwapBeforeRun(ctx context.Context, name string) error {
	r := &m.rc
	if m.Reclaim == nil || !r.touched[name] {
		return nil
	}
	r.forget(name)
	if err := m.Reclaim.CloseSwap(ctx, name); err != nil {
		return err
	}
	delete(r.touched, name)
	return nil
}

// sweepSwap runs before the instance loop: once at start for every box,
// since a tray killed mid-reclaim can leave swap open, and after a failed
// attempt for boxes that run.
func (m *Manager) sweepSwap(ctx context.Context, ins []Instance) error {
	r := &m.rc
	switch {
	case !r.swept:
		names := make([]string, len(ins))
		for i, in := range ins {
			names[i] = in.Name
		}
		if err := m.Reclaim.CloseSwap(ctx, names...); err != nil {
			return err
		}
		r.swept, r.sweep = true, false
	case r.sweep:
		if err := m.Reclaim.CloseSwap(ctx); err != nil {
			return err
		}
		r.sweep = false
	}
	return nil
}

// prune drops state for instances that are gone.
func (r *reclaims) prune(seen map[string]bool) {
	for name := range r.jobs {
		if !seen[name] {
			r.forget(name)
		}
	}
	for name := range r.touched {
		if !seen[name] {
			delete(r.touched, name)
		}
	}
}

// CgroupReclaim implements Reclaimer with cgroup v2's memory.reclaim on the
// container's cgroup. Run runs a command as root inside the distro.
//
// Incus gives every container memory.swap.max=0 and a restricted project
// forbids limits.memory.swap, so the script opens swap only for the reclaim
// and then puts 0 back: a running box never swaps. memory.reclaim works on a
// frozen cgroup, since the writer does the reclaim; it ends with EAGAIN when
// it can't take all it was asked for, which is why the script reports what
// is left instead. Measured on the PC on 2026-11-24: 3.2GB went out in 3.2s.
type CgroupReclaim struct{ Run RunFunc }

// The scripts get their arguments as $1, $2, ...; they have no double
// quotes or backslashes, so they pass through wsl.exe's command line intact.
//
// reclaimScript ($1 name, $2 init PID) finds the box's cgroup from its init
// process (LXC may add a -N suffix to the directory, so the name alone can
// point at a stale one), checks it is frozen right before opening swap, and
// restores swap on TERM: `timeout` sends that at ReclaimTimeout, and the
// reclaim write is the shell's own echo, so the signal cuts it short. It
// prints what the box still holds. Exit 2: not the box's cgroup; 3: not
// frozen; 4: cut off.
const reclaimScript = `p=$(sed -n 's|^0::/||p' /proc/$2/cgroup) || exit 2; t=${p%%/*}; ` +
	`case $t in lxc.payload.` + Project + `_$1|lxc.payload.` + Project + `_$1-[0-9]*) ;; *) echo not the box cgroup: $t >&2; exit 2;; esac; ` +
	`c=/sys/fs/cgroup/$t; grep -qx 'frozen 1' $c/cgroup.events || exit 3; ` +
	`o=$(cat $c/memory.swap.max) || exit 2; [ $o = max ] && o=0; ` +
	`trap 'echo $o > $c/memory.swap.max; exit 4' INT TERM HUP; ` +
	`echo max > $c/memory.swap.max; n=$(cat $c/memory.current); echo $n > $c/memory.reclaim 2>/dev/null; ` +
	`echo $o > $c/memory.swap.max; trap - INT TERM HUP; cat $c/memory.current`

// closeSwapScript ($@ names) sets memory.swap.max to 0 on the named boxes'
// cgroups and on every unfrozen box cgroup in the project.
const closeSwapScript = `for c in /sys/fs/cgroup/lxc.payload.` + Project + `_*; do ` +
	`[ -f $c/memory.swap.max ] || continue; b=${c#/sys/fs/cgroup/lxc.payload.` + Project + `_}; w=0; ` +
	`grep -qx 'frozen 1' $c/cgroup.events || w=1; ` +
	`for n in $@; do case $b in $n|$n-[0-9]*) w=1;; esac; done; ` +
	`[ $w = 1 ] && [ $(cat $c/memory.swap.max) != 0 ] && { echo 0 > $c/memory.swap.max || r=1; }; done; exit ${r:-0}`

var instanceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)

func (c CgroupReclaim) Reclaim(ctx context.Context, name string, pid int64) (int64, error) {
	if !instanceName.MatchString(name) || pid <= 0 {
		return 0, fmt.Errorf("reclaim: bad instance %q (pid %d)", name, pid)
	}
	secs := strconv.Itoa(int(ReclaimTimeout / time.Second))
	out, err := c.Run(ctx, "timeout", "-s", "TERM", secs, "sh", "-c", reclaimScript, "sh", name, strconv.FormatInt(pid, 10))
	if err != nil {
		return 0, fmt.Errorf("reclaim %s: %w", name, err)
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return 0, fmt.Errorf("reclaim %s: no memory.current", name)
	}
	left, perr := strconv.ParseInt(f[len(f)-1], 10, 64)
	if perr != nil {
		return 0, fmt.Errorf("reclaim %s: memory.current %q: %w", name, f[len(f)-1], perr)
	}
	return left, nil
}

func (c CgroupReclaim) CloseSwap(ctx context.Context, names ...string) error {
	for _, n := range names {
		if !instanceName.MatchString(n) {
			return fmt.Errorf("close swap: bad instance name %q", n)
		}
	}
	args := append([]string{"sh", "-c", closeSwapScript, "sh"}, names...)
	if _, err := c.Run(ctx, args...); err != nil {
		return errors.Join(errors.New("close swap"), err)
	}
	return nil
}
