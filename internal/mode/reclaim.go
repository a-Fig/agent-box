package mode

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Reclaimer is the cgroup side of giving frozen boxes' RAM back to Windows.
//
// Every box may swap up to its own memory limit (EnsureSwap, every tick), so
// reclaiming a frozen box is a single memory.reclaim write: nothing is opened
// for it and nothing needs putting back, so a reclaim that is cancelled,
// killed or raced can't leave a box in a state that needs repair. Reclaim is
// best effort and never stops a box itself; what it leaves only picks the
// frozen limit (FrozenStop, or FrozenSwappedStop when ReclaimLeft or less).
type Reclaimer interface {
	// EnsureSwap sets memory.swap.max on each box's cgroup where it differs
	// and returns an error for each box it couldn't (nil entries are fine).
	EnsureSwap(ctx context.Context, boxes []SwapBox) map[string]error
	// Reclaim writes memory.reclaim on frozen box name (init process pid)
	// and returns what it still holds in RAM. ran is false when the write
	// never happened: the box wasn't frozen, its cgroup wasn't the expected
	// one, or wsl.exe failed or timed out.
	Reclaim(ctx context.Context, name string, pid int64) (left int64, ran bool, err error)
}

// SwapBox is one box's wanted memory.swap.max.
type SwapBox struct {
	Name  string
	Pid   int64 // init process on the host; the cgroup is read from it
	Bytes int64
}

// Reclaimed is one reclaim that ran: what the box still holds afterwards,
// for the log and the dashboard. Only frozenLimit reads it.
type Reclaimed struct {
	Name string
	Left int64
}

const (
	// ReclaimLeft is the most a box may still hold after a reclaim for its
	// freeze to get FrozenSwappedStop; measured leftovers were about 10MB.
	ReclaimLeft = 256 << 20
	// ReclaimTimeout bounds one reclaim attempt inside the distro.
	ReclaimTimeout = 60 * time.Second
	// ReclaimRetry is the wait before retrying a reclaim whose write never
	// happened, times the failures so far, up to ReclaimRetryMax.
	ReclaimRetry    = 15 * time.Second
	ReclaimRetryMax = time.Minute
)

// reclaimJob is the reclaim state of one frozen box, for one identity (name
// and init PID: a box deleted and recreated under the same name starts
// fresh) and one freeze (a box resumed and paused again starts fresh).
type reclaimJob struct {
	pid    int64
	gen    int
	failed int // attempts whose write never happened
	next   time.Time
	cancel context.CancelFunc // set while an attempt runs
	done   bool               // the write happened; no more attempts this freeze
	left   int64              // what the box held after it (set when done)
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
// or reaps. Only Tick (and BeginRelease, under the tick lock) touches jobs;
// attempts report through results.
type reclaims struct {
	jobs map[string]*reclaimJob
	gen  int

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

// wait blocks until every attempt has reported.
func (r *reclaims) wait() { r.wg.Wait() }

// harvest applies the attempts that ended since the last tick.
func (r *reclaims) harvest(now time.Time) (done []Reclaimed, errs []error) {
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
		if x.ran {
			j.done, j.left = true, x.left
			done = append(done, Reclaimed{x.name, x.left})
			continue
		}
		j.failed++
		j.next = now.Add(min(ReclaimRetry*time.Duration(j.failed), ReclaimRetryMax))
		if x.err == nil {
			x.err = fmt.Errorf("reclaim %s did not run", x.name)
		}
		errs = append(errs, x.err)
	}
	return done, errs
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
		res.left, res.ran, res.err = rc.Reclaim(actx, name, pid)
		r.mu.Lock()
		r.results = append(r.results, res)
		r.mu.Unlock()
	}(j.gen, j.pid)
}

func pidOf(in Instance) int64 {
	if in.State == nil {
		return 0
	}
	return in.State.Pid
}

// reclaimFrozen is the reclaim step for a CPU box in My PC, after freeze:
// one write per freeze, retried with backoff only if it never happened.
// prev is the box's state before freeze ran.
func (m *Manager) reclaimFrozen(now time.Time, in Instance, prev, state string) {
	r := &m.rc
	pid := pidOf(in)
	if state != "frozen" || pid <= 0 {
		r.forget(in.Name)
		return
	}
	if j := r.jobs[in.Name]; prev != "frozen" || j == nil || j.pid != pid { // a new freeze or a new box
		r.forget(in.Name)
		r.gen++
		r.jobs[in.Name] = &reclaimJob{pid: pid, gen: r.gen}
	}
	// Not before this instance's swap.max is known to be set: Incus writes 0
	// when it starts a box, so the first freeze after a start (or a tray
	// start) would reclaim nothing and get the short limit. Swap is set at
	// the end of the tick, so this waits one tick at most.
	if j := r.jobs[in.Name]; !j.done && j.cancel == nil && !now.Before(j.next) && !m.releasing.Load() && m.swapSet[in.Name] == pid {
		r.start(m.workCtx, m.Reclaim, in.Name, j)
	}
}

