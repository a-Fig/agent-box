package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/mode"
)

func gpuConfig(extra ...string) map[string]string {
	c := map[string]string{mode.SizeKey: mode.GPUSize}
	for i := 0; i < len(extra); i += 2 {
		c[extra[i]] = extra[i+1]
	}
	return c
}

func TestGPURunLaunchesThrowawayWithDevices(t *testing.T) {
	for name, args := range map[string][]string{
		"--gpu":      {"--gpu"},
		"--size gpu": {"--size", "gpu"},
	} {
		t.Run(name, func(t *testing.T) {
			// The repo's own box exists, but a GPU run never uses it.
			f := &fakePC{t: t, mode: "shared", insts: []instance{{Name: "my-repo", Status: "Running"}}, exit: "0"}
			a, out := testApp(f)
			r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"nvidia-smi"}, Size: sizeFlag(t, args)})
			if err != nil || r.Code != 0 || r.Box == "my-repo" {
				t.Fatalf("Run = %+v, %v\n%s", r, err, out)
			}
			if len(f.launches) != 1 {
				t.Fatalf("launches = %q", f.launches)
			}
			got := strings.Join(f.launches[0], " ")
			for _, want := range []string{"-c user.box.size=gpu", "-c limits.cpu=4", "-c limits.memory=8GiB", "-d root,size=40GiB",
				"-c " + throwawayKey + "=true", "-c environment.LD_LIBRARY_PATH=/usr/lib/wsl/lib",
				"-c environment.PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/usr/lib/wsl/lib"} {
				if !strings.Contains(got, want) {
					t.Errorf("launch %s\nlacks %s", got, want)
				}
			}
			var in struct {
				Devices map[string]map[string]string `json:"devices"`
			}
			if err := json.Unmarshal([]byte(f.stdins[0]), &in); err != nil || !reflect.DeepEqual(in.Devices, mode.GPUDevices) {
				t.Errorf("launch stdin = %q (%v)", f.stdins[0], err)
			}
			if f.names() != "my-repo" { // the throwaway is gone once its job is done
				t.Errorf("boxes = %s", f.names())
			}
		})
	}
}

// sizeFlag runs the --gpu/--size flags through the CLI's folding.
func sizeFlag(t *testing.T, args []string) string {
	size, gpu := "", false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--gpu":
			gpu = true
		case "--size":
			i++
			size = args[i]
		}
	}
	a, _ := testApp(&fakePC{t: t})
	if !gpuSize(a, &size, gpu) {
		t.Fatalf("gpuSize refused %q", args)
	}
	return size
}

func TestGPUFlagsDontMixWithOtherSizes(t *testing.T) {
	for _, args := range [][]string{
		{"new", "--gpu", "--size", "large"},
		{"run", "--gpu", "--size", "small", "--reason", "Train the model on the GPU box.", "--", "true"},
	} {
		f := &fakePC{t: t}
		a, out := testApp(f)
		if code := run(context.Background(), a, args, nil); code != 2 || !strings.Contains(out.String(), "--gpu and --size") {
			t.Errorf("%q = %d\n%s", args, code, out)
		}
	}
	f := &fakePC{t: t}
	a, _ := testApp(f)
	res := callTool(context.Background(), a, json.RawMessage(`{"name":"box_run","arguments":{"dir":"/x","command":"true","reason":"Check that gpu and size don't mix.","gpu":true,"size":"small"}}`), nil)
	if res["isError"] != true || len(f.launches) != 0 {
		t.Errorf("box_run gpu+small = %v", res)
	}
}

func TestGPUNewAndMCP(t *testing.T) {
	f := &fakePC{t: t}
	a, out := testApp(f)
	if code := run(context.Background(), a, []string{"new", "trainer", "--gpu"}, nil); code != 0 {
		t.Fatalf("box new --gpu = %d\n%s", code, out)
	}
	if got := strings.Join(f.launches[0], " "); !strings.Contains(got, "user.box.size=gpu") || strings.Contains(got, throwawayKey) {
		t.Errorf("launch = %s", got)
	}
	// A second GPU box is refused while the first holds the GPU.
	out.Reset()
	if code := run(context.Background(), a, []string{"new", "--gpu"}, nil); code == 0 || !strings.Contains(out.String(), "the GPU box is in use (box trainer); run on CPU or wait") || len(f.launches) != 1 {
		t.Errorf("second box new --gpu = %d\n%s", code, out)
	}
	var schema struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	for _, tl := range tools {
		if tl.Name == "box_run" {
			b, _ := json.Marshal(tl.InputSchema)
			json.Unmarshal(b, &schema)
			if schema.Properties["gpu"]["type"] != "boolean" {
				t.Errorf("%s has no gpu flag", tl.Name)
			}
		}
	}
}

