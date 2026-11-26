package mode

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fake is an in-memory Incus. Calls are logged as "verb args".
type fake struct {
	state    map[string]string // instance -> Running/Frozen/Stopped
	lastUsed map[string]time.Time
	order    []string
	calls    []string
	failOn   string // a call prefix that returns an error
	project  map[string]string
	config   map[string]map[string]string // instance -> its own config
	logCap   bool                         // log project limits.memory writes (only cap tests want them)
}

func newFake(kv ...string) *fake {
	// The project starts at the Shared/My PC budget, so only Full writes the cap.
	f := &fake{
		state:    map[string]string{},
		lastUsed: map[string]time.Time{},
		project:  map[string]string{MemoryCapKey: "12GiB"},
		config:   map[string]map[string]string{},
	}
	for i := 0; i < len(kv); i += 2 {
		f.add(kv[i], kv[i+1])
	}
	return f
}

func (f *fake) add(name, status string) {
	f.state[name] = status
	if status != "Stopped" {
		f.lastUsed[name] = t0
	}
	f.order = append(f.order, name)
}

func (f *fake) rec(format string, a ...any) error {
	c := fmt.Sprintf(format, a...)
	f.calls = append(f.calls, c)
	if f.failOn != "" && strings.HasPrefix(c, f.failOn) {
		return errors.New("boom")
	}
	return nil
}

func (f *fake) take() []string { c := f.calls; f.calls = nil; return c }

func (f *fake) List(context.Context) ([]Instance, error) {
	if f.failOn == "list" {
		return nil, errors.New("down")
	}
	var out []Instance
	for _, n := range f.order {
		cfg := map[string]string{}
		for k, v := range f.config[n] {
			cfg[k] = v
		}
		in := Instance{Name: n, Status: f.state[n], LastUsedAt: f.lastUsed[n], Config: cfg}
		if f.state[n] != "Stopped" {
			in.State = &InstanceState{Pid: int64(1000 + len(out))}
		}
		out = append(out, in)
	}
	return out, nil
}
func (f *fake) Pause(_ context.Context, n string) error {
	if err := f.rec("pause %s", n); err != nil {
		return err
	}
	f.state[n] = "Frozen"
	return nil
}
func (f *fake) Resume(_ context.Context, n string) error {
	if err := f.rec("resume %s", n); err != nil {
		return err
	}
	f.state[n] = "Running"
	return nil
}
func (f *fake) Stop(_ context.Context, n string) error {
	if err := f.rec("stop %s", n); err != nil {
		return err
	}
	f.state[n] = "Stopped"
	return nil
}
func (f *fake) Delete(_ context.Context, n string) error {
	if err := f.rec("delete %s", n); err != nil {
		return err
	}
	delete(f.state, n)
	delete(f.lastUsed, n)
	delete(f.config, n)
	for i, o := range f.order {
		if o == n {
			f.order = append(f.order[:i:i], f.order[i+1:]...)
			break
		}
	}
	return nil
}
func (f *fake) SetInstanceConfig(_ context.Context, n, k, v string) error {
	if err := f.rec("set %s %s=%s", n, k, v); err != nil {
		return err
	}
	f.setCfg(n, k, v)
	return nil
}

func (f *fake) setCfg(n, k, v string) {
	if f.config[n] == nil {
		f.config[n] = map[string]string{}
	}
	f.config[n][k] = v
}
func (f *fake) ProjectConfig(_ context.Context, k string) (string, error) {
	if f.failOn == "project get" {
		return "", errors.New("boom")
	}
	return f.project[k], nil
}
func (f *fake) SetProjectConfig(_ context.Context, k, v string) error {
	if k == MemoryCapKey && !f.logCap {
		f.project[k] = v
		return nil
	}
	if err := f.rec("project %s=%s", k, v); err != nil {
		return err
	}
	f.project[k] = v
	return nil
}

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func tick(m *Manager, at time.Duration) Status {
	return m.Tick(context.Background(), t0.Add(at))
}

func equal(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls:\n got  %q\n want %q", got, want)
	}
}

