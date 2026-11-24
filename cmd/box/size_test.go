package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
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
		"-c user.box.size=large -c limits.cpu=4 -c limits.memory=8GiB -d root,size=40GiB",
		"-c user.box.size=medium -c limits.cpu=2 -c limits.memory=4GiB -d root,size=30GiB",
		"-c user.box.size=small -c limits.cpu=1 -c limits.memory=2GiB -d root,size=18GiB",
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
	for name, o := range map[string]RunOpts{
		"no size":       {Cmd: []string{"true"}},
		"matching size": {Cmd: []string{"true"}, Size: "large"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakePC{t: t, insts: []instance{{Name: "my-repo", Status: "Stopped", Config: map[string]string{"user.box.size": "large"}}}, exit: "0"}
			a, out := testApp(f)
			o.Dir = newRepo(t)
			r, err := a.Run(context.Background(), o)
			if err != nil || r.Code != 0 || r.Box != "my-repo" || len(f.launches) != 0 {
				t.Fatalf("Run = %+v, %v, launches %q\n%s", r, err, f.launches, out)
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
	for name, project := range map[string]map[string]string{
		"instances":  {"limits.instances": "0"},
		"cpu":        {"limits.cpu": "0"},
		"memory":     {"limits.memory": "0"},
		"disk":       {"info": `{"resources":{"disk":{"Limit":0,"Usage":0}}}`},
		"containers": {"info": `{"resources":{"containers":{"Limit":0,"Usage":0}}}`},
	} {
		t.Run(name, func(t *testing.T) {
			a, _ := testApp(&fakePC{t: t, project: project})
			if s := a.Status(context.Background(), ""); s.Available || s.Room != "none" {
				t.Errorf("available = %v, room = %q", s.Available, s.Room)
			}
		})
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
		if tl.Name != "box_new" && tl.Name != "box_run" {
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
	res := callTool(context.Background(), a, json.RawMessage(`{"name":"box_new","arguments":{"name":"s","size":"small"}}`), nil)
	if res["isError"] != false || len(f.launches) != 1 || !strings.Contains(strings.Join(f.launches[0], " "), "limits.memory=2GiB") {
		t.Errorf("box_new = %v, launches %q", res, f.launches)
	}
}
