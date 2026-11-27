package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// listJobs extends the running-box scan to configured destinations. An explicit
// box name still means a box on the default host.
func (a *App) listJobs(ctx context.Context, box string) ([]JobInfo, error) {
	if box != "" {
		return a.Jobs(ctx, box)
	}
	hosts, err := a.configuredHosts()
	if err != nil {
		return nil, err
	}
	if len(hosts) == 0 {
		return a.Jobs(ctx, "")
	}

	type listing struct {
		host     fleetHost
		bare     bool
		jobs     []JobInfo
		warnings []string
		err      error
	}
	defaultHost := fleetHost{Name: a.Remote, Remote: a.Remote, Project: a.Project}
	defaultKey := [2]string{a.Remote, a.Project}
	seen := make(map[[2]string]bool, len(hosts)+1)
	lists := make([]listing, 0, len(hosts)+1)
	for i, host := range append(hosts, defaultHost) {
		key := [2]string{host.Remote, host.Project}
		if !seen[key] {
			seen[key] = true
			lists = append(lists, listing{host: host, bare: i == len(hosts)})
		}
	}

	var wg sync.WaitGroup
	for i := range lists {
		result := &lists[i]
		host := result.host
		if !result.bare {
			tag, err := jobHostTag(host)
			if err == nil {
				// A row should only advertise IDs that the reader can resolve.
				_, err = resolveJobHost(tag, fleetHost{}, hosts)
			}
			if err != nil {
				result.err = err
				continue
			}
		}
		wg.Go(func() {
			hostCtx, cancel := context.WithTimeout(ctx, fleetProbeTimeout)
			defer cancel()
			app := *a
			app.Remote, app.Project = host.Remote, host.Project
			app.Warn = func(msg string) { result.warnings = append(result.warnings, msg) }
			result.jobs, result.err = app.Jobs(hostCtx, "")
		})
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
			continue
		}
		for _, msg := range result.warnings {
			a.warn(fmt.Sprintf("box: %s: %s", label, strings.TrimPrefix(msg, "box: ")))
		}
		for _, job := range result.jobs {
			// Keep IDs that already have bare records on the default host.
			legacy := [2]string{host.Remote, host.Project} == defaultKey && a.StateDir != "" &&
				(a.exists(job.Job, ".json") || a.exists(job.Job, ".result"))
			if !result.bare && !legacy {
				id, err := qualifyJobID(host, job.Job)
				if err != nil {
					a.warn(fmt.Sprintf("box: listing jobs on %s: %v", label, err))
					continue
				}
				job.Job = id.ID
			}
			job.Host = host.Name
			jobs = append(jobs, job)
		}
	}
	if len(failures) == len(lists) {
		return nil, errors.Join(failures...)
	}
	for _, err := range failures {
		a.warn("box: " + err.Error())
	}
	slices.SortStableFunc(jobs, func(a, b JobInfo) int { return b.Started.Compare(a.Started) })
	return jobs, nil
}
