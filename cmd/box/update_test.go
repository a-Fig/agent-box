package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/update"
	"github.com/a-Fig/agent-box/skills"
)

// fakeReleases serves releases whose box asset is a fake binary: a file holding its --version
// line, which fakeRun prints.
type fakeReleases struct {
	latest string
	asks   int
	body   map[string]string // tag -> the asset's content, if not the usual
}

func (f *fakeReleases) Latest(ctx context.Context) (update.Release, error) {
	f.asks++
	if f.latest == "" {
		return update.Release{}, errors.New("offline")
	}
	return update.Release{Tag: f.latest}, nil
}

func (f *fakeReleases) ByTag(ctx context.Context, tag string) (update.Release, error) {
	return update.Release{Tag: tag}, nil
}

func (f *fakeReleases) Asset(ctx context.Context, rel update.Release, name string) ([]byte, error) {
	if b, ok := f.body[rel.Tag]; ok {
		return []byte(b), nil
	}
	return []byte("box " + rel.Tag + " (abc1234)\n"), nil
}

type fakeBox struct {
	t     *testing.T
	calls []string
}

// run is updater.Run: --version prints the file; skill-sync prints the skill's sum or records.
func (b *fakeBox) run(ctx context.Context, exe string, args ...string) (string, error) {
	b.calls = append(b.calls, filepath.Base(exe)+" "+strings.Join(args, " "))
	content, err := os.ReadFile(exe)
	if err != nil {
		return "", err
	}
	switch {
	case args[0] == "--version":
		return string(content), nil
	case args[0] == "skill-sync" && len(args) == 1:
		sum := sha256.Sum256(content) // stands in for that binary's skill
		return hex.EncodeToString(sum[:]) + "\n", nil
	}
	return "", nil
}

