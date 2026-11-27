package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/a-Fig/agent-box/internal/dash"
)

// listJobs scans guest jobs and configured queues. An explicit box name
// limits the guest scan to the default host.
func (a *App) listJobs(ctx context.Context, box string) ([]JobInfo, error) {
	hosts, err := a.configuredHosts()
	if err != nil {
		return nil, err
	}
	if len(hosts) == 0 {
		return a.Jobs(ctx, box)
	}

	type listing struct {
		host     fleetHost
		bare     bool
		jobs     []JobInfo
		queued   []JobInfo
		warnings []string
		err      error
	}
	defaultHost := fleetHost{Name: a.Remote, Remote: a.Remote, Project: a.Project}
	defaultKey := [2]string{a.Remote, a.Project}
	seen := make(map[[2]string]bool, len(hosts)+1)
	lists := make([]listing, 0, len(hosts)+1)
	for i, host := range append(hosts, defaultHost) {
		key := [2]string{host.Remote, host.Project}
		if box != "" && key != defaultKey {
			continue
		}
		if !seen[key] {
			seen[key] = true
			lists = append(lists, listing{host: host, bare: i == len(hosts)})
		}
	}

	// A filtered legacy scan keeps its original output and errors.
	if box != "" && lists[0].bare {
		return a.Jobs(ctx, box)
	}

	var wg sync.WaitGroup
	for i := range lists {
		result := &lists[i]
		host := result.host
		if !result.bare {
			tag, err := jobHostTag(host)
			if err == nil {
				// A row should only advertise IDs that the reader can resolve.
				resolved, resolveErr := resolveJobHost(tag, fleetHost{}, hosts)
				host.DashboardURL, err = resolved.DashboardURL, resolveErr
			}
			if err != nil {
				result.err = err
				continue
			}
		}
		app := a.jobHostApp(host)
		app.Warn = func(msg string) { result.warnings = append(result.warnings, msg) }
		wg.Go(func() {
			hostCtx, cancel := context.WithTimeout(ctx, fleetProbeTimeout)
			defer cancel()
			result.jobs, result.err = app.Jobs(hostCtx, box)
		})
		if box == "" && !result.bare && !app.boxdDisabled {
			wg.Go(func() {
				hostCtx, cancel := context.WithTimeout(ctx, fleetProbeTimeout)
				defer cancel()
				result.queued = app.listQueuedJobs(hostCtx, host.DashboardURL)
			})
		}
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var jobs []JobInfo
	var failures []error
	for _, result := range lists {
		host := result.host
		label := fmt.Sprintf("%s (%s/%s)", host.Name, host.Remote, host.Project)
		if result.err != nil {
			failures = append(failures, fmt.Errorf("listing jobs on %s: %w", label, result.err))
		}
		for _, msg := range result.warnings {
			a.warn(fmt.Sprintf("box: %s: %s", label, strings.TrimPrefix(msg, "box: ")))
		}
		seen := make(map[string]bool, len(result.jobs)+len(result.queued))
		for _, job := range append(result.jobs, result.queued...) {
			if seen[job.Job] {
				continue
			}
			seen[job.Job] = true
			if !result.bare {
				id, err := qualifyJobID(host, job.Job)
				if err != nil {
					a.warn(fmt.Sprintf("box: listing jobs on %s: %v", label, err))
					continue
				}
				// Keep legacy IDs only when there is no qualified record.
				legacy := [2]string{host.Remote, host.Project} == defaultKey &&
					a.hasJobRecord(id.Bare) && !a.hasJobRecord(id.ID)
				if !legacy {
					job.Job = id.ID
				}
			}
			job.Host = host.Name
			jobs = append(jobs, job)
		}
	}
	if len(failures) == len(lists) && len(jobs) == 0 {
		return nil, errors.Join(failures...)
	}
	for _, err := range failures {
		a.warn("box: " + err.Error())
	}
	slices.SortStableFunc(jobs, func(a, b JobInfo) int { return b.Started.Compare(a.Started) })
	return jobs, nil
}

func (a *App) hasJobRecord(job string) bool {
	return a.StateDir != "" && (a.exists(job, ".json") || a.exists(job, ".result"))
}

// listQueuedJobs also recovers jobs whose submission reply was lost. They have
// no guest log yet, and boxd already publishes their bare ID in its state.
func (a *App) listQueuedJobs(ctx context.Context, dashboardURL string) []JobInfo {
	state, ok := a.readFleetBoxdState(ctx, dashboardURL)
	if !ok || !state.Queue {
		return nil
	}
	var jobs []JobInfo
	for _, j := range state.Jobs {
		if j.State == dash.JobQueued {
			jobs = append(jobs, JobInfo{Job: j.Box + "." + j.ID, Box: j.Box, State: stateQueued, Started: j.StartedAt})
		}
	}
	return jobs
}
