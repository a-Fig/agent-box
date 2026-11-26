package desktop

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The launcher starts the window as any other app starts: on Windows, a Start menu shortcut whose
// taskbar button can be pinned. `box app` puts it in place each time it runs, so it follows box if
// box moves. It points at the installed box rather than at a copy, so box's self-update, which
// replaces the binary where it is, updates the window too. Ported from Murmur's app
// (internal/desktop/launcher.go there).

// AppID names the window to the system: its AppUserModelID on Windows, which the shortcut and the
// window's process both carry, so Windows counts the window as the shortcut's.
const AppID = "dev.afig.agentbox"

// launcherVersion goes up when what a launcher holds changes, so every existing one is made again.
const launcherVersion = 1

// stamp records the launcher last put in place, so `box app` does nothing when it is already
// right, and a shortcut of the same name that box didn't make is never taken for box's: reading
// a shortcut back would take COM.
type stamp struct {
	Version int    `json:"version"`
	Exe     string `json:"exe"`
	Where   string `json:"where"`
}

const stampFile = "launcher.json"

// InstallLauncher puts the launcher for exe, the installed box, in place, or moves it onto exe.
// dir is box's directory in the user's config directory, which keeps the record and the icon the
// shortcut shows. It says where the launcher is, and whether it made or changed it now. It leaves
// alone a launcher of the same name that isn't box's, one the person took out (until box moves),
// and everything when exe is a build in a temporary directory, as `go run` makes.
func InstallLauncher(exe, dir string) (where string, changed bool, err error) {
	want := stamp{Version: launcherVersion, Exe: exe, Where: launcherPath()}
	if want.Where == "" || devBuild(exe) {
		return "", false, nil
	}
	had := readStamp(dir)
	switch {
	case had == want && exists(want.Where):
		return want.Where, false, nil
	case had == want:
		return "", false, nil // taken out by the person since it was put there
	case exists(want.Where) && had.Where != want.Where:
		return "", false, fmt.Errorf("%s is there already, and isn't box's", want.Where)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false, err
	}
	if err := installLauncher(exe, dir, want.Where); err != nil {
		return "", false, err
	}
	b, _ := json.Marshal(want)
	if err := os.WriteFile(filepath.Join(dir, stampFile), b, 0o600); err != nil {
		return "", false, err
	}
	return want.Where, true, nil
}

func readStamp(dir string) stamp {
	var s stamp
	if b, err := os.ReadFile(filepath.Join(dir, stampFile)); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

// devBuild says exe is a build in a temporary directory, which a launcher mustn't point at: it
// goes when its directory is cleaned up. A variable, so a test can build a launcher for one.
var devBuild = func(exe string) bool {
	tmp := filepath.Clean(os.TempDir()) + string(filepath.Separator)
	return strings.HasPrefix(strings.ToLower(filepath.Clean(exe)), strings.ToLower(tmp)) || strings.Contains(exe, "go-build")
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// icoFrom makes an .ico holding the PNGs as they are, which Windows has read since Vista.
func icoFrom(pngs ...[]byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, [3]uint16{0, 1, uint16(len(pngs))})
	offset := 6 + 16*len(pngs)
	for _, p := range pngs {
		w, h := pngSize(p)
		entry := struct {
			W, H, Colors, Reserved uint8
			Planes, Bits           uint16
			Size, Offset           uint32
		}{uint8(w % 256), uint8(h % 256), 0, 0, 1, 32, uint32(len(p)), uint32(offset)}
		binary.Write(&b, binary.LittleEndian, entry)
		offset += len(p)
	}
	for _, p := range pngs {
		b.Write(p)
	}
	return b.Bytes()
}

// pngSize reads a PNG's width and height from its header.
func pngSize(p []byte) (w, h int) {
	if len(p) < 24 {
		return 0, 0
	}
	return int(binary.BigEndian.Uint32(p[16:20])), int(binary.BigEndian.Uint32(p[20:24]))
}