func TestMyPCPausesRunningThenStopsAfter15Minutes(t *testing.T) {
	f := newFake("a", "Running", "b", "Stopped", "c", "Running")
	m := New(f, MyPC)

	st := tick(m, 0)
	equal(t, f.take(), []string{"project user.agent-box.mode=mypc", "pause a", "pause c"})
	if st.Running != 0 || st.Err != nil {
		t.Fatalf("status %+v", st)
	}
	tick(m, 14*time.Minute)
	equal(t, f.take(), nil)
	tick(m, 15*time.Minute)
	equal(t, f.take(), []string{"stop a", "stop c"})
}

func TestMyPCPausesBoxesStartedLater(t *testing.T) {
	f := newFake("a", "Frozen")
	m := New(f, MyPC)
	tick(m, 0)
	f.take()
	f.add("late", "Running")
	tick(m, time.Minute)
	equal(t, f.take(), []string{"pause late"})
}

func TestSharedCapsCPUResumesAndIsIdempotent(t *testing.T) {
	f := newFake("a", "Frozen", "b", "Running")
	m := New(f, Shared)

	st := tick(m, 0)
	equal(t, f.take(), []string{
		"project user.agent-box.mode=shared",
		"set a limits.cpu=2", "set a limits.cpu.priority=0", "resume a",
		"set b limits.cpu=2", "set b limits.cpu.priority=0",
	})
	if st.Running != 2 {
		t.Fatalf("running = %d", st.Running)
	}
	tick(m, 5*time.Second)
	equal(t, f.take(), nil)

	f.add("new", "Running")
	tick(m, 10*time.Second)
	equal(t, f.take(), []string{"set new limits.cpu=2", "set new limits.cpu.priority=0"})
}

func TestNeverWritesOtherProjectLimitsOrBoxMemory(t *testing.T) {
	f := newFake("a", "Running")
	f.logCap = true
	for _, c := range []Mode{Shared, Full, MyPC, Full} {
		m := New(f, c)
		tick(m, 0)
		tick(m, time.Hour)
	}
	for _, c := range f.take() {
		if strings.HasPrefix(c, "project ") && !strings.HasPrefix(c, "project user.agent-box.mode=") &&
			!strings.HasPrefix(c, "project limits.memory=") {
			t.Errorf("wrote a project limit: %s", c)
		}
		if strings.HasPrefix(c, "set ") && strings.Contains(c, "limits.memory") || strings.Contains(c, "limits.instances") {
			t.Errorf("touched %s", c)
		}
	}
}

// memSets is the project limits.memory writes among calls.
func memSets(calls []string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, "project limits.memory=") {
			out = append(out, c)
		}
	}
	return out
}

func TestMemoryCapFollowsModeAndOnlyChanges(t *testing.T) {
	f := newFake("a", "Running")
	f.logCap = true
	m := New(f, Full)
	tick(m, 0)
	equal(t, memSets(f.take()), []string{"project limits.memory=24GiB"})
	tick(m, 5*time.Second)
	equal(t, memSets(f.take()), nil) // already right: no write
	m.SetChosen(Shared)
	tick(m, 10*time.Second)
	equal(t, memSets(f.take()), []string{"project limits.memory=12GiB"})
	m.SetChosen(Full)
	tick(m, 15*time.Second)
	f.take()
	m.SetChosen(MyPC)
	tick(m, 20*time.Second) // My PC's budget
	equal(t, memSets(f.take()), []string{"project limits.memory=12GiB"})

	f.project[MemoryCapKey] = "12884901888" // the same value, written as bytes
	tick(m, 25*time.Second)
	equal(t, memSets(f.take()), nil)
}

func TestMemoryCapNeverBelowWhatBoxesHoldAndTightensAsTheyGo(t *testing.T) {
	f := newFake("big", "Stopped", "mid", "Running", "small", "Running")
	f.logCap = true
	f.config["big"] = map[string]string{SizeKey: "large", MemoryCapKey: "8GiB"}
	f.config["mid"] = map[string]string{} // no key: medium's 4GiB
	f.config["small"] = map[string]string{SizeKey: "small", MemoryCapKey: "2GiB"}
	f.project[MemoryCapKey] = "24GiB"
	m := New(f, Full)
	tick(m, 0)
	equal(t, memSets(f.take()), nil)

	f.add("more", "Running")
	f.config["more"] = map[string]string{SizeKey: "large", MemoryCapKey: "8GiB"}
	m.SetChosen(Shared) // 22GiB launched, more than Shared's 12GiB
	tick(m, 5*time.Second)
	equal(t, memSets(f.take()), []string{"project limits.memory=22GiB"})

	f.Delete(context.Background(), "big")
	f.take()
	tick(m, 10*time.Second)
	equal(t, memSets(f.take()), []string{"project limits.memory=14GiB"})
	f.Delete(context.Background(), "more")
	f.take()
	tick(m, 15*time.Second)
	equal(t, memSets(f.take()), []string{"project limits.memory=12GiB"})
}

