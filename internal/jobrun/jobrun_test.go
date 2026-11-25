package jobrun

import (
	"strings"
	"testing"
)

// The agent's own cancel marks the job so its watcher stays quiet; the page's
// doesn't, so the agent hears that its job ended.
func TestCancelScriptMarks(t *testing.T) {
	agent := CancelScript("/work", "b.20261124-010203-abcd", 130, true)
	page := CancelScript("/work", "b.20261124-010203-abcd", 130, false)
	if !strings.Contains(agent, `if true; then : > "$j.cancelled"; fi`) {
		t.Fatalf("agent's cancel doesn't mark:\n%s", agent)
	}
	if !strings.Contains(page, `if false; then : > "$j.cancelled"; fi`) {
		t.Fatalf("page's cancel marks:\n%s", page)
	}
	if !strings.Contains(agent, "\nj='b.20261124-010203-abcd'\n") || !strings.Contains(agent, "echo 130 >") {
		t.Fatalf("job or code missing:\n%s", agent)
	}
}
