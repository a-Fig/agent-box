package boxd

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/a-Fig/agent-box/internal/dash"
)

// The queue's endpoints (docs/queue.md, "API").
func (s *Server) queueRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/queue", s.enqueue)
	mux.HandleFunc("GET /api/queue", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, dash.QueuePage{Jobs: s.W.Queued()})
	})
	mux.HandleFunc("GET /api/queue/{job}", s.queuedJob)
	mux.Handle("POST /api/queue/{job}/cancel", control(http.HandlerFunc(s.cancelQueued)))
	mux.Handle("POST /api/queue/{job}/front", control(http.HandlerFunc(s.frontQueued)))
}

// enqueue takes a multipart body: the "request" part (dash.QueueRequest)
// first, then the "bundle" part, streamed to disk.
func (s *Server) enqueue(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(dash.ControlHeader) != "1" {
		writeError(w, http.StatusForbidden, dash.CodeForbidden, "a control needs the "+dash.ControlHeader+" header")
		return
	}
	if s.W.Queue == nil {
		writeError(w, http.StatusNotFound, dash.CodeNotFound, "this boxd has no queue")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, dash.QueueBytes+1<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, dash.CodeBadRequest, "the body must be multipart/form-data")
		return
	}
	part, err := mr.NextPart()
	if err != nil || part.FormName() != "request" {
		writeError(w, http.StatusBadRequest, dash.CodeBadRequest, `the first part must be "request"`)
		return
	}
	var req dash.QueueRequest
	if err := json.NewDecoder(io.LimitReader(part, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, dash.CodeBadRequest, "bad request part: "+err.Error())
		return
	}
	if err := check(&req); err != nil {
		writeError(w, http.StatusBadRequest, dash.CodeBadRequest, err.Error())
		return
	}
	// A GPU run doesn't wait out My PC (docs/queue.md).
	if m := s.W.Machine(); req.GPU && m.Mode == dash.ModeMyPC {
		writeError(w, http.StatusConflict, dash.CodeFrozen, "a GPU run isn't queued while the PC is in My PC")
		return
	}
	part, err = mr.NextPart()
	if err != nil || part.FormName() != "bundle" {
		writeError(w, http.StatusBadRequest, dash.CodeBadRequest, `the second part must be "bundle"`)
		return
	}
	it, err := s.W.Queue.Add(req, part, s.W.Now())
	switch {
	case errors.Is(err, errQueueFull):
		writeError(w, http.StatusInsufficientStorage, dash.CodeQueueFull, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, dash.CodeBadRequest, "storing the snapshot: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, s.W.Enqueued(it))
}

// queuedJob answers a job by its name (<box>.<id>), waiting or not, so box
// can follow a queued job through its start.
func (s *Server) queuedJob(w http.ResponseWriter, r *http.Request) {
	job := r.PathValue("job")
	if j, ok := s.W.QueuedView(job); ok {
		writeJSON(w, 200, j)
		return
	}
	box, id, _ := strings.Cut(job, ".")
	if j, ok := s.W.JobByID(box, id); ok {
		writeJSON(w, 200, j)
		return
	}
	writeError(w, http.StatusNotFound, dash.CodeNotFound, "no job "+job)
}

func (s *Server) cancelQueued(w http.ResponseWriter, r *http.Request) {
	if !s.W.CancelQueued(r.PathValue("job"), by(r)) {
		writeError(w, http.StatusConflict, dash.CodeNotRunning, "the job isn't waiting: it has started or gone")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) frontQueued(w http.ResponseWriter, r *http.Request) {
	j, ok := s.W.FrontQueued(r.PathValue("job"), by(r))
	if !ok {
		writeError(w, http.StatusConflict, dash.CodeNotRunning, "the job isn't waiting: it has started or gone")
		return
	}
	writeJSON(w, 200, j)
}

// by is who asked, for the history: the tailnet login.
func by(r *http.Request) string { return r.Header.Get(LoginHeader) }
