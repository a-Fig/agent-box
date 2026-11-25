package jobrun

import (
	"strings"
	"testing"
)

// The agent's own cancel marks the job so its watcher stays quiet; the page's
// doesn't, and says in the log who cancelled it, so the agent hears it ended
// and why.
func TestCancelScriptMarks(t *testing.T) {
	agent := CancelScript("/work", "b.20261124-010203-abcd", 130, "")
	page := CancelScript("/work", "b.20261124-010203-abcd", 130, "tyler's laptop")
	if !strings.Contains(agent, "\n: > \"$j.cancelled\"\n") || strings.Contains(agent, "dashboard") {
		t.Fatalf("agent's cancel:\n%s", agent)
	}
	if strings.Contains(page, `: > "$j.cancelled"`) ||
		!strings.Contains(page, `echo 'box: cancelled from the dashboard by tyler'\''s laptop' >> "$j.log"`) {
		t.Fatalf("page's cancel:\n%s", page)
	}
	if !strings.Contains(agent, "\nj='b.20261124-010203-abcd'\n") || !strings.Contains(agent, "echo 130 >") {
		t.Fatalf("job or code missing:\n%s", agent)
	}
}
