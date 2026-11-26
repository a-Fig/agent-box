package mode

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReaperShellParsing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("reaper.sh runs in the Linux distro")
	}
	// A malformed config value is still part of one instance's record.
	const records = `[
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
	for _, tc := range []struct {
		name, list, status string
		fail, dryRun       bool
		deleted            string
	}{
		{name: "expiry and hold decisions", list: records, deleted: "plain\njunk\noffset\npasthold\nmultiline\n"},
		{name: "dry run", list: records, dryRun: true},
		{name: "list failure with valid output", list: records, status: "1", fail: true},
		{name: "truncated JSON", list: records[:len(records)-1], fail: true},
		{name: "invalid record after expired one", list: `[{"name":"plain","config":{"user.expires":"2020-01-01T00:00:00Z"}},null]`, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			list := filepath.Join(dir, "list.json")
			log := filepath.Join(dir, "deleted")
			if err := os.WriteFile(list, []byte(tc.list), 0o644); err != nil {
				t.Fatal(err)
			}
			const fake = `#!/bin/sh
case "$*" in
    "list --project test -f json") cat "$REAPER_LIST"; exit "${REAPER_STATUS:-0}" ;;
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
			t.Setenv("REAPER_DELETES", log)
			t.Setenv("REAPER_STATUS", tc.status)
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
