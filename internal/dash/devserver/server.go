package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/dash/fixture"
)

// server holds the scenario as it plays: the state boxd would hold, the finished jobs its
// history would, and the logs on the boxes.
type server struct {
	mu   sync.Mutex
	now  func() time.Time
	sc   fixture.Scenario
	done []dash.Job // finished jobs, newest first: the history behind /api/jobs
	logs map[string]string
	subs map[chan dash.Event]bool
	down bool
	fail bool

	player  *fixture.Player
	plan    map[string]time.Time // when each running job will end
	tick    int
	applyAt time.Time // when the tray picks up a mode request
}

// newest orders finished jobs as /api/jobs pages them: by startedAt, then key, newest first.
func newest(a, b dash.Job) int {
	if c := b.StartedAt.Compare(a.StartedAt); c != 0 {
		return c
	}
	return strings.Compare(b.Key, a.Key)
}

func newServer(sc fixture.Scenario, now func() time.Time) *server {
	done := slices.Clone(sc.Jobs.Jobs)
	slices.SortStableFunc(done, newest)
	s := &server{
		now: now, sc: sc, done: done, logs: sc.Logs,
		subs: map[chan dash.Event]bool{}, player: fixture.NewPlayer(uint64(len(sc.Name)) * 7919),
		plan: map[string]time.Time{},
	}
	if s.logs == nil {
		s.logs = map[string]string{}
	}
	t := now()
	for i, j := range sc.State.Jobs {
		if j.State == dash.JobRunning || j.State == dash.JobPaused {
			s.plan[j.Key] = t.Add(time.Duration(40+25*i) * time.Second)
		}
	}
	return s
}

func (s *server) machine() *dash.Machine {
	if len(s.sc.State.Machines) == 0 {
		return nil
	}
	return &s.sc.State.Machines[0]
}

func (s *server) setDown(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = v
	if v {
		for c := range s.subs {
			close(c)
			delete(s.subs, c)
		}
	}
}

// publish sends an event to every open stream. A stream too slow to keep up is dropped; its page
// starts over from the state.
func (s *server) publish(name string, data any) {
	e := dash.Event{Name: name, Data: data}
	for c := range s.subs {
		select {
		case c <- e:
		default:
			close(c)
			delete(s.subs, c)
		}
	}
}

func (s *server) snapshot() dash.State {
	st := s.sc.State
	st.Now = s.now().UTC()
	st.Machines = slices.Clone(st.Machines)
	st.Boxes = slices.Clone(st.Boxes)
	st.Jobs = slices.Clone(st.Jobs)
	return st
}

func (s *server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	st := s.snapshot()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, st)
}

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "no-stream", "streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	c := make(chan dash.Event, 64)
	s.mu.Lock()
	first := dash.Event{Name: dash.EventState, Data: s.snapshot()}
	s.subs[c] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.subs[c] {
			delete(s.subs, c)
			close(c)
		}
		s.mu.Unlock()
	}()
	first.WriteTo(w)
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			w.Write([]byte(": ping\n\n"))
			fl.Flush()
		case e, ok := <-c:
			if !ok {
				return
			}
			e.WriteTo(w)
			fl.Flush()
		}
	}
}

// handleJobs filters the finished jobs as boxd's history will: by repo (its URL or last path
// element), agent (the session), kind, machine, result, q (a substring of the reason, command or
// branch) and limit. before is the last job of the page before, by key: the jobs after it in
// (startedAt, key) order, newest first, so jobs that started in the same second aren't skipped.
func (s *server) handleJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeErr(w, http.StatusBadRequest, dash.CodeBadRequest, "limit must be 1 to 200")
			return
		}
		limit = n
	}
	var before *dash.Job
	if v := q.Get("before"); v != "" {
		t, err := time.Parse(time.RFC3339, v[strings.LastIndex(v, "/")+1:])
		if err != nil {
			writeErr(w, http.StatusBadRequest, dash.CodeBadRequest, "before must be a job's key, box/id/startedAt")
			return
		}
		before = &dash.Job{Key: v, StartedAt: t}
	}
	if v := q.Get("result"); v != "" && !slices.Contains(dash.Results, v) {
		writeErr(w, http.StatusBadRequest, dash.CodeBadRequest, "result must be one of "+strings.Join(dash.Results, ", "))
		return
	}
	text := q.Get("q")
	s.mu.Lock()
	defer s.mu.Unlock()
	page := dash.JobsPage{Jobs: []dash.Job{}}
	for _, j := range s.done {
		short := j.Repo[strings.LastIndex(j.Repo, "/")+1:]
		switch {
		case q.Get("repo") != "" && q.Get("repo") != j.Repo && q.Get("repo") != short,
			q.Get("agent") != "" && q.Get("agent") != j.Agent.Session,
			q.Get("kind") != "" && q.Get("kind") != j.Kind,
			q.Get("machine") != "" && q.Get("machine") != j.Machine,
			q.Get("result") != "" && q.Get("result") != j.Result(),
			before != nil && newest(*before, j) >= 0,
			text != "" && !matchesText(text, j.Reason, j.Command, j.Branch):
			continue
		}
		if len(page.Jobs) == limit {
			page.More = true
			break
		}
		page.Jobs = append(page.Jobs, j)
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *server) findJob(key string) (*dash.Job, bool) {
	for i := range s.sc.State.Jobs {
		if s.sc.State.Jobs[i].Key == key {
			return &s.sc.State.Jobs[i], true
		}
	}
	for i := range s.done {
		if s.done[i].Key == key {
			return &s.done[i], false
		}
	}
	return nil, false
}

