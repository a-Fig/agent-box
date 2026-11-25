package boxd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// CheckClient says why boxd's incus CLI calls (each job scan, a log read, a
// queued start) would all fail because of boxd's own environment, or nil.
// The incus client writes its config directory on every call: $INCUS_CONF,
// else ~/.config/incus. Under systemd's ProtectHome, /root is read-only, so
// without INCUS_CONF every scan failed with "mkdir /root/.config: read-only
// file system", one log line per box per poll, and nothing said why
// (2026-11-25, a unit installed before INCUS_CONF was added). boxd checks at
// startup and says so once.
func CheckClient(getenv func(string) string, lookPath func(string) (string, error)) error {
	if _, err := lookPath("incus"); err != nil {
		return fmt.Errorf("the incus CLI isn't on PATH: %w", err)
	}
	dir := getenv("INCUS_CONF")
	if dir == "" {
		home := getenv("HOME")
		if home == "" {
			return errors.New("neither INCUS_CONF nor HOME is set, so the incus client has no config directory")
		}
		dir = filepath.Join(home, ".config", "incus")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("the incus client can't make its config directory: %w (set INCUS_CONF to a writable directory, as provision/systemd/agent-box-boxd.service does)", err)
	}
	f, err := os.CreateTemp(dir, ".boxd-check-*")
	if err != nil {
		return fmt.Errorf("the incus client can't write its config directory %s: %w (set INCUS_CONF to a writable directory, as provision/systemd/agent-box-boxd.service does)", dir, err)
	}
	f.Close()
	os.Remove(f.Name())
	return nil
}
