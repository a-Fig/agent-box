package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/update"
)

// trayReleases serves fake boxtray builds: files holding their --version line.
type trayReleases struct {
	latest string
	asks   int
	body   string // the asset, if not the release's own --version line
}

func (f *trayReleases) Latest(context.Context) (update.Release, error) {
	f.asks++
	if f.latest == "" {
		return update.Release{}, errors.New("offline")
	}
	return update.Release{Tag: f.latest}, nil
}

func (f *trayReleases) ByTag(_ context.Context, tag string) (update.Release, error) {
	return update.Release{Tag: tag}, nil
}

func (f *trayReleases) Asset(_ context.Context, rel update.Release, name string) ([]byte, error) {
	if name != trayAsset {
		return nil, fmt.Errorf("asked for %s", name)
	}
	if f.body != "" {
		return []byte(f.body), nil
	}
	return []byte("boxtray " + rel.Tag + " (abc1234)\n"), nil
}

func readVersion(_ context.Context, exe string, args ...string) (string, error) {
	b, err := os.ReadFile(exe)
	return string(b), err
}

func newTestTrayUpdater(t *testing.T, running, onDisk string, gh *trayReleases) (*trayUpdater, *int, *[]string) {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "boxtray.exe")
	os.WriteFile(exe, []byte("boxtray "+onDisk+" (e823842)\n"), 0o755)
	restarts := 0
	var logs []string
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	return &trayUpdater{Exe: exe, Pin: filepath.Join(filepath.Dir(exe), "boxtray-update.json"), Tag: running, GitHub: gh, Run: readVersion,
		Now:     func() time.Time { return now },
		Restart: func() { restarts++ },
		Logf:    func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	}, &restarts, &logs
}

func TestTrayInstallsANewerReleaseAndRestarts(t *testing.T) {
	gh := &trayReleases{latest: "v0.4.0"}
	u, restarts, _ := newTestTrayUpdater(t, "v0.3.0", "v0.3.0", gh)
	u.maintain(context.Background(), true)
	if b, _ := os.ReadFile(u.Exe); !strings.HasPrefix(string(b), "boxtray v0.4.0") {
		t.Fatalf("boxtray.exe = %q", b)
	}
	if b, _ := os.ReadFile(update.OldPath(u.Exe)); !strings.HasPrefix(string(b), "boxtray v0.3.0") {
		t.Fatalf("boxtray.old.exe = %q", b)
	}
	if *restarts != 1 {
		t.Fatalf("restarts %d", *restarts)
	}
	// Within the hour: nothing, not even a look at GitHub.
	u.maintain(context.Background(), true)
	if gh.asks != 1 || *restarts != 1 {
		t.Fatalf("asks %d restarts %d", gh.asks, *restarts)
	}
}

func TestTrayLeavesTheSameOrAnOlderReleaseAlone(t *testing.T) {
	for _, latest := range []string{"v0.3.0", "v0.2.9", "v0.3.1-nightly.20261125.1"} {
		gh := &trayReleases{latest: latest}
		u, restarts, _ := newTestTrayUpdater(t, "v0.3.1", "v0.3.1", gh)
		u.maintain(context.Background(), true)
		if b, _ := os.ReadFile(u.Exe); !strings.HasPrefix(string(b), "boxtray v0.3.1") || *restarts != 0 {
			t.Errorf("latest %s: boxtray.exe %q, restarts %d", latest, b, *restarts)
		}
	}
}

// The enforcer standing aside doesn't ask GitHub, but restarts into a newer boxtray on disk
// that the active one installed.
func TestTrayStandingAsideRestartsIntoANewerBinary(t *testing.T) {
	gh := &trayReleases{latest: "v0.4.0"}
	u, restarts, _ := newTestTrayUpdater(t, "v0.3.0", "v0.4.0", gh)
	u.maintain(context.Background(), false)
	if gh.asks != 0 || *restarts != 1 {
		t.Fatalf("asks %d restarts %d", gh.asks, *restarts)
	}
	u2, restarts2, _ := newTestTrayUpdater(t, "v0.3.0", "v0.3.0", gh)
	u2.maintain(context.Background(), false)
	if *restarts2 != 0 {
		t.Fatal("restarted with nothing newer on disk")
	}
}

