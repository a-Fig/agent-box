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
	Name           string       `json:"name"`
	Remote         string       `json:"remote,omitempty"`
	Project        string       `json:"project,omitempty"`
	Kind           string       `json:"kind,omitempty"`
	CloudMode      string       `json:"cloudMode,omitempty"`
	Priority       int          `json:"priority,omitempty"`
	Reachable      bool         `json:"reachable"`
	Available      bool         `json:"available"`
	Error          string       `json:"error,omitempty"`
	Mode           string       `json:"mode"`
	Room           string       `json:"room"`
	Sizes          []SizeRoom   `json:"sizes"`
	GPU            string       `json:"gpu"`
	Queued         *int         `json:"queued,omitempty"`
	Running        int          `json:"running,omitempty"`
	MaxConcurrent  int          `json:"maxConcurrent,omitempty"`
	Paused         bool         `json:"paused,omitempty"`
	Budget         *cloudBudget `json:"budget,omitempty"`
	LastSeen       *time.Time   `json:"lastSeen,omitempty"`
	LastState      *dash.State  `json:"lastState,omitempty"`
	LastStateStale bool         `json:"lastStateStale,omitempty"`
	now            time.Time
}

// fleetStatus reports fleet capacity while retaining the default host's
// advice and exit status.
func (a *App) fleetStatus(ctx context.Context, dir string, listOnly bool) (StatusReport, error) {
	hosts, err := a.configuredHosts()
	if err != nil {
		return StatusReport{}, err
	}
	if len(hosts) == 0 && !listOnly {
		s := a.Status(ctx, dir)
		a.enrichControllerStatus(ctx, nil, nil, &s, -1, false)
		return s, nil
	}
	visible := max(len(hosts), 1)
	selected := -1
	for i, host := range hosts {
		if host.Kind == "" && host.Remote == a.Remote && host.Project == a.Project {
			selected = i
			break
		}
	}
	if selected < 0 && (!listOnly || len(hosts) == 0) {
		selected = len(hosts)
		hosts = append(hosts, fleetHost{Name: a.Remote, Remote: a.Remote, Project: a.Project})
	}
	return a.probeFleetWithMode(ctx, dir, hosts, selected, visible, listOnly), nil
}

func (a *App) probeFleet(ctx context.Context, dir string, hosts []fleetHost, selected, visible int) StatusReport {
	return a.probeFleetWithMode(ctx, dir, hosts, selected, visible, false)
}

func (a *App) probeFleetWithMode(ctx context.Context, dir string, hosts []fleetHost, selected, visible int, listOnly bool) StatusReport {
	var wg sync.WaitGroup
	var current StatusReport
	var warnings []string
	rows := make([]fleetHostStatus, visible)
	for i, host := range hosts {
		wg.Go(func() {
			hostCtx, cancel := context.WithTimeout(ctx, fleetProbeTimeout)
			defer cancel()
			app := *a
			if host.Kind != "" {
				if i >= visible {
					return
				}
				rows[i] = app.probeCloudStatus(hostCtx, host)
				return
			}
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
				Name: host.Name, Remote: host.Remote, Project: host.Project, Priority: host.Priority,
				Reachable: s.Reachable, Available: s.Available, Error: s.Error,
				Mode: s.Mode, Room: s.Room, Sizes: s.Sizes, GPU: s.GPU,
			}
			if s.Reachable && s.Error == "" {
				queued, _, hostPrio := app.fleetBoxdState(hostCtx, host.DashboardURL)
				row.Queued = queued
				if hostPrio != nil {
					row.Priority = *hostPrio
				}
			}
			rows[i] = row
		})
	}
	wg.Wait()
	for _, message := range warnings {
		a.warn(message)
	}
	a.enrichControllerStatus(ctx, hosts, rows, &current, selected, listOnly)
	current.Hosts = rows
	return current
}

func (a *App) probeCloudStatus(ctx context.Context, host fleetHost) fleetHostStatus {
	row := fleetHostStatus{
		Name:      host.Name,
		Kind:      host.Kind,
		CloudMode: host.Mode,
		Priority:  host.Priority,
		GPU:       gpuNone,
	}
	if row.CloudMode == "" {
		row.CloudMode = cloudModeFunction
	}
	ch, err := a.newCloudHost(host)
	if err != nil {
		row.Error = firstLine(err.Error())
		return row
	}
	cand, p := ch.probeStatus(ctx)
	row.Reachable = cand.Reachable
	row.Error = cand.Error
	row.Mode = cand.Mode
	row.Sizes = cand.Sizes
	if cand.HasGPU {
		if cand.Unavailable != "" {
			row.GPU = "unavailable: " + cand.Unavailable
		} else {
			row.GPU = "free"
		}
	}
	if !cand.Reachable || cand.Error != "" {
		return row
	}
	row.Available = cand.Unavailable == ""
	row.Running = p.Running
	row.MaxConcurrent = p.MaxConcurrent
	row.Paused = p.Paused
	row.Budget = p.Budget
	row.Room = fmt.Sprintf("%d/%d running", p.Running, p.MaxConcurrent)
	return row
}