func TestAllocatedMemoryPrefersExpandedConfig(t *testing.T) {
	ins := []Instance{
		{Name: "a", Config: map[string]string{}, ExpandedConfig: map[string]string{MemoryCapKey: "4GiB"}},
		{Name: "b", Config: map[string]string{MemoryCapKey: "2GiB"}},
		{Name: "c", Config: map[string]string{SizeKey: "large"}},
		{Name: "d", Config: map[string]string{MemoryCapKey: "1GiB"}, ExpandedConfig: map[string]string{MemoryCapKey: "512MiB"}},
	}
	if got, want := AllocatedMemory(ins), int64(4<<30+2<<30+8<<30+512<<20); got != want {
		t.Fatalf("allocated %d, want %d", got, want)
	}
	for n, s := range map[int64]string{24 << 30: "24GiB", 512 << 20: "512MiB", 1000: "1000", 0: "0"} {
		if FormatBytes(n) != s {
			t.Errorf("FormatBytes(%d) = %s", n, FormatBytes(n))
		}
	}
}

func TestMemoryCapReadFailureIsReported(t *testing.T) {
	f := newFake("a", "Running")
	f.failOn = "project get"
	st := tick(New(f, Full), 0)
	if st.Err == nil || memSets(f.take()) != nil {
		t.Fatalf("err %v", st.Err)
	}
}

func TestFullRaisesCPU(t *testing.T) {
	f := newFake("a", "Frozen")
	f.config["a"] = map[string]string{"limits.cpu": "2", "limits.cpu.priority": "0"}
	m := New(f, Shared)
	tick(m, 0)
	equal(t, f.take(), []string{
		"project user.agent-box.mode=shared",
		"resume a",
	})
	m.SetChosen(Full)
	tick(m, time.Second)
	equal(t, f.take(), []string{
		"project user.agent-box.mode=full",
		"set a limits.cpu=4", "set a limits.cpu.priority=10",
	})
	tick(m, 2*time.Second)
	equal(t, f.take(), nil)
}

func TestOnlyDifferingKeysAreSet(t *testing.T) {
	f := newFake("a", "Running", "b", "Running")
	f.config["a"] = map[string]string{"limits.cpu": "4", "limits.cpu.priority": "10"} // already right for Full
	f.config["b"] = map[string]string{"limits.cpu": "4", "limits.cpu.priority": "0"}
	m := New(f, Full)
	tick(m, 0)
	equal(t, f.take(), []string{"project user.agent-box.mode=full", "set b limits.cpu.priority=10"})
}

func TestReenforcesLimitsAnAgentChanged(t *testing.T) {
	f := newFake("a", "Running")
	m := New(f, Shared)
	tick(m, 0)
	f.take()

	f.config["a"]["limits.cpu"] = "12"           // agent raised its CPUs
	delete(f.config["a"], "limits.cpu.priority") // and dropped the priority
	tick(m, time.Second)
	equal(t, f.take(), []string{"set a limits.cpu=2", "set a limits.cpu.priority=0"})
	tick(m, 2*time.Second)
	equal(t, f.take(), nil)
}

func TestMyPCRepausesAnInstanceAnAgentResumed(t *testing.T) {
	f := newFake("a", "Running")
	m := New(f, MyPC)
	tick(m, 0)
	f.take()
	f.state["a"] = "Running" // agent resumed it
	tick(m, time.Second)
	equal(t, f.take(), []string{"pause a"})
}

func TestMyPCLeavesInstanceConfigAlone(t *testing.T) {
	f := newFake("a", "Running")
	f.config["a"] = map[string]string{"limits.cpu": "12"}
	m := New(f, MyPC)
	tick(m, 0)
	equal(t, f.take(), []string{"project user.agent-box.mode=mypc", "pause a"})
}

