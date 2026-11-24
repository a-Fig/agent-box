package dash_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
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
	if m := get("yielding").State.Machines[0]; !m.Yielding || m.ModeRequested == nil || m.Mode != dash.ModeMyPC {
		t.Errorf("yielding: %+v", m)
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
	if s := get("tray-down"); s.State.Machines[0].TrayAlive(s.State.Now) {
		t.Error("tray-down's tray is alive")
	}
	if s := get("lost"); count(s.State.Jobs, dash.JobLost) == 0 {
		t.Error("lost has no lost job")
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