func TestGPUInUseExits75(t *testing.T) {
	busy := func(t *testing.T) *fakePC {
		return &fakePC{t: t, mode: "shared", running: "other-1234.20261123-110000-beef\n",
			insts: []instance{{Name: "other-1234", Status: "Running", Config: gpuConfig(throwawayKey, "true")}}}
	}
	want := "box: the GPU box is in use (job other-1234.20261123-110000-beef); run on CPU or wait\n"

	f := busy(t)
	a, out := testApp(f)
	r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, Size: "gpu"})
	if err != nil || r.Code != exitPaused || r.State != statePaused || !strings.Contains(out.String(), want) {
		t.Fatalf("Run = %+v, %v\n%s", r, err, out)
	}
	if len(f.launches) != 0 || f.pushed != 0 || f.names() != "other-1234" {
		t.Errorf("launches %q, pushed %d, boxes %s", f.launches, f.pushed, f.names())
	}

	f = busy(t)
	a, _ = testApp(f)
	dir := newRepo(t)
	res := callTool(context.Background(), a, json.RawMessage(`{"name":"box_run","arguments":{"dir":`+jsonString(dir)+`,"command":"true","gpu":true,"reason":"Run the tests in a box for this change."}}`), nil)
	if sc := res["structuredContent"].(struct {
		JobResult
		Warnings []string `json:"warnings,omitempty"`
	}); sc.Code != exitPaused {
		t.Errorf("box_run gpu = %v", res)
	}

	f = busy(t)
	a, out = testApp(f)
	if code := run(context.Background(), a, []string{"new", "--gpu"}, nil); code != exitPaused || !strings.Contains(out.String(), "the GPU box is in use") {
		t.Errorf("box new --gpu = %d\n%s", code, out)
	}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestGPUClaimRemovesDeadThrowaways(t *testing.T) {
	for name, in := range map[string]instance{
		"stopped by the tray": {Name: "old-1234", Status: "Stopped", Config: gpuConfig(throwawayKey, "true", mode.StoppedKey, "gaming: steam:570")},
		"no job left":         {Name: "old-1234", Status: "Running", Config: gpuConfig(throwawayKey, "true")},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakePC{t: t, mode: "shared", insts: []instance{in}, exit: "0"}
			a, out := testApp(f)
			r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, Size: "gpu"})
			if err != nil || r.Code != 0 {
				t.Fatalf("Run = %+v, %v\n%s", r, err, out)
			}
			if f.names() != "" || len(f.launches) != 1 || !strings.Contains(out.String(), "box: removing old-1234") {
				t.Errorf("boxes %q, launches %q\n%s", f.names(), f.launches, out)
			}
		})
	}
	// A stopped GPU box someone made with box new is left alone, and doesn't hold the GPU.
	f := &fakePC{t: t, mode: "shared", insts: []instance{{Name: "mine", Status: "Stopped", Config: gpuConfig()}}, exit: "0"}
	a, out := testApp(f)
	if r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, Size: "gpu"}); err != nil || r.Code != 0 || f.names() != "mine" {
		t.Errorf("Run = %+v, %v, boxes %s\n%s", r, err, f.names(), out)
	}
}

func TestGPULaunchRaceBacksOff(t *testing.T) {
	f := &fakePC{t: t, mode: "shared"}
	f.launched = func(f *fakePC) { // another run's GPU box appears right after ours
		f.insts = append(f.insts, instance{Name: "rival-1234", Status: "Running", Config: gpuConfig(throwawayKey, "true")})
		f.launched = nil
	}
	a, out := testApp(f)
	r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, Size: "gpu"})
	if err != nil || r.Code != exitPaused || !strings.Contains(out.String(), "the GPU box is in use (box rival-1234)") {
		t.Fatalf("Run = %+v, %v\n%s", r, err, out)
	}
	if f.names() != "rival-1234" || f.pushed != 0 {
		t.Errorf("boxes %s, pushed %d", f.names(), f.pushed)
	}
}

func TestGPURunWithBox(t *testing.T) {
	f := &fakePC{t: t, mode: "shared", insts: []instance{{Name: "plain", Status: "Running"}}}
	a, _ := testApp(f)
	if r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, Box: "plain", Size: "gpu"}); err == nil || r.Code != 2 || !strings.Contains(err.Error(), "isn't a GPU box") {
		t.Errorf("Run on a CPU box = %+v, %v", r, err)
	}
	// A box new --gpu box is reused by name, and doesn't count against itself.
	f = &fakePC{t: t, mode: "shared", insts: []instance{{Name: "trainer", Status: "Running", Config: gpuConfig()}}, exit: "0"}
	a, out := testApp(f)
	if r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, Box: "trainer", Size: "gpu"}); err != nil || r.Code != 0 || r.Box != "trainer" || len(f.launches) != 0 {
		t.Errorf("Run on the GPU box = %+v, %v, launches %q\n%s", r, err, f.launches, out)
	}
}

