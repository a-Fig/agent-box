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
	if host.Remote == "" || host.Project == "" || strings.ContainsRune(host.Remote, 0) || strings.ContainsRune(host.Project, 0) {
		return "", fmt.Errorf("job host requires a remote and project without NUL characters")
	}
	sum := sha256.Sum256([]byte(host.Remote + "\x00" + host.Project))
	return hex.EncodeToString(sum[:jobHostTagBytes]), nil
}

// resolveHost uses the configured destination for a qualified ID, or the
// supplied default when that exact destination matches. Bare IDs use the default.
func (id jobID) resolveHost(defaultHost fleetHost, hosts []fleetHost) (fleetHost, error) {
	if id.hostTag == "" {
		return defaultHost, nil
	}
	var match fleetHost
	for _, host := range hosts {
		tag, err := jobHostTag(host)
		if err != nil || tag != id.hostTag {
			continue
		}
		if match.Remote == "" {
			match = host
			continue
		}
		if match.Remote != host.Remote || match.Project != host.Project {
			return fleetHost{}, fmt.Errorf("multiple destinations match job host %s", id.hostTag)
		}
		if match.DashboardURL == "" {
			match.DashboardURL = host.DashboardURL
		} else if host.DashboardURL != "" && strings.TrimRight(match.DashboardURL, "/") != strings.TrimRight(host.DashboardURL, "/") {
			return fleetHost{}, fmt.Errorf("conflicting dashboard URLs for job host %s/%s", host.Remote, host.Project)
		}
	}
	if match.Remote != "" {
		return match, nil
	}
	if tag, err := jobHostTag(defaultHost); err == nil && tag == id.hostTag {
		return defaultHost, nil
	}
	return fleetHost{}, fmt.Errorf("job host %s is not configured", id.hostTag)
}
