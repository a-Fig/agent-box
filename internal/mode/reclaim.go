package mode

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Reclaimer is the cgroup side of giving frozen boxes' RAM back to Windows.
//
// Every box may swap up to its own memory limit (EnsureSwap, every tick), so
// reclaiming a frozen box is a single memory.reclaim write: nothing is opened
// for it and nothing needs putting back, so a reclaim that is cancelled,
// killed or raced can't leave a box in a state that needs repair.
type Reclaimer interface {
	// EnsureSwap sets memory.swap.max on each box's cgroup where it differs.
	EnsureSwap(ctx context.Context, boxes []SwapBox) error
	// Reclaim writes memory.reclaim on frozen box name (init process pid)
	// and returns what it still holds in RAM. ran is false when the reclaim
	// never got to the write: the box wasn't frozen, its cgroup wasn't the
	// expected one, or wsl.exe failed or timed out.
	Reclaim(ctx context.Context, name string, pid int64) (left int64, ran bool, err error)
}

// SwapBox is one box's wanted memory.swap.max.
type SwapBox struct {
	Name  string
	Pid   int64 // init process on the host; the cgroup is read from it
	Bytes int64
}

const (
	// ReclaimTimeout bounds one reclaim attempt inside the distro.
	ReclaimTimeout = 60 * time.Second
	// ReclaimLeft is how much RAM a box may still hold after a reclaim that
	// worked; measured leftovers were about 11MB.
	ReclaimLeft = 256 << 20
	// ReclaimRetry is the wait before the next attempt, times the attempts so
	// far, up to ReclaimRetryMax.
	ReclaimRetry    = 15 * time.Second
	ReclaimRetryMax = time.Minute
	// ReclaimTries is how many reclaims may run and still leave more than
	// ReclaimLeft before the box is stopped early, because a game needs the
	// RAM more than the box needs its job. Attempts that never ran (WSL,
	// PID or timeout failures) don't count: they retry, and FrozenStop still
	// stops the box after 15 minutes.
	ReclaimTries = 2
)

// reclaimJob is the reclaim state of one frozen box, for one identity (name
// and init PID: a box deleted and recreated under the same name starts
// fresh) and one freeze (a box resumed and paused again starts fresh).
type reclaimJob struct {
	pid    int64
	gen    int
	short  int // reclaims that ran and left more than ReclaimLeft
	failed int // attempts that never ran
	next   time.Time
	cancel context.CancelFunc // set while an attempt runs
	done   bool
	stop   bool
}

type reclaimResult struct {
	name string
	gen  int
	left int64
	ran  bool
	err  error
}

// reclaims runs reclaim attempts in the background, so a slow or stuck one
// never holds up the tick that pauses the next box, stops long-frozen ones,
// or reaps. Only Tick (and Release, under the tick lock) touches jobs;
// attempts report through results.
type reclaims struct {
	jobs      map[string]*reclaimJob
	gen       int
	releasing atomic.Bool // Release memory is under way: start no wsl.exe worker

	mu      sync.Mutex
	results []reclaimResult
	wg      sync.WaitGroup
}

func (r *reclaims) init() {
	if r.jobs == nil {
		r.jobs = map[string]*reclaimJob{}
	}
}

// forget drops a box's job, cancelling an attempt that runs; its result,
// if one still comes, matches no job and is dropped.
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

// wait blocks until every attempt has reported; tests use it. The tray
// never waits: there is nothing to clean up after a worker.
func (r *reclaims) wait() { r.wg.Wait() }

// harvest applies the attempts that ended since the last tick.
func (r *reclaims) harvest(now time.Time) (errs []error) {
	r.mu.Lock()
	res := r.results
	r.results = nil
	r.mu.Unlock()
	for _, x := range res {
		j := r.jobs[x.name]
		if j == nil || j.gen != x.gen {
			continue // the box was thawed, stopped, recreated or frozen again meanwhile
		}
		j.cancel = nil
		switch {
		case x.ran && x.left <= ReclaimLeft:
			j.done = true
			continue
		case x.ran:
			j.short++
			err := fmt.Errorf("reclaim %s: still holds %s in RAM", x.name, FormatBytes(x.left))
			if j.short >= ReclaimTries {
				j.stop = true
				err = fmt.Errorf("%w after %d reclaims; stopping it", err, j.short)
			} else {
				j.next = now.Add(ReclaimRetry * time.Duration(j.short))
			}
			errs = append(errs, err)
		default:
			j.failed++
			j.next = now.Add(min(ReclaimRetry*time.Duration(j.failed), ReclaimRetryMax))
			if x.err == nil {
				x.err = fmt.Errorf("reclaim %s did not run", x.name)
			}
			errs = append(errs, x.err)
		}
	}
	return errs
}

