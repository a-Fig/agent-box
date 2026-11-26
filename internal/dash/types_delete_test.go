package dash

import (
	"testing"
	"time"
)

func TestDeleteRisk(t *testing.T) {
	now := time.Date(2026, 11, 24, 18, 0, 0, 0, time.UTC)
	box := Box{Name: "w", Machine: "pc", UID: "u1", CreatedAt: now.Add(-time.Hour)}
	mk := func(id, state string, started time.Time, ended *time.Time) Job {
		return Job{Key: JobKey("w", id, started), ID: id, Box: "w", BoxUID: "u1", Machine: "pc", State: state, StartedAt: started, EndedAt: ended}
	}
	recentEnd := now.Add(-time.Minute)
	expiredEnd := now.Add(-3 * time.Minute)
	cases := []struct {
		name           string
		jobs           []Job
		active, recent int
	}{
		{"empty", nil, 0, 0},
		{"running", []Job{mk("a", JobRunning, now.Add(-time.Minute), nil)}, 1, 0},
		{"paused", []Job{mk("a", JobPaused, now.Add(-time.Minute), nil)}, 1, 0},
		{"queued is starting", []Job{mk("a", JobQueued, now.Add(-time.Minute), nil)}, 1, 0},
		{"recent", []Job{mk("a", JobDone, now.Add(-5*time.Minute), &recentEnd)}, 0, 1},
		{"expired", []Job{mk("a", JobDone, now.Add(-10*time.Minute), &expiredEnd)}, 0, 0},
		{"replacement box job", []Job{{Key: "x", ID: "a", Box: "w", BoxUID: "u2", Machine: "pc", State: JobRunning, StartedAt: now}}, 0, 0},
	}
	for _, c := range cases {
		w := DeleteRisk(c.jobs, box, now)
		if len(w.Active) != c.active || len(w.Recent) != c.recent {
			t.Errorf("%s: active %d recent %d, want %d/%d", c.name, len(w.Active), len(w.Recent), c.active, c.recent)
		}
	}
}
