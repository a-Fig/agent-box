package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
)

const fleetProbeTimeout = 5 * time.Second

type fleetHostStatus struct {
	Name      string     `json:"name"`
	Remote    string     `json:"remote"`
	Project   string     `json:"project"`
	Reachable bool       `json:"reachable"`
	Available bool       `json:"available"`
	Error     string     `json:"error,omitempty"`
	Mode      string     `json:"mode"`
	Room      string     `json:"room"`
	Sizes     []SizeRoom `json:"sizes"`
	GPU       string     `json:"gpu"`
	Queued    *int       `json:"queued,omitempty"`
}

// fleetStatus retains the currently selected remote's advice and exit status.
// A configured host becoming available does not change where box run goes.
func (a *App) fleetStatus(ctx context.Context, dir string, listOnly bool) (StatusReport, error) {
	hosts, err := a.configuredHosts()
	if err != nil {
		return StatusReport{}, err
	}
	if len(hosts) == 0 && !listOnly {
		return a.Status(ctx, dir), nil
	}
	visible := max(len(hosts), 1)
	selected := -1
	for i, host := range hosts {
		if host.Remote == a.Remote && host.Project == a.Project {
			selected = i
			break
		}
	}
	if selected < 0 && (!listOnly || len(hosts) == 0) {
		selected = len(hosts)
		hosts = append(hosts, fleetHost{Name: a.Remote, Remote: a.Remote, Project: a.Project})
	}
	return a.probeFleet(ctx, dir, hosts, selected, visible), nil
}

func (a *App) probeFleet(ctx context.Context, dir string, hosts []fleetHost, selected, visible int) StatusReport {
	var wg sync.WaitGroup
	var current StatusReport
	var warnings []string
	rows := make([]fleetHostStatus, visible)
	for i, host := range hosts {
		wg.Go(func() {
			hostCtx, cancel := context.WithTimeout(ctx, fleetProbeTimeout)
			defer cancel()
			app := *a
			app.Remote, app.Project = host.Remote, host.Project
			app.Warn = func(string) {}
			repoDir := ""
			if i == selected {
				repoDir = dir
				app.Warn = func(message string) { warnings = append(warnings, message) }
			}
			s := app.Status(hostCtx, repoDir)
			if s.Reachable && hostCtx.Err() != nil {
				s = StatusReport{
					Remote: s.Remote, Reachable: true, Mode: s.Mode, Update: s.Update, Repo: s.Repo,
					MaxBoxes: -1, SlotsFree: -1, Error: hostCtx.Err().Error(),
					Advice: fmt.Sprintf("unavailable: incomplete status for %s (%s)", host.Remote, hostCtx.Err()),
				}
			}
			if i == selected {
				current = s
			}
			if i >= visible {
				return
			}
			row := fleetHostStatus{
				Name: host.Name, Remote: host.Remote, Project: host.Project,
				Reachable: s.Reachable, Available: s.Available, Error: s.Error,
				Mode: s.Mode, Room: s.Room, Sizes: s.Sizes, GPU: s.GPU,
			}
			if s.Reachable && s.Error == "" {
				row.Queued = app.fleetQueueLength(hostCtx, host.DashboardURL)
			}
			rows[i] = row
		})
	}
	wg.Wait()
	for _, message := range warnings {
		a.warn(message)
	}
	current.Hosts = rows
	return current
}

func (a *App) fleetQueueLength(ctx context.Context, dashboardURL string) *int {
	var err error
	if dashboardURL == "" {
		dashboardURL, err = a.DashURL(ctx)
		if err != nil {
			return nil
		}
	}
	code, body, err := a.boxdRequest(ctx, http.MethodGet, strings.TrimRight(dashboardURL, "/")+"/api/state", "", nil)
	var state struct {
		Jobs []struct {
			State string `json:"state"`
		} `json:"jobs"`
	}
	if err != nil || code != http.StatusOK || json.Unmarshal(body, &state) != nil || state.Jobs == nil {
		return nil
	}
	queued := 0
	for _, job := range state.Jobs {
		if job.State == dash.JobQueued {
			queued++
		}
	}
	return &queued
}

func writeFleet(w io.Writer, hosts []fleetHostStatus) {
	ready, reachable := 0, 0
	for _, host := range hosts {
		if host.Available {
			ready++
		}
		if host.Reachable {
			reachable++
		}
	}
	fmt.Fprintf(w, "hosts: %d available, %d of %d reachable\n", ready, reachable, len(hosts))
	for _, host := range hosts {
		fmt.Fprintf(w, "  %s (%s/%s)", host.Name, host.Remote, host.Project)
		if !host.Reachable {
			fmt.Fprintf(w, ": unavailable (%s)\n", host.Error)
			continue
		}
		mode, queue := host.Mode, "unknown"
		if mode == "" {
			mode = "unknown"
		}
		if host.Error != "" {
			fmt.Fprintf(w, ": %s, capacity unknown (%s)\n", mode, host.Error)
			continue
		}
		if host.Queued != nil {
			queue = fmt.Sprint(*host.Queued)
		}
		fmt.Fprintf(w, ": %s, room %s, GPU %s, queue %s\n", mode, host.Room, host.GPU, queue)
	}
}
