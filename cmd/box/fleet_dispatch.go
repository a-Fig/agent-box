package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/a-Fig/agent-box/internal/mode"
)

// turnAwayInfo reports a pre-start refusal from runOnHost so runFleet can fall
// back to another eligible host or queue destination before printing a final
// refusal.
type turnAwayInfo struct {
	why   string
	msg   string
	queue queueRun
}

type fleetRun struct {
	repo repo
	info jobMeta
	snap worktreeSnapshot
	ref  string
}

// runDispatch routes a validated RunOpts either directly to the default host
// (preserving single-host behavior when no registry is configured or when --box
// is passed without --host) or through fleet probing and target selection.
func (a *App) runDispatch(ctx context.Context, o RunOpts, sz mode.Size) (JobResult, error) {
	configured, err := a.configuredHosts()
	if err != nil {
		return JobResult{Code: 1}, err
	}
	defaultHost := fleetHost{Name: a.Remote, Remote: a.Remote, Project: a.Project}
	targets, bypassFleet, err := resolveFleetTargets(defaultHost, configured, o.Host, o.Box)
	if err != nil {
		return JobResult{Code: 2}, err
	}
	if bypassFleet {
		r, _, err := a.runOnHost(ctx, o, sz, nil, nil)
		return r, err
	}
	return a.runFleet(ctx, o, sz, targets)
}

func (a *App) runFleet(ctx context.Context, o RunOpts, sz mode.Size, targets []fleetHost) (JobResult, error) {
	rp, err := openRepo(ctx, a.R, o.Dir)
	if err != nil {
		return JobResult{Code: 1}, err
	}
	box := o.Box
	if box == "" {
		box = rp.Name
	}
	if !validBox.MatchString(box) {
		return JobResult{Code: 2}, fmt.Errorf("bad box name %q", box)
	}
	a.warnSubmodules(ctx, rp.Top)
	info := a.runInfo(ctx, rp, o, sz)

	pickReq := pickRequest{
		Size:         sz,
		ExplicitSize: o.Size != "",
		New:          o.New,
		Fresh:        o.Fresh,
		Box:          o.Box,
		Repo:         rp.Name,
		Queue:        o.Queue,
	}
	candidates := a.probeFleetCandidates(ctx, targets, pickReq, info.Repo)

	prepared := fleetRun{repo: rp, info: info}
	defer func() {
		if prepared.snap.bundle != "" {
			os.RemoveAll(filepath.Dir(prepared.snap.bundle))
		}
	}()

	var failedSetup JobResult
	var setupErr error
	var lastCapacityHost *App
	var lastLimits []string
	for {
		pick := pickFleetHost(candidates, pickReq)
		if pick.Action == pickRefuse {
			if pick.Invalid {
				return JobResult{Code: 2}, fmt.Errorf("%s", pick.Reason)
			}
			if setupErr != nil {
				return failedSetup, setupErr
			}
			reporter := a
			var detail *capacityDetail
			switch {
			case len(candidates) == 1:
				reporter = a.jobHostApp(candidates[0].Host)
				detail = reporter.capacityHint(ctx, sz, o.Box, pick.Limits)
			case lastCapacityHost != nil:
				reporter = lastCapacityHost
				detail = reporter.capacityHint(ctx, sz, o.Box, lastLimits)
			case len(pick.Limits) != 0:
				detail = &capacityDetail{Limits: pick.Limits}
			}
			msg := pick.Reason + "; nothing started"
			r := JobResult{State: statePaused, Code: exitPaused, Capacity: detail}
			if detail != nil {
				msg = "refused: " + msg
				r.Host = reporter.jobHostName
			}
			if len(candidates) == 1 && pick.Why != "" && candidates[0].BoxdAvailable {
				reporter.refuse(ctx, msg, info, func(context.Context) string { return pick.Why })
			} else {
				fmt.Fprintln(a.Err, "box:", msg)
			}
			reporter.printCapacity(detail)
			return r, nil
		}

		if prepared.snap.bundle == "" {
			prepared.ref = a.Now().UTC().Format("20060102-150405") + "-" + a.Rand()
			prepared.snap, err = snapshot(ctx, a.R, rp.Top, prepared.ref)
			if err != nil {
				return JobResult{Code: 1}, err
			}
		}
		selected := pick.Host
		candidate := findCandidate(candidates, selected)
		hostApp := a.jobHostApp(selected)
		if pick.Action == pickStart {
			r, away, err := hostApp.runOnHost(ctx, o, sz, &selected, &prepared)
			if away == nil || ctx.Err() != nil {
				return r, err
			}
			if err != nil {
				failedSetup, setupErr = r, fmt.Errorf("%s: %w", selected.Name, err)
			}
			if len(away.queue.limits) > 0 {
				lastCapacityHost, lastLimits = hostApp, away.queue.limits
			}
			candidate.startRefusal = away
			continue
		}

		var q queueRun
		if candidate.startRefusal != nil {
			q = candidate.startRefusal.queue
		} else {
			q = fleetQueueRun(o, prepared, box, sz, &selected, *candidate, pick.Why)
		}
		q.limits = pick.Limits
		r, msg, failure := hostApp.enqueue(ctx, q)
		if msg == "" {
			return r, nil
		}
		// Only try another queue when boxd explicitly rejected the request.
		// A lost reply can mean the job was accepted and may already be running.
		switch failure {
		case queueRejected:
			if len(q.limits) > 0 {
				lastCapacityHost, lastLimits = hostApp, q.limits
			}
			candidate.BoxdAvailable = false
			candidate.queueError = msg
		case queueInvalid:
			return JobResult{Host: selected.Name, Code: 1}, fmt.Errorf("%s: %s", selected.Name, msg)
		default:
			return JobResult{Host: selected.Name, Code: 1}, fmt.Errorf("%s: queue submission could not be confirmed: %s. Check box jobs for the job before retrying", selected.Name, msg)
		}
	}
}

