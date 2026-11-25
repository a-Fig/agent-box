package boxd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckClient(t *testing.T) {
	dir := t.TempDir()
	found := func(string) (string, error) { return "/usr/bin/incus", nil }
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	if err := CheckClient(env(map[string]string{"INCUS_CONF": filepath.Join(dir, "conf")}), found); err != nil {
		t.Fatalf("a writable INCUS_CONF: %v", err)
	}
	if err := CheckClient(env(map[string]string{"HOME": dir}), found); err != nil {
		t.Fatalf("a writable HOME: %v", err)
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, ".config", "incus")); len(ents) != 0 {
		t.Fatalf("left files behind: %v", ents)
	}
	// A config directory that can't be made, as under a read-only /root.
	blocker := filepath.Join(dir, "file")
	os.WriteFile(blocker, nil, 0o600)
	err := CheckClient(env(map[string]string{"HOME": blocker}), found)
	if err == nil || !strings.Contains(err.Error(), "INCUS_CONF") {
		t.Fatalf("unwritable home: %v", err)
	}
	if err := CheckClient(env(nil), found); err == nil {
		t.Fatal("no INCUS_CONF or HOME: no error")
	}
	missing := func(string) (string, error) { return "", errors.New("not found") }
	if err := CheckClient(env(map[string]string{"INCUS_CONF": dir}), missing); err == nil || !strings.Contains(err.Error(), "PATH") {
		t.Fatalf("no incus: %v", err)
	}
}