func TestFrozenInstanceStaysFrozenIfLimitsFail(t *testing.T) {
	f := newFake("a", "Frozen")
	f.failOn = "set a limits.cpu=2"
	m := New(f, Shared)
	st := tick(m, 0)
	if st.Err == nil || st.Running != 0 {
		t.Fatalf("status %+v", st)
	}
	for _, c := range f.take() {
		if strings.HasPrefix(c, "resume") {
			t.Fatalf("resumed without limits: %s", c)
		}
	}
	f.failOn = ""
	tick(m, time.Second)
	equal(t, f.take(), []string{"set a limits.cpu=2", "resume a"})
}

func TestStoppedInstancesStayStopped(t *testing.T) {
	f := newFake("a", "Stopped")
	f.lastUsed["a"] = t0
	m := New(f, Full)
	tick(m, 0)
	for _, c := range f.take() {
		if strings.HasPrefix(c, "resume") {
			t.Fatalf("resumed a stopped instance: %s", c)
		}
	}
}

func TestThawSkipsNeverStartedStoppedInstance(t *testing.T) {
	// Incus CreateInternal inserts a new instance row as Stopped with
	// last_used_at=1970-01-01T00:00:00Z while EnsureImage unpacks the image.
	// Neither that epoch value nor a zero time may trigger config writes.
	for _, md := range []Mode{Shared, Full} {
		t.Run(string(md), func(t *testing.T) {
			f := newFake("creating", "Stopped", "zero", "Stopped")
			f.lastUsed["creating"] = time.Unix(0, 0).UTC()
			f.config["creating"] = map[string]string{"limits.cpu": "8", "limits.cpu.priority": "0"}
			m := New(f, md)

			tick(m, 0)
			equal(t, f.take(), []string{"project user.agent-box.mode=" + string(md)})

			// Once launch finishes and the container starts, limits are enforced.
			f.state["creating"] = "Running"
			f.lastUsed["creating"] = t0.Add(10 * time.Second)
			tick(m, 10*time.Second)
			want := []string{"set creating limits.cpu=2"} // an agent's 8 CPUs, back to Shared's
			if md == Full {
				want = []string{"set creating limits.cpu=4", "set creating limits.cpu.priority=10"}
			}
			equal(t, f.take(), want)
		})
	}
}

func TestThawUpdatesLimitsOnPreviouslyRunStoppedInstance(t *testing.T) {
	// A box the tray stopped after FrozenStop still gets its limits updated
	// when the mode changes back, without resuming, so a later start inherits
	// the current mode's limits.
	f := newFake("a", "Running", "old", "Stopped")
	f.lastUsed["old"] = t0.Add(-time.Hour)
	f.config["old"] = map[string]string{"limits.cpu": "4", "limits.cpu.priority": "10"}
	m := New(f, Full)

	tick(m, 0)
	equal(t, f.take(), []string{
		"project user.agent-box.mode=full",
		"set a limits.cpu=4", "set a limits.cpu.priority=10",
	})

	// Freeze and stop "a" in My PC.
	m.SetChosen(MyPC)
	tick(m, time.Second)
	f.take()
	tick(m, time.Second+FrozenStop)
	equal(t, f.take(), []string{"stop a"})
	if f.state["a"] != "Stopped" {
		t.Fatalf("a state = %q, want Stopped", f.state["a"])
	}

	// Switch to Shared: both previously-run stopped boxes get Shared limits
	// and stay stopped.
	m.SetChosen(Shared)
	tick(m, time.Second+FrozenStop+time.Second)
	equal(t, f.take(), []string{
		"project user.agent-box.mode=shared",
		"set a limits.cpu=2", "set a limits.cpu.priority=0",
		"set old limits.cpu=2", "set old limits.cpu.priority=0",
	})
	if f.state["a"] != "Stopped" || f.state["old"] != "Stopped" {
		t.Fatalf("states = %q, %q; want both Stopped", f.state["a"], f.state["old"])
	}
}

