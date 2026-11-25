package main

import (
	"slices"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/dash/fixture"
)

// play moves the scenario on every tick, as the PC would: running jobs write to their logs and
// finish when their time comes, a new job starts now and then (on a new box when it needs one),
// an idle throwaway box goes, the tray applies a mode request, and a game starts and ends. The
// tray-down scenario changes no mode, since nothing would apply one.
func (s *server) play(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for range t.C {
		s.mu.Lock()
		s.step()
		s.mu.Unlock()
	}
}

// heartbeat keeps a live tray alive while the scenario stands still, as the real tray writes
// tray-seen every 30s; a scenario whose tray is down stays down.
func (s *server) heartbeat() {
	s.mu.Lock()
	alive := s.machine().TrayAlive(s.now())
	s.mu.Unlock()
	for alive {
		time.Sleep(10 * time.Second)
		s.mu.Lock()
		seen := s.now().UTC().Truncate(time.Second)
		s.machine().TrayLastSeen = &seen
		s.publish(dash.EventMachine, *s.machine())
		s.mu.Unlock()
	}
}

func (s *server) step() {
	s.tick++
	now := s.now()
	m := s.machine()
	rnd := s.player.Rand()
	trayUp := m.TrayAlive(now)
	if trayUp {
		seen := now.UTC().Truncate(time.Second)
		m.TrayLastSeen = &seen
	}

	// The mode: a request the tray picks up, and a game that starts and ends.
	if !m.Yielding && m.ModeRequested != nil && !s.applyAt.IsZero() && now.After(s.applyAt) {
		m.Mode, m.ModeRequested, s.applyAt = m.ModeRequested.Mode, nil, time.Time{}
		m.Chosen = &dash.ChosenMode{Mode: m.Mode, At: now.UTC().Truncate(time.Second)}
		s.setFrozen(m.Mode == dash.ModeMyPC)
		s.reallot()
	}
	switch {
	case m.Yielding && now.After(s.yieldEnd):
		m.Yielding, m.ModeReason, m.Mode = false, "", s.before
		if m.Chosen != nil {
			m.Mode = m.Chosen.Mode // the pick comes back after the game
		}
		if m.ModeRequested != nil {
			m.Mode, m.ModeRequested = m.ModeRequested.Mode, nil
			m.Chosen = &dash.ChosenMode{Mode: m.Mode, At: now.UTC().Truncate(time.Second)}
		}
		s.yieldEnd = time.Time{}
		s.setFrozen(m.Mode == dash.ModeMyPC)
		s.reallot()
	case !m.Yielding && trayUp && s.tick%45 == 30:
		s.before = m.Mode
		m.Yielding, m.Mode, m.ModeReason = true, dash.ModeMyPC, "gaming: steam:570"
		s.yieldEnd = now.Add(30 * time.Second)
		s.setFrozen(true)
		s.reallot()
	}

	// Jobs: logs grow while their box runs; a job whose time has come ends.
	for i := range s.sc.State.Jobs {
		j := &s.sc.State.Jobs[i]
		if j.State != dash.JobRunning {
			continue
		}
		if until, ok := s.plan[j.Key]; ok && now.After(until) {
			exit := 0
			if rnd.IntN(5) == 0 {
				exit = 1
			}
			s.end(j, exit)
			continue
		}
		s.logs[j.Key] += s.player.Lines(*j, 1+rnd.IntN(3))
		j.Tail = fixture.Tail(s.logs[j.Key])
		s.publish(dash.EventJob, *j)
	}
	// Boxes: live use wanders; a throwaway whose job ended goes after a while.
	for i := range s.sc.State.Boxes {
		b := &s.sc.State.Boxes[i]
		if b.State != dash.BoxRunning {
			continue
		}
		busy := s.busy(b.Name)
		if busy {
			b.CPUPct = min(100, max(35, b.CPUPct+float64(rnd.IntN(21)-10)))
		}
		b.MemUsed = min(b.MemLimit, max(b.MemLimit/8, b.MemUsed+int64(rnd.IntN(200)-100)<<20))
		s.publish(dash.EventBox, *b)
	}
	for _, b := range slices.Clone(s.sc.State.Boxes) {
		if b.Throwaway && !s.busy(b.Name) && b.State == dash.BoxRunning && rnd.IntN(8) == 0 {
			s.removeBox(b.Name)
		}
	}
	// The queue: the next job that fits starts.
	s.stepQueue()
	// Now and then a new job, if the PC has room and isn't frozen.
	if m.Mode != dash.ModeMyPC && s.tick%20 == 8 && s.running() < 3 {
		s.startJob()
	}
	// The state's finished jobs are the last 50.
	s.trim()
}

func (s *server) busy(box string) bool {
	return slices.ContainsFunc(s.sc.State.Jobs, func(j dash.Job) bool {
		return j.Box == box && (j.State == dash.JobRunning || j.State == dash.JobPaused)
	})
}

func (s *server) running() int {
	n := 0
	for _, j := range s.sc.State.Jobs {
		if j.State == dash.JobRunning || j.State == dash.JobPaused {
			n++
		}
	}
	return n
}

func (s *server) startJob() {
	m := s.machine()
	box, job := s.player.Start(s.now(), func(name string) bool { return s.findBox(name) >= 0 })
	room := map[string]int{"small": m.Room.Small, "medium": m.Room.Medium, "large": m.Room.Large, "gpu": m.Room.GPU}
	if room[box.Size] == 0 {
		return // boxd would have seen a refusal here
	}
	s.sc.State.Boxes = append(s.sc.State.Boxes, box)
	s.reallot()
	s.sc.State.Jobs = append([]dash.Job{job}, s.sc.State.Jobs...)
	s.logs[job.Key] = "$ " + job.Command + "\n"
	s.plan[job.Key] = s.now().Add(time.Duration(30+s.player.Rand().IntN(60)) * time.Second)
	s.publish(dash.EventJob, job)
}

// setFrozen freezes or resumes every box, and pauses or resumes the jobs on them, as the tray
// does when the mode or a game says so.
func (s *server) setFrozen(frozen bool) {
	for i := range s.sc.State.Boxes {
		b := &s.sc.State.Boxes[i]
		if frozen && b.State == dash.BoxRunning {
			b.State = dash.BoxFrozen
		} else if !frozen && b.State == dash.BoxFrozen {
			b.State = dash.BoxRunning
		}
	}
	for i := range s.sc.State.Jobs {
		j := &s.sc.State.Jobs[i]
		was := j.State
		if frozen && j.State == dash.JobRunning {
			j.State = dash.JobPaused
		} else if !frozen && j.State == dash.JobPaused {
			j.State = dash.JobRunning
		}
		if j.State != was {
			if until, ok := s.plan[j.Key]; ok && !frozen {
				if soon := s.now().Add(20 * time.Second); until.Before(soon) {
					s.plan[j.Key] = soon
				}
			}
			s.publish(dash.EventJob, *j)
		}
	}
}

func (s *server) trim() {
	slices.SortStableFunc(s.sc.State.Jobs, func(a, b dash.Job) int { return b.StartedAt.Compare(a.StartedAt) })
	var keep []dash.Job
	finished := 0
	for _, j := range s.sc.State.Jobs {
		if j.State == dash.JobDone || j.State == dash.JobLost {
			finished++
			if finished > dash.FinishedInState {
				s.publish(dash.EventJobGone, dash.JobGone{Key: j.Key})
				continue
			}
		}
		keep = append(keep, j)
	}
	s.sc.State.Jobs = keep
}
