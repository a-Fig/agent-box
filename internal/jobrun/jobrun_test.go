package jobrun

import (
	"errors"
	"strings"
	"testing"

	"github.com/a-Fig/agent-box/internal/digest"
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

func TestIsDisallowedMetadata(t *testing.T) {
	if IsDisallowedMetadata(nil) {
		t.Error("nil error matched")
	}
	if IsDisallowedMetadata(errors.New("incus launch: Error: something else")) {
		t.Error("unrelated error matched")
	}
	err := errors.New("incus launch: Error: Failed creating instance from image: Instance metadata contained disallowed entries: [/var/lib/incus/...]")
	if !IsDisallowedMetadata(err) {
		t.Error("disallowed metadata error did not match")
	}
}

func TestPayloadIncludesDigestWrappers(t *testing.T) {
	for _, args := range [][]string{
		{"npm run test:unit"},
		{"npm -w packages/a test"},
		{"npm", "--prefix", "packages/a", "test"},
		{"echo", "hello"},
	} {
		got := Payload(nil, args, 0)
		if !strings.HasPrefix(got, digest.Wrappers) {
			t.Errorf("Payload(%q) missing digest.Wrappers", args)
		}
	}
}

func TestStartScriptRecordsCheckoutOwnerUnderLock(t *testing.T) {
	got := StartScript("/work", "my-repo.20261125-120000-abcd", "20261125-120000-abcd", "my-repo", "true")
	lockAt := strings.Index(got, "flock 9 || exit 1")
	ownerAt := strings.Index(got, "/work/.box/my-repo.checkout.tmp && mv /work/.box/my-repo.checkout.tmp /work/.box/my-repo.checkout")
	gitAt := strings.Index(got, "git init -q /work/my-repo")
	if lockAt < 0 || ownerAt < 0 || gitAt < 0 || !(lockAt < ownerAt && ownerAt < gitAt) {
		t.Fatalf("checkout owner write must run after flock and before git checkout:\n%s", got)
	}
}