// start runs one attempt in the background. ctx is the tray's, not the
// tick's: the attempt outlives the tick.
func (r *reclaims) start(ctx context.Context, rc Reclaimer, name string, j *reclaimJob) {
	actx, cancel := context.WithTimeout(ctx, ReclaimTimeout+15*time.Second)
	j.cancel = cancel
	r.wg.Add(1)
	go func(gen int, pid int64) {
		defer r.wg.Done()
		defer cancel()
		res := reclaimResult{name: name, gen: gen}
		if r.releasing.Load() || actx.Err() != nil {
			res.err = fmt.Errorf("reclaim %s: cancelled", name)
		} else {
			res.left, res.ran, res.err = rc.Reclaim(actx, name, pid)
		}
		r.mu.Lock()
		r.results = append(r.results, res)
		r.mu.Unlock()
	}(j.gen, j.pid)
}

// BeginRelease is called before the tray's "Release memory" runs `wsl
// --terminate`: queued and running reclaims are dropped (their wsl.exe
// calls cancelled) and no new one starts until EndRelease.
func (m *Manager) BeginRelease() {
	m.rc.releasing.Store(true)
	m.tickMu.Lock()
	defer m.tickMu.Unlock()
	m.rc.forgetAll()
}

// EndRelease lets reclaims start again.
func (m *Manager) EndRelease() { m.rc.releasing.Store(false) }

func pidOf(in Instance) int64 {
	if in.State == nil {
		return 0
	}
	return in.State.Pid
}

// reclaimFrozen is the reclaim step for a CPU box in My PC, after freeze.
// prev is its state before freeze ran. It returns the state the box ends in.
func (m *Manager) reclaimFrozen(ctx context.Context, now time.Time, in Instance, prev, state string) (string, error) {
	r := &m.rc
	pid := pidOf(in)
	if state != "frozen" || pid <= 0 {
		r.forget(in.Name)
		return state, nil
	}
	if j := r.jobs[in.Name]; prev != "frozen" || j == nil || j.pid != pid { // a new freeze or a new box
		r.forget(in.Name)
		r.gen++
		r.jobs[in.Name] = &reclaimJob{pid: pid, gen: r.gen}
	}
	j := r.jobs[in.Name]
	switch {
	case j.stop:
		return m.stopUnreclaimed(ctx, in, pid, state)
	case !j.done && j.cancel == nil && !now.Before(j.next) && !r.releasing.Load():
		r.start(ctx, m.Reclaim, in.Name, j)
	}
	return state, nil
}

// stopUnreclaimed stops a box that reclaims couldn't empty, after checking
// again that the PC still yields (the mode may have changed during the tick)
// and that the box is still the same frozen instance.
func (m *Manager) stopUnreclaimed(ctx context.Context, in Instance, pid int64, state string) (string, error) {
	if m.Chosen() != MyPC && !m.yielding {
		return state, nil
	}
	ins, err := m.Incus.List(ctx)
	if err != nil {
		return state, err
	}
	for _, now := range ins {
		if now.Name == in.Name && strings.EqualFold(now.Status, "frozen") && pidOf(now) == pid {
			if err := m.Incus.Stop(ctx, in.Name); err != nil {
				return state, err
			}
			m.rc.forget(in.Name)
			delete(m.frozenSince, in.Name)
			return "stopped", nil
		}
	}
	m.rc.forget(in.Name) // gone, resumed or recreated: nothing to stop
	return state, nil
}

// ensureSwap asks for every live box's memory.swap.max to be its memory
// limit. Incus writes 0 there whenever it starts a box, so this runs every
// tick; it writes only values that differ.
func (m *Manager) ensureSwap(ctx context.Context, ins []Instance) error {
	var boxes []SwapBox
	for _, in := range ins {
		if pid := pidOf(in); pid > 0 {
			boxes = append(boxes, SwapBox{Name: in.Name, Pid: pid, Bytes: swapFor(in)})
		}
	}
	if len(boxes) == 0 {
		return nil
	}
	return m.Reclaim.EnsureSwap(ctx, boxes)
}

// swapFor is a box's memory.swap.max: its own memory limit, so it can swap
// all of its RAM out when frozen. Page-aligned, as the kernel stores it.
func swapFor(in Instance) int64 {
	n := AllocatedMemory([]Instance{in})
	return n &^ 4095
}

