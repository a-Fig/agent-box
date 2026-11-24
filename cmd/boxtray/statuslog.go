package main

import "github.com/a-Fig/agent-box/internal/mode"

// statusLog turns the stream of tick results into log lines, one per change:
// mode picks, yields to games, the distro going up or down, errors. Ticks run
// every few seconds, so logging each one would bury what matters.
type statusLog struct {
	logf func(format string, args ...any)
	seen bool
	last mode.Status
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *statusLog) observe(st mode.Status) {
	prev := s.last
	first := !s.seen
	s.last, s.seen = st, true
	if first {
		s.logf("first status: chosen %s, effective %s, %d running", st.Chosen, st.Effective, st.Running)
	} else if st.Chosen != prev.Chosen {
		s.logf("mode chosen: %s (was %s)", st.Chosen, prev.Chosen)
	}
	if st.Yielding && (first || !prev.Yielding) {
		s.logf("yielding to a game (%s): instances paused", st.Reason)
	} else if !st.Yielding && prev.Yielding {
		s.logf("game ended: back to %s", st.Effective)
	}
	if st.Down && (first || !prev.Down) {
		s.logf("WSL distro is not running: nothing applied")
	} else if !st.Down && prev.Down {
		s.logf("WSL distro is running again")
	}
	if e := errText(st.Err); e != "" && e != errText(prev.Err) {
		s.logf("tick error: %s", e)
	} else if e == "" && errText(prev.Err) != "" {
		s.logf("tick errors cleared")
	}
}