func fleetQueueRun(o RunOpts, prepared fleetRun, box string, sz mode.Size, host *fleetHost, cand fleetCandidate, why string) queueRun {
	rp := prepared.repo
	sizeMismatch := o.Size != "" && cand.RepoBox != nil && !cand.RepoBoxThrowaway && boxMatches(cand.RepoBox, box) && cand.RepoBox.Size != sz.Name
	q := queueRun{o: o, rp: rp, box: box, new: o.New, sz: sz, info: prepared.info, snap: prepared.snap, ref: prepared.ref, host: host}
	if o.New || sz.GPU || why == refusedMyPC || sizeMismatch {
		q.box, q.new = rp.Name, true
	}
	return q
}

// The picker returns a host from candidates for both start and queue.
func findCandidate(candidates []fleetCandidate, host fleetHost) *fleetCandidate {
	for i := range candidates {
		if candidates[i].Host == host {
			return &candidates[i]
		}
	}
	panic("fleet picker returned an unknown host")
}

// followQueuedResult follows a queued JobResult through its destination host's
// queue and then into its box, preserving the public qualified ID and host name.
func (a *App) followQueuedResult(ctx context.Context, r JobResult) (JobResult, error) {
	if r.fleetHost == nil {
		return a.followQueued(ctx, r.Job)
	}
	id, err := parseJobID(r.Job)
	if err != nil {
		return JobResult{Code: 2}, err
	}
	target := a.jobHostApp(*r.fleetHost)
	res, err := target.followQueuedJob(ctx, id, true)
	res.Host = r.Host
	res.fleetHost = r.fleetHost
	return res, err
}

// cancelQueuedResult removes a queued job from the destination host's queue,
// sending the bare job ID to boxd when fleet-dispatched.
func (a *App) cancelQueuedResult(ctx context.Context, r JobResult) error {
	if r.fleetHost == nil {
		return a.cancelQueued(ctx, r.Job)
	}
	id, err := parseJobID(r.Job)
	if err != nil {
		return err
	}
	return a.jobHostApp(*r.fleetHost).cancelQueued(ctx, id.Bare)
}

func runJobID(targetHost *fleetHost, bare, box string) (jobID, error) {
	if targetHost == nil {
		return jobID{ID: bare, Bare: bare, Box: box}, nil
	}
	return qualifyJobID(*targetHost, bare)
}

func onHost(name string) string {
	if name != "" {
		return " on " + name
	}
	return ""
}
