package desktop

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePNG is a PNG's signature and IHDR, enough for pngSize.
func fakePNG(w, h uint32) []byte {
	b := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR")
	b = binary.BigEndian.AppendUint32(b, w)
	return binary.BigEndian.AppendUint32(b, h)
}

func TestIcoHoldsEachPNG(t *testing.T) {
	big, small := fakePNG(256, 256), fakePNG(32, 32)
	ico := icoFrom(big, small)
	if got := binary.LittleEndian.Uint16(ico[4:]); got != 2 {
		t.Fatalf("%d images, want 2", got)
	}
	// The first entry: 256 px is written as 0, then its size and where it starts.
	if ico[6] != 0 || ico[7] != 0 || binary.LittleEndian.Uint32(ico[14:]) != uint32(len(big)) || binary.LittleEndian.Uint32(ico[18:]) != 6+32 {
		t.Fatalf("first entry: % x", ico[6:22])
	}
	if ico[22] != 32 || binary.LittleEndian.Uint32(ico[34:]) != uint32(6+32+len(big)) {
		t.Fatalf("second entry: % x", ico[22:38])
	}
	if !bytes.HasSuffix(ico, append(append([]byte{}, big...), small...)) {
		t.Fatal("the PNGs aren't at the end, in order")
	}
}

// testHome points the Start menu at a temporary directory, never the real one, and lets a launcher
// point at a build in one, which is what the test has.
func testHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	want := launcherPath()
	if want == "" {
		t.Skip("this build has no launcher")
	}
	if !strings.HasPrefix(want, home) {
		t.Fatalf("the launcher would go to %s, outside the test's home", want)
	}
	was := devBuild
	devBuild = func(string) bool { return false }
	t.Cleanup(func() { devBuild = was })
}

// The launcher is put in place once, left as it is while it is right, and made again when box
// moves.
func TestInstallLauncher(t *testing.T) {
	testHome(t)
	dir := t.TempDir()
	exe := filepath.Join(t.TempDir(), "box.exe")
	os.WriteFile(exe, nil, 0o755)

	where, changed, err := InstallLauncher(exe, dir)
	if err != nil || !changed || !exists(where) {
		t.Fatalf("first install: %s, changed %v, %v", where, changed, err)
	}
	if _, changed, err := InstallLauncher(exe, dir); err != nil || changed {
		t.Fatalf("second install changed it (%v) or failed: %v", changed, err)
	}
	moved := filepath.Join(t.TempDir(), "box.exe")
	os.WriteFile(moved, nil, 0o755)
	if got, changed, err := InstallLauncher(moved, dir); err != nil || !changed || got != where {
		t.Fatalf("install after box moved: %s, changed %v, %v", got, changed, err)
	}
}

// A launcher the person took out stays out, until box moves; one of the same name that isn't
// box's is left as it is; a build in a temporary directory gets none.
func TestLauncherLeavesWhatIsntOurs(t *testing.T) {
	testHome(t)
	dir := t.TempDir()
	exe := filepath.Join(t.TempDir(), "box.exe")
	os.WriteFile(exe, nil, 0o755)

	where, _, err := InstallLauncher(exe, dir)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(where)
	if got, changed, err := InstallLauncher(exe, dir); got != "" || changed || err != nil || exists(where) {
		t.Fatalf("a launcher the person took out came back: %s, %v, %v", got, changed, err)
	}

	os.Remove(filepath.Join(dir, stampFile))
	os.WriteFile(where, []byte("someone else's"), 0o644)
	if _, _, err := InstallLauncher(exe, dir); err == nil {
		t.Fatal("put box's launcher over someone else's")
	}
	if b, _ := os.ReadFile(where); string(b) != "someone else's" {
		t.Fatal("someone else's launcher changed")
	}

	os.Remove(where)
	devBuild = func(string) bool { return true }
	if got, _, _ := InstallLauncher(exe, dir); got != "" || exists(where) {
		t.Fatal("a temporary build got a launcher")
	}
}