func TestFailuresAreReportedAndRetried(t *testing.T) {
	f := newFake("a", "Running")
	f.failOn = "project user.agent-box.mode"
	m := New(f, Shared)

	st := tick(m, 0)
	if st.Err == nil {
		t.Fatal("want an error")
	}
	if st.Running != 1 {
		t.Fatalf("instances should still be handled, running = %d", st.Running)
	}
	f.take()
	f.failOn = ""
	st = tick(m, time.Second)
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	equal(t, f.take(), []string{"project user.agent-box.mode=shared"})
}

func TestListFailureKeepsLastRunningCount(t *testing.T) {
	f := newFake("a", "Running")
	m := New(f, Full)
	tick(m, 0)
	f.failOn = "list"
	st := tick(m, time.Second)
	if st.Err == nil || st.Running != 1 {
		t.Fatalf("status %+v", st)
	}
}

func TestFrozenTimersArePrunedForGoneInstances(t *testing.T) {
	f := newFake("a", "Running")
	m := New(f, MyPC)
	tick(m, 0)
	delete(f.state, "a")
	f.order = nil
	tick(m, time.Minute)
	if len(m.frozenSince) != 0 {
		t.Fatalf("frozenSince = %v", m.frozenSince)
	}
}

func TestParse(t *testing.T) {
	if m, ok := Parse("full"); !ok || m != Full {
		t.Fatal("full")
	}
	if _, ok := Parse("nope"); ok {
		t.Fatal("nope")
	}
}

func TestCLIArgs(t *testing.T) {
	var got [][]string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		got = append(got, args)
		if args[0] == "list" {
			return []byte(`[{"name":"a","status":"Running","type":"container","last_used_at":"2026-11-25T06:50:46Z"},{"name":"b","status":"Frozen","last_used_at":"1970-01-01T00:00:00Z","config":{"limits.cpu":"2"}}]`), nil
		}
		return nil, nil
	}
	c, ctx := CLI{Run: run}, context.Background()
	ins, err := c.List(ctx)
	if err != nil || len(ins) != 2 || ins[1].Name != "b" || ins[1].Status != "Frozen" || ins[1].Config["limits.cpu"] != "2" || len(ins[0].Config) != 0 || ins[0].LastUsedAt.Unix() <= 0 || ins[1].LastUsedAt.Unix() > 0 {
		t.Fatalf("list = %v, %v", ins, err)
	}
	c.Pause(ctx, "a")
	c.Resume(ctx, "a")
	c.Stop(ctx, "a")
	c.Delete(ctx, "a")
	c.SetInstanceConfig(ctx, "a", "limits.cpu", "2")
	c.SetProjectConfig(ctx, ProjectModeKey, "full")
	c.ProjectConfig(ctx, MemoryCapKey)
	want := [][]string{
		{"list", "--project", "agents", "--format", "json"},
		{"pause", "a", "--project", "agents"},
		{"start", "a", "--project", "agents"},
		{"stop", "a", "--force", "--project", "agents"},
		{"delete", "a", "--force", "--project", "agents"},
		{"config", "set", "a", "limits.cpu=2", "--project", "agents"},
		{"project", "set", "agents", "user.agent-box.mode=full"},
		{"project", "get", "agents", "limits.memory"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func expiry(f *fake, name string, at time.Time) { f.setCfg(name, ExpiresKey, at.Format(time.RFC3339)) }

func TestReaperDeletesOnlyExpiredInEveryMode(t *testing.T) {
	for _, md := range []Mode{MyPC, Shared, Full} {
		f := newFake("old", "Running", "fresh", "Running", "none", "Running", "gone", "Stopped")
		expiry(f, "old", t0.Add(-time.Second))
		expiry(f, "fresh", t0.Add(time.Hour))
		expiry(f, "gone", t0.Add(-24*time.Hour))
		m := New(f, md)
		var logged []string
		m.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
		st := tick(m, 0)
		if st.Err != nil {
			t.Fatalf("%s: err %v", md, st.Err)
		}
		equal(t, logged, []string{
			"reaped old (expired " + t0.Add(-time.Second).Format(time.RFC3339) + ")",
			"reaped gone (expired " + t0.Add(-24*time.Hour).Format(time.RFC3339) + ")",
		})
		var dels []string
		for _, c := range f.calls {
			if strings.HasPrefix(c, "delete ") {
				dels = append(dels, c)
			}
		}
		equal(t, dels, []string{"delete old", "delete gone"})
		for _, c := range f.calls {
			if strings.HasSuffix(c, " old") && !strings.HasPrefix(c, "delete") {
				t.Fatalf("%s: touched the deleted instance after deleting it: %q", md, f.calls)
			}
		}
	}
}

// A person's extend (HoldKey) outlives an expiry a job's end shortened: the
// reaper waits for both (#198).
func TestReaperWaitsForTheHold(t *testing.T) {
	f := newFake("held", "Running", "done", "Running")
	expiry(f, "held", t0.Add(-time.Minute))
	f.setCfg("held", HoldKey, t0.Add(time.Hour).Format(time.RFC3339))
	expiry(f, "done", t0.Add(-time.Minute))
	f.setCfg("done", HoldKey, t0.Add(-time.Second).Format(time.RFC3339))
	tick(New(f, Shared), 0)
	var dels []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "delete ") {
			dels = append(dels, c)
		}
	}
	equal(t, dels, []string{"delete done"})
}

