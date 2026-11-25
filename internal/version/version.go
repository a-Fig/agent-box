// Package version says which commit this binary was built from. Go's build
// stamps it (vcs.revision, vcs.time, vcs.modified) when it builds in a git
// checkout; a build that can't, or a script that knows better, sets it with
//
//	-ldflags "-X github.com/a-Fig/agent-box/internal/version.Commit=<hash>
//	          -X github.com/a-Fig/agent-box/internal/version.Time=<RFC3339>
//	          -X github.com/a-Fig/agent-box/internal/version.Modified=true
//	          -X github.com/a-Fig/agent-box/internal/version.Tag=v0.3.0"
//
// Tag is set only by .github/workflows/release.yml (docs/releasing.md): a
// build without it is a dev build, which never updates itself.
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
// and Modified included; Time and Modified are ignored without it. Tag
// stands alone.
var Commit, Time, Modified, Tag string

// Get is this binary's version. Its Commit is "" when nothing recorded one.
func Get() dash.Version {
	var s map[string]string
	if bi, ok := debug.ReadBuildInfo(); ok {
		s = map[string]string{}
		for _, kv := range bi.Settings {
			s[kv.Key] = kv.Value
		}
	}
	v := from(s, Commit, Time, Modified)
	v.Tag = Tag
	return v
}

// Text is a binary's --version line: its name, the tag when it is a
// release, then the commit and its time, as "box v0.3.0 (e823842, 2026-11-25
// 07:12 UTC)", or "box e823842 (2026-11-25 07:12 UTC)" for a dev build. An
// updater reads the second word to check that a download is the release it
// asked for.
func Text(name string, v dash.Version) string {
	var at string
	if v.Time != nil {
		at = v.Time.Format("2006-01-02 15:04 MST")
	}
	switch {
	case v.Tag != "" && at != "":
		return name + " " + v.Tag + " (" + v.String() + ", " + at + ")"
	case v.Tag != "":
		return name + " " + v.Tag + " (" + v.String() + ")"
	case at != "":
		return name + " " + v.String() + " (" + at + ")"
	}
	return name + " " + v.String()
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
