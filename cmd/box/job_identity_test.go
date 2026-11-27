package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testBareJobID = "repo.20261126-223849-280c"

func requireQualifiedJobID(t *testing.T, host fleetHost, bare string) jobID {
	t.Helper()
	id, err := qualifyJobID(host, bare)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestQualifiedJobID(t *testing.T) {
	host := fleetHost{Name: "West Coast / 温", Remote: "west", Project: "agents", DashboardURL: "https://example.com/dashboard"}
	id := requireQualifiedJobID(t, host, testBareJobID)
	const want = "h1-230811a1fc37a254d30a5e16d2edf57b~" + testBareJobID
	if id.ID != want || id.Bare != testBareJobID || id.Box != "repo" {
		t.Fatalf("qualified job = %+v, want %s with bare ID %s", id, want, testBareJobID)
	}
	if parsed, err := parseJobID(id.ID); err != nil || parsed != id {
		t.Fatalf("parseJobID(%q) = %+v, %v, want %+v", id.ID, parsed, err, id)
	}
	updated := host
	updated.Name, updated.DashboardURL = "Renamed", "https://example.net/dashboard"
	if other := requireQualifiedJobID(t, updated, testBareJobID); other != id {
		t.Fatalf("changing display fields changed identity to %+v", other)
	}
	if got, err := id.resolveHost(fleetHost{}, []fleetHost{updated}); err != nil || got != updated {
		t.Fatalf("renamed host resolves to %+v, %v, want %+v", got, err, updated)
	}
}

func TestQualifiedJobIDFitsStateFilename(t *testing.T) {
	host := fleetHost{Remote: "west", Project: "agents"}
	for _, box := range []string{"con", strings.Repeat("a", 63)} {
		id := requireQualifiedJobID(t, host, box+".20261126-223849-280c")
		name := id.ID + ".watchlog"
		if len(name) > 255 || strings.ContainsAny(name, "<>:\"/\\|?* ") || strings.ToLower(name) != name {
			t.Fatalf("watcher filename is not Windows safe: %q", name)
		}
		if err := os.WriteFile(filepath.Join(t.TempDir(), name), nil, 0o600); err != nil {
			t.Fatalf("writing watcher file %q: %v", name, err)
		}
	}
}

func TestParseJobIDRejectsInvalidInput(t *testing.T) {
	tag := "h1-" + strings.Repeat("a", 32)
	for _, raw := range []string{
		"",
		"../" + testBareJobID,
		"C:\\" + testBareJobID,
		"h2-" + strings.Repeat("a", 32) + "~" + testBareJobID,
		"h1-" + strings.Repeat("A", 32) + "~" + testBareJobID,
		"h1-" + strings.Repeat("z", 32) + "~" + testBareJobID,
		"h1-" + strings.Repeat("a", 31) + "~" + testBareJobID,
		tag + "~../" + testBareJobID,
		tag + "~" + tag + "~" + testBareJobID,
		tag + "~" + testBareJobID + strings.Repeat("a", maxQualifiedJobIDLength),
		tag + "~" + strings.Repeat("a", 64) + ".20261126-223849-280c",
	} {
		if id, err := parseJobID(raw); err == nil {
			t.Errorf("parseJobID(%q) = %+v, want error", raw, id)
		}
	}
	for _, host := range []fleetHost{
		{Project: "agents"}, {Remote: "west"},
		{Remote: "west\x00east", Project: "agents"}, {Remote: "west", Project: "east\x00agents"},
	} {
		if id, err := qualifyJobID(host, testBareJobID); err == nil {
			t.Errorf("qualifyJobID(%+v) = %+v, want error", host, id)
		}
	}
}

func TestResolveJobHost(t *testing.T) {
	defaultHost := fleetHost{Name: "default", Remote: "west", Project: "agents"}
	configuredDefault := fleetHost{Name: "primary", Remote: "west", Project: "agents", DashboardURL: "https://example.com/primary"}
	otherProject := fleetHost{Name: "work", Remote: "west", Project: "work", DashboardURL: "https://example.com/work"}
	otherRemote := fleetHost{Name: "overflow", Remote: "east", Project: "agents", DashboardURL: "https://example.net"}
	hosts := []fleetHost{configuredDefault, otherProject, otherRemote}
	bare, err := parseJobID(testBareJobID)
	if err != nil || bare.ID != testBareJobID || bare.Bare != testBareJobID || bare.Box != "repo" {
		t.Fatalf("legacy job = %+v, %v", bare, err)
	}
	if got, err := bare.resolveHost(defaultHost, hosts); err != nil || got != defaultHost {
		t.Fatalf("bare ID resolved to %+v, %v, want default %+v", got, err, defaultHost)
	}
	seen := map[string]bool{bare.ID: true}
	for _, host := range hosts {
		id := requireQualifiedJobID(t, host, testBareJobID)
		if seen[id.ID] {
			t.Fatalf("duplicate public ID for %+v: %s", host, id.ID)
		}
		seen[id.ID] = true
		if got, err := id.resolveHost(defaultHost, hosts); err != nil || got != host {
			t.Errorf("qualified ID resolved to %+v, %v, want %+v", got, err, host)
		}
		if got, err := id.resolveHost(defaultHost, nil); host == configuredDefault {
			if err != nil || got != defaultHost {
				t.Errorf("qualified default resolved to %+v, %v, want %+v", got, err, defaultHost)
			}
		} else if err == nil || got != (fleetHost{}) || !strings.Contains(err.Error(), "not configured") {
			t.Errorf("unconfigured ID resolved to %+v, %v, want no host and configuration error", got, err)
		}
	}
}

func TestResolveJobHostDashboardAliases(t *testing.T) {
	primary := fleetHost{Name: "primary", Remote: "west", Project: "agents"}
	alias := fleetHost{Name: "alias", Remote: "west", Project: "agents", DashboardURL: "https://example.com/dashboard/"}
	id := requireQualifiedJobID(t, primary, testBareJobID)
	want := primary
	want.DashboardURL = alias.DashboardURL
	sameURL := alias
	sameURL.DashboardURL = strings.TrimSuffix(alias.DashboardURL, "/")
	if got, err := id.resolveHost(primary, []fleetHost{primary, alias, sameURL}); err != nil || got != want {
		t.Fatalf("aliases resolved to %+v, %v, want %+v", got, err, want)
	}
	conflicting := alias
	conflicting.DashboardURL = "https://example.net/dashboard"
	if got, err := id.resolveHost(primary, []fleetHost{primary, alias, conflicting}); err == nil || got != (fleetHost{}) || !strings.Contains(err.Error(), "conflicting dashboard URLs") {
		t.Fatalf("conflicting aliases resolved to %+v, %v, want no host and conflict error", got, err)
	}
}