// prune drops state for instances that are gone.
func (r *reclaims) prune(seen map[string]bool) {
	for name := range r.jobs {
		if !seen[name] {
			r.forget(name)
		}
	}
}

// CgroupReclaim implements Reclaimer with cgroup v2 files. Run runs a
// command as root inside the distro.
//
// memory.reclaim works on a frozen cgroup, since the writer does the
// reclaim; it ends with EAGAIN when it can't take all it was asked for,
// which is why the script reports what is left. Measured on the PC on
// 2026-11-24: 3.2GB went out in 3.2s.
type CgroupReclaim struct{ Run RunFunc }

// The scripts get their arguments as $1, $2, ...; they have no double
// quotes or backslashes, so they pass through wsl.exe's command line intact.
// Each finds a box's cgroup from its init process and accepts only the exact
// path lxc.payload.agents_<name>: LXC can add a -N suffix when a stale
// directory is in the way, and box and box-1 would then be ambiguous, so a
// suffixed cgroup is refused (the reclaim retries; FrozenStop covers it).
const cgroupOf = `t=$(sed -n 's|^0::/||p' /proc/$P/cgroup 2>/dev/null); t=${t%%/*}; `

// reclaimScript ($1 name, $2 PID) exits 2 for the wrong cgroup and 3 for a
// box that isn't frozen; after the write it prints "left <bytes>".
const reclaimScript = `P=$2; ` + cgroupOf +
	`[ x$t = xlxc.payload.` + Project + `_$1 ] || { echo cgroup: $t >&2; exit 2; }; c=/sys/fs/cgroup/$t; ` +
	`grep -qx 'frozen 1' $c/cgroup.events || { echo not frozen >&2; exit 3; }; ` +
	`n=$(cat $c/memory.current); echo $n > $c/memory.reclaim 2>/dev/null; echo left $(cat $c/memory.current)`

// ensureSwapScript ($@ name PID bytes, repeated) sets memory.swap.max where
// it differs and exits 1, naming them, if any box's cgroup was wrong.
const ensureSwapScript = `r=0; while [ $# -ge 3 ]; do N=$1; P=$2; B=$3; shift 3; ` + cgroupOf +
	`if [ x$t != xlxc.payload.` + Project + `_$N ]; then echo $N: cgroup $t >&2; r=1; continue; fi; ` +
	`c=/sys/fs/cgroup/$t; [ x$(cat $c/memory.swap.max) = x$B ] && continue; ` +
	`echo $B > $c/memory.swap.max || { echo $N: write failed >&2; r=1; }; done; exit $r`

var instanceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)

func (c CgroupReclaim) Reclaim(ctx context.Context, name string, pid int64) (int64, bool, error) {
	if !instanceName.MatchString(name) || pid <= 0 {
		return 0, false, fmt.Errorf("reclaim: bad instance %q (pid %d)", name, pid)
	}
	secs := strconv.Itoa(int(ReclaimTimeout / time.Second))
	out, err := c.Run(ctx, "timeout", secs, "sh", "-c", reclaimScript, "sh", name, strconv.FormatInt(pid, 10))
	if err != nil {
		return 0, false, fmt.Errorf("reclaim %s: %w", name, err)
	}
	f := strings.Fields(string(out))
	if len(f) < 2 || f[len(f)-2] != "left" {
		return 0, false, fmt.Errorf("reclaim %s: unexpected output %q", name, out)
	}
	left, perr := strconv.ParseInt(f[len(f)-1], 10, 64)
	if perr != nil {
		return 0, false, fmt.Errorf("reclaim %s: memory.current %q: %w", name, f[len(f)-1], perr)
	}
	return left, true, nil
}

func (c CgroupReclaim) EnsureSwap(ctx context.Context, boxes []SwapBox) error {
	args := []string{"sh", "-c", ensureSwapScript, "sh"}
	for _, b := range boxes {
		if !instanceName.MatchString(b.Name) || b.Pid <= 0 || b.Bytes < 0 {
			return fmt.Errorf("swap: bad instance %q (pid %d)", b.Name, b.Pid)
		}
		args = append(args, b.Name, strconv.FormatInt(b.Pid, 10), strconv.FormatInt(b.Bytes, 10))
	}
	if _, err := c.Run(ctx, args...); err != nil {
		return errors.Join(errors.New("set memory.swap.max"), err)
	}
	return nil
}
