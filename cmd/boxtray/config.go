package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/a-Fig/agent-box/internal/mode"
)

// config is the tray's only persisted setting: the mode Tyler picked.
type config struct {
	Mode mode.Mode `json:"mode"`
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

// saveMode writes the mode atomically.
func saveMode(path string, m mode.Mode) error {
	b, err := json.Marshal(config{Mode: m})
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
