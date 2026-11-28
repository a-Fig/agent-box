package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// EnqueueCIJob adds a queued CI job, as a webhook that passed the controller's
// guards would, and returns its ID. hostClass is JobClassAny or JobClassCloud.
func (s *MemServer) EnqueueCIJob(repo, sha, hostClass string, pr *int) string {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ciSeq++
	id := fmt.Sprintf("%08x-0000-4000-8000-%012x", s.ciSeq, s.ciSeq)
	s.ciJobs = append(s.ciJobs, &CIJob{
		ID: id, Repo: repo, SHA: sha, PR: pr, HostClass: hostClass, State: CIQueued,
		CreatedAt: now, UpdatedAt: now,
	})
	return id
}

// CIJob returns a copy of the CI job with this ID.
func (s *MemServer) CIJob(id string) (CIJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.ciJobs {
		if j.ID == id {
			return *j, true
		}
	}
	return CIJob{}, false
}

func (s *MemServer) handleCINext(w http.ResponseWriter, r *http.Request, now time.Time) {
	worker := r.URL.Query().Get("worker")
	hostClass := r.URL.Query().Get("hostClass")
	if !ValidHostID(worker) {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid worker ID.")
		return
	}
	// Home hardware only ever sees 'any' jobs; 'cloud' jobs (fork PRs) are for cloud workers.
	var eligible func(string) bool
	switch hostClass {
	case HostClassHome:
		eligible = func(c string) bool { return c == JobClassAny }
	case HostClassCloud:
		eligible = func(c string) bool { return c == JobClassAny || c == JobClassCloud }
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "hostClass must be home or cloud.")
		return
	}

	s.mu.Lock()
	var claimed *CIJob
	for _, j := range s.ciJobs {
		if j.State == CIQueued && eligible(j.HostClass) {
			j.State, j.ClaimedBy, j.UpdatedAt = CIRunning, worker, now
			c := *j
			claimed = &c
			break
		}
	}
	s.mu.Unlock()
	if claimed == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, CINextResponse{Job: claimed})
}

func (s *MemServer) handleCIPatch(w http.ResponseWriter, r *http.Request, id string, now time.Time) {
	if !ValidCIJobID(id) {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid job ID.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("Request body exceeds the %d-byte limit.", MaxBodyBytes))
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "Unable to read request body.")
		return
	}
	var p struct {
		Worker   string  `json:"worker"`
		State    string  `json:"state"`
		ExitCode *int    `json:"exitCode"`
		Summary  *string `json:"summary"`
		URL      *string `json:"url"`
	}
	if json.Unmarshal(body, &p) != nil || !ValidHostID(p.Worker) ||
		(p.State != CIDone && p.State != CIFailed && p.State != CICancelled) ||
		(p.URL != nil && !strings.HasPrefix(*p.URL, "https://")) {
		writeError(w, http.StatusBadRequest, "bad_request", "Missing or invalid job update fields.")
		return
	}

	s.mu.Lock()
	var job *CIJob
	for _, j := range s.ciJobs {
		if j.ID == id {
			job = j
			break
		}
	}
	switch {
	case job == nil:
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "not_found", "Unknown job.")
		return
	case job.ClaimedBy != p.Worker:
		s.mu.Unlock()
		writeError(w, http.StatusForbidden, "forbidden", "Job is not claimed by this worker.")
		return
	case job.State != CIRunning:
		state := job.State
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "conflict", fmt.Sprintf("Job is already %s.", state))
		return
	}
	job.State, job.UpdatedAt = p.State, now
	job.Result = &CIResult{ExitCode: p.ExitCode, Summary: p.Summary, URL: p.URL}
	out := *job
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, CIJobResponse{Job: out})
}
