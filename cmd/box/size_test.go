package main

import (
	"context"
	"encoding/json"
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
	if _, err := a.New(context.Background(), "plain", time.Hour, ""); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{
		"-c user.box.size=large -c limits.cpu=4 -c limits.memory=8GiB -d root,size=40GiB",
		"-c user.box.size=medium -c limits.cpu=2 -c limits.memory=4GiB -d root,size=30GiB",
	} {
		if got := strings.Join(f.launches[i], " "); !strings.Contains(got, want) {
			t.Errorf("launch %d = %s\nwant %s", i, got, want)
		}
	}
	if _, err := a.New(context.Background(), "odd", time.Hour, "huge"); err == nil || !strings.Contains(err.Error(), "small, medium, large") {
		t.Errorf("unknown size: %v", err)
	}
	if len(f.launches) != 2 {
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
		if got := strings.Join(schema.Properties["size"].Enum, ","); got != "small,medium,large" {
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
