package mode

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeReclaim records reclaims (from goroutines, so it has its own lock).
// left and notRan say what each name's reclaim returns; block makes a
// reclaim wait until its context ends, like a stalled one.
type fakeReclaim struct {
	mu        sync.Mutex
	calls     []string
	swaps     [][]SwapBox
	left      map[string]int64
	notRan    map[string]bool
	swapFail  map[string]bool
	block     bool
	f         *fake // if set, EnsureSwap logs "swap" to its calls
	inFlight  int
	maxFlight int
}

func (r *fakeReclaim) Reclaim(ctx context.Context, n string, pid int64) (int64, bool, error) {
	r.mu.Lock()
	r.calls = append(r.calls, fmt.Sprintf("%s/%d", n, pid))
	r.inFlight++
	r.maxFlight = max(r.maxFlight, r.inFlight)
	block, left, notRan := r.block, r.left[n], r.notRan[n]
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.inFlight--; r.mu.Unlock() }()
	if block {
		<-ctx.Done()
		return 0, false, ctx.Err()
	}
	time.Sleep(10 * time.Millisecond) // overlap with the other attempts
	if notRan {
		return 0, false, errors.New("wsl.exe failed")
	}
	return left, true, nil
}

// EnsureSwap runs inside the tick, so it can log to the fake Incus's calls,
// where its order against pauses and resumes shows.
func (r *fakeReclaim) EnsureSwap(_ context.Context, boxes []SwapBox) map[string]error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.swaps = append(r.swaps, boxes)
	if r.f != nil {
		r.f.calls = append(r.f.calls, "swap")
	}
	out := map[string]error{}
	for _, b := range boxes {
		if r.swapFail[b.Name] {
			out[b.Name] = errors.New("swap " + b.Name + ": gone")
		}
	}
	return out
}

func (r *fakeReclaim) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.calls
	r.calls = nil
	sort.Strings(c)
	return c
}

