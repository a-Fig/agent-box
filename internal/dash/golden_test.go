package dash_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/dash/fixture"
)

var update = flag.Bool("update", false, "rewrite testdata/*.json from the fixture scenarios")

// goldenNow is the moment every golden scenario is built at.
var goldenNow = time.Date(2026, 11, 24, 17, 30, 0, 0, time.UTC)

// The fixtures in testdata are what boxd and the page are built against, so
// they must come from the types: this test fails when the code would write
// something else. Run it with -update to write them again.
func TestGoldenFixtures(t *testing.T) {
	for _, name := range fixture.Names() {
		t.Run(name, func(t *testing.T) {
			s, err := fixture.Build(name, goldenNow)
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.MarshalIndent(s, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			path := filepath.Join("testdata", name+".json")
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v; run go test ./internal/dash -run Golden -update", err)
			}
			if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), got) {
				t.Errorf("%s is out of date; run go test ./internal/dash -run Golden -update", path)
			}
		})
	}
}

// Each scenario shows what its name says, so a change to the builders can't
// quietly empty one.
func TestScenariosShowTheirCase(t *testing.T) {
	get := func(name string) fixture.Scenario {
		s, err := fixture.Build(name, goldenNow)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	count := func(js []dash.Job, state string) int {
		n := 0
		for _, j := range js {
			if j.State == state {
				n++
			}
		}
		return n
	}
	if s := get("empty"); len(s.State.Boxes) != 0 || len(s.State.Jobs) != 0 {
		t.Error("empty has boxes or jobs")
	}
	if s := get("busy"); len(s.State.Boxes) != 3 || count(s.State.Jobs, dash.JobRunning) != 2 {
		t.Errorf("busy has %d boxes and %d running jobs, want 3 and 2", len(s.State.Boxes), count(s.State.Jobs, dash.JobRunning))
	}
	if s := get("mypc"); s.State.Machines[0].Mode != dash.ModeMyPC || count(s.State.Jobs, dash.JobPaused) != 1 {
		t.Errorf("mypc: %+v", s.State.Machines[0])
	}
	full := get("full")
	if r := full.State.Machines[0].Room; r != (dash.Room{}) {
		t.Errorf("full has room %+v", r)
	}
	refused := 0
	for _, b := range full.Day.Buckets {
		refused += b.Refusals.ByWhy[dash.RefusedFull]
	}
	if refused == 0 {
		t.Error("full has no refusals for full caps")
	}
	for name, want := range map[string]int{"full": 3, "mypc": 2, "busy": 0} {
		if n := count(get(name).State.Jobs, dash.JobQueued); n != want {
			t.Errorf("%s has %d queued jobs, want %d", name, n, want)
		}
	}
	if m := get("headless").State.Machines[0]; m.Enforcer != dash.EnforcerHeadless || !m.TrayAlive(goldenNow) || m.Mode != dash.ModeFull || m.ModeRequested == nil {
		t.Errorf("headless: %+v", m)
	}
	if m := get("busy").State.Machines[0]; m.Enforcer != dash.EnforcerTray {
		t.Errorf("busy's enforcer is %q", m.Enforcer)
	}
	if s := get("tray-down"); s.State.Machines[0].TrayAlive(s.State.Now) {
		t.Error("tray-down's tray is alive")
	}
	if s := get("lost"); count(s.State.Jobs, dash.JobLost) == 0 {
		t.Error("lost has no lost job")
	}
	if s := get("cloud"); len(s.State.Cloud) != 4 || s.State.Controller == nil || s.State.CloudPaused {
		t.Errorf("cloud has %d cloud hosts and controller %v", len(s.State.Cloud), s.State.Controller)
	}
	for _, c := range get("cloud").State.Cloud {
		if err := (dash.CloudUsageBody{Host: &c}).Validate(); err != nil {
			t.Errorf("cloud host %s: %v", c.Name, err)
		}
	}
	week := get("week")
	days := map[string]bool{}
	for _, j := range week.Jobs.Jobs {
		days[j.StartedAt.Format(time.DateOnly)] = true
	}
	if len(days) < 7 || len(week.Week.Buckets) != 7 || len(week.Day.Buckets) != 24 {
		t.Errorf("week has jobs on %d days and %d/%d buckets", len(days), len(week.Week.Buckets), len(week.Day.Buckets))
	}
}

// Every job in every scenario keeps the contract's rules.
func TestScenarioJobs(t *testing.T) {
	for _, name := range fixture.Names() {
		s, err := fixture.Build(name, goldenNow)
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range append(s.State.Jobs, s.Jobs.Jobs...) {
			if j.Key != dash.JobKey(j.Box, j.ID, j.StartedAt) {
				t.Errorf("%s: key %q isn't box/id/startedAt", name, j.Key)
			}
			if j.State == dash.JobQueued {
				// While it waits, StartedAt is QueuedAt (dash.Job).
				if j.QueuedAt == nil || !j.StartedAt.Equal(*j.QueuedAt) || j.Position < 1 || !slices.Contains(dash.Refusals, j.Why) {
					t.Errorf("%s: queued job %+v breaks the queue's rules", name, j)
				}
			} else if j.Position != 0 || j.Why != "" {
				t.Errorf("%s: %s job %q has a place in the queue", name, j.State, j.Key)
			}
			if j.Kind != dash.Classify(j.Command, j.GPU) {
				t.Errorf("%s: %q is %s, but the table says %s", name, j.Command, j.Kind, dash.Classify(j.Command, j.GPU))
			}
			if len(j.Tail) > dash.TailBytes {
				t.Errorf("%s: tail of %d bytes", name, len(j.Tail))
			}
			if j.Reason != "" && (len(j.Reason) < 10 || len(j.Reason) > 300) {
				t.Errorf("%s: reason %q is outside 10 to 300 characters", name, j.Reason)
			}
			// The repo is public: fixtures use made-up repos and machines.
			if !strings.HasPrefix(j.Repo, "https://github.com/acme/") || j.Agent.Host != "dev-laptop" {
				t.Errorf("%s: %s from %s isn't one of the fixture's made-up repos and hosts", name, j.Repo, j.Agent.Host)
			}
		}
	}
}

func TestEventWriteTo(t *testing.T) {
	var b bytes.Buffer
	if _, err := (dash.Event{Name: dash.EventJobGone, Data: dash.JobGone{Key: "a/b/c"}}).WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	if want := "event: job-gone\ndata: {\"key\":\"a/b/c\"}\n\n"; b.String() != want {
		t.Errorf("got %q, want %q", b.String(), want)
	}
}
