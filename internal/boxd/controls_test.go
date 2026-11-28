package boxd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
)

func (r *queueRig) post(path, body string) (int, []byte) {
	req, _ := http.NewRequest("POST", r.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(dash.ControlHeader, "1")
	req.Header.Set(LoginHeader, "owner@example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	var b []byte
	buf := make([]byte, 64<<10)
	n, _ := resp.Body.Read(buf)
	b = buf[:n]
	return resp.StatusCode, b
}

func TestControls(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "full", "limits.memory": "24GiB"})
	created := t0.Add(-time.Hour)
	job := "w.20261124-175900-ab12"
	r.f.set(func() {
		r.f.ins = []Instance{inst("w", "Running", "small", created)}
		r.f.jobs["w"] = []fakeJob{{name: job, state: "running", log: "x", meta: metaJSON}}
	})
	r.poll()

	// Mode: written as a request for the tray, never as the mode.
	if code, b := r.post("/api/mode", `{"mode":"bored"}`); code != 400 {
		t.Fatalf("bad mode: %d %s", code, b)
	}
	code, b := r.post("/api/mode", `{"mode":"shared"}`)
	var m dash.Machine
	json.Unmarshal(b, &m)
	if code != 202 || m.ModeRequested == nil || m.ModeRequested.Mode != "shared" || m.ModeRequested.By != "owner@example.com" || m.Mode != "full" {
		t.Fatalf("mode: %d %s", code, b)
	}
	if last := r.f.cli[len(r.f.cli)-1]; !strings.HasPrefix(last, `project set agents user.agent-box.mode-request={"mode":"shared","at":`) {
		t.Fatalf("mode request: %s", last)
	}

	// Extend: from the later of now and the expiry (the fake's is 2026-11-25T00:00Z).
	if code, b := r.post("/api/boxes/w/extend", `{"hours":3,"createdAt":"`+created.Format(time.RFC3339)+`"}`); code != 400 {
		t.Fatalf("3h: %d %s", code, b)
	}
	if code, b := r.post("/api/boxes/w/extend", `{"hours":4,"createdAt":"2026-11-24T10:00:00Z"}`); code != 409 || !strings.Contains(string(b), `"gone"`) {
		t.Fatalf("another box of the name: %d %s", code, b)
	}
	code, b = r.post("/api/boxes/w/extend", `{"hours":4,"createdAt":"`+created.Format(time.RFC3339)+`"}`)
	var box dash.Box
	json.Unmarshal(b, &box)
	if code != 200 || box.ExpiresAt == nil || box.ExpiresAt.Format(time.RFC3339) != "2026-11-25T04:00:00Z" {
		t.Fatalf("extend: %d %s", code, b)
	}

	// Cancel: a frozen box can't run the kill.
	key := url.PathEscape(dash.JobKey("w", "20261124-175900-ab12", t0.Add(-time.Minute)))
	r.f.set(func() { r.f.ins[0].Status = "Frozen" })
	r.poll()
	if code, b := r.post("/api/jobs/"+key+"/cancel", `{}`); code != 409 || !strings.Contains(string(b), `"frozen"`) {
		t.Fatalf("cancel frozen: %d %s", code, b)
	}
	r.f.set(func() { r.f.ins[0].Status = "Running" })
	r.poll()
	code, b = r.post("/api/jobs/"+key+"/cancel", `{}`)
	var j dash.Job
	json.Unmarshal(b, &j)
	if code != 200 || j.Result() != dash.ResultCancelled {
		t.Fatalf("cancel: %d %s", code, b)
	}
	// The agent hears of it, and why: no .cancelled mark, a line in the log.
	if s := r.f.cancel; strings.Contains(s, `: > "$j.cancelled"`) || !strings.Contains(s, "cancelled from the dashboard by owner@example.com") {
		t.Fatalf("cancel script:\n%s", s)
	}
	if code, b := r.post("/api/jobs/"+key+"/cancel", `{}`); code != 409 || !strings.Contains(string(b), `"not-running"`) {
		t.Fatalf("cancel again: %d %s", code, b)
	}

	// Delete: only the box the page saw, and a running job needs the warning first (#247).
	if code, _ := r.post("/api/boxes/w/delete", `{"createdAt":"2026-11-24T10:00:00Z"}`); code != 409 {
		t.Fatalf("delete another: %d", code)
	}
	if code, b := r.post("/api/boxes/w/delete", `{"createdAt":"`+created.Format(time.RFC3339)+`"}`); code != 409 || !strings.Contains(string(b), dash.CodeNeedsConfirm) {
		t.Fatalf("delete unconfirmed with a running job: %d %s", code, b)
	}
	if code, b := r.post("/api/boxes/w/delete", `{"createdAt":"`+created.Format(time.RFC3339)+`","confirmed":true}`); code != 204 {
		t.Fatalf("delete: %d %s", code, b)
	}
	if last := r.f.cli[len(r.f.cli)-1]; last != "delete w --force" {
		t.Fatalf("delete ran %s", last)
	}
	if code, _ := r.post("/api/boxes/nope/delete", `{"createdAt":"`+created.Format(time.RFC3339)+`"}`); code != 404 {
		t.Fatalf("delete missing: %d", code)
	}
	// The controls are in the history, with who asked.
	rs := readEvents(t, r.dir+"/history")
	var acts []string
	for _, rec := range rs {
		if rec.Type == recControl {
			acts = append(acts, rec.Control.Action+":"+rec.Control.By)
		}
	}
	if strings.Join(acts, ",") != "mode shared:owner@example.com,extend 4h:owner@example.com,cancel:owner@example.com,delete:owner@example.com" {
		t.Fatalf("controls recorded: %v", acts)
	}
	_ = context.Background
}

