package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// CI host classes and job states, as the controller's CI queue names them
// (docs/controller-protocol.md, CI endpoints).
const (
	// HostClassHome is a worker on owned hardware: it only gets "any" jobs.
	HostClassHome = "home"
	// HostClassCloud is a worker that runs on cloud hosts: it gets "cloud" and "any" jobs.
	HostClassCloud = "cloud"

	// JobClassAny may run on home or cloud hardware.
	JobClassAny = "any"
	// JobClassCloud (fork PRs) must never run on home hardware.
	JobClassCloud = "cloud"

	CIQueued    = "queued"
	CIRunning   = "running"
	CIDone      = "done"
	CIFailed    = "failed"
	CICancelled = "cancelled"
)

// CIResult is a finished job's reported result.
type CIResult struct {
	ExitCode *int    `json:"exitCode"`
	Summary  *string `json:"summary"`
	URL      *string `json:"url"`
}

// CIJob is one job in the controller's CI queue.
type CIJob struct {
	ID         string    `json:"id"`
	Repo       string    `json:"repo"`
	SHA        string    `json:"sha"`
	PR         *int      `json:"pr"`
	HostClass  string    `json:"hostClass"`
	State      string    `json:"state"`
	ClaimedBy  string    `json:"claimedBy"`
	CheckRunID *int64    `json:"checkRunId"`
	Result     *CIResult `json:"result"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// CINextResponse is the 200 body of GET /v1/ci/next.
type CINextResponse struct {
	Job *CIJob `json:"job"`
}

// CIJobResponse is the 200 body of PATCH /v1/ci/jobs/{id}.
type CIJobResponse struct {
	Job CIJob `json:"job"`
}

// CIJobPatch is the body of PATCH /v1/ci/jobs/{id}: the claiming worker's final state.
type CIJobPatch struct {
	Worker   string `json:"worker"`
	State    string `json:"state"` // done, failed or cancelled
	ExitCode *int   `json:"exitCode,omitempty"`
	Summary  string `json:"summary,omitempty"`
	URL      string `json:"url,omitempty"` // https:// only
}

var (
	repoRE  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
	shaRE   = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	jobIDRE = regexp.MustCompile(`^[0-9a-f-]{36}$`)
)

// ValidRepo reports whether repo is a GitHub owner/name the controller accepts,
// with neither part all dots (so it is safe as two path components).
func ValidRepo(repo string) bool {
	if !repoRE.MatchString(repo) {
		return false
	}
	owner, name, _ := strings.Cut(repo, "/")
	// All-dot parts ("." , "..", "...") are path tricks, and Windows strips trailing dots.
	return strings.Trim(owner, ".") != "" && strings.Trim(name, ".") != ""
}

// ValidSHA reports whether sha is a full lowercase hex commit ID (SHA-1 or SHA-256).
func ValidSHA(sha string) bool { return shaRE.MatchString(sha) }

// ValidCIJobID reports whether id looks like a controller CI job ID (a UUID).
func ValidCIJobID(id string) bool { return jobIDRE.MatchString(id) }

// NextCIJob calls GET /v1/ci/next, claiming the oldest queued job this worker
// may run. It returns (nil, nil) when the queue is empty (204).
func (c *Client) NextCIJob(ctx context.Context, worker, hostClass string) (*CIJob, error) {
	q := url.Values{"worker": {worker}, "hostClass": {hostClass}}
	var out CINextResponse
	if err := c.doJSON(ctx, http.MethodGet, "/v1/ci/next?"+q.Encode(), true, nil, &out); err != nil {
		return nil, err
	}
	return out.Job, nil
}

// PatchCIJob calls PATCH /v1/ci/jobs/{id} with a claimed job's final state.
// A 403 means another worker claimed it; a 409 that it had already finished.
func (c *Client) PatchCIJob(ctx context.Context, id string, patch CIJobPatch) error {
	if !ValidCIJobID(id) {
		return errors.New("controller: invalid CI job id")
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("controller: encode CI job patch: %w", err)
	}
	return c.doJSON(ctx, http.MethodPatch, "/v1/ci/jobs/"+url.PathEscape(id), true, body, nil)
}
