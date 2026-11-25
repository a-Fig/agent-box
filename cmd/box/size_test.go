package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/mode"
)

const memoryRefusal = `Error: Failed checking if instance creation allowed: Reached maximum aggregate value "12GiB" for "limits.memory" in project "agents"`

func TestNewSetsSize(t *testing.T) {
	f := &fakePC{t: t}
	a, out := testApp(f)
	if code := run(context.Background(), a, []string{"new", "big", "--size", "large"}, nil); code != 0 {
		t.Fatalf("box new = %d\n%s", code, out)
	}
	if _, err := a.New(context.Background(), "plain", time.Hour, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := a.New(context.Background(), "little", time.Hour, "small", ""); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{
		"-c user.box.size=large -c limits.cpu=4 -c limits.cpu.priority=0 -c limits.memory=8GiB -d root,size=40GiB",
		"-c user.box.size=medium -c limits.cpu=2 -c limits.cpu.priority=0 -c limits.memory=4GiB -d root,size=30GiB",
		"-c user.box.size=small -c limits.cpu=1 -c limits.cpu.priority=0 -c limits.memory=2GiB -d root,size=18GiB",
	} {
		if got := strings.Join(f.launches[i], " "); !strings.Contains(got, want) {
			t.Errorf("launch %d = %s\nwant %s", i, got, want)
		}
	}
	if _, err := a.New(context.Background(), "odd", time.Hour, "huge", ""); err == nil || !strings.Contains(err.Error(), "small, medium, large") {
		t.Errorf("unknown size: %v", err)
	}
	if len(f.launches) != 3 {
		t.Errorf("launched %d boxes", len(f.launches))
	}
}

func TestNewWithoutRoomExits75(t *testing.T) {
	f := &fakePC{t: t, refuse: memoryRefusal}
	a, out := testApp(f)
	if code := run(context.Background(), a, []string{"new", "--size", "large"}, nil); code != exitPaused {
		t.Fatalf("box new = %d\n%s", code, out)
	}
	if !strings.Contains(out.String(), "no room on the PC for a large box (Error: Failed checking") || !strings.Contains(out.String(), "run it locally") {
		t.Errorf("output = %s", out)
	}
	if code := run(context.Background(), &App{R: &fakePC{t: t, refuse: "Error: boom"}, Remote: "pc", Project: "agents", Out: out, Err: out, Now: a.Now, Rand: a.Rand}, []string{"new"}, nil); code != 1 {
		t.Errorf("other launch failure = %d", code)
	}
}

func TestLaunchDistinguishesQuotaFromOrdinaryErrors(t *testing.T) {
	for _, tc := range []struct {
		message string
		noRoom  bool
	}{
		{memoryRefusal, true},
		{`Error: Reached maximum number of instances in project "agents"`, true},
		{`Error: Reached maximum number of instances of type "container" in project "agents"`, true},
		{`Error: Reached maximum aggregate value "150GiB" for "limits.disk" in project "agents"`, true},
		{`Error: Failed creating instance record: Instance "quota-worker" already exists`, false},
		{`Error: Instance name exceeds 63 characters`, false},
	} {
		f := &fakePC{t: t, refuse: tc.message}
		a, _ := testApp(f)
		_, err := a.New(context.Background(), "quota-worker", time.Hour, "small", "")
		if err == nil || isNoRoom(err) != tc.noRoom {
			t.Errorf("%s: error = %v, want no room = %t", tc.message, err, tc.noRoom)
		}
	}
}

func TestRunWithoutRoomExits75(t *testing.T) {
	for name, o := range map[string]RunOpts{
		"repo box":  {Cmd: []string{"true"}, Size: "large"},
		"throwaway": {Cmd: []string{"true"}, New: true, TTL: time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakePC{t: t, refuse: memoryRefusal}
			a, out := testApp(f)
			o.Dir = newRepo(t)
			r, err := a.Run(context.Background(), o)
			if err != nil || r.Code != exitPaused || r.State != statePaused {
				t.Fatalf("Run = %+v, %v\n%s", r, err, out)
			}
			if !strings.Contains(out.String(), "box: no room on the PC for a") || f.pushed != 0 {
				t.Errorf("output = %s", out)
			}
		})
	}
}

