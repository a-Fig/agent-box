package main

// box updates itself to the latest stable release (docs/releasing.md). box mcp checks at start
// and then hourly; one check an hour runs for all sessions, under a lock file and a stamp beside
// the jobs folder. An update downloads the release's box, checks it against SHA256SUMS, runs it
// with --version, and swaps it in: box.exe becomes box.old.exe and the new file takes its name.
// Windows allows that rename while box.exe runs, and box mcp runs each call through the file on
// disk (mcpexe.go), so running sessions get the new code at their next call. The skill is
// updated with it wherever it is still the old release's. `box update --rollback` swaps back.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/a-Fig/agent-box/internal/update"
	"github.com/a-Fig/agent-box/internal/version"
	"github.com/a-Fig/agent-box/skills"
)

const (
	checkEvery = time.Hour
)

// updater updates one box binary. Its fields are what tests replace.
type updater struct {
	Exe    string // the box binary to update
	Dir    string // where the lock and stamp live
	GitHub interface {
		Latest(context.Context) (update.Release, error)
		ByTag(context.Context, string) (update.Release, error)
		Asset(context.Context, update.Release, string) ([]byte, error)
	}
	Asset string // this platform's asset name
	Now   func() time.Time
	// Run runs a box binary and returns its stdout: --version, and skill-sync.
	Run update.Run
	Out io.Writer
}

func newUpdater(a *App, out io.Writer) (*updater, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	return &updater{Exe: exe, Dir: filepath.Dir(a.StateDir), GitHub: update.Client{}, Asset: assetName(runtime.GOOS, runtime.GOARCH),
		Now: time.Now, Run: runBox, Out: out}, nil
}

func assetName(goos, arch string) string {
	if goos == "windows" {
		return "box-" + goos + "-" + arch + ".exe"
	}
	return "box-" + goos + "-" + arch
}

func runBox(ctx context.Context, exe string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, args...).Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return string(out), err
}

// stamp is update.json: when GitHub was last asked, what it said, a release not to install until
// a newer one comes out (rolled back from, or left by hand), and the SHA-256 of every skill an
// update replaced.
type stamp struct {
	Checked time.Time `json:"checked"`
	Latest  string    `json:"latest,omitempty"`
	Skip    string    `json:"skip,omitempty"`
	Skills  []string  `json:"skills,omitempty"`
}

// rememberSkill adds the SHA-256 of a skill an update replaced to the stamp.
func (u *updater) rememberSkill(sum string) {
	s := u.readStamp()
	if !slices.Contains(s.Skills, sum) {
		s.Skills = append(s.Skills, sum)
		u.writeStamp(s)
	}
}

func (u *updater) stampPath() string { return filepath.Join(u.Dir, "update.json") }

func (u *updater) readStamp() stamp {
	var s stamp
	if b, err := os.ReadFile(u.stampPath()); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

func (u *updater) writeStamp(s stamp) {
	b, _ := json.Marshal(s)
	os.MkdirAll(u.Dir, 0o755)
	tmp := u.stampPath() + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, u.stampPath())
	}
}

// lock takes update.lock, or reports false if another update holds it.
func (u *updater) lock() (unlock func(), ok bool) {
	return update.Lock(filepath.Join(u.Dir, "update.lock"), u.Now())
}

// installed is the release tag of the box binary on disk ("" for a dev build), read from its
// --version, since another session may have updated it since this process started.
func (u *updater) installed(ctx context.Context) string {
	out, err := u.Run(ctx, u.Exe, "--version")
	if err != nil {
		return ""
	}
	return update.VersionTag(out)
}