func TestHeld(t *testing.T) {
	cfg := map[string]string{HoldKey: t0.Format(time.RFC3339)}
	if got := Held(cfg, t0.Add(-time.Hour)); !got.Equal(t0) {
		t.Errorf("Held before the hold = %v", got)
	}
	if got := Held(cfg, t0.Add(time.Hour)); !got.Equal(t0.Add(time.Hour)) {
		t.Errorf("Held after the hold = %v", got)
	}
	if got := Held(map[string]string{HoldKey: "soon"}, t0); !got.Equal(t0) {
		t.Errorf("Held with a bad hold = %v", got)
	}
}

func TestReaperKeepsInstanceExpiringExactlyNow(t *testing.T) {
	f := newFake("a", "Running")
	expiry(f, "a", t0)
	tick(New(f, Shared), 0)
	for _, c := range f.calls {
		if strings.HasPrefix(c, "delete") {
			t.Fatalf("deleted at the exact expiry time: %q", f.calls)
		}
	}
}

func TestReaperReportsBadValueOnceAndSkips(t *testing.T) {
	f := newFake("a", "Running")
	f.setCfg("a", ExpiresKey, "tomorrow")
	m := New(f, Shared)
	st := tick(m, 0)
	if st.Err == nil || !strings.Contains(st.Err.Error(), "tomorrow") {
		t.Fatalf("first tick should report the bad value: %v", st.Err)
	}
	if st.Running != 1 {
		t.Fatalf("running = %d", st.Running)
	}
	if st = tick(m, time.Minute); st.Err != nil {
		t.Fatalf("second tick repeated the report: %v", st.Err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "delete") {
			t.Fatalf("deleted an instance with a bad value: %q", f.calls)
		}
	}
	// A different bad value is news; so is the same one on a new instance.
	f.setCfg("a", ExpiresKey, "next week")
	if st = tick(m, 2*time.Minute); st.Err == nil {
		t.Fatal("changed bad value should be reported")
	}
	// Once the instance is gone and a new one has the same bad value, report again.
	delete(f.config, "a")
	tick(m, 3*time.Minute)
	f.setCfg("a", ExpiresKey, "next week")
	if st = tick(m, 4*time.Minute); st.Err == nil {
		t.Fatal("bad value still reported once per instance lifetime")
	}
}

func TestReaperRetriesFailedDelete(t *testing.T) {
	f := newFake("a", "Running")
	expiry(f, "a", t0.Add(-time.Minute))
	m := New(f, Full)
	f.failOn = "delete"
	st := tick(m, 0)
	if st.Err == nil || !strings.Contains(st.Err.Error(), "boom") {
		t.Fatalf("delete failure not reported: %v", st.Err)
	}
	if st.Running != 1 {
		t.Fatalf("an instance that could not be deleted still runs: %d", st.Running)
	}
	f.failOn = ""
	f.take()
	st = tick(m, 10*time.Second)
	if st.Err != nil {
		t.Fatalf("retry: %v", st.Err)
	}
	if got := f.take(); !reflect.DeepEqual(got, []string{"delete a"}) {
		t.Fatalf("retry calls %q", got)
	}
	if st.Running != 0 {
		t.Fatalf("running after reap = %d", st.Running)
	}
}