// A mode request that loses its ETag race with the tray's project writes is
// written again, not answered 502.
func TestModeRequestRetriesETagRace(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "full"})
	lost := 2
	r.f.set(func() {
		r.f.cliErr = func(args []string) error {
			if args[0] == "project" && lost > 0 {
				lost--
				return errors.New("incus project: exit status 1: Error: ETag doesn't match: 1 vs 2")
			}
			return nil
		}
	})
	if code, b := r.post("/api/mode", `{"mode":"shared"}`); code != 202 {
		t.Fatalf("mode: %d %s", code, b)
	}
	sets := 0
	for _, c := range r.f.cli {
		if strings.HasPrefix(c, "project set") {
			sets++
		}
	}
	if sets != 3 {
		t.Errorf("project sets = %d, want 2 lost and 1 written", sets)
	}
}

// An extend that loses its race to a job starting in the box (box run sets
// 24h) counts from the new expiry, never writing back the old one.
func TestExtendRetryReadsTheNewExpiry(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "full"})
	created := t0.Add(-time.Hour)
	r.f.set(func() {
		r.f.ins = []Instance{inst("w", "Running", "small", created)} // expires 2026-11-25T00:00:00Z
		r.f.between = func(cfg map[string]string) {
			cfg["user.expires"] = "2026-11-26T00:00:00Z"
		}
	})
	r.poll()
	if code, b := r.post("/api/boxes/w/extend", `{"hours":4,"createdAt":"`+created.Format(time.RFC3339)+`"}`); code != 200 {
		t.Fatalf("extend: %d %s", code, b)
	}
	in, _ := r.f.instance("w")
	if in.Config["user.expires"] != "2026-11-26T04:00:00Z" || in.Config["user.box.hold-until"] != in.Config["user.expires"] {
		t.Errorf("config after the lost race: %v", in.Config)
	}
}

