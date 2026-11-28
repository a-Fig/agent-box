package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	hostKindModal     = "modal"
	cloudModeFunction = "function"
	cloudModeSandbox  = "sandbox"
)

type fleetHost struct {
	Name         string `json:"name"`
	Remote       string `json:"remote"`
	Project      string `json:"project"`
	DashboardURL string `json:"dashboard_url"`
	Priority     int    `json:"priority,omitempty"`
	Kind         string `json:"kind,omitempty"`
	Endpoint     string `json:"endpoint,omitempty"`
	TokenFile    string `json:"token_file,omitempty"`
	Mode         string `json:"mode,omitempty"`
}

type rawFleetHost struct {
	Name         string `json:"name"`
	Remote       string `json:"remote"`
	Project      string `json:"project"`
	DashboardURL string `json:"dashboard_url"`
	Priority     *int   `json:"priority,omitempty"`
	Kind         string `json:"kind,omitempty"`
	Endpoint     string `json:"endpoint,omitempty"`
	TokenFile    string `json:"token_file,omitempty"`
	Mode         string `json:"mode,omitempty"`
}

// configuredHosts reads agent-box/hosts.json in the user's config directory.
// An absent config directory, file, or host list leaves single-host defaults.
func (a *App) configuredHosts() ([]fleetHost, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, nil
	}
	configDir := filepath.Join(dir, "agent-box")
	path := filepath.Join(configDir, "hosts.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var config struct {
		Hosts []rawFleetHost `json:"hosts"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	hosts := make([]fleetHost, len(config.Hosts))
	names := make(map[string]bool, len(config.Hosts))
	priorities := make(map[string]int, len(config.Hosts))
	for i, raw := range config.Hosts {
		host := fleetHost{
			Name:         strings.TrimSpace(raw.Name),
			Remote:       strings.TrimSpace(raw.Remote),
			Project:      strings.TrimSpace(raw.Project),
			DashboardURL: raw.DashboardURL,
			Kind:         strings.TrimSpace(raw.Kind),
			Endpoint:     strings.TrimSpace(raw.Endpoint),
			TokenFile:    strings.TrimSpace(raw.TokenFile),
			Mode:         strings.TrimSpace(raw.Mode),
		}
		if host.Kind == "" {
			if host.Name == "" || host.Remote == "" {
				return nil, fmt.Errorf("%s: host %d requires a name and remote", path, i+1)
			}
			if host.Project == "" {
				host.Project = a.Project
			}
			if raw.Priority != nil {
				host.Priority = *raw.Priority
			}
		} else {
			if host.Name == "" {
				return nil, fmt.Errorf("%s: host %d requires a name", path, i+1)
			}
			if host.Kind != hostKindModal {
				return nil, fmt.Errorf("%s: host %d unknown kind %q", path, i+1, host.Kind)
			}
			if host.Remote != "" || host.Project != "" {
				return nil, fmt.Errorf("%s: host %d cloud host cannot set remote or project", path, i+1)
			}
			if !validCloudEndpoint(host.Endpoint) {
				return nil, fmt.Errorf("%s: host %d endpoint must be an absolute https:// URL (or http:// on loopback) without credentials, query, or fragment", path, i+1)
			}
			if host.TokenFile == "" {
				return nil, fmt.Errorf("%s: host %d cloud host requires token_file", path, i+1)
			}
			if !filepath.IsAbs(host.TokenFile) {
				host.TokenFile = filepath.Join(configDir, host.TokenFile)
			}
			if host.Mode == "" {
				host.Mode = cloudModeFunction
			} else if host.Mode != cloudModeFunction && host.Mode != cloudModeSandbox {
				return nil, fmt.Errorf("%s: host %d cloud mode must be %q or %q", path, i+1, cloudModeFunction, cloudModeSandbox)
			}
			if raw.Priority != nil {
				host.Priority = *raw.Priority
			} else {
				// Cost-safety default: overflow cloud hosts never beat owned priority-0 hardware unless configured.
				host.Priority = -1
			}
		}
		if names[host.Name] {
			return nil, fmt.Errorf("%s: duplicate host name %q", path, host.Name)
		}
		names[host.Name] = true
		if host.DashboardURL != "" && !validDashboardURL(host.DashboardURL) {
			return nil, fmt.Errorf("%s: host %d dashboard_url must be an absolute HTTP(S) URL without credentials, query, or fragment", path, i+1)
		}
		key := "incus:" + host.Remote + "\x00" + host.Project
		targetDesc := host.Remote + "/" + host.Project
		if host.Kind != "" {
			ep := strings.TrimRight(host.Endpoint, "/")
			key = "cloud:" + host.Kind + "\x00" + ep
			targetDesc = ep
		}
		if prev, ok := priorities[key]; ok && prev != host.Priority {
			return nil, fmt.Errorf("%s: conflicting priorities for %s (%d and %d)", path, targetDesc, prev, host.Priority)
		}
		priorities[key] = host.Priority
		hosts[i] = host
	}
	return hosts, nil
}

func validDashboardURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" &&
		u.User == nil && u.RawQuery == "" && !u.ForceQuery && !strings.Contains(raw, "#")
}

func validCloudEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme == "http" {
		h := u.Hostname()
		return h == "127.0.0.1" || strings.EqualFold(h, "localhost") || h == "::1"
	}
	return false
}
