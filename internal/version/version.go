// Package version says which commit this binary was built from. Go's build
// stamps it (vcs.revision, vcs.time, vcs.modified) when it builds in a git
// checkout; a build that can't, or a script that knows better, sets it with
//
//	-ldflags "-X github.com/a-Fig/agent-box/internal/version.Commit=<hash>
//	          -X github.com/a-Fig/agent-box/internal/version.Time=<RFC3339>
//	          -X github.com/a-Fig/agent-box/internal/version.Modified=true"
//
// These were added after boxd and its unit drifted from the repo twice on
// 2026-11-25 without anything showing it.
package version

import (
	"runtime/debug"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
)

// Set with -ldflags -X. A set Commit replaces the build's own stamp, Time
// and Modified included; Time and Modified are ignored without it.
var Commit, Time, Modified string

// Get is this binary's version. Its Commit is "" when nothing recorded one.
func Get() dash.Version {
	var s map[string]string
	if bi, ok := debug.ReadBuildInfo(); ok {
		s = map[string]string{}
		for _, kv := range bi.Settings {
			s[kv.Key] = kv.Value
		}
	}
	return from(s, Commit, Time, Modified)
}

func from(stamp map[string]string, commit, at, modified string) dash.Version {
	if commit == "" {
		commit, at, modified = stamp["vcs.revision"], stamp["vcs.time"], stamp["vcs.modified"]
	}
	v := dash.Version{Commit: commit, Modified: modified == "true"}
	if t, err := time.Parse(time.RFC3339, at); err == nil && commit != "" {
		t = t.UTC()
		v.Time = &t
	}
	return v
}