func TestCPUsFollowEachBoxSize(t *testing.T) {
	f := newFake("s", "Frozen", "m", "Frozen", "l", "Frozen", "old", "Frozen", "odd", "Frozen")
	f.config["s"] = map[string]string{SizeKey: "small"}
	f.config["m"] = map[string]string{SizeKey: "medium"}
	f.config["l"] = map[string]string{SizeKey: "large"}
	f.config["odd"] = map[string]string{SizeKey: "huge"} // unknown sizes count as medium
	m := New(f, Shared)
	tick(m, 0)
	equal(t, f.take(), []string{
		"project user.agent-box.mode=shared",
		"set s limits.cpu=1", "set s limits.cpu.priority=0", "resume s",
		"set m limits.cpu=2", "set m limits.cpu.priority=0", "resume m",
		"set l limits.cpu=4", "set l limits.cpu.priority=0", "resume l",
		"set old limits.cpu=2", "set old limits.cpu.priority=0", "resume old",
		"set odd limits.cpu=2", "set odd limits.cpu.priority=0", "resume odd",
	})
	m.SetChosen(Full)
	tick(m, time.Second)
	equal(t, f.take(), []string{
		"project user.agent-box.mode=full",
		"set s limits.cpu=2", "set s limits.cpu.priority=10",
		"set m limits.cpu=4", "set m limits.cpu.priority=10",
		"set l limits.cpu=8", "set l limits.cpu.priority=10",
		"set old limits.cpu=4", "set old limits.cpu.priority=10",
		"set odd limits.cpu=4", "set odd limits.cpu.priority=10",
	})
	tick(m, 2*time.Second)
	equal(t, f.take(), nil)

	f.config["l"]["limits.cpu"] = "12" // an agent raised a large box's CPUs
	tick(m, 3*time.Second)
	equal(t, f.take(), []string{"set l limits.cpu=8"})

	m.SetChosen(MyPC)
	tick(m, 4*time.Second)
	for _, c := range f.take() {
		if strings.HasPrefix(c, "set ") {
			t.Errorf("My PC changed a limit: %s", c)
		}
	}
}

func TestSizeTable(t *testing.T) {
	want := map[string][4]string{
		"small":  {"1", "2", "2GiB", "18GiB"},
		"medium": {"2", "4", "4GiB", "30GiB"},
		"large":  {"4", "8", "8GiB", "40GiB"},
		"gpu":    {"4", "6", "8GiB", "40GiB"},
	}
	if len(Sizes) != len(want) {
		t.Fatalf("%d sizes", len(Sizes))
	}
	for _, s := range Sizes {
		got := [4]string{fmt.Sprint(s.CPUs(Shared)), fmt.Sprint(s.CPUs(Full)), s.Memory, s.Disk}
		if got != want[s.Name] || s.CPUs(MyPC) != 0 {
			t.Errorf("%s = %v", s.Name, got)
		}
	}
	if SizeOf(nil).Name != DefaultSize || DefaultSize != "medium" {
		t.Errorf("default size = %s", SizeOf(nil).Name)
	}
	for in, n := range map[string]int64{"4GiB": 4 << 30, "512MiB": 512 << 20, "2GB": 2e9, "1024": 1024, " 12GiB\n": 12 << 30, "2PiB": 2 << 50, "512 bytes": 512} {
		if got, ok := ParseBytes(in); !ok || got != n {
			t.Errorf("ParseBytes(%q) = %d, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "50%", "-1GiB", "lots", "8EiB"} {
		if _, ok := ParseBytes(in); ok {
			t.Errorf("ParseBytes(%q) ok", in)
		}
	}
}

// Another enforcer may overwrite the label during a hand-over; a minute later
// this one reads it back and puts its own value back, with no write otherwise.
func TestModeLabelIsReadBackAndRepaired(t *testing.T) {
	f := newFake()
	m := New(f, Full)
	tick(m, 0)
	equal(t, f.take(), []string{"project user.agent-box.mode=full"})
	tick(m, ProjectRecheck)
	equal(t, f.take(), nil) // read back, unchanged: no write
	f.project[ProjectModeKey] = "mypc"
	tick(m, ProjectRecheck+ProjectRecheck/2)
	equal(t, f.take(), nil) // not due yet
	tick(m, 2*ProjectRecheck)
	equal(t, f.take(), []string{"project user.agent-box.mode=full"})
}