type fleetBoxdSnapshot struct {
	Queue    bool       `json:"queue"`
	Priority *int       `json:"priority,omitempty"`
	Jobs     []dash.Job `json:"jobs"`
}

func (a *App) readFleetBoxdState(ctx context.Context, dashboardURL string) (fleetBoxdSnapshot, bool) {
	if dashboardURL == "" {
		var err error
		dashboardURL, err = a.DashURL(ctx)
		if err != nil {
			return fleetBoxdSnapshot{}, false
		}
	}
	code, body, err := a.boxdRequest(ctx, http.MethodGet, strings.TrimRight(dashboardURL, "/")+"/api/state", "", nil)
	var state fleetBoxdSnapshot
	if err != nil || code != http.StatusOK || json.Unmarshal(body, &state) != nil || state.Jobs == nil {
		return fleetBoxdSnapshot{}, false
	}
	return state, true
}

func (a *App) fleetBoxdState(ctx context.Context, dashboardURL string) (queued *int, queueCapable bool, priority *int) {
	state, ok := a.readFleetBoxdState(ctx, dashboardURL)
	if !ok {
		return nil, false, nil
	}
	n := 0
	for _, job := range state.Jobs {
		if job.State == dash.JobQueued {
			n++
		}
	}
	return &n, state.Queue, state.Priority
}

func writeFleet(w io.Writer, hosts []fleetHostStatus) {
	ready, reachable, showPriority := 0, 0, false
	for _, host := range hosts {
		if host.Available {
			ready++
		}
		if host.Reachable {
			reachable++
		}
		if host.Priority != 0 {
			showPriority = true
		}
	}
	fmt.Fprintf(w, "hosts: %d available, %d of %d reachable\n", ready, reachable, len(hosts))
	for _, host := range hosts {
		prio := ""
		if showPriority {
			prio = fmt.Sprintf("priority %d, ", host.Priority)
		}
		if host.Kind != "" {
			cloudMode := host.CloudMode
			if cloudMode == "" {
				cloudMode = cloudModeFunction
			}
			fmt.Fprintf(w, "  %s (%s, %s)", host.Name, host.Kind, cloudMode)
			if !host.Reachable || host.Error != "" {
				fmt.Fprintf(w, ": %sunavailable (%s)\n", prio, host.Error)
				continue
			}
			parts := []string{fmt.Sprintf("%d/%d running", host.Running, host.MaxConcurrent)}
			if b := host.Budget; b != nil {
				if b.CapUSD > 0 {
					parts = append(parts, fmt.Sprintf("$%.2f of $%.2f this month", b.SpentUSD, b.CapUSD))
				} else {
					parts = append(parts, fmt.Sprintf("$%.2f this month", b.SpentUSD))
				}
			}
			if host.Paused {
				parts = append(parts, "paused")
			}
			if b := host.Budget; b != nil && b.CapUSD > 0 {
				if b.SpentUSD+b.RunningUSD >= b.CapUSD {
					parts = append(parts, "cap reached")
				} else if b.WarnAt > 0 && b.SpentUSD/b.CapUSD >= b.WarnAt {
					parts = append(parts, "warning")
				}
			}
			fmt.Fprintf(w, ": %s%s\n", prio, strings.Join(parts, " · "))
			continue
		}
		fmt.Fprintf(w, "  %s (%s/%s)", host.Name, host.Remote, host.Project)
		if !host.Reachable {
			if host.LastSeen != nil && host.LastState != nil {
				fmt.Fprintf(w, ": %s\n", formatControllerCachedSummary(*host.LastSeen, host.LastStateStale, *host.LastState, host.now))
			} else {
				fmt.Fprintf(w, ": %sunavailable (%s)\n", prio, host.Error)
			}
			continue
		}
		mode, queue := host.Mode, "unknown"
		if mode == "" {
			mode = "unknown"
		}
		if host.Error != "" {
			fmt.Fprintf(w, ": %s%s, capacity unknown (%s)\n", prio, mode, host.Error)
			continue
		}
		if host.Queued != nil {
			queue = fmt.Sprint(*host.Queued)
		}
		fmt.Fprintf(w, ": %s%s, room %s, GPU %s, queue %s\n", prio, mode, host.Room, host.GPU, queue)
	}
}
