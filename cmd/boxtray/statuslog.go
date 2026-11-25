package main

import "github.com/a-Fig/agent-box/internal/mode"

// statusLog turns the stream of tick results into log lines, one per change:
// mode picks, the distro going up or down, errors. Ticks run
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
		s.logf("first status: chosen %s, %d running", st.Chosen, st.Running)
	} else if st.Chosen != prev.Chosen {
		s.logf("mode chosen: %s (was %s)", st.Chosen, prev.Chosen)
	}
	if st.Down && (first || !prev.Down) {
		s.logf("WSL distro is not running: nothing applied")
	} else if !st.Down && prev.Down {
		s.logf("WSL distro is running again")
	}
	for _, r := range st.Reclaimed {
		s.logf("reclaimed %s: %.0fMiB left in RAM", r.Name, float64(r.Left)/(1<<20))
	}
	if e := errText(st.Err); e != "" && e != errText(prev.Err) {
		s.logf("tick error: %s", e)
	} else if e == "" && errText(prev.Err) != "" {
		s.logf("tick errors cleared")
	}
}