// auto is box mcp's check: at most hourly across sessions, stable releases only, never older,
// never a release rolled back from, and never a dev build.
func (u *updater) auto(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute) // a stalled download mustn't hold the lock
	defer cancel()
	unlock, ok := u.lock()
	if !ok {
		return
	}
	defer unlock()
	s := u.readStamp()
	if u.Now().Sub(s.Checked) < checkEvery {
		return
	}
	cur := u.installed(ctx)
	if cur == "" {
		return
	}
	rel, err := u.GitHub.Latest(ctx)
	if err != nil {
		return // offline, or GitHub is down: the next check tries again
	}
	s.Checked, s.Latest = u.Now(), rel.Tag
	u.writeStamp(s)
	if rel.Tag == s.Skip || !update.After(rel.Tag, cur) {
		return
	}
	if err := u.install(ctx, rel, cur); err != nil {
		fmt.Fprintf(u.Out, "box: update to %s: %v\n", rel.Tag, err)
	}
}

// Update is `box update [--version tag]`: the latest stable release, or tag (a nightly, or an
// older release by hand).
func (u *updater) Update(ctx context.Context, tag string) error {
	unlock, ok := u.lock()
	if !ok {
		return errors.New("another box is updating; try again in a minute")
	}
	defer unlock()
	cur := u.installed(ctx)
	var rel update.Release
	var err error
	if tag == "" {
		if cur == "" {
			return fmt.Errorf("%s is a dev build, which doesn't update itself; pass --version <tag> to install a release", u.Exe)
		}
		rel, err = u.GitHub.Latest(ctx)
	} else {
		rel, err = u.GitHub.ByTag(ctx, tag)
	}
	if err != nil {
		return err
	}
	s := u.readStamp()
	if tag == "" {
		s.Checked, s.Latest = u.Now(), rel.Tag
		u.writeStamp(s)
		if !update.After(rel.Tag, cur) {
			fmt.Fprintf(u.Out, "box %s is up to date (latest release %s)\n", cur, rel.Tag)
			return nil
		}
	} else if rel.Tag == cur {
		fmt.Fprintf(u.Out, "box %s is already installed\n", cur)
		return nil
	}
	if err := u.install(ctx, rel, cur); err != nil {
		return err
	}
	// Installing by hand ends a rollback's pin, and makes one of its own when it leaves the
	// latest stable release, so the hourly check doesn't undo it.
	latest := rel.Tag
	if tag != "" {
		if l, err := u.GitHub.Latest(ctx); err == nil {
			latest = l.Tag
		}
	}
	s = u.readStamp()
	s.Skip = ""
	if update.After(latest, rel.Tag) {
		s.Skip = latest
		fmt.Fprintf(u.Out, "box: %s won't be installed by the hourly check; box update installs it\n", latest)
	}
	u.writeStamp(s)
	return nil
}

func (u *updater) oldPath() string { return update.OldPath(u.Exe) }

// install downloads rel's box, checks it, swaps it in and updates the skill.
func (u *updater) install(ctx context.Context, rel update.Release, from string) error {
	data, err := u.GitHub.Asset(ctx, rel, u.Asset)
	if err != nil {
		return err
	}
	was := u.skillSum(ctx)
	if err := update.Install(ctx, u.Exe, rel.Tag, data, u.Run, u.Now()); err != nil {
		return err
	}
	u.rememberSkill(was)
	u.syncSkill(ctx)
	if from == "" {
		from = "a dev build"
	}
	fmt.Fprintf(u.Out, "box: updated %s from %s to %s (the old one is %s; box update --rollback)\n", u.Exe, from, rel.Tag, filepath.Base(u.oldPath()))
	return nil
}

