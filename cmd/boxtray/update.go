package main

// boxtray updates itself to the latest stable release (docs/releasing.md, step 4). Once an hour
// the active enforcer, tray or headless, asks GitHub for the latest release and, when it is newer,
// installs boxtray-windows-amd64.exe with internal/update: checked against SHA256SUMS, probed with
// --version, swapped in with the old one kept as boxtray.old.exe. Every boxtray process, active
// or standing aside, stats boxtray.exe on each tick; when the file has changed and is a newer
// release than the process, it starts the new one in its place and exits, so the tray and the
// headless enforcer both run the new code within seconds of a swap, by update or by hand
// (`boxtray.exe --update <tag>`).
//
// The scheduled tasks don't do the restart: Task Scheduler's restart-on-failure doesn't restart a
// program that exits with an error (#97), so the old process starts the new one itself.

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/a-Fig/agent-box/internal/update"
)

const trayCheckEvery = time.Hour

// trayUpdater is one boxtray process's updater. Its fields are what tests replace.
type trayUpdater struct {
	Exe    string // boxtray.exe on disk
	Pin    string // boxtray-update.json: a release the hourly check doesn't install
	Tag    string // this process's release tag; "" (a dev build) never updates
	GitHub interface {
		Latest(context.Context) (update.Release, error)
		ByTag(context.Context, string) (update.Release, error)
		Asset(context.Context, update.Release, string) ([]byte, error)
	}
	Run     update.Run
	Now     func() time.Time
	Restart func() // start the boxtray on disk in this process's place, and exit
	Logf    func(format string, args ...any)
	next    time.Time   // when to ask GitHub next
	seen    os.FileInfo // boxtray.exe as last probed
}

const trayAsset = "boxtray-windows-amd64.exe"

// maintain runs on every tick of the mode loop. The active enforcer asks GitHub at most hourly;
// every process probes boxtray.exe only when the file has changed. active says whether this
// process is the enforcer (the headless one standing aside isn't).
func (u *trayUpdater) maintain(ctx context.Context, active bool) {
	if u == nil || u.Tag == "" {
		return
	}
	fi, err := os.Stat(u.Exe)
	// Not os.SameFile: on Windows a path's os.Stat may load the file ID later, from the new file.
	changed := err == nil && (u.seen == nil || !fi.ModTime().Equal(u.seen.ModTime()) || fi.Size() != u.seen.Size())
	ask := active && !u.Now().Before(u.next)
	if !changed && !ask {
		return
	}
	u.seen = fi
	if ask {
		u.next = u.Now().Add(trayCheckEvery) // before the probe: a dev build on disk probes "" every time
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	disk := u.onDisk(ctx)
	if ask && disk != "" {
		if rel, err := u.GitHub.Latest(ctx); err != nil {
			u.Logf("update: no latest release (%v)", err)
		} else if update.After(rel.Tag, disk) && rel.Tag != u.skip() {
			if err := u.install(ctx, rel); err != nil {
				u.Logf("update to %s: %v", rel.Tag, err)
			} else {
				u.Logf("update: installed %s over %s (the old one is %s)", rel.Tag, disk, update.OldPath(u.Exe))
				disk = rel.Tag
				u.seen, _ = os.Stat(u.Exe)
			}
		}
	}
	// Any other release on disk, older too: --update <older tag> is the rollback, and it rolls back
	// the running processes. It can't loop: the pin keeps the hourly check off the skipped release.
	if disk != "" && disk != u.Tag {
		u.Logf("update: %s is %s, not this process's %s: restarting into it", u.Exe, disk, u.Tag)
		u.Restart()
	}
}

// Install is `boxtray.exe --update <tag>`: that release, a nightly, a test prerelease or an older
// one, put in place by hand. The running tray and headless enforcer restart into it within a tick.
// One older than the latest stable release pins: the hourly check
// skips that stable release until a newer one comes out, as box update --rollback does.
func (u *trayUpdater) Install(ctx context.Context, tag string) error {
	rel, err := u.GitHub.ByTag(ctx, tag)
	if err != nil {
		return err
	}
	if err := u.install(ctx, rel); err != nil {
		return err
	}
	skip := ""
	if latest, err := u.GitHub.Latest(ctx); err == nil && update.After(latest.Tag, rel.Tag) {
		skip = latest.Tag
		u.Logf("--update: the hourly check won't install %s; a newer release or --update %s will", skip, skip)
	}
	b, _ := json.Marshal(map[string]string{"skip": skip})
	return os.WriteFile(u.Pin, b, 0o644)
}

func (u *trayUpdater) skip() string {
	var p struct{ Skip string }
	if b, err := os.ReadFile(u.Pin); err == nil {
		json.Unmarshal(b, &p)
	}
	return p.Skip
}

func (u *trayUpdater) install(ctx context.Context, rel update.Release) error {
	data, err := u.GitHub.Asset(ctx, rel, trayAsset)
	if err != nil {
		return err
	}
	return update.Install(ctx, u.Exe, rel.Tag, data, u.Run, u.Now())
}

// onDisk is the release tag of boxtray.exe on disk, from its --version; "" for a dev build or if
// it can't run.
func (u *trayUpdater) onDisk(ctx context.Context) string {
	out, err := u.Run(ctx, u.Exe, "--version")
	if err != nil {
		return ""
	}
	return update.VersionTag(out)
}
