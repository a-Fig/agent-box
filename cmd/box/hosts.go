package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/a-Fig/agent-box/internal/controller"
)

type fleetHost struct {
	Name         string `json:"name"`
	Remote       string `json:"remote"`
	Project      string `json:"project"`
	DashboardURL string `json:"dashboard_url"`
	ControllerID string `json:"controller_id,omitempty"`
}

// configuredHosts reads agent-box/hosts.json in the user's config directory.
// An absent config directory, file, or host list leaves single-host defaults.
func (a *App) configuredHosts() ([]fleetHost, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, nil
	}
	path := filepath.Join(dir, "agent-box", "hosts.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var config struct {
		Hosts []fleetHost `json:"hosts"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	names := make(map[string]bool, len(config.Hosts))
	for i := range config.Hosts {
		host := &config.Hosts[i]
		host.Name = strings.TrimSpace(host.Name)
		host.Remote = strings.TrimSpace(host.Remote)
		host.ControllerID = strings.TrimSpace(host.ControllerID)
		if host.Name == "" || host.Remote == "" {
			return nil, fmt.Errorf("%s: host %d requires a name and remote", path, i+1)
		}
		if host.ControllerID != "" && !controller.ValidHostID(host.ControllerID) {
			return nil, fmt.Errorf("%s: host %d controller_id %q must match ^[a-z0-9-]{1,40}$", path, i+1, host.ControllerID)
		}
		if names[host.Name] {
			return nil, fmt.Errorf("%s: duplicate host name %q", path, host.Name)
		}
		names[host.Name] = true
		if host.Project == "" {
			host.Project = a.Project
		}
		if host.DashboardURL != "" && !validDashboardURL(host.DashboardURL) {
			return nil, fmt.Errorf("%s: host %d dashboard_url must be an absolute HTTP(S) URL without credentials, query, or fragment", path, i+1)
		}
	}
	return config.Hosts, nil
}

func validDashboardURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" &&
		u.User == nil && u.RawQuery == "" && !u.ForceQuery && !strings.Contains(raw, "#")
}