// only keeps the calls that start with one of the prefixes.
func only(calls []string, prefixes ...string) []string {
	var out []string
	for _, c := range calls {
		for _, p := range prefixes {
			if strings.HasPrefix(c, p) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// rtick ticks and waits for the reclaims it started.
func rtick(m *Manager, at time.Duration) Status {
	st := tick(m, at)
	m.rc.wait()
	return st
}

func newFakeReclaim() *fakeReclaim {
	return &fakeReclaim{left: map[string]int64{}, notRan: map[string]bool{}, swapFail: map[string]bool{}}
}

// newReclaimFake gives every box a 4GiB limits.memory, as `box` does.
func newReclaimFake(kv ...string) (*fake, *Manager, *fakeReclaim) {
	f := newFake(kv...)
	for i := 0; i < len(kv); i += 2 {
		f.setCfg(kv[i], MemoryCapKey, "4GiB")
	}
	m := New(f, Full)
	r := newFakeReclaim()
	m.Reclaim = r
	return f, m, r
}

func TestEverySwapMaxIsTheBoxMemoryLimitEachTick(t *testing.T) {
	f, m, r := newReclaimFake("a", "Running", "b", "Frozen", "c", "Stopped")
	f.config["a"][MemoryCapKey] = "2GiB"
	f.config["b"][MemoryCapKey] = "3000000000" // not page-aligned
	rtick(m, 0)
	rtick(m, 5*time.Second)
	want := []SwapBox{{"a", 1000, 2 << 30}, {"b", 1001, 3000000000 &^ 4095}}
	if len(r.swaps) != 2 || fmt.Sprint(r.swaps[0]) != fmt.Sprint(want) || fmt.Sprint(r.swaps[1]) != fmt.Sprint(want) {
		t.Fatalf("swaps %v", r.swaps)
	}
}

func TestSwapSkipsBoxesWithoutAMemoryLimit(t *testing.T) {
	f, m, r := newReclaimFake("a", "Running", "b", "Running")
	delete(f.config["b"], MemoryCapKey) // no limit, own or inherited: not the size table's
	f.config["b"][SizeKey] = "large"
	st := rtick(m, 0)
	if len(r.swaps) != 1 || len(r.swaps[0]) != 1 || r.swaps[0][0].Name != "a" {
		t.Fatalf("swaps %v", r.swaps)
	}
	if st.Err == nil || !strings.Contains(st.Err.Error(), "b has no limits.memory") {
		t.Fatalf("err %v", st.Err)
	}
}

func TestSwapResultsArePerBoxAndRetried(t *testing.T) {
	_, m, r := newReclaimFake("a", "Running", "b", "Running")
	r.swapFail["a"] = true
	st := rtick(m, 0)
	if st.Err == nil || !strings.Contains(st.Err.Error(), "swap a: gone") || strings.Contains(st.Err.Error(), "swap b") {
		t.Fatalf("err %v", st.Err)
	}
	r.swapFail["a"] = false
	if st := rtick(m, 5*time.Second); st.Err != nil {
		t.Fatalf("err %v", st.Err)
	}
	if len(r.swaps) != 2 || len(r.swaps[1]) != 2 {
		t.Fatalf("swaps %v", r.swaps)
	}
}

func TestUpkeepRunsAfterPausesStopsAndThaws(t *testing.T) {
	f, m, r := newReclaimFake("a", "Running", "g", "Running")
	f.config["g"][SizeKey] = GPUSize
	f.logCap = true
	r.f = f
	rtick(m, 0)
	f.take()
	m.SetChosen(MyPC)
	rtick(m, time.Second)
	equal(t, only(f.take(), "pause ", "stop ", "project limits.memory", "swap"),
		[]string{"pause a", "stop g", "project limits.memory=12GiB", "swap"})
	m.SetChosen(Full)
	rtick(m, 2*time.Second)
	equal(t, only(f.take(), "resume ", "project limits.memory", "swap"),
		[]string{"resume a", "project limits.memory=24GiB", "swap"})
}

func TestMyPCReclaimsFrozenBoxesInParallelNotGPUOnes(t *testing.T) {
	f, m, r := newReclaimFake("a", "Running", "b", "Frozen", "c", "Stopped", "g", "Running")
	f.config["g"][SizeKey] = GPUSize
	rtick(m, 0)
	f.take()

	m.SetChosen(MyPC)
	rtick(m, time.Second)
	equal(t, r.take(), []string{"a/1000", "b/1001"})
	if r.maxFlight != 2 {
		t.Errorf("%d reclaims at once, want 2", r.maxFlight)
	}
	equal(t, only(f.take(), "pause "), []string{"pause a", "pause b"})
	if st := rtick(m, 2*time.Second); len(st.Reclaimed) != 2 {
		t.Fatalf("reclaimed %v", st.Reclaimed)
	}
	equal(t, r.take(), nil)

	// Back to Full: they resume with nothing to undo first.
	m.SetChosen(Full)
	rtick(m, 3*time.Second)
	equal(t, only(f.take(), "resume "), []string{"resume a", "resume b"})

	// My PC again: a new freeze, reclaimed afresh.
	m.SetChosen(MyPC)
	rtick(m, time.Minute)
	equal(t, r.take(), []string{"a/1000", "b/1001"})
}

func TestStalledReclaimDoesntBlockTheTick(t *testing.T) {
	f, m, r := newReclaimFake("a", "Running")
	r.block = true
	m.Tick(context.Background(), t0)
	start := time.Now()
	m.SetChosen(MyPC)
	st := m.Tick(context.Background(), t0.Add(time.Second))
	if time.Since(start) > time.Second || st.Err != nil {
		t.Fatalf("tick took %v, err %v", time.Since(start), st.Err)
	}
	if f.state["a"] != "Frozen" {
		t.Fatalf("a is %s", f.state["a"])
	}
	// The reaper deleting the box cancels its reclaim.
	expiry(f, "a", t0)
	m.Tick(context.Background(), t0.Add(2*time.Second))
	m.rc.wait()
	if _, ok := f.state["a"]; ok {
		t.Fatal("a not reaped")
	}
}

// Reclaim is best effort: a write that happened is never repeated within a
// freeze, however much is left, and nothing stops the box for it. Only a
// write that never happened is retried. Only FrozenStop stops a CPU box.
func TestReclaimIsBestEffortAndOnlyFrozenStopStops(t *testing.T) {
	f, m, r := newReclaimFake("a", "Running", "b", "Running")
	r.notRan["a"] = true  // WSL failures: retried with backoff
	r.left["b"] = 3 << 30 // the write happened, the RAM is still there
	rtick(m, 0)
	f.take()
	m.SetChosen(MyPC)
	rtick(m, time.Second)
	equal(t, r.take(), []string{"a/1000", "b/1001"})

	st := rtick(m, 2*time.Second)
	if st.Err == nil || !strings.Contains(st.Err.Error(), "wsl.exe failed") || len(st.Reclaimed) != 1 || st.Reclaimed[0] != (Reclaimed{"b", 3 << 30}) {
		t.Fatalf("err %v, reclaimed %v", st.Err, st.Reclaimed)
	}
	rtick(m, 2*time.Second+ReclaimRetry)
	equal(t, r.take(), []string{"a/1000"})
	for i := 0; i < 12; i++ {
		rtick(m, time.Duration(30+60*i)*time.Second)
	}
	equal(t, only(f.take(), "stop "), nil)
	if f.state["a"] != "Frozen" || f.state["b"] != "Frozen" {
		t.Fatalf("states %v", f.state)
	}
	if calls := r.take(); len(calls) == 0 || only(calls, "b/") != nil {
		t.Fatalf("calls %q", calls) // a kept retrying, b never again
	}
	rtick(m, time.Second+FrozenStop)
	equal(t, only(f.take(), "stop "), []string{"stop a", "stop b"})
}

func TestReclaimIsResetWhenABoxIsPausedAgainOrRecreated(t *testing.T) {
	f, m, r := newReclaimFake("a", "Running")
	m.SetChosen(MyPC)
	rtick(m, 0) // paused; its swap.max is set at the end of this tick
	equal(t, r.take(), nil)
	rtick(m, time.Second)
	equal(t, r.take(), []string{"a/1000"})
	f.state["a"] = "Running" // an agent resumes it
	rtick(m, 2*time.Second)
	equal(t, r.take(), []string{"a/1000"})

	// Deleted and recreated under the same name, still frozen: a new PID,
	// so it waits for its own swap.max first.
	f.Delete(context.Background(), "a")
	f.add("x", "Running")
	f.add("a", "Frozen")
	f.setCfg("x", MemoryCapKey, "4GiB")
	f.setCfg("a", MemoryCapKey, "4GiB")
	rtick(m, 3*time.Second)
	equal(t, r.take(), nil)
	rtick(m, 4*time.Second)
	equal(t, r.take(), []string{"a/1001", "x/1000"})
}

// blockingList is an Incus whose first List waits for its context to end,
// like a wsl.exe call in flight when Release memory starts.
type blockingList struct {
	*fake
	entered chan struct{}
	left    chan struct{} // closed when that List returns
	lists   atomic.Int32
}

func (b *blockingList) List(ctx context.Context) ([]Instance, error) {
	if b.lists.Add(1) == 1 {
		close(b.entered)
		<-ctx.Done()
		close(b.left)
		return nil, ctx.Err()
	}
	return b.fake.List(ctx)
}

func TestReleaseCancelsATickInFlightAndBlocksNewOnes(t *testing.T) {
	f := newFake("a", "Running")
	f.setCfg("a", MemoryCapKey, "4GiB")
	inc := &blockingList{fake: f, entered: make(chan struct{}), left: make(chan struct{})}
	m := New(inc, MyPC)
	m.Reclaim = newFakeReclaim()
	done := make(chan Status, 1)
	go func() { done <- m.Tick(context.Background(), t0) }()
	<-inc.entered
	if !m.BeginRelease() {
		t.Fatal("first release refused")
	}
	// Checked on the List, not on done: Tick's result is sent after it
	// unlocks, so done may still be empty when BeginRelease returns.
	select {
	case <-inc.left: // BeginRelease waited for the cancelled tick
	default:
		t.Fatal("BeginRelease returned before the tick ended")
	}
	<-done
	if m.BeginRelease() {
		t.Fatal("a second release ran while one is held")
	}
	f.take()
	st := m.Tick(context.Background(), t0.Add(time.Second))
	if !st.Down || f.take() != nil || inc.lists.Load() != 1 {
		t.Fatalf("tick during release: %+v, lists %d", st, inc.lists.Load())
	}
	m.EndRelease()
	m.Tick(context.Background(), t0.Add(2*time.Second))
	m.rc.wait()
	if inc.lists.Load() != 2 || f.state["a"] != "Frozen" {
		t.Fatalf("after release: lists %d, a %s", inc.lists.Load(), f.state["a"])
	}
}

func TestReleaseDropsReclaimsAndStartsNone(t *testing.T) {
	f, m, r := newReclaimFake("a", "Running")
	r.mu.Lock()
	r.block = true
	r.mu.Unlock()
	m.Tick(context.Background(), t0)
	m.SetChosen(MyPC)
	m.Tick(context.Background(), t0.Add(time.Second))
	m.BeginRelease() // returns once the blocked reclaim was cancelled and ended
	r.mu.Lock()
	r.block = false
	r.mu.Unlock()
	r.take()
	f.add("b", "Running")
	f.setCfg("b", MemoryCapKey, "4GiB")
	rtick(m, 2*time.Second)
	if calls := r.take(); calls != nil {
		t.Fatalf("reclaims during release: %q", calls)
	}
	m.EndRelease()
	rtick(m, 4*time.Second) // a's swap was confirmed before; b's is now
	equal(t, r.take(), []string{"a/1000"})
	rtick(m, 5*time.Second)
	equal(t, r.take(), []string{"b/1001"})
}

func TestCgroupReclaimArgs(t *testing.T) {
	var got []string
	out := "left 11935744\n"
	r := CgroupReclaim{Run: func(_ context.Context, args ...string) ([]byte, error) { got = args; return []byte(out), nil }}
	left, ran, err := r.Reclaim(context.Background(), "box-1", 4242)
	if err != nil || !ran || left != 11935744 {
		t.Fatalf("left %d, %v, %v", left, ran, err)
	}
	equal(t, got, []string{"timeout", "60", "sh", "-c", reclaimScript, "sh", "box-1", "4242"})
	out = ""
	if _, ran, err := r.Reclaim(context.Background(), "box-1", 4242); ran || err == nil {
		t.Errorf("empty output counted as ran: %v %v", ran, err)
	}
	for _, s := range []string{"/proc/$P/cgroup", "[ x$t = xlxc.payload.agents_$1 ]", "frozen 1", "memory.reclaim"} {
		if !strings.Contains(reclaimScript, s) {
			t.Errorf("reclaim script lacks %q", s)
		}
	}
	if strings.Contains(reclaimScript, "swap.max") || strings.Contains(reclaimScript, "-[0-9]") {
		t.Error("reclaim script touches swap or accepts suffixed cgroups")
	}
	for _, s := range []string{reclaimScript, ensureSwapScript} {
		if strings.ContainsAny(s, "\\\"") {
			t.Errorf("script has quotes or backslashes wsl.exe would mangle: %s", s)
		}
	}
	for _, bad := range []string{"../x", "a b", "", "-rf"} {
		if _, _, err := r.Reclaim(context.Background(), bad, 1); err == nil {
			t.Errorf("accepted %q", bad)
		}
		if res := r.EnsureSwap(context.Background(), []SwapBox{{bad, 1, 1}}); res[bad] == nil {
			t.Errorf("swap accepted %q", bad)
		}
	}
	if _, _, err := r.Reclaim(context.Background(), "a", 0); err == nil {
		t.Error("accepted pid 0")
	}

	// Per-box results: one box gone mid-batch doesn't hide the others.
	out = "ok a\nerr b cgroup \n"
	res := r.EnsureSwap(context.Background(), []SwapBox{{"a", 7, 2 << 30}, {"b", 8, 4 << 30}, {"c", 9, 1 << 30}})
	equal(t, got, []string{"sh", "-c", ensureSwapScript, "sh", "a", "7", "2147483648", "b", "8", "4294967296", "c", "9", "1073741824"})
	if res["a"] != nil || res["b"] == nil || !strings.Contains(res["b"].Error(), "cgroup") || res["c"] == nil {
		t.Fatalf("results %v", res)
	}
}

// A box whose reclaim ran and left ReclaimLeft or less may stay frozen for
// FrozenSwappedStop; every other frozen box keeps FrozenStop.
func TestSwappedBoxesStayFrozenLonger(t *testing.T) {
	f, m, r := newReclaimFake("ok", "Running", "full", "Running", "never", "Running")
	r.left["ok"] = 10 << 20
	r.left["full"] = ReclaimLeft + 1 // no swap to go to, or swap ran out
	r.notRan["never"] = true
	rtick(m, 0)
	m.SetChosen(MyPC)
	rtick(m, time.Second) // all three paused, reclaims start
	rtick(m, 2*time.Second)
	f.take()
	rtick(m, time.Second+FrozenStop)
	equal(t, only(f.take(), "stop "), []string{"stop full", "stop never"})
	rtick(m, time.Hour)
	equal(t, only(f.take(), "stop "), nil)
	rtick(m, time.Second+FrozenSwappedStop)
	equal(t, only(f.take(), "stop "), []string{"stop ok"})
}

// The long limit belongs to one instance: a box recreated under the same
// name while frozen starts a new 15-minute timer until its own reclaim ran.
func TestFrozenLimitFollowsTheInstance(t *testing.T) {
	f, m, r := newReclaimFake("a", "Running")
	r.left["a"] = 10 << 20
	rtick(m, 0)
	m.SetChosen(MyPC)
	rtick(m, time.Second)
	rtick(m, 2*time.Second)
	if got := m.frozenLimit("a", 1000); got != FrozenSwappedStop {
		t.Fatalf("limit %v", got)
	}
	f.Delete(context.Background(), "a")
	f.add("x", "Stopped") // shifts the fake's PIDs: the new a is 1000+1
	f.add("a", "Frozen")
	f.setCfg("a", MemoryCapKey, "4GiB")
	r.notRan["a"] = true // its reclaim doesn't get to run
	rtick(m, time.Minute)
	if got := m.frozenLimit("a", 1001); got != FrozenStop {
		t.Fatalf("new instance limit %v", got)
	}
	f.take()
	rtick(m, time.Minute+FrozenStop)
	equal(t, only(f.take(), "stop "), []string{"stop a"})
}

// Incus writes memory.swap.max=0 when it starts a box, so a reclaim before
// the tray has set it would move nothing and give the box the short limit
// (seen live on 2026-11-24: 2092MiB left, stopped at 15 minutes). A box is
// reclaimed only once its swap.max is confirmed for this instance, and a box
// stopped this tick isn't sent to the swap batch.
func TestReclaimWaitsForSwapAndStoppedBoxesSkipTheBatch(t *testing.T) {
	f, m, r := newReclaimFake("a", "Frozen", "g", "Running")
	f.config["g"][SizeKey] = GPUSize
	m.SetChosen(MyPC)
	r.swapFail["a"] = true
	rtick(m, 0)
	if len(r.swaps) != 1 || len(r.swaps[0]) != 1 || r.swaps[0][0].Name != "a" {
		t.Fatalf("swaps %v (the GPU box was stopped this tick)", r.swaps)
	}
	rtick(m, time.Second) // the batch failed for a: no reclaim yet
	equal(t, r.take(), nil)
	r.swapFail["a"] = false
	rtick(m, 2*time.Second) // confirmed at the end of this tick
	equal(t, r.take(), nil)
	rtick(m, 3*time.Second)
	equal(t, r.take(), []string{"a/1000"})
}
