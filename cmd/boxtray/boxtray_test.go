package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/mode"
)

func TestConfigRoundTripAndDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "boxtray.json")
	if got := loadMode(p); got != mode.MyPC {
		t.Fatalf("missing file: %q", got)
	}
	if err := saveMode(p, mode.Full); err != nil {
		t.Fatal(err)
	}
	if got := loadMode(p); got != mode.Full {
		t.Fatalf("got %q", got)
	}
	if err := saveMode(p, "bogus"); err != nil {
		t.Fatal(err)
	}
	if got := loadMode(p); got != mode.MyPC {
		t.Fatalf("unknown mode: %q", got)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		app    uint32
		state  int
		gaming bool
		reason string
	}{
		{0, 0, false, ""},
		{0, 5, false, ""}, // accepts notifications
		{570, 5, true, "steam:570"},
		{0, 3, true, "fullscreen"},
		{0, 2, true, "busy"},
		{570, 3, true, "steam:570"},
	}
	for _, c := range cases {
		g, r := classify(c.app, c.state)
		if g != c.gaming || r != c.reason {
			t.Errorf("classify(%d,%d) = %v %q", c.app, c.state, g, r)
		}
	}
}

func TestDistroListed(t *testing.T) {
	utf16 := func(s string) []byte {
		b := []byte{0xff, 0xfe}
		for _, r := range s {
			b = append(b, byte(r), 0)
		}
		return b
	}
	if !distroListed(utf16("Ubuntu\r\nagents\r\n"), "agents") {
		t.Error("agents should be listed")
	}
	if distroListed(utf16("agents-old\r\n"), "agents") {
		t.Error("prefix match")
	}
	if distroListed(utf16("There are no running distributions.\r\n"), "agents") {
		t.Error("none running")
	}
}

func TestStatusText(t *testing.T) {
	cases := []struct {
		st   mode.Status
		want string
	}{
		{mode.Status{Chosen: mode.Full, Effective: mode.Full, Running: 2}, "Full, 2 running"},
		{mode.Status{Chosen: mode.Full, Effective: mode.MyPC, Yielding: true, Reason: "steam:570"}, "Paused for game (steam:570), Full chosen"},
		{mode.Status{Chosen: mode.Shared, Down: true}, "Shared chosen, WSL not running"},
	}
	for _, c := range cases {
		if got := statusText(c.st); got != c.want {
			t.Errorf("got %q want %q", got, c.want)
		}
	}
}

type fakeSensors struct {
	mu     sync.Mutex
	up     bool
	gaming bool
	awake  []bool
}

func (f *fakeSensors) Gaming() (bool, string)      { return f.gaming, "steam:1" }
func (f *fakeSensors) HostUp(context.Context) bool { return f.up }
func (f *fakeSensors) HoldAwake(on bool) {
	f.mu.Lock()
	f.awake = append(f.awake, on)
	f.mu.Unlock()
}

type noIncus struct{ calls int }

func (n *noIncus) List(context.Context) ([]mode.Instance, error) {
	n.calls++
	return []mode.Instance{{Name: "a", Status: "Running"}}, nil
}
func (n *noIncus) Pause(context.Context, string) error  { n.calls++; return nil }
func (n *noIncus) Resume(context.Context, string) error { n.calls++; return nil }
func (n *noIncus) Stop(context.Context, string) error   { n.calls++; return nil }
func (n *noIncus) SetInstanceConfig(context.Context, string, string, string) error {
	n.calls++
	return nil
}
func (n *noIncus) UnsetInstanceConfig(context.Context, string, string) error { n.calls++; return nil }
func (n *noIncus) SetProjectConfig(context.Context, string, string) error    { n.calls++; return nil }

func TestRunSkipsIncusWhileHostDownAndHoldsAwake(t *testing.T) {
	inc := &noIncus{}
	s := &fakeSensors{up: false}
	m := mode.New(inc, mode.Full)
	ctx, cancel := context.WithCancel(context.Background())
	wake := make(chan struct{})
	results := make(chan mode.Status, 8)
	done := make(chan struct{})
	go func() { run(ctx, m, s, time.Hour, wake, func(st mode.Status) { results <- st }); close(done) }()

	if st := <-results; !st.Down || inc.calls != 0 {
		t.Fatalf("down: %+v calls=%d", st, inc.calls)
	}
	s.mu.Lock()
	s.up = true
	s.mu.Unlock()
	wake <- struct{}{}
	if st := <-results; st.Down || st.Running != 1 || st.Effective != mode.Full {
		t.Fatalf("up: %+v", st)
	}
	cancel()
	<-done
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.awake) < 3 || !s.awake[1] || s.awake[len(s.awake)-1] {
		t.Fatalf("awake = %v", s.awake)
	}
}