func TestExtendRetriesAfterConcurrentExtend(t *testing.T) {
	r := newQueueRig(t, nil)
	created := t0.Add(-time.Hour)
	r.f.set(func() {
		r.f.ins = []Instance{inst("w", "Running", "small", created)}
		r.f.between = func(cfg map[string]string) {
			cfg["user.expires"] = "2026-11-25T01:00:00Z"
			cfg["user.box.hold-until"] = cfg["user.expires"]
		}
	})
	r.poll()
	if code, b := r.post("/api/boxes/w/extend", `{"hours":4,"createdAt":"`+created.Format(time.RFC3339)+`"}`); code != 200 {
		t.Fatalf("extend: %d %s", code, b)
	}
	in, _ := r.f.instance("w")
	if in.Config["user.expires"] != "2026-11-25T05:00:00Z" || in.Config["user.box.hold-until"] != in.Config["user.expires"] {
		t.Fatalf("config after both extends: %v", in.Config)
	}
}

func TestExtendReplacesLegacySentinelHold(t *testing.T) {
	r := newQueueRig(t, nil)
	created := t0.Add(-time.Hour)
	r.f.set(func() {
		in := inst("w", "Running", "small", created)
		in.Config["user.expires"] = "9999-01-01T01:00:00Z"
		in.Config["user.box.hold-until"] = in.Config["user.expires"]
		r.f.ins = []Instance{in}
	})
	r.poll()
	if code, b := r.post("/api/boxes/w/extend", `{"hours":4,"createdAt":"`+created.Format(time.RFC3339)+`"}`); code != 200 {
		t.Fatalf("extend: %d %s", code, b)
	}
	in, _ := r.f.instance("w")
	if in.Config["user.expires"] != "9999-01-01T01:00:00Z" ||
		in.Config["user.box.hold-until"] != r.c.now().Add(4*time.Hour).Format(time.RFC3339) {
		t.Fatalf("config after extend: %v", in.Config)
	}
}

func TestExtendRejectsReplacementOnRetry(t *testing.T) {
	r := newQueueRig(t, nil)
	created := t0.Add(-time.Hour)
	r.f.set(func() {
		r.f.ins = []Instance{inst("w", "Running", "small", created)}
		r.f.between = func(cfg map[string]string) {
			r.f.ins[0].CreatedAt = t0
		}
	})
	r.poll()
	if code, b := r.post("/api/boxes/w/extend", `{"hours":4,"createdAt":"`+created.Format(time.RFC3339)+`"}`); code != 409 {
		t.Fatalf("extend replacement: %d %s", code, b)
	}
	in, _ := r.f.instance("w")
	if in.Config["user.box.hold-until"] != "" {
		t.Fatalf("replacement received a hold: %v", in.Config)
	}
}

type extendBeforeReturn struct {
	Incus
	afterWrite func()
}

func TestExtendReplacesStaleCachedJobDeadline(t *testing.T) {
	r := newQueueRig(t, nil)
	created := t0.Add(-time.Hour)
	r.f.set(func() {
		in := inst("w", "Running", "small", created)
		in.Config["user.expires"] = t0.Add(24 * time.Hour).Format(time.RFC3339)
		r.f.ins = []Instance{in}
	})
	r.poll()
	r.f.set(func() {
		r.f.ins[0].Config["user.expires"] = t0.Add(2 * time.Hour).Format(time.RFC3339)
	})
	got, err := r.w.ExtendBox(context.Background(), "w", created, 4, "owner")
	want := t0.Add(6 * time.Hour)
	if err != nil || got.ExpiresAt == nil || !got.ExpiresAt.Equal(want) {
		t.Fatalf("extend after cleanup: %+v, %v", got, err)
	}
	cached, err := r.w.boxFor("w", created)
	if err != nil || cached.ExpiresAt == nil || !cached.ExpiresAt.Equal(want) {
		t.Fatalf("cached box: %+v, %v", cached, err)
	}
}

func (f *extendBeforeReturn) SetConfigIf(ctx context.Context, name string, decide func(Instance) map[string]string) (bool, error) {
	wrote, err := f.Incus.SetConfigIf(ctx, name, decide)
	if wrote && f.afterWrite != nil {
		run := f.afterWrite
		f.afterWrite = nil
		run()
	}
	return wrote, err
}

