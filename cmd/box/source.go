package main

import (
	"encoding/base64"
	"encoding/json"

	"github.com/a-Fig/agent-box/internal/dash"
)

// sourceLine describes the bytes sent to the job, regardless of what is
// currently in the user's worktree.
func sourceLine(s *dash.SourceRevision) string {
	if !s.Valid() {
		return "source unknown"
	}
	head, state := s.Commit, "state unknown"
	if head == "" {
		head = "unknown"
	}
	if s.Dirty != nil {
		state = "clean"
		if *s.Dirty {
			state = "dirty"
		}
	}
	line := "source " + head + " (" + state + ")"
	if s.SnapshotCommit != s.Commit {
		line += ", snapshot " + s.SnapshotCommit
	}
	return line
}

// decodeSource reads the base64-encoded job metadata carried by follow and
// log page headers. A legacy or malformed record has unknown provenance.
func decodeSource(encoded string) *dash.SourceRevision {
	var meta jobMeta
	b, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || json.Unmarshal(b, &meta) != nil || !meta.Source.Valid() {
		return nil
	}
	return meta.Source
}
