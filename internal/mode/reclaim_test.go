package mode

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeReclaim records reclaims (from goroutines, so it has its own lock).
// left, notRan and fail say what each name's reclaim returns; block makes a
// reclaim wait until its context ends, like a stalled one.
type fakeReclaim struct {
	mu        sync.Mutex
	calls     []string
	swaps     [][]SwapBox
	left      map[string]int64
	notRan    map[string]bool
	block     bool
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

func (r *fakeReclaim) EnsureSwap(_ context.Context, boxes []SwapBox) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.swaps = append(r.swaps, boxes)
	return nil
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
func rtick(m *Manager, at time.Duration, gaming bool) Status {
	st := tick(m, at, gaming)
	m.rc.wait()
	return st
}

func newReclaimFake(kv ...string) (*fake, *Manager, *fakeReclaim) {
	f := newFake(kv...)
	m := New(f, Full)
	r := &fakeReclaim{left: map[string]int64{}, notRan: map[string]bool{}}
	m.Reclaim = r
	return f, m, r
}

func TestEverySwapMaxIsTheBoxMemoryLimitEachTick(t *testing.T) {
	f, m, r := newReclaimFake("a", "Running", "b", "Frozen", "c", "Stopped")
	f.config["a"] = map[string]string{SizeKey: "small"}
	f.config["b"] = map[string]string{MemoryCapKey: "3000000000"} // not page-aligned
	rtick(m, 0, false)
	rtick(m, 5*time.Second, false)
	want := []SwapBox{{"a", 1000, 2 << 30}, {"b", 1001, 3000000000 &^ 4095}}
	if len(r.swaps) != 2 || fmt.Sprint(r.swaps[0]) != fmt.Sprint(want) || fmt.Sprint(r.swaps[1]) != fmt.Sprint(want) {
		t.Fatalf("swaps %v", r.swaps)
	}
}

func TestMyPCReclaimsFrozenBoxesInParallelNotGPUOnes(t *testing.T) {
	f, m, r := newReclaimFake("a", "Running", "b", "Frozen", "c", "Stopped", "g", "Running")
	f.config["g"] = map[string]string{SizeKey: GPUSize}
	rtick(m, 0, false)
	f.take()

	rtick(m, time.Second, true) // a game starts
	equal(t, r.take(), []string{"a/1000", "b/1001"})
	if r.maxFlight != 2 {
		t.Errorf("%d reclaims at once, want 2", r.maxFlight)
	}
	equal(t, only(f.take(), "pause "), []string{"pause a", "pause b"})
	rtick(m, 2*time.Second, true)
	equal(t, r.take(), nil)

	// The game ends: they resume with nothing to undo first.
	rtick(m, 3*time.Second, false)
	rtick(m, 3*time.Second+YieldDebounce, false)
	equal(t, only(f.take(), "resume "), []string{"resume a", "resume b"})

	// Another game: a new freeze, reclaimed afresh.
	rtick(m, time.Minute, true)
	equal(t, r.take(), []string{"a/1000", "b/1001"})
}

func TestStalledReclaimDoesntBlockTheTick(t *testing.T) {
	f, m, r := newReclaimFake("a", "Running")
	r.block = true
	m.Tick(context.Background(), t0, false, "")
	start := time.Now()
	st := m.Tick(context.Background(), t0.Add(time.Second), true, "steam:1")
	if time.Since(start) > time.Second || st.Err != nil {
		t.Fatalf("tick took %v, err %v", time.Since(start), st.Err)
	}
	if f.state["a"] != "Frozen" {
		t.Fatalf("a is %s", f.state["a"])
	}
	// The reaper deleting the box cancels its reclaim.
	expiry(f, "a", t0)
	m.Tick(context.Background(), t0.Add(2*time.Second), true, "steam:1")
	m.rc.wait()
	if _, ok := f.state["a"]; ok {
		t.Fatal("a not reaped")
	}
}

func TestOnlyReclaimsThatRanAndLeftMuchStopABox(t *testing.T) {
	f, m, r := newReclaimFake("a", "Running", "b", "Running")
	r.notRan["a"] = true  // WSL failures: retried, never stopped early
	r.left["b"] = 3 << 30 // reclaim ran but the RAM is still there
	rtick(m, 0, false)
	f.take()
	rtick(m, time.Second, true)
	equal(t, r.take(), []string{"a/1000", "b/1001"})

	st := rtick(m, 2*time.Second, true)
	if st.Err == nil || !strings.Contains(st.Err.Error(), "wsl.exe failed") || !strings.Contains(st.Err.Error(), "still holds 3GiB") {
		t.Fatalf("err %v", st.Err)
	}
	rtick(m, 2*time.Second+ReclaimRetry, true)
	equal(t, r.take(), []string{"a/1000", "b/1001"})

	st = rtick(m, 20*time.Second, true) // b's second short reclaim: stopped
	if st.Err == nil || !strings.Contains(st.Err.Error(), "stopping it") {
		t.Fatalf("err %v", st.Err)
	}
	equal(t, only(f.take(), "stop "), []string{"stop b"})
	for i := 0; i < 6; i++ { // a keeps retrying, with backoff, and stays frozen
		rtick(m, time.Duration(30+60*i)*time.Second, true)
	}
	if f.state["a"] != "Frozen" || len(r.take()) == 0 {
		t.Fatalf("a %s", f.state["a"])
	}
	// FrozenStop still stops it.
	rtick(m, time.Second+FrozenStop, true)
	if f.state["a"] != "Stopped" {
		t.Fatalf("a %s after FrozenStop", f.state["a"])
	}
}

func TestStopRechecksModeAndState(t *testing.T) {
	f := newFake("b", "Running")
	m := New(f, Full)
	r := &fakeReclaim{left: map[string]int64{"b": 3 << 30}}
	m.Reclaim = r
	rtick(m, 0, false)
	rtick(m, time.Second, true)
	rtick(m, 2*time.Second, true)
	rtick(m, 2*time.Second+ReclaimRetry, true) // second short reclaim reported next tick
	f.take()

	// The user picks Full while the game still runs: the stop goes ahead,
	// since the PC still yields. Picking it after the game: no stop.
	m.yielding = false
	m.SetChosen(Full)
	if st, err := m.stopUnreclaimed(context.Background(), Instance{Name: "b", State: &InstanceState{Pid: 1000}}, 1000, "frozen"); err != nil || st != "frozen" {
		t.Fatalf("%s, %v", st, err)
	}
	equal(t, only(f.take(), "stop "), nil)

	// Still yielding, but the box is no longer the frozen one we reclaimed.
	m.yielding = true
	f.state["b"] = "Running"
	if st, _ := m.stopUnreclaimed(context.Background(), Instance{Name: "b"}, 1000, "frozen"); st != "frozen" {
		t.Fatalf("%s", st)
	}
	f.state["b"] = "Frozen"
	if st, _ := m.stopUnreclaimed(context.Background(), Instance{Name: "b"}, 999, "frozen"); st != "frozen" {
		t.Fatalf("other pid: %s", st)
	}
	equal(t, only(f.take(), "stop "), nil)
	if st, _ := m.stopUnreclaimed(context.Background(), Instance{Name: "b"}, 1000, "frozen"); st != "stopped" {
		t.Fatalf("%s", st)
	}
}

func TestReclaimIsResetWhenABoxIsPausedAgainOrRecreated(t *testing.T) {
	f, m, r := newReclaimFake("a", "Running")
	m.SetChosen(MyPC)
	rtick(m, 0, false)
	equal(t, r.take(), []string{"a/1000"})
	rtick(m, time.Second, false)
	f.state["a"] = "Running" // an agent resumes it
	rtick(m, 2*time.Second, false)
	equal(t, r.take(), []string{"a/1000"})

	// Deleted and recreated under the same name, still frozen: a new PID.
	f.Delete(context.Background(), "a")
	f.add("x", "Running")
	f.add("a", "Frozen")
	rtick(m, 3*time.Second, false)
	equal(t, r.take(), []string{"a/1001", "x/1000"})
}

func TestReleaseDropsReclaimsAndStartsNone(t *testing.T) {
	f, m, r := newReclaimFake("a", "Running")
	r.block = true
	m.Tick(context.Background(), t0, false, "")
	m.Tick(context.Background(), t0.Add(time.Second), true, "steam:1")
	m.BeginRelease()
	m.rc.wait() // the running one was cancelled
	r.block = false
	r.take()
	f.add("b", "Running")
	rtick(m, 2*time.Second, true)
	rtick(m, 3*time.Second, true)
	if calls := r.take(); calls != nil {
		t.Fatalf("reclaims during release: %q", calls)
	}
	m.EndRelease()
	rtick(m, 4*time.Second, true)
	equal(t, r.take(), []string{"a/1000", "b/1001"})
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
		if r.EnsureSwap(context.Background(), []SwapBox{{bad, 1, 1}}) == nil {
			t.Errorf("swap accepted %q", bad)
		}
	}
	if _, _, err := r.Reclaim(context.Background(), "a", 0); err == nil {
		t.Error("accepted pid 0")
	}
	r.EnsureSwap(context.Background(), []SwapBox{{"a", 7, 2 << 30}, {"b", 8, 4 << 30}})
	equal(t, got, []string{"sh", "-c", ensureSwapScript, "sh", "a", "7", "2147483648", "b", "8", "4294967296"})
}
