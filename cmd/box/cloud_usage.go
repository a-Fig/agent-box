package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
)

// cloudUsageTimeout bounds a usage report to boxd, the probe before one at a job's end included.
const cloudUsageTimeout = 2 * time.Second

// reportCloudUsage tells the default host's boxd what a cloud runner's probe said, for the
// dashboard's cloud host page (dash/cloud.go). It is a best effort: errors are dropped, and it
// gives up after cloudUsageTimeout.
func (a *App) reportCloudUsage(ctx context.Context, host fleetHost, p cloudProbeResponse) {
	if a.Post == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cloudUsageTimeout)
	defer cancel()
	a.postCloudUsage(ctx, host, p)
}

func (a *App) postCloudUsage(ctx context.Context, host fleetHost, p cloudProbeResponse) {
	if a.home != nil {
		a = a.home
	}
	if a.Post == nil {
		return
	}
	address := a.boxdURL(ctx, "/api/cloud/usage")
	if address == "" {
		return
	}
	mode := host.Mode
	if mode == "" {
		mode = cloudModeFunction
	}
	h := dash.CloudHost{Name: host.Name, Kind: host.Kind, Mode: mode, Paused: p.Paused, Running: p.Running, MaxConcurrent: p.MaxConcurrent}
	if b := p.Budget; b != nil {
		h.Budget = &dash.CloudBudget{Month: b.Month, SpentUSD: b.SpentUSD, RunningUSD: b.RunningUSD, CapUSD: b.CapUSD, CreditUSD: b.CreditUSD, WarnAt: b.WarnAt}
	}
	body, err := json.Marshal(dash.CloudUsageBody{Host: &h})
	if err != nil {
		return
	}
	_ = a.Post(ctx, address, body)
}

// reportUsage probes the runner again once a job ends, so the page's spend and running count
// include it, and reports that; both within cloudUsageTimeout.
func (c *cloudHost) reportUsage(ctx context.Context) {
	a := c.app
	if a == nil || a.Post == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cloudUsageTimeout)
	defer cancel()
	if cand, p := c.probeStatus(ctx); cand.Reachable && cand.Error == "" {
		a.postCloudUsage(ctx, c.host, p)
	}
}
