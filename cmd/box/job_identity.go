package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	jobHostPrefix           = "h1-"
	jobHostTagBytes         = 16
	maxQualifiedJobIDLength = 160 // leaves room for extensions such as .watchlog
)

// jobID keeps the public ID separate from the bare ID used by boxd and the box.
type jobID struct {
	ID, Bare, Box string
	hostTag       string
}

func parseJobID(raw string) (jobID, error) {
	prefix, bare, qualified := strings.Cut(raw, "~")
	var tag string
	if qualified {
		var ok bool
		tag, ok = strings.CutPrefix(prefix, jobHostPrefix)
		if !ok || len(tag) != 2*jobHostTagBytes || len(raw) > maxQualifiedJobIDLength || tag != strings.ToLower(tag) {
			return jobID{}, fmt.Errorf("bad job id %q", raw)
		}
		if _, err := hex.DecodeString(tag); err != nil {
			return jobID{}, fmt.Errorf("bad job id %q", raw)
		}
	} else {
		bare = raw
	}
	box, _, _ := strings.Cut(bare, ".")
	if !validJob.MatchString(bare) || (qualified && !validBox.MatchString(box)) {
		return jobID{}, fmt.Errorf("bad job id %q", raw)
	}
	return jobID{ID: raw, Bare: bare, Box: box, hostTag: tag}, nil
}

func qualifyJobID(host fleetHost, bare string) (jobID, error) {
	tag, err := jobHostTag(host)
	if err != nil {
		return jobID{}, err
	}
	return parseJobID(jobHostPrefix + tag + "~" + bare)
}

// jobHostTag identifies the routing destination. The display name and dashboard
// address can change while existing job IDs still identify the same destination.
func jobHostTag(host fleetHost) (string, error) {
	if host.Kind != "" {
		ep := strings.TrimRight(host.Endpoint, "/")
		if ep == "" || strings.ContainsRune(host.Kind, 0) || strings.ContainsRune(ep, 0) {
			return "", fmt.Errorf("cloud job host requires a kind and endpoint without NUL characters")
		}
		sum := sha256.Sum256([]byte(host.Kind + "\x00" + ep))
		return hex.EncodeToString(sum[:jobHostTagBytes]), nil
	}
	if host.Remote == "" || host.Project == "" || strings.ContainsRune(host.Remote, 0) || strings.ContainsRune(host.Project, 0) {
		return "", fmt.Errorf("job host requires a remote and project without NUL characters")
	}
	sum := sha256.Sum256([]byte(host.Remote + "\x00" + host.Project))
	return hex.EncodeToString(sum[:jobHostTagBytes]), nil
}

func sameHostDestination(a, b fleetHost) bool {
	if a.Kind != b.Kind {
		return false
	}
	if a.Kind != "" {
		return strings.TrimRight(a.Endpoint, "/") == strings.TrimRight(b.Endpoint, "/")
	}
	return a.Remote == b.Remote && a.Project == b.Project
}

// resolveHost uses the configured destination for a qualified ID, or the
// supplied default when that exact destination matches. Bare IDs use the default.
func (id jobID) resolveHost(defaultHost fleetHost, hosts []fleetHost) (fleetHost, error) {
	if id.hostTag == "" {
		return defaultHost, nil
	}
	return resolveJobHost(id.hostTag, defaultHost, hosts)
}

func resolveJobHost(hostTag string, defaultHost fleetHost, hosts []fleetHost) (fleetHost, error) {
	var match fleetHost
	found := false
	for _, host := range hosts {
		tag, err := jobHostTag(host)
		if err != nil || tag != hostTag {
			continue
		}
		if !found {
			match = host
			found = true
			continue
		}
		if !sameHostDestination(match, host) {
			return fleetHost{}, fmt.Errorf("multiple destinations match job host %s", hostTag)
		}
		targetDesc := host.Remote + "/" + host.Project
		if host.Kind != "" {
			targetDesc = strings.TrimRight(host.Endpoint, "/")
		}
		if match.DashboardURL == "" {
			match.DashboardURL = host.DashboardURL
		} else if host.DashboardURL != "" && strings.TrimRight(match.DashboardURL, "/") != strings.TrimRight(host.DashboardURL, "/") {
			return fleetHost{}, fmt.Errorf("conflicting dashboard URLs for job host %s", targetDesc)
		}
		if match.TokenFile == "" {
			match.TokenFile = host.TokenFile
		} else if host.TokenFile != "" && match.TokenFile != host.TokenFile {
			return fleetHost{}, fmt.Errorf("conflicting token files for job host %s", targetDesc)
		}
		if match.Mode == "" {
			match.Mode = host.Mode
		} else if host.Mode != "" && match.Mode != host.Mode {
			return fleetHost{}, fmt.Errorf("conflicting modes for job host %s", targetDesc)
		}
		if match.Priority != host.Priority {
			return fleetHost{}, fmt.Errorf("conflicting priorities for job host %s (%d and %d)", targetDesc, match.Priority, host.Priority)
		}
	}
	if found {
		return match, nil
	}
	if tag, err := jobHostTag(defaultHost); err == nil && tag == hostTag {
		return defaultHost, nil
	}
	return fleetHost{}, fmt.Errorf("job host %s is not configured", hostTag)
}
