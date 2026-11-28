package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/a-Fig/agent-box/internal/mode"
)

// config is the tray's persisted settings: the mode Tyler picked, which the tray writes, and an
// optional dashboard address, which only a person writes (dashboard.go).
type config struct {
	Mode         mode.Mode `json:"mode"`
	DashboardURL string    `json:"dashboard_url,omitempty"`
}

// configPath is %APPDATA%\agent-box\boxtray.json on Windows.
func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agent-box", "boxtray.json"), nil
}

// loadMode returns the saved mode. A missing, corrupt or unknown value gives
// My PC, the safe choice on a gaming machine.
func loadMode(path string) mode.Mode {
	b, err := os.ReadFile(path)
	if err != nil {
		return mode.MyPC
	}
	var c config
	if json.Unmarshal(b, &c) != nil {
		return mode.MyPC
	}
	if m, ok := mode.Parse(string(c.Mode)); ok {
		return m
	}
	return mode.MyPC
}

// loadDashboardURL returns boxtray.json's dashboard_url, or "" when it has none.
func loadDashboardURL(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var c config
	if json.Unmarshal(b, &c) != nil {
		return ""
	}
	return c.DashboardURL
}

var saveMu sync.Mutex // a click and an adopted request may save at once

// saveMode writes the mode atomically, keeping every other key the file has.
func saveMode(path string, m mode.Mode) error {
	saveMu.Lock()
	defer saveMu.Unlock()
	keys := map[string]json.RawMessage{}
	if b, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(b, &keys) != nil {
			keys = map[string]json.RawMessage{} // a corrupt file is replaced, as before
		}
	}
	mb, err := json.Marshal(m)
	if err != nil {
		return err
	}
	keys["mode"] = mb
	b, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