// Rollback is `box update --rollback`: box.old and box trade places, and the release rolled back
// from isn't installed again by the hourly check.
func (u *updater) Rollback(ctx context.Context) error {
	unlock, ok := u.lock()
	if !ok {
		return errors.New("another box is updating; try again in a minute")
	}
	defer unlock()
	old := u.oldPath()
	if _, err := os.Stat(old); err != nil {
		return fmt.Errorf("no %s to roll back to", filepath.Base(old))
	}
	from, was := u.installed(ctx), u.skillSum(ctx)
	tmp := filepath.Join(filepath.Dir(u.Exe), ".box-rollback"+filepath.Ext(u.Exe))
	os.Remove(tmp)
	if err := os.Rename(u.Exe, tmp); err != nil {
		return err
	}
	if err := os.Rename(old, u.Exe); err != nil {
		os.Rename(tmp, u.Exe)
		return err
	}
	if err := os.Rename(tmp, old); err != nil {
		return err
	}
	u.rememberSkill(was)
	s := u.readStamp()
	s.Skip = from
	u.writeStamp(s)
	u.syncSkill(ctx)
	to := u.installed(ctx)
	if to == "" {
		to = "a dev build"
	}
	fmt.Fprintf(u.Out, "box: rolled back to %s; %s won't be installed again until a newer release\n", to, from)
	return nil
}

// skillSum is the SHA-256 of the skill in the box on disk, which `box skill-sync` prints. A box
// from before skill-sync can't say, and its skill is taken to be this process's.
func (u *updater) skillSum(ctx context.Context) string {
	if out, err := u.Run(ctx, u.Exe, "skill-sync"); err == nil {
		if s := strings.TrimSpace(out); len(s) == 64 {
			return s
		}
	}
	return skillSHA()
}

func skillSHA() string {
	sum := sha256.Sum256([]byte(skills.Box))
	return hex.EncodeToString(sum[:])
}

// syncSkill has the box now on disk install its skill wherever the installed skill is one an
// update replaced; an edited skill matches none and is left alone. It runs after each update and
// hourly in every box mcp, since each session sees only its own configs ($CLAUDE_CONFIG_DIR).
func (u *updater) syncSkill(ctx context.Context) {
	known := u.readStamp().Skills
	if len(known) == 0 {
		return
	}
	if out, err := u.Run(ctx, u.Exe, append([]string{"skill-sync"}, known...)...); err != nil {
		fmt.Fprintf(u.Out, "box: skill not updated: %v\n", err)
	} else if out = strings.TrimSpace(out); out != "" {
		fmt.Fprintln(u.Out, out)
	}
}

// skillSync is `box skill-sync <sha256>...`: writes this binary's skill over each installed copy
// whose SHA-256 is one of the given ones.
func skillSync(c joinConfig, was ...string) error {
	var errs []error
	for _, p := range c.skillPaths() {
		b, err := os.ReadFile(p)
		if err != nil {
			continue // not installed there
		}
		sum := sha256.Sum256(b)
		if !slices.Contains(was, hex.EncodeToString(sum[:])) || string(b) == skills.Box {
			continue
		}
		tmp := p + ".tmp"
		if err := os.WriteFile(tmp, []byte(skills.Box), 0o644); err == nil {
			err = os.Rename(tmp, p)
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		fmt.Fprintf(c.Out, "skill: updated %s\n", p)
	}
	return errors.Join(errs...)
}

// autoUpdate runs box mcp's checks until ctx ends, unless AGENT_BOX_NO_UPDATE is set or this is
// a dev build.
func autoUpdate(ctx context.Context, a *App) {
	if os.Getenv("AGENT_BOX_NO_UPDATE") != "" || version.Tag == "" {
		return
	}
	u, err := newUpdater(a, os.Stderr) // stdout is the MCP stream
	if err != nil {
		return
	}
	for {
		u.auto(ctx)
		u.syncSkill(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(checkEvery):
		}
	}
}

// behind is box status's line when a newer stable release is out than this box, as the last
// check saw it.
func behind(stateDir, tag string) string {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(stateDir), "update.json"))
	if err != nil || tag == "" {
		return ""
	}
	var s stamp
	if json.Unmarshal(b, &s) != nil || !update.After(s.Latest, tag) || s.Latest == s.Skip {
		return ""
	}
	return fmt.Sprintf("box %s is behind %s: box update", tag, s.Latest)
}
