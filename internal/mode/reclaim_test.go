package mode

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeReclaim records reclaims (from goroutines, so it has its own lock).
// left and fail say what each name's reclaim returns; block makes a reclaim
// wait until its context ends, like a stalled one.
type fakeReclaim struct {
	mu        sync.Mutex
	calls     []string
	closes    []string
	left      map[string]int64
	fail      map[string]bool
	block     bool
	closeErr  error
	inFlight  int
	maxFlight int
}

func (r *fakeReclaim) Reclaim(ctx context.Context, n string, pid int64) (int64, error) {
	r.mu.Lock()
	r.calls = append(r.calls, n)
	r.inFlight++
	r.maxFlight = max(r.maxFlight, r.inFlight)
	block, left, fail := r.block, r.left[n], r.fail[n]
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.inFlight--; r.mu.Unlock() }()
	if pid <= 0 {
		return 0, errors.New("no pid")
	}
	if block {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	time.Sleep(10 * time.Millisecond) // overlap with the other attempts
	if fail {
		return 0, errors.New("no swap")
	}
	return left, nil
}

func (r *fakeReclaim) CloseSwap(_ context.Context, names ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closes = append(r.closes, "close "+strings.Join(names, ","))
	return r.closeErr
}

func (r *fakeReclaim) take() (calls, closes []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	calls, closes = r.calls, r.closes
	r.calls, r.closes = nil, nil
	sort.Strings(calls)
	return calls, closes
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
	r := &fakeReclaim{left: map[string]int64{}, fail: map[string]bool{}}
	m.Reclaim = r
	return f, m, r
}

func TestMyPCReclaimsFrozenBoxesInParallelNotGPUOnes(t *testing.T) {
	f, m, r := newReclaimFake("a", "Running", "b", "Frozen", "c", "Stopped", "g", "Running")
	f.config["g"] = map[string]string{SizeKey: GPUSize}
	rtick(m, 0, false)
	if _, closes := r.take(); len(closes) != 1 || closes[0] != "close a,b,c,g" {
		t.Fatalf("start sweep %q", closes)
	}
	f.take()

	rtick(m, time.Second, true) // a game starts
	calls, _ := r.take()
	equal(t, calls, []string{"a", "b"})
	if r.maxFlight != 2 {
		t.Errorf("%d reclaims at once, want 2", r.maxFlight)
	}
	equal(t, only(f.take(), "pause "), []string{"pause a", "pause b"})
	rtick(m, 2*time.Second, true)
	if calls, _ := r.take(); calls != nil {
		t.Fatalf("reclaimed again: %q", calls)
	}

	// The game ends: swap is closed on each box before it resumes.
	rtick(m, 3*time.Second, false)
	rtick(m, 3*time.Second+YieldDebounce, false)
	_, closes := r.take()
	equal(t, closes, []string{"close a", "close b"})
	equal(t, only(f.take(), "resume "), []string{"resume a", "resume b"})

	// Another game: a new freeze, reclaimed afresh.
	rtick(m, time.Minute, true)
	calls, _ = r.take()
	equal(t, calls, []string{"a", "b"})
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
	// A stop (here: the reaper) cancels it.
	expiry(f, "a", t0)
	m.Tick(context.Background(), t0.Add(2*time.Second), true, "steam:1")
	m.rc.wait()
	if _, ok := f.state["a"]; ok {
		t.Fatal("a not reaped")
	}
}

