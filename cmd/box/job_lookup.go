package main

import (
	"context"
	"math"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/mode"
)

type jobLookup struct {
	app  *App
	id   jobID
	host string
}

func (a *App) lookupJob(raw string) (jobLookup, error) {
	id, err := parseJobID(raw)
	if err != nil {
		return jobLookup{}, err
	}
	if id.hostTag == "" {
		return jobLookup{app: a, id: id}, nil
	}
	hosts, err := a.configuredHosts()
	if err != nil {
		return jobLookup{}, err
	}
	host, err := id.resolveHost(fleetHost{Name: a.Remote, Remote: a.Remote, Project: a.Project}, hosts)
	if err != nil {
		return jobLookup{}, err
	}
	scoped := *a
	scoped.Remote, scoped.Project, scoped.dashboardURL = host.Remote, host.Project, host.DashboardURL
	scoped.boxdDisabled = host.Project != mode.Project
	return jobLookup{app: &scoped, id: id, host: host.Name}, nil
}

// A qualified ID can come from another client, with no local queue marker.
func (l jobLookup) queued(ctx context.Context) (dash.Job, bool, error) {
	if l.app.boxdDisabled {
		return dash.Job{}, false, nil
	}
	local := l.app.wasQueued(l.id.ID)
	if l.id.hostTag == "" && !local {
		return dash.Job{}, false, nil
	}
	j, found, err := l.app.queuedJob(ctx, l.id.Bare)
	if err != nil {
		return j, local, err // the CLI rides out a queue outage for a recorded job
	}
	return j, found && j.State == dash.JobQueued, err
}

func (l jobLookup) job(ctx context.Context, wait time.Duration) (JobResult, error) {
	r, err := l.app.job(ctx, l.id, wait)
	r.Host = l.host
	return r, err
}

func (l jobLookup) page(ctx context.Context, offset, limit int64) (Page, *jobEnd, error) {
	p, err := l.app.LogPage(ctx, l.id.Bare, offset, limit)
	if err == nil {
		p.Job, p.Host = l.id.ID, l.host
		return p, nil, nil
	}
	if offset >= 0 && limit >= 0 && offset <= math.MaxInt64-maxPage {
		if end, ok := l.app.endedElsewhere(ctx, l.id); ok {
			return Page{}, &end, err
		}
	}
	return Page{}, nil, err
}
