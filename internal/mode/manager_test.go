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
	state   map[string]string // instance -> Running/Frozen/Stopped
	order   []string
	calls   []string
	failOn  string // a call prefix that returns an error
	project map[string]string
	config  map[string]map[string]string // instance -> its own config
}

func newFake(kv ...string) *fake {
	f := &fake{state: map[string]string{}, project: map[string]string{}, config: map[string]map[string]string{}}
	for i := 0; i < len(kv); i += 2 {
		f.add(kv[i], kv[i+1])
	}
	return f
}

func (f *fake) add(name, status string) { f.state[name] = status; f.order = append(f.order, name) }

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
		out = append(out, Instance{Name: n, Status: f.state[n], Config: cfg})
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
func (f *fake) UnsetInstanceConfig(_ context.Context, n, k string) error {
	if err := f.rec("unset %s %s", n, k); err != nil {
		return err
	}
	delete(f.config[n], k)
	return nil
}
func (f *fake) SetProjectConfig(_ context.Context, k, v string) error {
	if err := f.rec("project %s=%s", k, v); err != nil {
		return err
	}
	f.project[k] = v
	return nil
}

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func tick(m *Manager, at time.Duration, gaming bool) Status {
	return m.Tick(context.Background(), t0.Add(at), gaming, "steam:570")
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

	st := tick(m, 0, false)
	equal(t, f.take(), []string{"project user.agent-box.mode=mypc", "pause a", "pause c"})
	if st.Running != 0 || st.Err != nil {
		t.Fatalf("status %+v", st)
	}
	tick(m, 14*time.Minute, false)
	equal(t, f.take(), nil)
	tick(m, 15*time.Minute, false)
	equal(t, f.take(), []string{"stop a", "stop c"})
}

func TestMyPCPausesBoxesStartedLater(t *testing.T) {
	f := newFake("a", "Frozen")
	m := New(f, MyPC)
	tick(m, 0, false)
	f.take()
	f.add("late", "Running")
	tick(m, time.Minute, false)
	equal(t, f.take(), []string{"pause late"})
}

func TestSharedCapsCPUResumesAndIsIdempotent(t *testing.T) {
	f := newFake("a", "Frozen", "b", "Running")
	m := New(f, Shared)

	st := tick(m, 0, false)
	equal(t, f.take(), []string{
		"project user.agent-box.mode=shared",
		"set a limits.cpu=2", "set a limits.cpu.priority=0", "resume a",
		"set b limits.cpu=2", "set b limits.cpu.priority=0",
	})
	if st.Running != 2 {
		t.Fatalf("running = %d", st.Running)
	}
	tick(m, 5*time.Second, false)
	equal(t, f.take(), nil)

	f.add("new", "Running")
	tick(m, 10*time.Second, false)
	equal(t, f.take(), []string{"set new limits.cpu=2", "set new limits.cpu.priority=0"})
}

func TestNeverWritesProjectLimitsOrMemory(t *testing.T) {
	f := newFake("a", "Running")
	for _, c := range []Mode{Shared, Full, MyPC, Full} {
		m := New(f, c)
		tick(m, 0, false)
		tick(m, time.Hour, false)
	}
	for _, c := range f.take() {
		if strings.HasPrefix(c, "project ") && !strings.HasPrefix(c, "project user.agent-box.mode=") {
			t.Errorf("wrote a project limit: %s", c)
		}
		if strings.Contains(c, "limits.memory") || strings.Contains(c, "limits.instances") {
			t.Errorf("touched %s", c)
		}
	}
}

func TestFullRaisesCPUAndRemovesLegacyAllowance(t *testing.T) {
	f := newFake("a", "Frozen")
	f.config["a"] = map[string]string{"limits.cpu": "2", "limits.cpu.priority": "0", "limits.cpu.allowance": "50%"}
	m := New(f, Shared)
	tick(m, 0, false)
	equal(t, f.take(), []string{
		"project user.agent-box.mode=shared",
		"unset a limits.cpu.allowance", "resume a",
	})
	m.SetChosen(Full)
	tick(m, time.Second, false)
	equal(t, f.take(), []string{
		"project user.agent-box.mode=full",
		"set a limits.cpu=4", "set a limits.cpu.priority=10",
	})
	tick(m, 2*time.Second, false)
	equal(t, f.take(), nil)
}

func TestOnlyDifferingKeysAreSet(t *testing.T) {
	f := newFake("a", "Running", "b", "Running")
	f.config["a"] = map[string]string{"limits.cpu": "4", "limits.cpu.priority": "10"} // already right for Full
	f.config["b"] = map[string]string{"limits.cpu": "4", "limits.cpu.priority": "0"}
	m := New(f, Full)
	tick(m, 0, false)
	equal(t, f.take(), []string{"project user.agent-box.mode=full", "set b limits.cpu.priority=10"})
}

func TestReenforcesLimitsAnAgentChanged(t *testing.T) {
	f := newFake("a", "Running")
	m := New(f, Shared)
	tick(m, 0, false)
	f.take()

	f.config["a"]["limits.cpu"] = "12"            // agent raised its CPUs
	f.config["a"]["limits.cpu.allowance"] = "90%" // and added an allowance
	delete(f.config["a"], "limits.cpu.priority")  // and dropped the priority
	tick(m, time.Second, false)
	equal(t, f.take(), []string{"set a limits.cpu=2", "set a limits.cpu.priority=0", "unset a limits.cpu.allowance"})
	tick(m, 2*time.Second, false)
	equal(t, f.take(), nil)
}

