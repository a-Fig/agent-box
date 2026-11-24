package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/a-Fig/agent-box/internal/mode"
)

// StatusReport says whether offloading to the PC is worth it now.
type StatusReport struct {
	Advice    string   `json:"advice"`
	Available bool     `json:"available"` // box_run can start a job now
	Remote    string   `json:"remote"`
	Reachable bool     `json:"reachable"`
	Error     string   `json:"error,omitempty"` // why the PC can't be reached
	Mode      string   `json:"mode"`            // mypc, shared, full, or "" if unknown
	Yielding  bool     `json:"yielding"`        // the tray has paused boxes for a game
	Reason    string   `json:"reason,omitempty"`
	Boxes     int      `json:"boxes"`
	MaxBoxes  int      `json:"maxBoxes"`  // -1: no limit
	SlotsFree int      `json:"slotsFree"` // -1: no limit
	BoxCPUs   int      `json:"boxCpus"`   // CPUs each box gets now
	BoxMemory string   `json:"boxMemory"`
	Repo      string   `json:"repo,omitempty"`    // when run in a git repo
	RepoBox   *RepoBox `json:"repoBox,omitempty"` // the repo's own box, when it exists
}

// RepoBox is a repo's own box and the jobs running in it.
type RepoBox struct {
	Name    string    `json:"name"`
	Status  string    `json:"status"`
	Expires string    `json:"expires,omitempty"`
	Running []JobInfo `json:"running"`
}

// Status reports the PC's state and, when dir is in a git repo, the repo's
// box and its running jobs. Only failing to read the repo is an error: an
// unreachable PC is a report.
func (a *App) Status(ctx context.Context, dir string) StatusReport {
	s := StatusReport{Remote: a.Remote, MaxBoxes: -1, SlotsFree: -1}
	if dir != "" {
		if rp, err := openRepo(ctx, a.R, dir); err == nil {
			s.Repo = rp.Name
		}
	}
	insts, err := a.list(ctx)
	if err != nil {
		s.Error = firstLine(err.Error())
		s.Advice = fmt.Sprintf("unavailable: can't reach %s (%s): run locally", a.Remote, s.Error)
		return s
	}
	s.Reachable = true
	s.Boxes = len(insts)
	s.Mode = a.mode(ctx)
	s.Reason = a.projectKey(ctx, mode.ProjectReasonKey)
	s.Yielding = s.Reason != ""
	if n, err := strconv.Atoi(a.projectKey(ctx, "limits.instances")); err == nil {
		s.MaxBoxes, s.SlotsFree = n, max(n-len(insts), 0)
	}
	s.BoxMemory = "4GiB"
	if out, err := a.incusOK(ctx, "profile", "get", a.target("default"), "limits.memory"); err == nil && strings.TrimSpace(out) != "" {
		s.BoxMemory = strings.TrimSpace(out)
	}
	switch {
	case s.Yielding:
	case s.Mode == "": // no tray: the profile's limits.cpu applies, which is Shared's
		s.BoxCPUs = mode.Shared.BoxCPUs()
	default:
		s.BoxCPUs = mode.Mode(s.Mode).BoxCPUs()
	}
	var repoBox *instance
	if s.Repo != "" {
		repoBox = find(insts, s.Repo)
	}
	if repoBox != nil {
		s.RepoBox = &RepoBox{Name: repoBox.Name, Status: repoBox.Status, Expires: repoBox.Config[expiresKey], Running: []JobInfo{}}
		if repoBox.Status == "Running" {
			jobs, err := a.boxJobs(ctx, repoBox.Name)
			if err != nil {
				a.warn(fmt.Sprintf("box: listing jobs in %s: %v", repoBox.Name, err))
			}
			for _, j := range jobs {
				if j.State == jobRunning || j.State == jobStarting {
					s.RepoBox.Running = append(s.RepoBox.Running, j)
				}
			}
		}
	}
	s.Advice, s.Available = advice(s, repoBox)
	return s
}

func advice(s StatusReport, repoBox *instance) (string, bool) {
	size := fmt.Sprintf("%d CPUs/%s per box", s.BoxCPUs, s.BoxMemory)
	switch {
	case s.Mode == string(mode.MyPC):
		return "unavailable: PC is in My PC mode: run locally", false
	case s.Yielding:
		return fmt.Sprintf("unavailable: PC is yielding to a game (%s): run locally", s.Reason), false
	case repoBox != nil && repoBox.Config[throwawayKey] != "true":
		busy := ""
		if n := len(s.RepoBox.Running); n > 0 {
			busy = fmt.Sprintf(", %d job(s) running there (runs take turns)", n)
		}
		return fmt.Sprintf("available: this repo's box %s is %s%s; %s", repoBox.Name, strings.ToLower(repoBox.Status), busy, size) + modeNote(s), true
	case s.SlotsFree == 0:
		return fmt.Sprintf("busy: all %d box slots are taken: run locally, or reuse a box with box_run box", s.MaxBoxes), false
	case s.SlotsFree < 0:
		return "available: " + size + modeNote(s), true
	}
	return fmt.Sprintf("available: %d slot(s) free, %s", s.SlotsFree, size) + modeNote(s), true
}

func modeNote(s StatusReport) string {
	if s.Mode == "" {
		return " (mode unknown: the tray hasn't set it)"
	}
	return ""
}

// projectKey reads one key of the box project's config; "" if unset or unreadable.
func (a *App) projectKey(ctx context.Context, key string) string {
	out, err := a.incusOK(ctx, "project", "get", a.target(a.Project), key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

// writeStatus prints a report: the advice first, then the details.
func writeStatus(w io.Writer, s StatusReport) {
	fmt.Fprintln(w, s.Advice)
	if !s.Reachable {
		return
	}
	m := s.Mode
	if m == "" {
		m = "unknown"
	}
	if s.Yielding {
		m += ", yielding: " + s.Reason
	}
	fmt.Fprintf(w, "mode: %s\n", m)
	if s.MaxBoxes >= 0 {
		fmt.Fprintf(w, "boxes: %d of %d (%d free)\n", s.Boxes, s.MaxBoxes, s.SlotsFree)
	} else {
		fmt.Fprintf(w, "boxes: %d\n", s.Boxes)
	}
	fmt.Fprintf(w, "per box: %d CPUs, %s RAM\n", s.BoxCPUs, s.BoxMemory)
	switch {
	case s.RepoBox != nil:
		fmt.Fprintf(w, "repo box: %s, %s, expires %s\n", s.RepoBox.Name, s.RepoBox.Status, s.RepoBox.Expires)
		for _, j := range s.RepoBox.Running {
			fmt.Fprintf(w, "  %s %s since %s\n", j.Job, j.State, j.Started.Format("2006-01-02 15:04:05Z"))
		}
	case s.Repo != "":
		fmt.Fprintf(w, "repo box: %s (not created yet)\n", s.Repo)
	}
}