func TestExtendOlderCompletionKeepsNewerCachedDeadline(t *testing.T) {
	r := newQueueRig(t, nil)
	created := t0.Add(-time.Hour)
	r.f.set(func() { r.f.ins = []Instance{inst("w", "Running", "small", created)} })
	r.poll()
	ctx := context.Background()
	r.w.Incus = &extendBeforeReturn{Incus: r.f, afterWrite: func() {
		if _, err := r.w.ExtendBox(ctx, "w", created, 4, "second"); err != nil {
			t.Fatal(err)
		}
	}}
	got, err := r.w.ExtendBox(ctx, "w", created, 1, "first")
	if err != nil || got.ExpiresAt == nil || got.ExpiresAt.Format(time.RFC3339) != "2026-11-25T05:00:00Z" {
		t.Fatalf("older completion: %+v, %v", got, err)
	}
	cached, err := r.w.boxFor("w", created)
	if err != nil || cached.ExpiresAt == nil || !cached.ExpiresAt.Equal(*got.ExpiresAt) {
		t.Fatalf("cached box: %+v, %v", cached, err)
	}
}

// Machine says which enforcer runs, from boxtray's project keys; a value
// boxtray doesn't write is left out.
func TestMachineEnforcer(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "mypc",
		"user.agent-box.enforcer": "headless", "user.agent-box.tray-seen": "2026-11-24T17:59:30Z"})
	m := r.w.Machine()
	if m.Enforcer != dash.EnforcerHeadless || m.TrayLastSeen == nil {
		t.Fatalf("machine: %+v", m)
	}
	r.f.set(func() {
		r.f.cfg["user.agent-box.enforcer"] = "someone"
	})
	r.poll()
	if m := r.w.Machine(); m.Enforcer != "" {
		t.Fatalf("unknown values: %+v", m)
	}
}

// Machine carries boxd's own build and the tray's, from user.agent-box.tray-version.
func TestMachineVersions(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "full",
		"user.agent-box.tray-version": `{"commit":"1378501abc","time":"2026-11-24T14:00:00-08:00","modified":true}`})
	if m := r.w.Machine(); m.Boxd != nil || m.Tray == nil || m.Tray.String() != "1378501+" ||
		!m.Tray.Time.Equal(time.Date(2026, 11, 24, 22, 0, 0, 0, time.UTC)) || m.Tray.Time.Location() != time.UTC {
		t.Fatalf("machine: %+v %+v", m.Boxd, m.Tray)
	}
	// Since is when the key took its value: kept while it holds it, moved when it changes.
	first := t0.UTC().Truncate(time.Second)
	if m := r.w.Machine(); m.Tray.Since == nil || !m.Tray.Since.Equal(first) {
		t.Fatalf("since: %v, want %v", m.Tray.Since, first)
	}
	r.c.add(time.Hour)
	r.poll()
	if m := r.w.Machine(); !m.Tray.Since.Equal(first) {
		t.Fatalf("since moved with the value unchanged: %v", m.Tray.Since)
	}
	r.f.set(func() { r.f.cfg["user.agent-box.tray-version"] = `{"commit":"f5c1be4abc"}` })
	r.poll()
	if m := r.w.Machine(); m.Tray.Commit != "f5c1be4abc" || !m.Tray.Since.Equal(first.Add(time.Hour)) {
		t.Fatalf("after an update: %+v", m.Tray)
	}
	r.w.Version = &dash.Version{Commit: "e823842def"}
	for _, bad := range []string{"not json", `{"commit":"","time":null,"modified":false}`} {
		r.f.set(func() { r.f.cfg["user.agent-box.tray-version"] = bad })
		r.poll()
		if m := r.w.Machine(); m.Boxd == nil || m.Boxd.Commit != "e823842def" || m.Tray != nil {
			t.Fatalf("after %s: %+v %+v", bad, m.Boxd, m.Tray)
		}
	}
}

