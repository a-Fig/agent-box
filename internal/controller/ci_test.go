package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

func TestCIQueueEndToEnd(t *testing.T) {
	ctx := context.Background()
	ms := &MemServer{HostToken: "host-secret", ClientToken: "client-secret"}
	srv := httptest.NewServer(ms)
	defer srv.Close()
	host := &Client{URL: srv.URL, Token: "host-secret"}

	if job, err := host.NextCIJob(ctx, "pc", HostClassHome); job != nil || err != nil {
		t.Fatalf("empty queue: NextCIJob = %+v, %v; want nil, nil (204)", job, err)
	}

	pr := 7
	cloudID := ms.EnqueueCIJob("a-Fig/fork-pr", testSHA, JobClassCloud, &pr)
	anyID := ms.EnqueueCIJob("a-Fig/agent-box", testSHA, JobClassAny, nil)

	// A home worker skips the older cloud-only job.
	job, err := host.NextCIJob(ctx, "pc", HostClassHome)
	if err != nil || job == nil || job.ID != anyID || job.State != CIRunning || job.ClaimedBy != "pc" || job.HostClass != JobClassAny {
		t.Fatalf("home NextCIJob = %+v, %v; want the any job claimed by pc", job, err)
	}
	if job, err := host.NextCIJob(ctx, "pc", HostClassHome); job != nil || err != nil {
		t.Fatalf("home worker got %+v, %v; want 204 with only a cloud job left", job, err)
	}
	cjob, err := host.NextCIJob(ctx, "modal-1", HostClassCloud)
	if err != nil || cjob == nil || cjob.ID != cloudID || cjob.PR == nil || *cjob.PR != 7 {
		t.Fatalf("cloud NextCIJob = %+v, %v; want the cloud job", cjob, err)
	}

	code := 0
	err = host.PatchCIJob(ctx, anyID, CIJobPatch{Worker: "other", State: CIDone, ExitCode: &code})
	if !apiStatus(err, http.StatusForbidden) {
		t.Fatalf("patch by another worker: %v; want 403", err)
	}
	if err := host.PatchCIJob(ctx, anyID, CIJobPatch{Worker: "pc", State: CIDone, ExitCode: &code, Summary: "ok"}); err != nil {
		t.Fatalf("patch: %v", err)
	}
	got, _ := ms.CIJob(anyID)
	if got.State != CIDone || got.Result == nil || got.Result.ExitCode == nil || *got.Result.ExitCode != 0 || *got.Result.Summary != "ok" {
		t.Fatalf("stored job = %+v", got)
	}
	err = host.PatchCIJob(ctx, anyID, CIJobPatch{Worker: "pc", State: CIFailed})
	if !apiStatus(err, http.StatusConflict) {
		t.Fatalf("second patch: %v; want 409", err)
	}
	err = host.PatchCIJob(ctx, cloudID, CIJobPatch{Worker: "modal-1", State: "passed"})
	if !apiStatus(err, http.StatusBadRequest) {
		t.Fatalf("bad state: %v; want 400", err)
	}
	if err := host.PatchCIJob(ctx, "../../x", CIJobPatch{Worker: "pc", State: CIDone}); err == nil {
		t.Fatal("PatchCIJob accepted a bad job ID")
	}

	if _, err := host.NextCIJob(ctx, "pc", "laptop"); !apiStatus(err, http.StatusBadRequest) {
		t.Fatalf("bad hostClass: %v; want 400", err)
	}
	if _, err := host.NextCIJob(ctx, "Bad_Worker", HostClassHome); !apiStatus(err, http.StatusBadRequest) {
		t.Fatalf("bad worker: %v; want 400", err)
	}
	client := &Client{URL: srv.URL, Token: "client-secret"}
	if _, err := client.NextCIJob(ctx, "pc", HostClassHome); !apiStatus(err, http.StatusForbidden) {
		t.Fatalf("client token: %v; want 403", err)
	}
	nobody := &Client{URL: srv.URL}
	if _, err := nobody.NextCIJob(ctx, "pc", HostClassHome); !apiStatus(err, http.StatusUnauthorized) {
		t.Fatalf("no token: %v; want 401", err)
	}
}

func apiStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

func TestValidRepoAndSHA(t *testing.T) {
	for repo, want := range map[string]bool{
		"a-Fig/agent-box": true, "o/n.js": true, "my_org/repo.v2": true,
		"../x": false, "x/..": false, "./x": false, "a/b/c": false, "a": false, "a/": false,
		`a\b/c`: false, "a/b c": false, "": false, "o/" + strings.Repeat("n", 101): false,
	} {
		if got := ValidRepo(repo); got != want {
			t.Errorf("ValidRepo(%q) = %v, want %v", repo, got, want)
		}
	}
	for sha, want := range map[string]bool{
		testSHA: true, strings.Repeat("a", 64): true,
		strings.ToUpper(testSHA): false, testSHA[:39]: false, "--upload-pack=x": false, "": false,
	} {
		if got := ValidSHA(sha); got != want {
			t.Errorf("ValidSHA(%q) = %v, want %v", sha, got, want)
		}
	}
}