func TestFailedReclaimRetriesThenStopsTheBox(t *testing.T) {
	f, m, r := newReclaimFake("a", "Running", "b", "Running")
	r.fail["a"] = true
	r.left["b"] = 3 << 30 // reclaim "worked" but the RAM is still there
	rtick(m, 0, false)
	r.take()
	f.take()
	rtick(m, time.Second, true)
	calls, _ := r.take()
	equal(t, calls, []string{"a", "b"})

	st := rtick(m, 2*time.Second, true) // both failed: reported, retry after 15s
	if st.Err == nil || !strings.Contains(st.Err.Error(), "no swap") || !strings.Contains(st.Err.Error(), "still holds 3GiB") {
		t.Fatalf("err %v", st.Err)
	}
	if calls, closes := r.take(); calls != nil || len(closes) != 1 || closes[0] != "close " {
		t.Fatalf("calls %q, sweep %q", calls, closes) // a failure sweeps running boxes once
	}
	rtick(m, 2*time.Second+ReclaimRetry, true)
	calls, _ = r.take()
	equal(t, calls, []string{"a", "b"})

	st = rtick(m, 20*time.Second, true) // second failure: stopped
	if st.Err == nil || !strings.Contains(st.Err.Error(), "stopping it") {
		t.Fatalf("err %v", st.Err)
	}
	equal(t, only(f.take(), "stop "), []string{"stop a", "stop b"})
	if f.state["a"] != "Stopped" || f.state["b"] != "Stopped" {
		t.Fatalf("states %v", f.state)
	}
}

func TestReclaimIsResetWhenABoxIsPausedAgain(t *testing.T) {
	f, m, r := newReclaimFake("a", "Running")
	m.SetChosen(MyPC)
	rtick(m, 0, false)
	calls, _ := r.take()
	equal(t, calls, []string{"a"})
	rtick(m, time.Second, false)
	f.state["a"] = "Running" // an agent resumes it
	rtick(m, 2*time.Second, false)
	calls, _ = r.take()
	equal(t, calls, []string{"a"})
}

func TestSwapStaysFrozenUntilClosed(t *testing.T) {
	f, m, r := newReclaimFake("a", "Running")
	rtick(m, 0, false)
	rtick(m, time.Second, true)
	r.take()
	f.take()
	r.closeErr = errors.New("wsl down")
	m.SetChosen(Shared)
	rtick(m, 2*time.Second, false)
	st := rtick(m, 2*time.Second+YieldDebounce, false)
	if st.Err == nil || f.state["a"] != "Frozen" {
		t.Fatalf("err %v, a %s", st.Err, f.state["a"])
	}
	r.closeErr = nil
	rtick(m, 30*time.Second, false)
	if f.state["a"] != "Running" {
		t.Fatalf("a %s", f.state["a"])
	}
}

func TestCgroupReclaimArgs(t *testing.T) {
	var got []string
	r := CgroupReclaim{Run: func(_ context.Context, args ...string) ([]byte, error) { got = args; return []byte("11935744\n"), nil }}
	left, err := r.Reclaim(context.Background(), "box-1", 4242)
	if err != nil || left != 11935744 {
		t.Fatalf("left %d, %v", left, err)
	}
	want := []string{"timeout", "-s", "TERM", "60", "sh", "-c", reclaimScript, "sh", "box-1", "4242"}
	equal(t, got, want)
	for _, s := range []string{"/proc/$2/cgroup", "lxc.payload.agents_$1-[0-9]*", "frozen 1", "memory.reclaim", "trap"} {
		if !strings.Contains(reclaimScript, s) {
			t.Errorf("reclaim script lacks %q", s)
		}
	}
	for _, s := range []string{reclaimScript, closeSwapScript} {
		if strings.ContainsAny(s, "\\\"") {
			t.Errorf("script has quotes or backslashes wsl.exe would mangle: %s", s)
		}
	}
	for _, bad := range []string{"../x", "a b", "", "-rf"} {
		if _, err := r.Reclaim(context.Background(), bad, 1); err == nil {
			t.Errorf("accepted %q", bad)
		}
		if r.CloseSwap(context.Background(), bad) == nil {
			t.Errorf("close accepted %q", bad)
		}
	}
	if _, err := r.Reclaim(context.Background(), "a", 0); err == nil {
		t.Error("accepted pid 0")
	}
	r.CloseSwap(context.Background(), "a", "b")
	equal(t, got, []string{"sh", "-c", closeSwapScript, "sh", "a", "b"})
}
