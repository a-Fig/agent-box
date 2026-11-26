package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type fleetHost struct {
	Name         string `json:"name"`
	Remote       string `json:"remote"`
	Project      string `json:"project"`
	DashboardURL string `json:"dashboard_url"`
}

// configuredHosts reads agent-box/hosts.json in the user's config directory.
// An absent file or empty list leaves the caller on the single-host defaults.
func (a *App) configuredHosts() ([]fleetHost, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("find host config directory: %w", err)
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
		if host.Name == "" || host.Remote == "" {
			return nil, fmt.Errorf("%s: host %d requires a name and remote", path, i+1)
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