func newTestUpdater(t *testing.T, installed string, gh *fakeReleases) (*updater, *fakeBox, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "bin", "box.exe")
	os.MkdirAll(filepath.Dir(exe), 0o755)
	os.WriteFile(exe, []byte(installed), 0o755)
	fb := &fakeBox{t: t}
	var out bytes.Buffer
	now := time.Now() // the lock compares it with a real file time
	return &updater{Exe: exe, Dir: filepath.Join(dir, "state"), GitHub: gh, Asset: "box-windows-amd64.exe",
		Now: func() time.Time { return now }, Run: fb.run, Out: &out}, fb, &out
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUpdateSwapsInTheLatestAndKeepsOld(t *testing.T) {
	gh := &fakeReleases{latest: "v0.3.1"}
	u, fb, out := newTestUpdater(t, "box v0.3.0 (e823842)\n", gh)
	if err := u.Update(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if got := read(t, u.Exe); !strings.HasPrefix(got, "box v0.3.1") {
		t.Fatalf("box.exe = %q", got)
	}
	if got := read(t, u.oldPath()); !strings.HasPrefix(got, "box v0.3.0") || filepath.Base(u.oldPath()) != "box.old.exe" {
		t.Fatalf("%s = %q", u.oldPath(), got)
	}
	// The skill is synced by the new box, from the old box's skill.
	oldSum := sha256.Sum256([]byte("box v0.3.0 (e823842)\n"))
	if last := fb.calls[len(fb.calls)-1]; last != "box.exe skill-sync "+hex.EncodeToString(oldSum[:]) {
		t.Fatalf("calls %v", fb.calls)
	}
	if !strings.Contains(out.String(), "from v0.3.0 to v0.3.1") {
		t.Fatalf("out %q", out)
	}
	if entries, _ := os.ReadDir(filepath.Dir(u.Exe)); len(entries) != 2 {
		t.Fatalf("left behind: %v", entries)
	}
	// Again: up to date, nothing swapped.
	out.Reset()
	if err := u.Update(context.Background(), ""); err != nil || !strings.Contains(out.String(), "up to date") {
		t.Fatalf("%v %q", err, out)
	}
}

func TestUpdateRefusesABinaryThatIsntTheRelease(t *testing.T) {
	gh := &fakeReleases{latest: "v0.3.1", body: map[string]string{"v0.3.1": "box v0.3.0 (oops)\n"}}
	u, _, _ := newTestUpdater(t, "box v0.3.0 (e823842)\n", gh)
	if err := u.Update(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "not v0.3.1") {
		t.Fatalf("err %v", err)
	}
	if got := read(t, u.Exe); got != "box v0.3.0 (e823842)\n" {
		t.Fatalf("box.exe changed: %q", got)
	}
	if entries, _ := os.ReadDir(filepath.Dir(u.Exe)); len(entries) != 1 {
		t.Fatalf("left behind: %v", entries)
	}
}

func TestUpdateByTagAndDevBuilds(t *testing.T) {
	gh := &fakeReleases{latest: "v0.3.1"}
	u, _, _ := newTestUpdater(t, "box e823842 (2026-11-25 07:12 UTC)\n", gh)
	if err := u.Update(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "dev build") {
		t.Fatalf("a dev build updated itself: %v", err)
	}
	if err := u.Update(context.Background(), "v0.3.2-nightly.20261125.1"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, u.Exe); !strings.HasPrefix(got, "box v0.3.2-nightly.20261125.1") {
		t.Fatalf("box.exe = %q", got)
	}
}

func TestAutoChecksHourlyAndOnlyMovesForward(t *testing.T) {
	gh := &fakeReleases{latest: "v0.3.1"}
	u, _, _ := newTestUpdater(t, "box v0.3.0 (e823842)\n", gh)
	u.auto(context.Background())
	if gh.asks != 1 || !strings.HasPrefix(read(t, u.Exe), "box v0.3.1") {
		t.Fatalf("asks %d, box %q", gh.asks, read(t, u.Exe))
	}
	gh.latest = "v0.3.2"
	u.auto(context.Background()) // within the hour: GitHub isn't asked
	if gh.asks != 1 {
		t.Fatalf("asked again within the hour")
	}
	start := u.Now()
	u.Now = func() time.Time { return start.Add(checkEvery) }
	gh.latest = "v0.3.0" // older than what is installed
	u.auto(context.Background())
	if gh.asks != 2 || !strings.HasPrefix(read(t, u.Exe), "box v0.3.1") {
		t.Fatalf("moved back: %q", read(t, u.Exe))
	}
	if s := u.readStamp(); s.Latest != "v0.3.0" || !s.Checked.Equal(start.Add(checkEvery)) {
		t.Fatalf("stamp %+v", s)
	}
}

func TestAutoWaitsForTheLock(t *testing.T) {
	gh := &fakeReleases{latest: "v0.3.1"}
	u, _, _ := newTestUpdater(t, "box v0.3.0 (e823842)\n", gh)
	unlock, ok := u.lock()
	if !ok {
		t.Fatal("no lock")
	}
	u.auto(context.Background())
	if gh.asks != 0 {
		t.Fatal("checked while another update held the lock")
	}
	unlock()
	// A lock left by a killed update is taken once it is stale.
	p := filepath.Join(u.Dir, "update.lock")
	os.WriteFile(p, nil, 0o644)
	old := u.Now().Add(-lockStale - time.Minute)
	os.Chtimes(p, old, old)
	u.auto(context.Background())
	if gh.asks != 1 {
		t.Fatal("a stale lock blocked the check")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("the lock wasn't released")
	}
}

func TestRollbackSwapsBackAndSkipsThatRelease(t *testing.T) {
	gh := &fakeReleases{latest: "v0.3.1"}
	u, _, out := newTestUpdater(t, "box v0.3.0 (e823842)\n", gh)
	if err := u.Update(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if err := u.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(read(t, u.Exe), "box v0.3.0") || !strings.HasPrefix(read(t, u.oldPath()), "box v0.3.1") {
		t.Fatalf("box %q, old %q", read(t, u.Exe), read(t, u.oldPath()))
	}
	if !strings.Contains(out.String(), "rolled back to v0.3.0; v0.3.1 won't be installed again") {
		t.Fatalf("out %q", out)
	}
	start := u.Now()
	u.Now = func() time.Time { return start.Add(2 * checkEvery) }
	u.auto(context.Background()) // v0.3.1 is still the latest: skipped
	if !strings.HasPrefix(read(t, u.Exe), "box v0.3.0") {
		t.Fatal("the hourly check put back the release rolled back from")
	}
	gh.latest = "v0.3.2"
	u.Now = func() time.Time { return start.Add(4 * checkEvery) }
	u.auto(context.Background()) // a newer one is installed
	if !strings.HasPrefix(read(t, u.Exe), "box v0.3.2") {
		t.Fatalf("box %q", read(t, u.Exe))
	}
}

func TestSkillSyncLeavesEditedSkillsAlone(t *testing.T) {
	home := t.TempDir()
	var out bytes.Buffer
	c := joinConfig{Home: home, Out: &out}
	paths := c.skillPaths()
	old := "old skill\n"
	os.MkdirAll(filepath.Dir(paths[0]), 0o755)
	os.WriteFile(paths[0], []byte(old), 0o644) // the old release's: updated
	os.MkdirAll(filepath.Dir(paths[1]), 0o755)
	os.WriteFile(paths[1], []byte("old skill, edited\n"), 0o644) // edited: left alone
	sum := sha256.Sum256([]byte(old))
	if err := skillSync(c, "1111", hex.EncodeToString(sum[:])); err != nil { // any remembered skill
		t.Fatal(err)
	}
	if read(t, paths[0]) != skills.Box || read(t, paths[1]) != "old skill, edited\n" {
		t.Fatalf("skills: %q / %q", read(t, paths[0])[:20], read(t, paths[1]))
	}
	if !strings.Contains(out.String(), "skill: updated "+paths[0]) || strings.Contains(out.String(), paths[1]) {
		t.Fatalf("out %q", out.String())
	}
}

func TestBehind(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "jobs")
	u := &updater{Dir: filepath.Dir(stateDir)}
	u.writeStamp(stamp{Latest: "v0.3.1"})
	if got := behind(stateDir, "v0.3.0"); got != "box v0.3.0 is behind v0.3.1: box update" {
		t.Fatalf("behind = %q", got)
	}
	for _, tag := range []string{"v0.3.1", "v0.4.0", ""} {
		if got := behind(stateDir, tag); got != "" {
			t.Errorf("%q: %q", tag, got)
		}
	}
	u.writeStamp(stamp{Latest: "v0.3.1", Skip: "v0.3.1"}) // rolled back from: not nagged about
	if got := behind(stateDir, "v0.3.0"); got != "" {
		t.Errorf("skipped: %q", got)
	}
}

// Installing an older release by hand pins it: the hourly check leaves it until a newer release.
func TestUpdateByHandToAnOlderReleaseStays(t *testing.T) {
	gh := &fakeReleases{latest: "v0.3.1"}
	u, _, out := newTestUpdater(t, "box v0.3.1 (e823842)\n", gh)
	if err := u.Update(context.Background(), "v0.3.0"); err != nil {
		t.Fatal(err)
	}
	if s := u.readStamp(); s.Skip != "v0.3.1" || !strings.Contains(out.String(), "v0.3.1 won't be installed by the hourly check") {
		t.Fatalf("stamp %+v, out %q", s, out)
	}
	start := u.Now()
	u.Now = func() time.Time { return start.Add(2 * checkEvery) }
	u.auto(context.Background())
	if !strings.HasPrefix(read(t, u.Exe), "box v0.3.0") {
		t.Fatalf("the hourly check undid it: %q", read(t, u.Exe))
	}
	if err := u.Update(context.Background(), ""); err != nil || !strings.HasPrefix(read(t, u.Exe), "box v0.3.1") {
		t.Fatalf("box update didn't take the latest: %v %q", err, read(t, u.Exe))
	}
	if s := u.readStamp(); s.Skip != "" {
		t.Fatalf("pin left after box update: %+v", s)
	}
}

// Each update remembers the skill it replaced, and every sync passes all of them, so a config
// that one session can't see is caught up by a session that can.
func TestSkillSyncPassesEveryReplacedSkill(t *testing.T) {
	gh := &fakeReleases{latest: "v0.3.1"}
	u, fb, _ := newTestUpdater(t, "box v0.3.0 (e823842)\n", gh)
	u.Update(context.Background(), "")
	gh.latest = "v0.3.2"
	u.Update(context.Background(), "")
	s := u.readStamp()
	if len(s.Skills) != 2 {
		t.Fatalf("skills %v", s.Skills)
	}
	fb.calls = nil
	u.syncSkill(context.Background())
	if len(fb.calls) != 1 || fb.calls[0] != "box.exe skill-sync "+strings.Join(s.Skills, " ") {
		t.Fatalf("calls %v", fb.calls)
	}
}
