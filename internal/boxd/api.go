package boxd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/dash/static"
)

// Server is boxd's HTTP API and page (docs/ui.md, "API"). Wrap it in Owners.
type Server struct {
	W    *Watcher
	Page fs.FS // static.FS, or a folder while working on the page
}

// Handler routes the API and serves the page.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.W.State()) })
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("GET /api/jobs", s.jobs)
	mux.HandleFunc("GET /api/jobs/{key}", s.job)
	mux.HandleFunc("GET /api/jobs/{key}/log", s.log)
	mux.HandleFunc("GET /api/history", s.history)
	mux.Handle("POST /api/refusals", control(http.HandlerFunc(s.refusal)))
	mux.Handle("POST /api/jobs/ended", control(http.HandlerFunc(s.ended)))
	s.queueRoutes(mux)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, dash.CodeNotFound, "no such endpoint")
	})
	page := s.Page
	if page == nil {
		page = static.FS
	}
	mux.Handle("/", static.Handler(page))
	return mux
}

// control guards a POST: it needs ControlHeader, which another site can't
// send without a CORS preflight boxd never grants, and a JSON body.
func control(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(dash.ControlHeader) != "1" {
			writeError(w, http.StatusForbidden, dash.CodeForbidden, "a control needs the "+dash.ControlHeader+" header")
			return
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			writeError(w, http.StatusBadRequest, dash.CodeBadRequest, "the body must be JSON")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, dash.CodeIncus, "no streaming")
		return
	}
	st, ch, cancel := s.W.Subscribe()
	defer cancel()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	if _, err := (dash.Event{Name: dash.EventState, Data: st}).WriteTo(w); err != nil {
		return
	}
	fl.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-ch:
			if !ok {
				return // too far behind: the page reconnects and starts over
			}
			if _, err := e.WriteTo(w); err != nil {
				return
			}
			fl.Flush()
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

func (s *Server) jobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := Filter{Repo: q.Get("repo"), Agent: q.Get("agent"), Kind: q.Get("kind"), Machine: q.Get("machine"),
		Result: q.Get("result"), Q: q.Get("q"), Limit: 50}
	if v := q.Get("before"); v != "" {
		t, err := time.Parse(time.RFC3339, v[strings.LastIndex(v, "/")+1:])
		if err != nil {
			writeError(w, http.StatusBadRequest, dash.CodeBadRequest, "before must be a job's key, box/id/startedAt")
			return
		}
		f.Before = &dash.Job{Key: v, StartedAt: t}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, dash.CodeBadRequest, "limit must be 1 to 200")
			return
		}
		f.Limit = n
	}
	if f.Result != "" && !slices.Contains(dash.Results, f.Result) {
		writeError(w, http.StatusBadRequest, dash.CodeBadRequest, "unknown result")
		return
	}
	writeJSON(w, 200, s.W.Hist.Finished(f))
}

// job answers one job, running or from history, for a page that let it go.
func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	j, ok := s.W.Job(r.PathValue("key"))
	if !ok {
		writeError(w, http.StatusNotFound, dash.CodeNotFound, "no such job")
		return
	}
	writeJSON(w, 200, j)
}

func (s *Server) log(w http.ResponseWriter, r *http.Request) {
	var offset int64
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, dash.CodeBadRequest, "offset must be a byte count")
			return
		}
		offset = n
	}
	j, ok := s.W.Job(r.PathValue("key"))
	if !ok {
		writeError(w, http.StatusNotFound, dash.CodeNotFound, "no such job")
		return
	}
	b, ok := s.W.Box(j.Box)
	if !ok || !holds(b.CreatedAt, j.StartedAt) {
		writeError(w, http.StatusNotFound, dash.CodeNotFound, "the job's box is gone")
		return
	}
	switch b.State {
	case dash.BoxFrozen:
		writeJSON(w, 200, dash.LogChunk{Offset: offset, Data: "", Paused: true})
		return
	case dash.BoxRunning:
	default:
		writeError(w, http.StatusNotFound, dash.CodeNotFound, "the job's box is "+b.State)
		return
	}
	chunk, err := readLog(r.Context(), s.W.Incus, j, offset)
	var ee *ExitError
	switch {
	case errors.As(err, &ee) && ee.Code == 3:
		writeError(w, http.StatusNotFound, dash.CodeNotFound, "the job's log is gone")
	case err != nil:
		writeError(w, http.StatusBadGateway, dash.CodeIncus, "reading the log: "+err.Error())
	default:
		writeJSON(w, 200, chunk)
	}
}

// readLog reads at most MaxLogChunk bytes of a job's log from offset, cut
// at a character's start.
func readLog(ctx context.Context, inc Incus, j dash.Job, offset int64) (dash.LogChunk, error) {
	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	out, err := inc.Exec(ctx, j.Box, logScript(j.Box+"."+j.ID, offset))
	if err != nil {
		return dash.LogChunk{}, err
	}
	line, data, _ := bytes.Cut(out, []byte("\n"))
	size, err := strconv.ParseInt(string(bytes.TrimSpace(line)), 10, 64)
	if err != nil {
		return dash.LogChunk{}, errors.New("bad log size")
	}
	if offset+int64(len(data)) < size {
		// A character cut by the chunk's end comes whole in the next one. At
		// the log's end nothing more follows, so the bytes are kept: dropping
		// them would never reach eof.
		if cut := cutUTF8(data); len(cut) > 0 {
			data = cut
		}
	}
	next := offset + int64(len(data))
	return dash.LogChunk{Offset: next, Data: strings.ToValidUTF8(string(data), "�"), EOF: next >= size}, nil
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = dash.RangeDay
	}
	if rng != dash.RangeDay && rng != dash.RangeWeek {
		writeError(w, http.StatusBadRequest, dash.CodeBadRequest, "range is day or week")
		return
	}
	writeJSON(w, 200, s.W.Hist.Range(rng, s.W.Live()))
}

// refusal records a run box turned away (docs/ui.md, "Refusals").
func (s *Server) refusal(w http.ResponseWriter, r *http.Request) {
	var b dash.RefusalBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, dash.CodeBadRequest, "bad JSON: "+err.Error())
		return
	}
	if !slices.Contains(dash.Refusals, b.Why) {
		writeError(w, http.StatusBadRequest, dash.CodeBadRequest, "why is one of "+strings.Join(dash.Refusals, ", "))
		return
	}
	b.Reason, b.Size, b.Repo, b.Agent = clip(b.Reason, 300), clip(b.Size, 16), Mask(clip(b.Repo, 300)), clip(b.Agent, 100)
	s.W.record(record{Type: recRefusal, Refusal: &b})
	w.WriteHeader(http.StatusNoContent)
}

// ended records how a job ended, from box (dash.JobEndedBody).
func (s *Server) ended(w http.ResponseWriter, r *http.Request) {
	var b dash.JobEndedBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.Box == "" || b.ID == "" {
		writeError(w, http.StatusBadRequest, dash.CodeBadRequest, "the body is {box, id, exit, endedAt, tail}")
		return
	}
	if !s.W.EndJob(r.Context(), b) {
		writeError(w, http.StatusNotFound, dash.CodeNotFound, "no running job "+b.Box+"."+b.ID)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// clip cuts s to n characters.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
