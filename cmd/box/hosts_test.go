package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func hostsTestPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("APPDATA", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(configDir, "agent-box", "hosts.json")
}

func TestConfiguredHostsMissing(t *testing.T) {
	hostsTestPath(t)
	t.Setenv("BOX_REMOTE", "existing-remote")
	t.Setenv("BOX_PROJECT", "existing-project")
	a := newApp()
	hosts, err := a.configuredHosts()
	if err != nil || len(hosts) != 0 {
		t.Fatalf("configuredHosts() = %v, %v, want no hosts and no error", hosts, err)
	}
	if a.Remote != "existing-remote" || a.Project != "existing-project" {
		t.Fatalf("default host = %q / %q, want existing-remote / existing-project", a.Remote, a.Project)
	}
}

func TestConfiguredHostsFile(t *testing.T) {
	for _, tc := range []struct {
		name, data, wantErr string
		want                []fleetHost
	}{
		{
			name: "multiple hosts in file order",
			data: `{"hosts":[
				{"name":"primary","remote":"pc","project":"primary-project","dashboard_url":"https://example.com:8443/dashboard","controller_id":"gaming-pc"},
				{"name":"backup","remote":"spare"}
			]}`,
			want: []fleetHost{
				{Name: "primary", Remote: "pc", Project: "primary-project", DashboardURL: "https://example.com:8443/dashboard", ControllerID: "gaming-pc"},
				{Name: "backup", Remote: "spare", Project: "existing-project"},
			},
		},
		{name: "invalid controller_id", data: `{"hosts":[{"name":"primary","remote":"pc","controller_id":"BAD_ID"}]}`, wantErr: "host 1 controller_id"},
		{name: "empty host list", data: `{"hosts":[]}`},
		{name: "malformed JSON", data: `{"hosts":`, wantErr: "parse"},
		{name: "missing name", data: `{"hosts":[{"name":" ","remote":"pc"}]}`, wantErr: "host 1 requires a name and remote"},
		{name: "missing remote", data: `{"hosts":[{"name":"primary","remote":" "}]}`, wantErr: "host 1 requires a name and remote"},
		{name: "duplicate name", data: `{"hosts":[{"name":"primary","remote":"pc"},{"name":"primary","remote":"spare"}]}`, wantErr: `duplicate host name "primary"`},
		{name: "relative dashboard URL", data: `{"hosts":[{"name":"primary","remote":"pc","dashboard_url":"/dash"}]}`, wantErr: "host 1 dashboard_url"},
		{name: "dashboard URL scheme", data: `{"hosts":[{"name":"primary","remote":"pc","dashboard_url":"ftp://example.com"}]}`, wantErr: "host 1 dashboard_url"},
		{name: "dashboard URL missing hostname", data: `{"hosts":[{"name":"primary","remote":"pc","dashboard_url":"https://:8443"}]}`, wantErr: "host 1 dashboard_url"},
		{name: "dashboard URL credentials", data: `{"hosts":[{"name":"primary","remote":"pc","dashboard_url":"https://user@example.com"}]}`, wantErr: "host 1 dashboard_url"},
		{name: "dashboard URL query", data: `{"hosts":[{"name":"primary","remote":"pc","dashboard_url":"https://example.com?q=1"}]}`, wantErr: "host 1 dashboard_url"},
		{name: "dashboard URL fragment", data: `{"hosts":[{"name":"primary","remote":"pc","dashboard_url":"https://example.com/#part"}]}`, wantErr: "host 1 dashboard_url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := hostsTestPath(t)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			a := &App{Project: "existing-project"}
			hosts, err := a.configuredHosts()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), path) || hosts != nil {
					t.Fatalf("configuredHosts() = %v, %v, want no hosts and %q with config path", hosts, err, tc.wantErr)
				}
				return
			}
			if err != nil || !slices.Equal(hosts, tc.want) {
				t.Fatalf("configuredHosts() = %v, %v, want %v", hosts, err, tc.want)
			}
		})
	}
}
