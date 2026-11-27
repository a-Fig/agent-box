package boxd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/a-Fig/agent-box/internal/jobrun"
)

func TestLegacyTimerKeepsProtectedJobsOnRollback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("reaper.sh runs in the Linux distro")
	}
	// A malformed config value is still part of one instance's record.
	records := `[
        {"name":"protected","config":{"user.expires":"` + jobrun.ProtectedExpiry + `"}},
		{"name":"plain","config":{"user.expires":"2020-01-01T00:00:00Z"}},
		{"name":"held","config":{"user.expires":"2020-01-01T00:00:00Z","user.box.hold-until":"2999-01-01T00:00:00Z"}},
		{"name":"fraction","config":{"user.expires":"2020-01-01T00:00:00Z","user.box.hold-until":"2999-01-01T00:00:00.5Z"}},
		{"name":"junk","config":{"user.expires":"2020-01-01T00:00:00Z","user.box.hold-until":"tomorrow"}},
		{"name":"offset","config":{"user.expires":"2020-01-01T00:00:00Z","user.box.hold-until":"2999-01-01T00:00:00+02:00"}},
		{"name":"badexp","config":{"user.expires":"whenever","user.box.hold-until":"2999-01-01T00:00:00Z"}},
		{"name":"noexp","config":{}},
		{"name":"pasthold","config":{"user.expires":"2020-01-01T00:00:00Z","user.box.hold-until":"2021-01-01T00:00:00Z"}},
		{"name":"future","config":{"user.expires":"2999-01-01T00:00:00Z","user.box.hold-until":"2020-01-01T00:00:00Z"}},
		{"name":"multiline","config":{"user.expires":"2020-01-01T00:00:00Z","user.box.hold-until":"bad\nheld,2020-01-01T00:00:00Z,\n"}},
		{"name":"badtype","config":{"user.expires":1}}
	]`
	const expired = `[{"name":"plain","config":{"user.expires":"2020-01-01T00:00:00Z"}}]`
	for _, tc := range []struct {
		name, list, current, status string
		fail, dryRun, old           bool
		deleted                     string
	}{
		{name: "expiry and hold decisions", list: records, deleted: "plain\njunk\noffset\npasthold\nmultiline\n"},
		{name: "rolled-back boxd", list: records, old: true, deleted: "plain\njunk\noffset\npasthold\nmultiline\n"},
		{name: "dry run", list: records, dryRun: true},
		{name: "list failure with valid output", list: records, status: "1", fail: true},
		{name: "truncated JSON", list: records[:len(records)-1], fail: true},
		{name: "invalid record after expired one", list: `[{"name":"plain","config":{"user.expires":"2020-01-01T00:00:00Z"}},null]`, fail: true},
		{name: "setup reserved after list", list: expired, current: `[{"name":"plain","config":{"user.expires":"` + jobrun.ProtectedExpiry + `"}}]`},
		{name: "held after list", list: expired, current: `[{"name":"plain","config":{"user.expires":"2020-01-01T00:00:00Z","user.box.hold-until":"2999-01-01T00:00:00Z"}}]`},
		{name: "recheck unavailable", list: expired, current: "[]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			list := filepath.Join(dir, "list.json")
			log := filepath.Join(dir, "deleted")
			if err := os.WriteFile(list, []byte(tc.list), 0o644); err != nil {
				t.Fatal(err)
			}
			current := list
			if tc.current != "" {
				current = filepath.Join(dir, "current.json")
				if err := os.WriteFile(current, []byte(tc.current), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			const fake = `#!/bin/sh
case "$*" in
    "list --project test -f json") cat "$REAPER_LIST"; exit "${REAPER_STATUS:-0}" ;;
    "query /1.0/instances/"*'?project=test')
        target=${2%%\?*}
        python3 -c '
import json, sys
with open(sys.argv[1]) as f:
    for box in json.load(f):
        if box["name"] == sys.argv[2]:
            print(json.dumps(box))
            sys.exit(0)
sys.exit(1)
' "${REAPER_CURRENT:-$REAPER_LIST}" "${target##*/}"
        ;;
    "delete --force --project test -- "*) printf '%s\n' "$6" >> "$REAPER_DELETES" ;;
    *) exit 2 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "incus"), []byte(fake), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("AGENTS_PROJECT", "test")
			t.Setenv("REAPER_LIST", list)
			t.Setenv("REAPER_CURRENT", current)
			t.Setenv("REAPER_DELETES", log)
			t.Setenv("REAPER_STATUS", tc.status)
			boxd := filepath.Join(dir, "boxd")
			t.Setenv("AGENT_BOX_BOXD", boxd)
			if tc.old {
				if err := os.WriteFile(boxd, []byte("#!/bin/sh\n[ \"$1\" = -help ] || exit 2\necho '  -version' >&2\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("sh", "../../provision/reaper.sh")
			if tc.dryRun {
				cmd.Args = append(cmd.Args, "--dry-run")
			}
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.fail {
				t.Fatalf("reaper.sh: %v\n%s", err, out)
			}
			if tc.dryRun && (!strings.Contains(string(out), "would delete plain") || strings.Contains(string(out), "would delete held")) {
				t.Fatalf("unexpected dry run:\n%s", out)
			}
			b, err := os.ReadFile(log)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if got := string(b); got != tc.deleted {
				t.Fatalf("deleted %q, want %q\n%s", got, tc.deleted, out)
			}
		})
	}
}

func TestReaperUsesCapableBoxdEvenWhenItsProbeFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("reaper.sh runs in the Linux distro")
	}
	boxd := filepath.Join(t.TempDir(), "boxd")
	const fake = `#!/bin/sh
case "$*" in
    -help) printf '  -reap\n' >&2 ;;
    "-reap -project test --dry-run") echo "host probe failed"; exit 7 ;;
    *) exit 2 ;;
esac
`
	if err := os.WriteFile(boxd, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_BOX_BOXD", boxd)
	t.Setenv("AGENTS_PROJECT", "test")
	out, err := exec.Command("sh", "../../provision/reaper.sh", "--dry-run").CombinedOutput()
	if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 7 || string(out) != "host probe failed\n" {
		t.Fatalf("reaper: %v\n%s", err, out)
	}
}

func TestPackagedBoxdInstallerCarriesReaper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("installer runs in the Linux distro")
	}
	want, err := os.ReadFile("../../provision/reaper.sh")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "../../provision/package-boxd-install.sh").CombinedOutput()
	if err != nil {
		t.Fatalf("package installer: %v\n%s", err, out)
	}
	_, body, found := strings.Cut(string(out), "cat <<'AGENT_BOX_REAPER'\n")
	wrapper, _, closed := strings.Cut(body, "AGENT_BOX_REAPER\n")
	if !found || !closed || wrapper != string(want) {
		t.Fatal("the packaged installer did not include the reaper wrapper")
	}
}