func TestRunSizeMismatchUsesThrowaway(t *testing.T) {
	large := map[string]string{"user.box.size": "large"}
	f := &fakePC{t: t, insts: []instance{{Name: "my-repo", Status: "Running", Config: large}}, exit: "0"}
	a, out := testApp(f)
	r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, Size: "small"})
	if err != nil || r.Code != 0 || r.Box == "my-repo" {
		t.Fatalf("Run = %+v, %v\n%s", r, err, out)
	}
	if !strings.Contains(out.String(), "box: my-repo is large, so this run uses a throwaway small box\n") {
		t.Errorf("output = %s", out)
	}
	if len(f.launches) != 1 || !strings.Contains(strings.Join(f.launches[0], " "), "user.box.size=small") || !strings.Contains(strings.Join(f.launches[0], " "), throwawayKey+"=true") {
		t.Errorf("launches = %q", f.launches)
	}
	if f.names() != "my-repo" { // the throwaway is gone, the large box untouched
		t.Errorf("boxes = %s", f.names())
	}
}

func TestRunReusesBoxOfAnySizeWithoutSize(t *testing.T) {
	for name, tc := range map[string]struct {
		o      RunOpts
		status string
		size   string
		gpu    bool
	}{
		"no size, stopped large": {o: RunOpts{Cmd: []string{"true"}}, status: "Stopped", size: "large"},
		"no size, running small": {o: RunOpts{Cmd: []string{"true"}}, status: "Running", size: "small"},
		"no size, running gpu":   {o: RunOpts{Cmd: []string{"true"}}, status: "Running", size: "gpu", gpu: true},
		"matching size":          {o: RunOpts{Cmd: []string{"true"}, Size: "large"}, status: "Stopped", size: "large"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakePC{t: t, insts: []instance{{Name: "my-repo", Status: tc.status, Config: map[string]string{"user.box.size": tc.size}}}, exit: "0"}
			a, out := testApp(f)
			o := tc.o
			o.Dir = newRepo(t)
			r, err := a.Run(context.Background(), o)
			if err != nil || r.Code != 0 || r.Box != "my-repo" || len(f.launches) != 0 {
				t.Fatalf("Run = %+v, %v, launches %q\n%s", r, err, f.launches, out)
			}
			var got jobMeta
			raw := f.files["pc:my-repo/work/.box/"+r.Job+".meta"]
			if err := json.Unmarshal([]byte(raw), &got); err != nil {
				t.Fatalf("meta %q: %v", raw, err)
			}
			if got.Size != tc.size || got.GPU != tc.gpu {
				t.Fatalf("meta size=%q gpu=%v, want size=%q gpu=%v", got.Size, got.GPU, tc.size, tc.gpu)
			}
		})
	}
	f := &fakePC{t: t}
	a, _ := testApp(f)
	if r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, Size: "tiny"}); err == nil || r.Code != 2 {
		t.Errorf("bad size = %+v, %v", r, err)
	}
}

func TestRunKeepsRequestedSizeAfterLosingLaunchRace(t *testing.T) {
	f := &fakePC{t: t, race: map[string]bool{"my-repo": true}, exit: "0"}
	a, out := testApp(f)
	r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, Size: "large"})
	if err != nil || r.Code != 0 || r.Box == "my-repo" {
		t.Fatalf("Run = %+v, %v", r, err)
	}
	if len(f.launches) != 2 {
		t.Fatalf("launches = %q", f.launches)
	}
	if got := strings.Join(f.launches[1], " "); !strings.Contains(got, "user.box.size=large") || !strings.Contains(got, throwawayKey+"=true") {
		t.Errorf("second launch = %s", got)
	}
	if f.names() != "my-repo" {
		t.Errorf("boxes after run = %s", f.names())
	}
	if !strings.Contains(out.String(), "my-repo is medium, so this run uses a throwaway large box") {
		t.Errorf("output = %s", out)
	}
}

func TestStatusCountsLimitsInheritedFromProfiles(t *testing.T) {
	f := &fakePC{t: t, mode: "shared", project: map[string]string{
		"limits.instances": "6", "limits.cpu": "10", "limits.memory": "8GiB",
	}, insts: []instance{{
		Name: "old-box", Status: "Stopped", ExpandedConfig: map[string]string{"limits.cpu": "8", "limits.memory": "6GiB"},
	}}}
	a, _ := testApp(f)
	if got := a.Status(context.Background(), "").Room; got != "1 small" {
		t.Errorf("room = %q, want 1 small", got)
	}
}

