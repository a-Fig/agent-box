package main

import (
	"net/http"
	"slices"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
)

// The queue as boxd keeps it (docs/queue.md, internal/boxd/queue_api.go): waiting jobs in
// State.Jobs with their place in line, the two controls, and, while the scenario plays, the first
// job that fits starting on its box. Every change to a waiting job's place or reason is sent as a
// job event, as boxd sends it.

// queue is the waiting jobs in line order, as pointers into State.Jobs: valid until State.Jobs
// changes.
func (s *server) queue() []*dash.Job {
	var q []*dash.Job
	for i := range s.sc.State.Jobs {
		if s.sc.State.Jobs[i].State == dash.JobQueued {
			q = append(q, &s.sc.State.Jobs[i])
		}
	}
	slices.SortStableFunc(q, func(a, b *dash.Job) int { return a.Position - b.Position })
	return q
}

// waiting is the waiting job named job ("<box>.<id>", as box names jobs), or nil.
func (s *server) waiting(job string) *dash.Job {
	for _, j := range s.queue() {
		if j.Box+"."+j.ID == job {
			return j
		}
	}
	return nil
}

// renumber gives the line in q's order the places 1, 2, ..., and sends each job whose place
// changed.
func (s *server) renumber(q []*dash.Job) {
	for i, j := range q {
		if j.Position != i+1 {
			j.Position = i + 1
			s.publish(dash.EventJob, *j)
		}
	}
}

// notWaiting is boxd's answer for a job that isn't in the line: started, cancelled or never queued.
func notWaiting(job string) error {
	return fail(http.StatusConflict, dash.CodeNotRunning, "job %s isn't waiting: it has started or gone", job)
}

// removeQueued takes the job with key out of State.Jobs and says it went.
func (s *server) removeQueued(key string) {
	s.sc.State.Jobs = slices.DeleteFunc(s.sc.State.Jobs, func(x dash.Job) bool { return x.Key == key })
	s.publish(dash.EventJobGone, dash.JobGone{Key: key})
}

// handleQueueCancel takes a job out of the line; boxd also deletes its snapshot. 204.
func (s *server) handleQueueCancel(r *http.Request, body []byte) (int, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := r.PathValue("job")
	j := s.waiting(job)
	if j == nil {
		return 0, nil, notWaiting(job)
	}
	s.removeQueued(j.Key)
	s.renumber(s.queue()) // pointers taken after the removal
	return http.StatusNoContent, nil, nil
}

// handleQueueFront moves a job to the front of the line. 200, the job.
func (s *server) handleQueueFront(r *http.Request, body []byte) (int, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := r.PathValue("job")
	j := s.waiting(job)
	if j == nil {
		return 0, nil, notWaiting(job)
	}
	q := s.queue()
	i := slices.Index(q, j)
	s.renumber(append([]*dash.Job{j}, slices.Delete(slices.Clone(q), i, i+1)...))
	return http.StatusOK, *j, nil
}

// whyWaiting is why a job waits now: the PC frozen, or no room for its size.
func whyWaiting(m *dash.Machine, gpu bool) string {
	switch {
	case m.Mode == dash.ModeMyPC:
		return dash.RefusedMyPC
	case gpu:
		return dash.RefusedGPUBusy
	}
	return dash.RefusedFull
}

// fits says whether j can start now: on its own box if that exists and is idle, else on a new box
// of its size.
func (s *server) fits(j *dash.Job) bool {
	if i := s.findBox(j.Box); i >= 0 {
		return s.sc.State.Boxes[i].State == dash.BoxRunning && !s.busy(j.Box)
	}
	m := s.machine()
	room := map[string]int{"small": m.Room.Small, "medium": m.Room.Medium, "large": m.Room.Large, "gpu": m.Room.GPU}
	return room[j.Size] > 0
}

// stepQueue starts the first waiting job that fits, if the PC isn't frozen, and keeps each waiting
// job's reason current.
func (s *server) stepQueue() {
	m := s.machine()
	if m.Mode != dash.ModeMyPC {
		for _, j := range s.queue() {
			if s.fits(j) {
				s.startQueued(*j)
				s.renumber(s.queue()) // State.Jobs changed: fresh pointers
				break
			}
		}
	}
	for _, j := range s.queue() {
		if why := whyWaiting(m, j.GPU); why != j.Why {
			j.Why = why
			s.publish(dash.EventJob, *j)
		}
	}
}

// startQueued starts a waiting job as boxd's starter does: its waiting key goes, and the running
// job comes under its own key, with the same ID and QueuedAt, on its box (made if it is gone).
func (s *server) startQueued(q dash.Job) {
	box, job := s.player.StartQueued(s.now(), q)
	s.removeQueued(q.Key)
	if i := s.findBox(box.Name); i < 0 {
		s.sc.State.Boxes = append(s.sc.State.Boxes, box)
		s.reallot()
	} else {
		job.BoxUID = s.sc.State.Boxes[i].UID
	}
	s.sc.State.Jobs = append([]dash.Job{job}, s.sc.State.Jobs...)
	s.logs[job.Key] = "$ " + job.Command + "\n"
	s.plan[job.Key] = s.now().Add(time.Duration(30+s.player.Rand().IntN(60)) * time.Second)
	s.publish(dash.EventJob, job)
}