func (s *server) findBox(name string) int {
	return slices.IndexFunc(s.sc.State.Boxes, func(b dash.Box) bool { return b.Name == name })
}

// jobBox is the box j ran on, or -1: the box of that name made no later than dash.JobBoxGrace
// after j started. Names get reused, and a newer box of the same name holds none of j's log.
func (s *server) jobBox(j *dash.Job) int {
	i := s.findBox(j.Box)
	if i < 0 || s.sc.State.Boxes[i].CreatedAt.After(j.StartedAt.Add(dash.JobBoxGrace)) {
		return -1
	}
	return i
}

// handleJob answers one job by key, running or from history, for a page that has let it go.
func (s *server) handleJob(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	s.mu.Lock()
	defer s.mu.Unlock()
	j, _ := s.findJob(key)
	if j == nil {
		writeErr(w, http.StatusNotFound, dash.CodeNotFound, "no job "+key)
		return
	}
	writeJSON(w, http.StatusOK, *j)
}

// handleLog answers a page of a job's log from offset: at most 64KiB, cut at a character's start,
// paused while the box is frozen, and 404 once the box is gone, since logs go with their box.
func (s *server) handleLog(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	off, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if r.URL.Query().Get("offset") == "" {
		off, err = 0, nil
	}
	if err != nil || off < 0 {
		writeErr(w, http.StatusBadRequest, dash.CodeBadRequest, "offset must be a byte count")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	j, _ := s.findJob(key)
	if j == nil {
		writeErr(w, http.StatusNotFound, dash.CodeNotFound, "no job "+key)
		return
	}
	bi := s.jobBox(j)
	if bi < 0 {
		writeErr(w, http.StatusNotFound, dash.CodeNotFound, "box "+j.Box+" is gone, and its log went with it; the job keeps its tail")
		return
	}
	if s.sc.State.Boxes[bi].State == dash.BoxFrozen {
		writeJSON(w, http.StatusOK, dash.LogChunk{Offset: off, Paused: true})
		return
	}
	log := s.logs[key]
	if off > int64(len(log)) {
		off = int64(len(log))
	}
	end := min(int64(len(log)), off+dash.MaxLogChunk)
	for end > off && end < int64(len(log)) && !utf8.RuneStart(log[end]) {
		end--
	}
	writeJSON(w, http.StatusOK, dash.LogChunk{Offset: end, Data: log[off:end], EOF: end == int64(len(log))})
}

func (s *server) handleHistory(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.URL.Query().Get("range") {
	case dash.RangeDay, "":
		writeJSON(w, http.StatusOK, s.sc.Day)
	case dash.RangeWeek:
		writeJSON(w, http.StatusOK, s.sc.Week)
	default:
		writeErr(w, http.StatusBadRequest, dash.CodeBadRequest, "range must be day or week")
	}
}

// The controls.

func decode(body []byte, v any) error {
	if err := json.Unmarshal(body, v); err != nil {
		return fail(http.StatusBadRequest, dash.CodeBadRequest, "the body isn't the JSON this control takes: %v", err)
	}
	return nil
}

// handleMode writes a mode request, as boxd writes user.agent-box.mode-request; the tray applies
// it within a tick, or never while it is down. The reply is the
// machine with the request on it: 202, as nothing has changed yet.
func (s *server) handleMode(r *http.Request, body []byte) (int, any, error) {
	var b dash.ModeBody
	if err := decode(body, &b); err != nil {
		return 0, nil, err
	}
	if !slices.Contains(dash.Modes, b.Mode) {
		return 0, nil, fail(http.StatusBadRequest, dash.CodeBadRequest, "mode must be one of %s", strings.Join(dash.Modes, ", "))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.machine()
	now := s.now().UTC().Truncate(time.Second)
	m.ModeRequested = &dash.ModeRequest{Mode: b.Mode, At: now, By: "devserver-browser"}
	if m.TrayAlive(now) {
		s.applyAt = s.now().Add(2 * time.Second)
	}
	s.publish(dash.EventMachine, *m)
	return http.StatusAccepted, *m, nil
}

func (s *server) handleCancel(r *http.Request, body []byte) (int, any, error) {
	key := r.PathValue("key")
	s.mu.Lock()
	defer s.mu.Unlock()
	j, live := s.findJob(key)
	switch {
	case j == nil:
		return 0, nil, fail(http.StatusNotFound, dash.CodeNotFound, "no job %s", key)
	case j.State == dash.JobQueued:
		return 0, nil, fail(http.StatusConflict, dash.CodeNotRunning, "job %s.%s is waiting in the queue, not running: cancel it with /api/queue/%s.%s/cancel", j.Box, j.ID, j.Box, j.ID)
	case !live || j.State == dash.JobDone || j.State == dash.JobLost:
		return 0, nil, fail(http.StatusConflict, dash.CodeNotRunning, "job %s has already ended", j.ID)
	case j.State == dash.JobPaused:
		return 0, nil, fail(http.StatusConflict, dash.CodeFrozen, "box %s is frozen in My PC mode, so its job can't be stopped; switch to Shared or Full first", j.Box)
	}
	s.end(j, 130)
	return http.StatusOK, *j, nil
}

// matchesText is the jobs search: q in any one field, case folded by strings.ToLower (Unicode's
// simple lower-case mapping, code point by code point). The page folds the same way.
func matchesText(q string, fields ...string) bool {
	q = strings.ToLower(q)
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

// thisBox finds the box a control names: the path's name, made at createdAt. A box of that name
// made at another time is a replacement, which the control was never meant for.
func (s *server) thisBox(name string, createdAt time.Time) (int, error) {
	if createdAt.IsZero() {
		return -1, fail(http.StatusBadRequest, dash.CodeBadRequest, "box controls need the box's createdAt")
	}
	i := s.findBox(name)
	switch {
	case i < 0:
		return -1, fail(http.StatusNotFound, dash.CodeNotFound, "no box %s", name)
	case !s.sc.State.Boxes[i].CreatedAt.Equal(createdAt):
		return -1, fail(http.StatusConflict, dash.CodeGone, "box %s was replaced by a newer box of the same name; nothing was done", name)
	}
	return i, nil
}

func (s *server) handleDelete(r *http.Request, body []byte) (int, any, error) {
	name := r.PathValue("name")
	var b dash.DeleteBody
	if err := decode(body, &b); err != nil {
		return 0, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.thisBox(name, b.CreatedAt); err != nil {
		return 0, nil, err
	}
	s.removeBox(name)
	return http.StatusNoContent, nil, nil
}

func (s *server) handleExtend(r *http.Request, body []byte) (int, any, error) {
	name := r.PathValue("name")
	var b dash.ExtendBody
	if err := decode(body, &b); err != nil {
		return 0, nil, err
	}
	if !slices.Contains(dash.ExtendHours, b.Hours) {
		return 0, nil, fail(http.StatusBadRequest, dash.CodeBadRequest, "hours must be 1, 4 or 24")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i, err := s.thisBox(name, b.CreatedAt)
	if err != nil {
		return 0, nil, err
	}
	bx := &s.sc.State.Boxes[i]
	from := s.now().UTC().Truncate(time.Second)
	if bx.ExpiresAt != nil && bx.ExpiresAt.After(from) {
		from = *bx.ExpiresAt
	}
	t := from.Add(time.Duration(b.Hours) * time.Hour)
	bx.ExpiresAt = &t
	s.publish(dash.EventBox, *bx)
	return http.StatusOK, *bx, nil
}

// handleJobEnded is box's report that a run ended, sent before it deletes a throwaway. Only a
// job running now is ended by it. The devserver's tails are made up, so it masks nothing; boxd
// masks this tail as it masks every tail.
func (s *server) handleJobEnded(r *http.Request, body []byte) (int, any, error) {
	var b dash.JobEndedBody
	if err := decode(body, &b); err != nil {
		return 0, nil, err
	}
	if b.Box == "" || b.ID == "" || b.EndedAt.IsZero() {
		return 0, nil, fail(http.StatusBadRequest, dash.CodeBadRequest, "box, id and endedAt are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.sc.State.Jobs {
		j := &s.sc.State.Jobs[i]
		if j.Box == b.Box && j.ID == b.ID && (j.State == dash.JobRunning || j.State == dash.JobPaused) {
			if b.Tail != "" {
				s.logs[j.Key] = b.Tail
			}
			s.end(j, b.Exit)
			ended := b.EndedAt.UTC()
			j.EndedAt = &ended
			if b.Tail != "" {
				j.Tail = fixture.Tail(b.Tail)
			}
			s.done[slices.IndexFunc(s.done, func(d dash.Job) bool { return d.Key == j.Key })] = *j
			s.publish(dash.EventJob, *j)
			return http.StatusNoContent, nil, nil
		}
	}
	return 0, nil, fail(http.StatusNotFound, dash.CodeNotFound, "no running job %s on %s", b.ID, b.Box)
}

func (s *server) handleRefusal(r *http.Request, body []byte) (int, any, error) {
	var b dash.RefusalBody
	if err := decode(body, &b); err != nil {
		return 0, nil, err
	}
	if !slices.Contains(dash.Refusals, b.Why) {
		return 0, nil, fail(http.StatusBadRequest, dash.CodeBadRequest, "why must be one of %s", strings.Join(dash.Refusals, ", "))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range []*dash.History{&s.sc.Day, &s.sc.Week} {
		if n := len(h.Buckets); n > 0 {
			h.Buckets[n-1].Refusals.ByWhy[b.Why]++
		}
	}
	return http.StatusNoContent, nil, nil
}

// end finishes a running job with an exit code, moving it to the finished jobs.
func (s *server) end(j *dash.Job, exit int) {
	now := s.now().UTC().Truncate(time.Second)
	j.State, j.Exit, j.EndedAt = dash.JobDone, &exit, &now
	if exit == 130 {
		s.logs[j.Key] += "^C\nbox: cancelled\n"
	} else {
		j.Exit = &exit
		s.logs[j.Key] += s.player.End(*j)
	}
	j.Tail = fixture.Tail(s.logs[j.Key])
	delete(s.plan, j.Key)
	s.done = append([]dash.Job{*j}, s.done...)
	slices.SortStableFunc(s.done, newest)
	s.publish(dash.EventJob, *j)
	if i := s.findBox(j.Box); i >= 0 {
		b := &s.sc.State.Boxes[i]
		exp := now.Add(2 * time.Hour)
		b.ExpiresAt, b.CPUPct = &exp, 1.5
		s.publish(dash.EventBox, *b)
	}
}

// removeBox deletes a box: its running jobs are lost, and its logs go with it.
func (s *server) removeBox(name string) {
	now := s.now().UTC().Truncate(time.Second)
	for i := range s.sc.State.Jobs {
		j := &s.sc.State.Jobs[i]
		if j.Box == name && (j.State == dash.JobRunning || j.State == dash.JobPaused) {
			j.State, j.EndedAt = dash.JobLost, &now
			j.Tail = fixture.Tail(s.logs[j.Key])
			delete(s.plan, j.Key)
			s.done = append([]dash.Job{*j}, s.done...)
			slices.SortStableFunc(s.done, newest)
			s.publish(dash.EventJob, *j)
		}
		if j.Box == name {
			delete(s.logs, j.Key)
		}
	}
	i := s.findBox(name)
	gone := s.sc.State.Boxes[i]
	s.sc.State.Boxes = slices.Delete(s.sc.State.Boxes, i, i+1)
	s.publish(dash.EventBoxGone, dash.BoxGone{Name: name, Machine: gone.Machine})
	s.reallot()
}

func (s *server) reallot() {
	m := s.machine()
	fixture.Allot(m, s.sc.State.Boxes)
	for _, b := range s.sc.State.Boxes {
		s.publish(dash.EventBox, b)
	}
	s.publish(dash.EventMachine, *m)
}