func TestGPUBoxStoppedByTrayTellsTheAgent(t *testing.T) {
	for why, want := range map[string]string{
		"gaming: steam:570": "box: the GPU box was stopped because the PC's owner started a game; rerun later or on CPU\n",
		"mypc":              "box: the GPU box was stopped because the PC's owner switched it to My PC; rerun later or on CPU\n",
	} {
		t.Run(why, func(t *testing.T) {
			f := &fakePC{t: t, mode: "mypc", stopFor: why,
				insts: []instance{{Name: "my-repo-1234", Status: "Running", Config: gpuConfig(throwawayKey, "true")}}}
			a, out := testApp(f)
			if code := run(context.Background(), a, []string{"job", "my-repo-1234.20261123-110000-beef"}, nil); code != exitPaused {
				t.Fatalf("box job = %d\n%s", code, out)
			}
			if !strings.Contains(out.String(), want) {
				t.Errorf("output = %s", out)
			}
		})
	}
	// After the game, in Shared, the stopped box still explains itself.
	f := &fakePC{t: t, mode: "shared",
		insts: []instance{{Name: "my-repo-1234", Status: "Stopped", Config: gpuConfig(throwawayKey, "true", mode.StoppedKey, "gaming")}}}
	a, out := testApp(f)
	r, err := a.Job(context.Background(), "my-repo-1234.20261123-110000-beef", time.Minute)
	if err != nil || r.Code != exitPaused || !strings.Contains(out.String(), "started a game") {
		t.Errorf("Job = %+v, %v\n%s", r, err, out)
	}
}

func TestStatusGPU(t *testing.T) {
	caps := map[string]string{"limits.instances": "6", "limits.cpu": "24", "limits.memory": "12GiB"}
	for name, tc := range map[string]struct {
		f    *fakePC
		want string
	}{
		"free": {&fakePC{mode: "shared", project: caps}, "free"},
		"in use": {&fakePC{mode: "full", project: caps, running: "g-1.20261123-110000-beef\n",
			insts: []instance{{Name: "g-1", Status: "Running", Config: gpuConfig()}}}, "in use: job g-1.20261123-110000-beef"},
		"stopped box": {&fakePC{mode: "shared", project: map[string]string{"limits.memory": "24GiB"},
			insts: []instance{{Name: "g-1", Status: "Stopped", Config: gpuConfig()}}}, "free"},
		"no room": {&fakePC{mode: "shared", project: caps,
			insts: []instance{{Name: "a", Config: map[string]string{mode.SizeKey: "large"}}}}, "free, but no room for a gpu box now"},
		"game": {&fakePC{mode: "mypc", project: map[string]string{"user.agent-box.reason": "gaming: steam:570"}},
			"unavailable: the PC is yielding to a game"},
		"my pc": {&fakePC{mode: "mypc", project: caps}, "unavailable: the PC is in My PC mode"},
	} {
		t.Run(name, func(t *testing.T) {
			tc.f.t = t
			a, _ := testApp(tc.f)
			s := a.Status(context.Background(), "")
			if s.GPU != tc.want {
				t.Errorf("gpu = %q, want %q", s.GPU, tc.want)
			}
			var b strings.Builder
			writeStatus(&b, s)
			if !strings.Contains(b.String(), "\ngpu: "+tc.want+"\n") {
				t.Errorf("text =\n%s", b.String())
			}
			if strings.Contains(s.Room, "gpu") {
				t.Errorf("room = %q", s.Room)
			}
		})
	}
}

// A GPU box the tray stopped still holds its memory and disk against the
// caps, so a CPU run that doesn't fit deletes it and tries once more.
func TestNoRoomReclaimsStoppedGPUThrowaways(t *testing.T) {
	stopped := instance{Name: "gpu-1234", Status: "Stopped", Config: gpuConfig(throwawayKey, "true", mode.StoppedKey, "gaming: steam:570")}
	f := &fakePC{t: t, mode: "shared", refuse: memoryRefusal, insts: []instance{stopped}}
	a, out := testApp(f)
	r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, New: true})
	if err != nil || r.Code != exitPaused {
		t.Fatalf("Run = %+v, %v\n%s", r, err, out)
	}
	if f.names() != "" || len(f.launches) != 2 || !strings.Contains(out.String(), "box: removing gpu-1234") {
		t.Errorf("boxes %q, %d launches\n%s", f.names(), len(f.launches), out)
	}
	// Nothing to reclaim (a box new --gpu box is the owner's): one launch.
	f = &fakePC{t: t, mode: "shared", refuse: memoryRefusal, insts: []instance{{Name: "mine", Status: "Stopped", Config: gpuConfig()}}}
	a, out = testApp(f)
	if r, _ := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, New: true}); r.Code != exitPaused || len(f.launches) != 1 || f.names() != "mine" {
		t.Errorf("Run = %+v, %d launches, boxes %q\n%s", r, len(f.launches), f.names(), out)
	}
}