func TestStatusHonorsZeroAndContainerCaps(t *testing.T) {
	for name, tc := range map[string]struct {
		project map[string]string
		room    string
	}{
		"instances":  {map[string]string{"limits.instances": "0"}, "none (box slots: 0 of 0)"},
		"cpu":        {map[string]string{"limits.cpu": "0"}, "none (CPUs: 0 of 0 reserved)"},
		"memory":     {map[string]string{"limits.memory": "0"}, "none (memory: 0GiB of 0GiB allocated)"},
		"disk":       {map[string]string{"info": `{"resources":{"disk":{"Limit":0,"Usage":0}}}`}, "none (disk: 0GiB of 0GiB allocated)"},
		"containers": {map[string]string{"info": `{"resources":{"containers":{"Limit":0,"Usage":0}}}`}, "none (containers)"},
	} {
		t.Run(name, func(t *testing.T) {
			a, _ := testApp(&fakePC{t: t, project: tc.project})
			if s := a.Status(context.Background(), ""); s.Available || s.Room != tc.room {
				t.Errorf("available = %v, room = %q, want %q", s.Available, s.Room, tc.room)
			}
		})
	}
}

// Slots free but the disk cap full: status says which cap, so an agent knows
// a smaller size won't help and deleting a box would (#92).
func TestStatusNamesTheFullCap(t *testing.T) {
	f := &fakePC{t: t, project: map[string]string{
		"limits.instances": "10", "limits.cpu": "24", "limits.memory": "24GiB",
		"info": fmt.Sprintf(`{"resources":{"disk":{"Limit":%d,"Usage":%d}}}`, int64(210)<<30, int64(205)<<30+int64(300)<<20),
	}}
	a, out := testApp(f)
	s := a.Status(context.Background(), "")
	if s.Room != "none (disk: 205.3GiB of 210GiB allocated)" || !strings.Contains(s.Advice, "busy: no room for another box (disk: 205.3GiB of 210GiB allocated)") {
		t.Errorf("room = %q, advice = %q\n%s", s.Room, s.Advice, out)
	}

	// The slots are full too, but a launch would delete the stopped GPU
	// throwaway first: only the disk, which that doesn't free enough of, is named.
	f = &fakePC{t: t, project: map[string]string{
		"limits.instances": "2", "limits.cpu": "24", "limits.memory": "24GiB",
		"info": fmt.Sprintf(`{"resources":{"disk":{"Limit":%d,"Usage":%d}}}`, int64(100)<<30, int64(145)<<30),
	}, insts: []instance{
		{Name: "a", Status: "Running", Config: map[string]string{mode.SizeKey: "small"}},
		{Name: "g-1", Status: "Stopped", Config: gpuConfig(throwawayKey, "true", mode.StoppedKey, "mypc")},
	}}
	a, out = testApp(f)
	if s := a.Status(context.Background(), ""); s.Room != "none (disk: 145GiB of 100GiB allocated)" {
		t.Errorf("with a reclaimable box: room = %q\n%s", s.Room, out)
	}
}

func TestStatusFitsSixSmallBoxesBesideCache(t *testing.T) {
	f := &fakePC{t: t, project: map[string]string{
		"limits.instances": "6", "limits.cpu": "24", "limits.memory": "12GiB",
		"info": fmt.Sprintf(`{"resources":{"disk":{"Limit":%d,"Usage":%d}}}`, int64(150)<<30, int64(30)<<30),
	}}
	a, _ := testApp(f)
	if got := a.Status(context.Background(), "").Room; got != "1 large, or 3 medium, or 6 small" {
		t.Errorf("room = %q", got)
	}
}

func TestStatusSkipsUnknownDiskLimit(t *testing.T) {
	a, _ := testApp(&fakePC{t: t, project: map[string]string{
		"limits.instances": "1", "info": `{"resources":{"disk":{"Usage":0}}}`,
	}})
	if s := a.Status(context.Background(), ""); !s.Available || s.Room != "1 large, or 1 medium, or 1 small" {
		t.Errorf("available = %v, room = %q", s.Available, s.Room)
	}
}

func TestMCPSize(t *testing.T) {
	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	for _, tl := range tools {
		if tl.Name != "box_run" {
			continue
		}
		b, _ := json.Marshal(tl.InputSchema)
		json.Unmarshal(b, &schema)
		if got := strings.Join(schema.Properties["size"].Enum, ","); got != "small,medium,large,gpu" {
			t.Errorf("%s size enum = %q", tl.Name, got)
		}
	}
	f := &fakePC{t: t}
	a, _ := testApp(f)
	if code := run(context.Background(), a, []string{"new", "s", "--size", "small"}, nil); code != 0 || len(f.launches) != 1 || !strings.Contains(strings.Join(f.launches[0], " "), "limits.memory=2GiB") {
		t.Errorf("box new = %d, launches %q", code, f.launches)
	}
}