// Machine.Config.GPU needs the device and a project that doesn't block GPU devices, and says
// which is missing; a cap set to 0 is told apart from one that isn't set.
func TestMachineConfigGPUAndCaps(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "full", "limits.instances": "0"})
	m := r.w.Machine()
	if m.Config.GPU || m.Config.GPUWhy != "device" {
		t.Fatalf("no device: %+v", m.Config)
	}
	if !m.Config.Capped.Instances || m.Caps.Instances != 0 || m.Config.Capped.CPU {
		t.Fatalf("caps: %+v %+v", m.Config.Capped, m.Caps)
	}
	r.w.HasGPU = func() bool { return true }
	for _, c := range []struct {
		key        string
		restricted string
		gpu        bool
		why        string
		have       bool
	}{{"", "", true, "", true}, {"", "false", true, "", true}, {"", "true", false, "blocked", true}, {"", "Yes", false, "blocked", true}, {"", "0", true, "", true}, {"allow", "true", true, "", true},
		{"managed", "true", true, "", true}, {"block", "true", false, "blocked", true}, {"block", "", false, "both", false}} {
		r.f.set(func() { r.f.cfg["restricted.devices.unix-char"] = c.key; r.f.cfg["restricted"] = c.restricted })
		r.w.HasGPU = func() bool { return c.have }
		r.poll()
		if m := r.w.Machine(); m.Config.GPU != c.gpu || m.Config.GPUWhy != c.why {
			t.Errorf("key %q restricted %q device %v: %+v", c.key, c.restricted, c.have, m.Config)
		}
	}
}

// A host's name is a label the page sets (#291): written to one project key, read back on every
// poll, and unset to go back to the config's name.
func TestRenameMachine(t *testing.T) {
	r := newQueueRig(t, map[string]string{"user.agent-box.mode": "full"})
	if code, b := r.post("/api/machine/name", `{"name":"`+strings.Repeat("x", dash.MaxMachineName+1)+`"}`); code != 400 {
		t.Fatalf("too long: %d %s", code, b)
	}
	if code, b := r.post("/api/machine/name", `{"name":"two\nlines"}`); code != 400 {
		t.Fatalf("control character: %d %s", code, b)
	}
	if code, b := r.post("/api/machine/name", `{"name":"‮evil"}`); code != 400 {
		t.Fatalf("bidi override: %d %s", code, b)
	}
	code, b := r.post("/api/machine/name", `{"name":"  Tower  "}`)
	var m dash.Machine
	json.Unmarshal(b, &m)
	if code != 200 || m.Name != "Tower" || m.ID != r.w.MachineID {
		t.Fatalf("rename: %d %s", code, b)
	}
	if last := r.f.cli[len(r.f.cli)-1]; last != "project set agents user.agent-box.name=Tower" {
		t.Fatalf("rename ran %s", last)
	}
	// What the project holds is the name on every poll.
	r.f.set(func() { r.f.cfg[ProjectNameKey] = "Tower" })
	r.poll()
	if got := r.w.State().Machines[0].Name; got != "Tower" {
		t.Fatalf("polled name %q", got)
	}
	code, b = r.post("/api/machine/name", `{"name":""}`)
	json.Unmarshal(b, &m)
	if code != 200 || m.Name != r.w.MachineName {
		t.Fatalf("clear: %d %s", code, b)
	}
	if last := r.f.cli[len(r.f.cli)-1]; last != "project unset agents user.agent-box.name" {
		t.Fatalf("clear ran %s", last)
	}
	r.f.set(func() { delete(r.f.cfg, ProjectNameKey) })
	r.poll()
	if got := r.w.State().Machines[0].Name; got != r.w.MachineName {
		t.Fatalf("polled name after clearing %q", got)
	}
	var acts []string
	for _, rec := range readEvents(t, r.dir+"/history") {
		if rec.Type == recControl {
			acts = append(acts, rec.Control.Action)
		}
	}
	if strings.Join(acts, ",") != `rename "Tower",rename ""` {
		t.Fatalf("controls recorded: %v", acts)
	}
}