func TestMyPCRepausesAnInstanceAnAgentResumed(t *testing.T) {
	f := newFake("a", "Running")
	m := New(f, MyPC)
	tick(m, 0, false)
	f.take()
	f.state["a"] = "Running" // agent resumed it
	tick(m, time.Second, false)
	equal(t, f.take(), []string{"pause a"})
}

func TestMyPCLeavesInstanceConfigAlone(t *testing.T) {
	f := newFake("a", "Running")
	f.config["a"] = map[string]string{"limits.cpu": "12", "limits.cpu.allowance": "50%"}
	m := New(f, MyPC)
	tick(m, 0, false)
	equal(t, f.take(), []string{"project user.agent-box.mode=mypc", "pause a"})
}

func TestFrozenInstanceStaysFrozenIfLimitsFail(t *testing.T) {
	f := newFake("a", "Frozen")
	f.failOn = "set a limits.cpu=2"
	m := New(f, Shared)
	st := tick(m, 0, false)
	if st.Err == nil || st.Running != 0 {
		t.Fatalf("status %+v", st)
	}
	for _, c := range f.take() {
		if strings.HasPrefix(c, "resume") {
			t.Fatalf("resumed without limits: %s", c)
		}
	}
	f.failOn = ""
	tick(m, time.Second, false)
	equal(t, f.take(), []string{"set a limits.cpu=2", "resume a"})
}

func TestStoppedInstancesStayStopped(t *testing.T) {
	f := newFake("a", "Stopped")
	m := New(f, Full)
	tick(m, 0, false)
	for _, c := range f.take() {
		if strings.HasPrefix(c, "resume") {
			t.Fatalf("resumed a stopped instance: %s", c)
		}
	}
}

func TestGamingYieldAndDebounce(t *testing.T) {
	f := newFake("a", "Running")
	m := New(f, Full)
	tick(m, 0, false)
	f.take()

	st := tick(m, 5*time.Second, true) // game starts: freeze at once
	equal(t, f.take(), []string{"project user.agent-box.mode=mypc", "pause a"})
	if !st.Yielding || st.Effective != MyPC || st.Chosen != Full || st.Reason != "steam:570" {
		t.Fatalf("status %+v", st)
	}

	tick(m, 10*time.Second, false) // alt-tab: signal clears briefly
	tick(m, 20*time.Second, true)  // and comes back
	tick(m, 25*time.Second, false)
	tick(m, 40*time.Second, false) // only 15s clear: still yielding
	equal(t, f.take(), nil)

	st = tick(m, 45*time.Second, false) // 20s clear: restore Full
	if st.Yielding || st.Effective != Full {
		t.Fatalf("status %+v", st)
	}
	equal(t, f.take(), []string{
		"project user.agent-box.mode=full", "resume a", // limits were never lost: nothing to reset
	})
}

func TestYieldWinsOverModeChangeWhileGaming(t *testing.T) {
	f := newFake("a", "Frozen")
	m := New(f, MyPC)
	tick(m, 0, true)
	f.take()
	m.SetChosen(Full)
	st := tick(m, time.Second, true)
	if st.Effective != MyPC || len(f.take()) != 0 {
		t.Fatalf("status %+v", st)
	}
}

func TestFailuresAreReportedAndRetried(t *testing.T) {
	f := newFake("a", "Running")
	f.failOn = "project user.agent-box.mode"
	m := New(f, Shared)

	st := tick(m, 0, false)
	if st.Err == nil {
		t.Fatal("want an error")
	}
	if st.Running != 1 {
		t.Fatalf("instances should still be handled, running = %d", st.Running)
	}
	f.take()
	f.failOn = ""
	st = tick(m, time.Second, false)
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	equal(t, f.take(), []string{"project user.agent-box.mode=shared"})
}

func TestListFailureKeepsLastRunningCount(t *testing.T) {
	f := newFake("a", "Running")
	m := New(f, Full)
	tick(m, 0, false)
	f.failOn = "list"
	st := tick(m, time.Second, false)
	if st.Err == nil || st.Running != 1 {
		t.Fatalf("status %+v", st)
	}
}

func TestFrozenTimersArePrunedForGoneInstances(t *testing.T) {
	f := newFake("a", "Running")
	m := New(f, MyPC)
	tick(m, 0, false)
	delete(f.state, "a")
	f.order = nil
	tick(m, time.Minute, false)
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
			return []byte(`[{"name":"a","status":"Running","type":"container"},{"name":"b","status":"Frozen","config":{"limits.cpu":"2"}}]`), nil
		}
		return nil, nil
	}
	c, ctx := CLI{Run: run}, context.Background()
	ins, err := c.List(ctx)
	if err != nil || len(ins) != 2 || ins[1].Name != "b" || ins[1].Status != "Frozen" || ins[1].Config["limits.cpu"] != "2" || len(ins[0].Config) != 0 {
		t.Fatalf("list = %v, %v", ins, err)
	}
	c.Pause(ctx, "a")
	c.Resume(ctx, "a")
	c.Stop(ctx, "a")
	c.SetInstanceConfig(ctx, "a", "limits.cpu", "2")
	c.UnsetInstanceConfig(ctx, "a", "limits.cpu.allowance")
	c.SetProjectConfig(ctx, ProjectModeKey, "full")
	want := [][]string{
		{"list", "--project", "agents", "--format", "json"},
		{"pause", "a", "--project", "agents"},
		{"start", "a", "--project", "agents"},
		{"stop", "a", "--force", "--project", "agents"},
		{"config", "set", "a", "limits.cpu=2", "--project", "agents"},
		{"config", "unset", "a", "limits.cpu.allowance", "--project", "agents"},
		{"project", "set", "agents", "user.agent-box.mode=full"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}
