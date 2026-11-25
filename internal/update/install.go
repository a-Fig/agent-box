package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Run runs a binary with args and returns its stdout; Install uses it for the --version probe.
type Run func(ctx context.Context, exe string, args ...string) (string, error)

// Install puts data, the release tag's build of exe, in exe's place. It writes data beside exe,
// runs it with --version and refuses it unless the second word is tag, then renames exe to
// OldPath(exe) and the new file to exe. Windows allows those renames while exe runs, so a
// running program can replace itself. A previous .old that is still running (Windows won't
// delete it) is renamed aside for Sweep. If the last rename fails, exe is put back.
func Install(ctx context.Context, exe, tag string, data []byte, run Run, now time.Time) error {
	dir, ext := filepath.Dir(exe), filepath.Ext(exe)
	Sweep(exe)
	staged := filepath.Join(dir, "."+strings.TrimSuffix(filepath.Base(exe), ext)+"-update-"+tag+ext)
	if err := os.WriteFile(staged, data, 0o755); err != nil {
		return err
	}
	defer os.Remove(staged) // gone already once it is swapped in
	out, err := run(ctx, staged, "--version")
	if f := strings.Fields(out); err != nil || len(f) < 2 || f[1] != tag {
		return fmt.Errorf("the downloaded %s says %q, not %s (%v)", filepath.Base(exe), strings.TrimSpace(out), tag, err)
	}
	old := OldPath(exe)
	if err := os.Remove(old); err != nil && !errors.Is(err, os.ErrNotExist) {
		aside := fmt.Sprintf("%s.old-%d%s", strings.TrimSuffix(exe, ext), now.UnixNano(), ext)
		if err := os.Rename(old, aside); err != nil {
			return fmt.Errorf("can't move %s aside: %w", old, err)
		}
	}
	if err := os.Rename(exe, old); err != nil {
		return err
	}
	if err := os.Rename(staged, exe); err != nil {
		os.Rename(old, exe)
		return err
	}
	return nil
}

// OldPath is where Install keeps the replaced binary: box.exe's is box.old.exe.
func OldPath(exe string) string {
	ext := filepath.Ext(exe)
	return strings.TrimSuffix(exe, ext) + ".old" + ext
}

// Sweep removes the .old-* files Install set aside, once nothing runs them.
func Sweep(exe string) {
	ext := filepath.Ext(exe)
	matches, _ := filepath.Glob(strings.TrimSuffix(exe, ext) + ".old-*" + ext)
	for _, m := range matches {
		os.Remove(m)
	}
}

// LockStale is how old a lock file must be before Lock takes it over: one left by an update that
// was killed.
const LockStale = 10 * time.Minute

// Lock takes the lock file at path, or reports false if another update holds it.
func Lock(path string, now time.Time) (unlock func(), ok bool) {
	os.MkdirAll(filepath.Dir(path), 0o755)
	for range 2 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			return func() { os.Remove(path) }, true
		}
		fi, serr := os.Stat(path)
		if serr != nil || now.Sub(fi.ModTime()) < LockStale {
			return nil, false
		}
		os.Remove(path)
	}
	return nil, false
}

// VersionTag is the release tag in a --version line ("box v0.3.0 (…)"), or "" for a dev build.
func VersionTag(out string) string {
	if f := strings.Fields(out); len(f) > 1 && ValidTag(f[1]) {
		return f[1]
	}
	return ""
}