// ensureSwap asks for every live box's memory.swap.max to be its memory
// limit. Incus writes 0 there whenever it starts a box, so this runs every
// tick, after the instance actions; it writes only values that differ. A
// box with no limits.memory (own or from a profile) is reported and left
// alone: it has no limit to swap up to.
func (m *Manager) ensureSwap(ctx context.Context, ins []Instance, ended map[string]string) (errs []error) {
	set := map[string]int64{}
	defer func() { m.swapSet = set }()
	var boxes []SwapBox
	for _, in := range ins {
		pid := pidOf(in)
		if st := ended[in.Name]; pid <= 0 || (st != "running" && st != "frozen") {
			continue // stopped (or gone) this tick: no cgroup to write
		}
		b, ok := memoryLimit(in)
		if !ok {
			errs = append(errs, fmt.Errorf("%s has no limits.memory: not letting it swap", in.Name))
			continue
		}
		boxes = append(boxes, SwapBox{Name: in.Name, Pid: pid, Bytes: b &^ 4095}) // page-aligned, as the kernel stores it
	}
	if len(boxes) == 0 {
		return errs
	}
	res := m.Reclaim.EnsureSwap(ctx, boxes)
	for _, b := range boxes { // in order, so the error text is stable
		if err := res[b.Name]; err != nil {
			errs = append(errs, err)
		} else {
			set[b.Name] = b.Pid
		}
	}
	return errs
}

// memoryLimit is the box's limits.memory, from its expanded config (profiles
// included) or its own; false if neither has a size.
func memoryLimit(in Instance) (int64, bool) {
	v, ok := in.ExpandedConfig[MemoryCapKey]
	if !ok {
		v = in.Config[MemoryCapKey]
	}
	return ParseBytes(v)
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
// it differs and prints one line per box: "ok <name>" or "err <name> <why>".
// It always exits 0, so one box gone mid-batch doesn't hide the others.
const ensureSwapScript = `while [ $# -ge 3 ]; do N=$1; P=$2; B=$3; shift 3; ` + cgroupOf +
	`if [ x$t != xlxc.payload.` + Project + `_$N ]; then echo err $N cgroup $t; continue; fi; ` +
	`c=/sys/fs/cgroup/$t; if [ x$(cat $c/memory.swap.max 2>/dev/null) = x$B ]; then echo ok $N; continue; fi; ` +
	`if echo $B > $c/memory.swap.max; then echo ok $N; else echo err $N write failed; fi; done; exit 0`

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
		return 0, true, fmt.Errorf("reclaim %s: memory.current %q: %w", name, f[len(f)-1], perr)
	}
	return left, true, nil
}

func (c CgroupReclaim) EnsureSwap(ctx context.Context, boxes []SwapBox) map[string]error {
	res := map[string]error{}
	args := []string{"sh", "-c", ensureSwapScript, "sh"}
	var sent []SwapBox
	for _, b := range boxes {
		if !instanceName.MatchString(b.Name) || b.Pid <= 0 || b.Bytes < 0 {
			res[b.Name] = fmt.Errorf("swap: bad instance %q (pid %d)", b.Name, b.Pid)
			continue
		}
		sent = append(sent, b)
		args = append(args, b.Name, strconv.FormatInt(b.Pid, 10), strconv.FormatInt(b.Bytes, 10))
	}
	if len(sent) == 0 {
		return res
	}
	out, err := c.Run(ctx, args...)
	got := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && (f[0] == "ok" || f[0] == "err") {
			got[f[1]] = strings.Join(f[2:], " ")
			if f[0] == "ok" {
				got[f[1]] = "ok"
			}
		}
	}
	for _, b := range sent {
		switch why, ok := got[b.Name]; {
		case ok && why == "ok":
		case ok:
			res[b.Name] = fmt.Errorf("swap %s: %s", b.Name, why)
		case err != nil:
			res[b.Name] = fmt.Errorf("swap %s: %w", b.Name, err)
		default:
			res[b.Name] = fmt.Errorf("swap %s: no result", b.Name)
		}
	}
	return res
}
