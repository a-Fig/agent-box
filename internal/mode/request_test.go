package mode

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func reqJSON(t *testing.T, m Mode, at time.Time) string {
	t.Helper()
	b, err := json.Marshal(ModeRequest{Mode: m, At: at, By: "phone"})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func handshakeMgr(f *fake, start Mode) (*Manager, *[]Mode) {
	m := New(f, start)
	m.Handshake = true
	var saved []Mode
	m.Persist = func(c Mode) { saved = append(saved, c) }
	return m, &saved
}

func storedChoice(t *testing.T, f *fake) chosenValue {
	t.Helper()
	var v chosenValue
	if err := json.Unmarshal([]byte(f.project[ProjectChosenKey]), &v); err != nil {
		t.Fatalf("chosen key %q: %v", f.project[ProjectChosenKey], err)
	}
	return v
}

func TestRequestIsAdoptedStoredPersistedAndCleared(t *testing.T) {
	f := newFake()
	at := t0.Add(-time.Minute)
	f.project[ProjectModeRequestKey] = reqJSON(t, Shared, at)
	m, saved := handshakeMgr(f, Full)
	st := tick(m, 0, false)
	if st.Chosen != Shared || st.Effective != Shared || st.Err != nil {
		t.Fatalf("status %+v", st)
	}
	if v := storedChoice(t, f); v.Mode != Shared || !v.At.Equal(at) {
		t.Fatalf("stored %+v", v)
	}
	if f.project[ProjectModeRequestKey] != "" {
		t.Fatalf("request not cleared: %q", f.project[ProjectModeRequestKey])
	}
	if f.project[ProjectModeKey] != "shared" {
		t.Fatalf("mode label %q", f.project[ProjectModeKey])
	}
	if len(*saved) != 1 || (*saved)[0] != Shared {
		t.Fatalf("persisted %v", *saved)
	}
}

func TestRequestOlderThanTenMinutesIsDroppedAndCleared(t *testing.T) {
	f := newFake()
	f.project[ProjectModeRequestKey] = reqJSON(t, MyPC, t0.Add(-RequestMaxAge-time.Second))
	m, saved := handshakeMgr(f, Full)
	if st := tick(m, 0, false); st.Chosen != Full {
		t.Fatalf("a stale request was applied: %+v", st)
	}
	if f.project[ProjectModeRequestKey] != "" || len(*saved) != 0 || f.project[ProjectChosenKey] != "" {
		t.Fatalf("request=%q saved=%v chosen=%q", f.project[ProjectModeRequestKey], *saved, f.project[ProjectChosenKey])
	}
}

func TestRequestWaitsForTheGameAndIsKeptPending(t *testing.T) {
	f := newFake()
	raw := reqJSON(t, Shared, t0.Add(-time.Minute))
	f.project[ProjectModeRequestKey] = raw
	m, _ := handshakeMgr(f, Full)
	if st := tick(m, 0, true); st.Chosen != Full || !st.Yielding {
		t.Fatalf("during the game: %+v", st)
	}
	if f.project[ProjectModeRequestKey] != raw {
		t.Fatalf("a pending request was cleared: %q", f.project[ProjectModeRequestKey])
	}
	tick(m, time.Minute, false) // quiet, but inside the debounce: still yielding
	if f.project[ProjectModeRequestKey] != raw {
		t.Fatal("cleared inside the debounce")
	}
	if st := tick(m, time.Minute+YieldDebounce, false); st.Chosen != Shared || st.Yielding {
		t.Fatalf("after the game: %+v", st)
	}
	if f.project[ProjectModeRequestKey] != "" {
		t.Fatal("not cleared after the game")
	}
}

func TestRequestThatWaitedTooLongForTheGameIsDropped(t *testing.T) {
	f := newFake()
	f.project[ProjectModeRequestKey] = reqJSON(t, MyPC, t0)
	m, _ := handshakeMgr(f, Full)
	tick(m, 0, true)
	st := tick(m, RequestMaxAge+time.Minute+YieldDebounce, false)
	tick(m, RequestMaxAge+time.Minute+2*YieldDebounce, false)
	if st.Chosen != Full || f.project[ProjectModeRequestKey] != "" {
		t.Fatalf("%+v request=%q", st, f.project[ProjectModeRequestKey])
	}
}

func TestLastChoiceWinsByTime(t *testing.T) {
	f := newFake()
	m, _ := handshakeMgr(f, Full)
	m.SetChosenAt(MyPC, t0) // a click at t0
	f.project[ProjectModeRequestKey] = reqJSON(t, Shared, t0.Add(-time.Second))
	if st := tick(m, time.Second, false); st.Chosen != MyPC {
		t.Fatalf("an older request beat a newer click: %+v", st)
	}
	if f.project[ProjectModeRequestKey] != "" {
		t.Fatal("the losing request stays")
	}
	f.project[ProjectModeRequestKey] = reqJSON(t, Shared, t0.Add(2*time.Second))
	if st := tick(m, 3*time.Second, false); st.Chosen != Shared {
		t.Fatalf("a newer request lost to an older click: %+v", st)
	}
}

func TestClickIsStoredInTheProject(t *testing.T) {
	f := newFake()
	m, _ := handshakeMgr(f, Full)
	m.SetChosenAt(Shared, t0)
	tick(m, 0, false)
	if v := storedChoice(t, f); v.Mode != Shared || !v.At.Equal(t0) {
		t.Fatalf("stored %+v", v)
	}
}

func TestUnreadableOrUnknownRequestIsCleared(t *testing.T) {
	for _, raw := range []string{"not json", `{"mode":"turbo","at":"2026-01-01T11:59:00Z","by":"x"}`} {
		f := newFake()
		f.project[ProjectModeRequestKey] = raw
		m, _ := handshakeMgr(f, Full)
		if st := tick(m, 0, false); st.Chosen != Full || f.project[ProjectModeRequestKey] != "" {
			t.Fatalf("%q: %+v request=%q", raw, st, f.project[ProjectModeRequestKey])
		}
	}
}

// What the headless enforcer chooses survives its hand-over to the tray, and
// the other way round: a fresh manager adopts the stored pick before applying.
func TestChosenModeIsTheProjectsAcrossEnforcers(t *testing.T) {
	f := newFake()
	headless, _ := handshakeMgr(f, Full) // Full is its default, not a choice
	f.project[ProjectModeRequestKey] = reqJSON(t, MyPC, t0.Add(-time.Minute))
	if st := tick(headless, 0, false); st.Chosen != MyPC || st.Effective != MyPC {
		t.Fatalf("headless: %+v", st)
	}
	tray, saved := handshakeMgr(f, Shared) // its boxtray.json says Shared
	tray.SeedChosen = true
	if st := tick(tray, time.Minute, false); st.Chosen != MyPC {
		t.Fatalf("tray did not adopt the project's pick: %+v", st)
	}
	if len(*saved) != 1 || (*saved)[0] != MyPC {
		t.Fatalf("tray persisted %v", *saved)
	}
	tray.SetChosenAt(Full, t0.Add(2*time.Minute)) // Tyler clicks, then logs off
	tick(tray, 3*time.Minute, false)
	headless2, _ := handshakeMgr(f, Full)
	if st := tick(headless2, 4*time.Minute, false); st.Chosen != Full {
		t.Fatalf("headless after logoff: %+v", st)
	}
}

func TestNothingIsStoredUntilSomeoneChooses(t *testing.T) {
	f := newFake()
	m, _ := handshakeMgr(f, Full) // headless: a default, not a choice
	tick(m, 0, false)
	if f.project[ProjectChosenKey] != "" {
		t.Fatalf("a default was stored: %q", f.project[ProjectChosenKey])
	}
	tray, _ := handshakeMgr(f, Shared)
	tray.SeedChosen = true // the tray's saved pick seeds an empty project
	tick(tray, time.Minute, false)
	if v := storedChoice(t, f); v.Mode != Shared {
		t.Fatalf("stored %+v", v)
	}
}

// A new enforcer applies nothing until it has read the project's chosen mode.
func TestNothingIsAppliedUntilTheChosenModeWasRead(t *testing.T) {
	f := newFake("a", "Running")
	f.project[ProjectChosenKey] = `{"mode":"mypc","at":"2026-01-01T11:00:00Z"}`
	f.failOn = "project get"
	m, _ := handshakeMgr(f, Full)
	if st := tick(m, 0, false); !st.Down || st.Err == nil {
		t.Fatalf("status %+v", st)
	}
	equal(t, f.take(), nil) // Full's default was not applied to the box
	f.failOn = ""
	if st := tick(m, 5*time.Second, false); st.Chosen != MyPC || st.Effective != MyPC {
		t.Fatalf("status %+v", st)
	}
	equal(t, f.take(), []string{"project user.agent-box.mode=mypc", "pause a"})
}

// racy lets a second request arrive while the first is being handled.
type racy struct {
	*fake
	second string
}

func (r *racy) SetProjectConfig(ctx context.Context, k, v string) error {
	err := r.fake.SetProjectConfig(ctx, k, v)
	if k == ProjectChosenKey && r.second != "" {
		r.project[ProjectModeRequestKey], r.second = r.second, ""
	}
	return err
}

func TestASecondRequestArrivingMeanwhileIsNotCleared(t *testing.T) {
	f := newFake()
	f.project[ProjectModeRequestKey] = reqJSON(t, Shared, t0.Add(-2*time.Minute))
	second := reqJSON(t, MyPC, t0.Add(-time.Minute))
	m := New(&racy{f, second}, Full)
	m.Handshake = true
	if st := tick(m, 0, false); st.Chosen != Shared {
		t.Fatalf("first: %+v", st)
	}
	if f.project[ProjectModeRequestKey] != second {
		t.Fatalf("the second request was cleared: %q", f.project[ProjectModeRequestKey])
	}
	if st := tick(m, time.Second, false); st.Chosen != MyPC || f.project[ProjectModeRequestKey] != "" {
		t.Fatalf("second: %+v", st)
	}
}

// Seeding an empty project stores the tray's old pick under the time of
// seeding, but that must not make a slightly older request look outdated.
func TestSeedingDoesNotOutdateARecentRequest(t *testing.T) {
	f := newFake()
	f.project[ProjectModeRequestKey] = reqJSON(t, MyPC, t0.Add(-time.Minute))
	m, _ := handshakeMgr(f, Shared)
	m.SeedChosen = true
	if st := tick(m, 0, false); st.Chosen != MyPC {
		t.Fatalf("%+v", st)
	}
	if v := storedChoice(t, f); v.Mode != MyPC {
		t.Fatalf("stored %+v", v)
	}
}

func TestHandshakeOffTouchesNoRequestKeys(t *testing.T) {
	f := newFake()
	f.project[ProjectModeRequestKey] = reqJSON(t, MyPC, t0)
	m := New(f, Full)
	if st := tick(m, 0, false); st.Chosen != Full || f.project[ProjectModeRequestKey] == "" {
		t.Fatalf("%+v", st)
	}
}