func TestStatusDoesntCountStoppedGPUThrowaways(t *testing.T) {
	f := &fakePC{t: t, mode: "shared", project: map[string]string{
		"limits.instances": "2", "limits.cpu": "24", "limits.memory": "12GiB",
		"info": fmt.Sprintf(`{"resources":{"disk":{"Limit":%d,"Usage":%d},"containers":{"Limit":2,"Usage":2}}}`, int64(150)<<30, int64(110)<<30),
	}, insts: []instance{
		{Name: "pi-vi", Status: "Running", Config: map[string]string{"limits.memory": "4GiB"}},
		{Name: "gpu-1234", Status: "Stopped", Config: gpuConfig(throwawayKey, "true", mode.StoppedKey, "mypc")},
	}}
	a, _ := testApp(f)
	if s := a.Status(context.Background(), ""); s.Room != "1 large, or 1 medium, or 1 small" || s.GPU != "free" || s.SlotsFree != 1 || !s.Available {
		t.Errorf("room = %q, gpu = %q, slots %d, available %v", s.Room, s.GPU, s.SlotsFree, s.Available)
	}
}

// In Shared, a stopped GPU throwaway (8GiB) plus 6GiB of other boxes holds
// the tray's cap at 14GiB. Deleting the GPU box lets the tray lower it to
// 12GiB, so after eviction only 6GiB is free: no large box, though 14-6
// would say one fits.
func TestStatusRoomAfterEvictionUsesTheCapTheTrayWouldSet(t *testing.T) {
	f := &fakePC{t: t, mode: "shared", project: map[string]string{
		"limits.instances": "10", "limits.cpu": "24", "limits.memory": "14GiB",
	}, insts: []instance{
		{Name: "a", Status: "Running", Config: map[string]string{"limits.memory": "4GiB"}},
		{Name: "b", Status: "Running", Config: map[string]string{mode.SizeKey: "small", "limits.memory": "2GiB"}},
		{Name: "gpu-1234", Status: "Stopped", Config: gpuConfig(throwawayKey, "true", mode.StoppedKey, "mypc")},
	}}
	a, _ := testApp(f)
	if s := a.Status(context.Background(), ""); s.Room != "1 medium, or 3 small" {
		t.Errorf("room = %q", s.Room)
	}
	// In Full the budget is 24GiB, so the eviction frees the GPU box's 8GiB
	// under a cap that stays at 14GiB (the tray will raise it next tick).
	f.mode = "full"
	if s := a.Status(context.Background(), ""); s.Room != "1 large, or 2 medium, or 4 small" {
		t.Errorf("full room = %q", s.Room)
	}
}

// Between launch and its job's PID file a GPU box runs no job, and Incus
// creates it stopped before starting it. A run in that window must not take
// it for dead.
func TestGPUClaimSparesStartingBoxes(t *testing.T) {
	for name, status := range map[string]string{"created, not started": "Stopped", "started, no job yet": "Running"} {
		t.Run(name, func(t *testing.T) {
			f := &fakePC{t: t, mode: "shared", exit: "0"}
			a, out := testApp(f)
			f.insts = []instance{{Name: "new-1234", Status: status, Config: gpuConfig(throwawayKey, "true"), CreatedAt: a.Now().Add(-time.Minute)}}
			r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, Size: "gpu"})
			if err != nil || r.Code != exitPaused || f.names() != "new-1234" || len(f.launches) != 0 {
				t.Errorf("Run = %+v, %v, boxes %q, launches %q\n%s", r, err, f.names(), f.launches, out)
			}
		})
	}
}

// If another run deleted our new GPU box, we back off rather than run in it.
func TestGPUConfirmNoticesOwnBoxGone(t *testing.T) {
	f := &fakePC{t: t, mode: "shared", exit: "0"}
	f.launched = func(f *fakePC) { f.insts = nil }
	a, out := testApp(f)
	if r, err := a.Run(context.Background(), RunOpts{Dir: newRepo(t), Cmd: []string{"true"}, Size: "gpu"}); err != nil || r.Code != exitPaused {
		t.Errorf("Run = %+v, %v\n%s", r, err, out)
	}
}
