package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/mode"
)

// A saved tail is already a usable answer. Give the optional full-page
// lookup a short budget, including name resolution and both boxd requests.
const stoppedPageWithTailBudget = time.Second

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
	return jobLookup{app: a.jobHostApp(host), id: id, host: host.Name}, nil
}

func (a *App) jobHostApp(host fleetHost) *App {
	scoped := *a
	scoped.Remote, scoped.Project, scoped.dashboardURL = host.Remote, host.Project, host.DashboardURL
	scoped.boxdDisabled = host.Project != mode.Project
	return &scoped
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
	r, err := l.app.job(ctx, l.id, wait, nil)
	r.Host = l.host
	return r, err
}

func (l jobLookup) page(ctx context.Context, offset, limit int64) (Page, *jobEnd, error) {
	p, err := l.app.LogPage(ctx, l.id.Bare, offset, limit)
	if err == nil {
		p.Job, p.Host = l.id.ID, l.host
		if p.Source == nil {
			p.Source = l.app.savedJobSource(l.id)
		}
		return p, nil, nil
	}
	if offset < 0 || limit < 0 || offset > math.MaxInt64-maxPage {
		return Page{}, nil, err
	}
	if l.app.Project != mode.Project || l.app.boxdDisabled {
		// Boxd reads the agents project. Keep the existing tail lookup for
		// other projects without trying its new full-log path.
		if end, ok := l.app.endedElsewhere(ctx, l.id); ok {
			return Page{}, &end, err
		}
		return Page{}, nil, err
	}
	end, kept := l.app.recordedEnd(l.id.ID)
	budget := queueTimeout
	if kept {
		budget = stoppedPageWithTailBudget
	}
	look, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	j, found, lookupErr := l.app.queuedJob(look, l.id.Bare)
	ended := lookupErr == nil && found && j.State == dash.JobDone && j.Exit != nil
	if ended {
		if p, ok := l.stoppedPage(look, j, offset, limit); ok {
			return p, nil, nil
		}
	}
	if kept {
		end.Source = l.app.savedJobSource(l.id)
		return Page{}, &end, err
	}
	if ended {
		return Page{}, &jobEnd{Code: *j.Exit, Tail: j.Tail, Source: j.Source}, err
	}
	return Page{}, nil, err
}

// stoppedPage reads a recorded job's file through the same host's boxd.
// A server without stopped paging leaves the caller's existing tail fallback.
func (l jobLookup) stoppedPage(ctx context.Context, j dash.Job, offset, limit int64) (Page, bool) {
	if j.Box != l.id.Box || j.Box+"."+j.ID != l.id.Bare || j.BoxUID == "" || j.StartedAt.IsZero() ||
		j.Key != dash.JobKey(j.Box, j.ID, j.StartedAt) {
		return Page{}, false
	}
	if limit == 0 {
		limit = defaultPage
	}
	limit = min(limit, dash.MaxLogChunk)
	path := fmt.Sprintf("/api/jobs/%s/log?stopped=1&offset=%d&limit=%d", url.PathEscape(j.Key), offset, limit)
	code, body, err := l.app.boxdDo(ctx, http.MethodGet, path, "", nil)
	var chunk dash.LogChunk
	if err != nil || code != http.StatusOK || json.Unmarshal(body, &chunk) != nil || !chunk.Stopped || chunk.Paused || chunk.LogBytes < 0 {
		return Page{}, false
	}
	data := chunk.Data
	if chunk.DataBase64 != "" {
		b, err := base64.StdEncoding.DecodeString(chunk.DataBase64)
		if err != nil {
			return Page{}, false
		}
		data = string(b)
	}
	want := min(limit, max(0, chunk.LogBytes-offset))
	if int64(len(data)) != want || chunk.Offset != offset+want || chunk.EOF != (chunk.Offset >= chunk.LogBytes) {
		return Page{}, false
	}
	info := JobInfo{Job: l.id.ID, Box: j.Box, Host: l.host, State: jobDone, Code: j.Exit, Started: j.StartedAt, LogBytes: chunk.LogBytes}
	source := j.Source
	if source == nil {
		source = l.app.savedJobSource(l.id)
	}
	return Page{JobInfo: info, Source: source, Offset: offset, NextOffset: chunk.Offset, More: !chunk.EOF, Text: data}, true
}