func TestTrayRefusesABadDownloadAndDevBuilds(t *testing.T) {
	gh := &trayReleases{latest: "v0.4.0", body: "boxtray v0.3.0 (oops)\n"}
	u, restarts, logs := newTestTrayUpdater(t, "v0.3.0", "v0.3.0", gh)
	u.maintain(context.Background(), true)
	if b, _ := os.ReadFile(u.Exe); !strings.HasPrefix(string(b), "boxtray v0.3.0") || *restarts != 0 {
		t.Fatalf("boxtray.exe %q, restarts %d", b, *restarts)
	}
	if len(*logs) != 1 || !strings.Contains((*logs)[0], "not v0.4.0") {
		t.Fatalf("logs %v", *logs)
	}
	dev, devRestarts, _ := newTestTrayUpdater(t, "", "v0.3.0", &trayReleases{latest: "v0.4.0"})
	dev.maintain(context.Background(), true)
	if *devRestarts != 0 || dev.GitHub.(*trayReleases).asks != 0 {
		t.Fatal("a dev build updated itself")
	}
	var nilUpdater *trayUpdater // AGENT_BOX_NO_UPDATE
	nilUpdater.maintain(context.Background(), true)
}

// A swap by hand (boxtray --update) is noticed on the next tick, not the next hour; an unchanged
// file isn't probed again.
func TestTrayNoticesASwappedFileAtOnce(t *testing.T) {
	gh := &trayReleases{latest: "v0.3.0"}
	u, restarts, _ := newTestTrayUpdater(t, "v0.3.0", "v0.3.0", gh)
	probes := 0
	u.Run = func(ctx context.Context, exe string, args ...string) (string, error) {
		probes++
		return readVersion(ctx, exe, args...)
	}
	u.maintain(context.Background(), false) // the first tick probes
	u.maintain(context.Background(), false)
	if probes != 1 {
		t.Fatalf("probes %d", probes)
	}
	os.WriteFile(u.Exe, []byte("boxtray v0.3.1 (new)\n"), 0o755)
	later := time.Now().Add(time.Minute)
	os.Chtimes(u.Exe, later, later)
	u.maintain(context.Background(), false)
	if probes != 2 || *restarts != 1 || gh.asks != 0 {
		t.Fatalf("probes %d restarts %d asks %d", probes, *restarts, gh.asks)
	}
}

// --update to an older release pins it: the hourly check leaves it until a newer release.
func TestTrayUpdateByHandToAnOlderReleaseStays(t *testing.T) {
	gh := &trayReleases{latest: "v0.4.0"}
	u, restarts, _ := newTestTrayUpdater(t, "v0.4.0", "v0.4.0", gh)
	if err := u.Install(context.Background(), "v0.3.0"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(u.Exe); !strings.HasPrefix(string(b), "boxtray v0.3.0") {
		t.Fatalf("boxtray.exe = %q", b)
	}
	u.Tag = "v0.3.0" // the restarted process
	u.maintain(context.Background(), true)
	if b, _ := os.ReadFile(u.Exe); !strings.HasPrefix(string(b), "boxtray v0.3.0") || *restarts != 0 {
		t.Fatalf("the hourly check undid it: %q, restarts %d", b, *restarts)
	}
	gh.latest = "v0.4.1"
	u.next = time.Time{}
	u.maintain(context.Background(), true)
	if b, _ := os.ReadFile(u.Exe); !strings.HasPrefix(string(b), "boxtray v0.4.1") || *restarts != 1 {
		t.Fatalf("a newer release wasn't installed: %q, restarts %d", b, *restarts)
	}
}
