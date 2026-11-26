package mode

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// provision/reaper.sh reads user.expires and the hold the way Held does: only
// RFC3339 UTC counts, Incus's csv quotes are stripped, and a malformed hold
// is ignored, never taken for a future time. Run against a fake incus.
func TestReaperShellParsing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("reaper.sh runs in the Linux distro")
	}
	dir := t.TempDir()
	list := filepath.Join(dir, "list.csv")
	log := filepath.Join(dir, "deleted")
	fake := "#!/bin/sh\nif [ \"$1\" = list ]; then cat '" + list + "'; else echo \"$@\" >> '" + log + "'; fi\n"
	if err := os.WriteFile(filepath.Join(dir, "incus"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	// All expiries are long past; only a valid, future, UTC hold keeps a box.
	rows := []string{
		"plain,2020-01-01T00:00:00Z,",
		"held,2020-01-01T00:00:00Z,2999-01-01T00:00:00Z",
		"quoted,2020-01-01T00:00:00Z,\"2999-01-01T00:00:00Z\"",
		"fraction,2020-01-01T00:00:00Z,2999-01-01T00:00:00.5Z",
		"junk,2020-01-01T00:00:00Z,tomorrow",
		"offset,2020-01-01T00:00:00Z,2999-01-01T00:00:00+02:00",
		"spaced,2020-01-01T00:00:00Z,2999-01-01 00:00:00Z",
		"relative,2020-01-01T00:00:00Z,next year",
		"badexp,whenever,2999-01-01T00:00:00Z",
		"noexp,,",
		"pasthold,2020-01-01T00:00:00Z,2021-01-01T00:00:00Z",
	}
	if err := os.WriteFile(list, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "../../provision/reaper.sh")
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("reaper.sh: %v\n%s", err, out)
	}
	b, _ := os.ReadFile(log)
	var got []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		got = append(got, l[strings.LastIndex(l, " ")+1:])
	}
	equal(t, got, []string{"plain", "junk", "offset", "spaced", "relative", "pasthold"})
}
