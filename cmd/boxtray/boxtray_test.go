package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
func (n *noIncus) Delete(context.Context, string) error { n.calls++; return nil }
func (n *noIncus) SetInstanceConfig(context.Context, string, string, string) error {
	n.calls++
	return nil
}
func (n *noIncus) UnsetInstanceConfig(context.Context, string, string) error { n.calls++; return nil }
func (n *noIncus) SetProjectConfig(context.Context, string, string) error    { n.calls++; return nil }
func (n *noIncus) ProjectConfig(context.Context, string) (string, error)     { n.calls++; return "", nil }

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

func TestLogCapKeepsNewerHalfAtLineBoundary(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "boxtray.log")
	l := openLog(p, 400)
	for i := 0; i < 50; i++ {
		l.Printf("line %02d", i)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 400 {
		t.Fatalf("log is %d bytes, cap 400", len(b))
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) < 3 || !strings.HasSuffix(lines[len(lines)-1], "line 49") {
		t.Fatalf("newest line missing: %q", lines)
	}
	for _, ln := range lines {
		if !strings.Contains(ln, " line ") || !strings.HasPrefix(ln, "20") {
			t.Fatalf("partial or untimestamped line %q", ln)
		}
	}
	if strings.HasSuffix(lines[0], "line 00") {
		t.Fatal("older lines were not dropped")
	}
}

func TestLogTrimsOversizedFileAtOpenAndNilIsSafe(t *testing.T) {
	p := filepath.Join(t.TempDir(), "boxtray.log")
	big := strings.Repeat("old line\n", 100)
	if err := os.WriteFile(p, []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	openLog(p, 300)
	b, _ := os.ReadFile(p)
	if len(b) > 150 || !strings.HasPrefix(string(b), "old line\n") {
		t.Fatalf("got %d bytes starting %q", len(b), b[:min(len(b), 12)])
	}
	var nilLog *fileLog
	nilLog.Printf("ignored")
}

// fakeClock advances only when told to.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newRetry(c *fakeClock, add func() error) (*iconRetry, *[]string) {
	var lines []string
	r := &iconRetry{add: add, now: c.now, limit: iconRetryLimit,
		logf: func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }}
	r.restart()
	return r, &lines
}

func TestIconRetrySucceedsAfterFailures(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	n := 0
	r, _ := newRetry(c, func() error {
		n++
		if n < 4 {
			return errors.New("no taskbar")
		}
		return nil
	})
	for r.try() {
		c.t = c.t.Add(iconRetryEvery)
	}
	if !r.added || r.gaveUp || r.tries != 4 {
		t.Fatalf("added=%v gaveUp=%v tries=%d", r.added, r.gaveUp, r.tries)
	}
	if r.try() || n != 4 {
		t.Fatal("kept adding after success")
	}
}

func TestIconRetryGivesUpAfterTwoMinutes(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	r, lines := newRetry(c, func() error { return errors.New("no taskbar") })
	for r.try() {
		c.t = c.t.Add(iconRetryEvery)
	}
	if !r.gaveUp || r.added {
		t.Fatalf("added=%v gaveUp=%v", r.added, r.gaveUp)
	}
	if got := c.t.Sub(time.Unix(0, 0)); got != iconRetryLimit {
		t.Fatalf("gave up at %v, want %v", got, iconRetryLimit)
	}
	if r.tries != 61 { // t=0,2,...,120s
		t.Fatalf("tries=%d", r.tries)
	}
	if last := (*lines)[len(*lines)-1]; !strings.Contains(last, "giving up") || !strings.Contains(last, "without it") {
		t.Fatalf("last log line %q", last)
	}
}

func TestIconRetryRestartAfterExplorerRestart(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	fail := false
	r, _ := newRetry(c, func() error {
		if fail {
			return errors.New("no taskbar")
		}
		return nil
	})
	r.try()
	if !r.added {
		t.Fatal("first add failed")
	}
	c.t = c.t.Add(time.Hour)
	fail = true
	r.restart() // TaskbarCreated
	if r.added || !r.try() {
		t.Fatal("restart should retry after a failure")
	}
	fail = false
	c.t = c.t.Add(iconRetryEvery)
	if r.try() || !r.added {
		t.Fatal("should have added on the retry")
	}
}

func TestStatusLogOnlyLogsChanges(t *testing.T) {
	var lines []string
	s := &statusLog{logf: func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }}
	st := mode.Status{Chosen: mode.Shared, Effective: mode.Shared}
	s.observe(st)
	s.observe(st)
	s.observe(st)
	if len(lines) != 1 {
		t.Fatalf("steady state logged %q", lines)
	}
	y := st
	y.Yielding, y.Reason, y.Effective = true, "steam:570", mode.MyPC
	s.observe(y)
	s.observe(y)
	s.observe(st)
	c := st
	c.Chosen = mode.Full
	s.observe(c)
	d := c
	d.Down = true
	d.Err = errors.New("boom")
	s.observe(d)
	s.observe(d)
	r := d
	r.Reclaimed = []mode.Reclaimed{{Name: "a", Left: 11 << 20}}
	s.observe(r)
	joined := strings.Join(lines, "|")
	for _, want := range []string{"yielding to a game (steam:570)", "game ended", "mode chosen: ", "not running", "tick error: boom", "reclaimed a: 11MiB left in RAM"} {
		if strings.Count(joined, want) != 1 {
			t.Fatalf("want one %q in %q", want, joined)
		}
	}
}
