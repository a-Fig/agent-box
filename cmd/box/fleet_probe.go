package main

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"

	"github.com/a-Fig/agent-box/internal/jobrun"
	"github.com/a-Fig/agent-box/internal/mode"
)

// probeFleetTiers groups targets by Priority descending and probes each tier
// concurrently, stopping right after the first tier where pickFleetHost returns
// pickStart. Any unprobed lower-priority hosts are returned in rest.
func (a *App) probeFleetTiers(ctx context.Context, targets []fleetHost, req pickRequest, repoOrigin string) (cands []fleetCandidate, rest []fleetHost) {
	tiers := fleetPriorityTiers(targets)
	for i, tier := range tiers {
		cands = append(cands, a.probeFleetCandidates(ctx, tier, req, repoOrigin)...)
		if pickFleetHost(cands, req).Action == pickStart {
			for _, rem := range tiers[i+1:] {
				rest = append(rest, rem...)
			}
			return cands, rest
		}
	}
	return cands, nil
}

func fleetPriorityTiers(targets []fleetHost) [][]fleetHost {
	if len(targets) == 0 {
		return nil
	}
	ordered := slices.Clone(targets)
	slices.SortStableFunc(ordered, func(a, b fleetHost) int {
		return cmp.Compare(b.Priority, a.Priority)
	})
	var tiers [][]fleetHost
	start := 0
	for i := 1; i < len(ordered); i++ {
		if ordered[i].Priority != ordered[start].Priority {
			tiers = append(tiers, ordered[start:i])
			start = i
		}
	}
	tiers = append(tiers, ordered[start:])
	return tiers
}

// probeFleetCandidates inspects each resolved target host concurrently within
// fleetProbeTimeout (5s) and returns typed fleetCandidate facts in targets order.
//
// Each host runs independently: an unreachable or slow host never blocks or
// invalidates other hosts. A timeout during the Incus probe marks capacity
// unknown so pickFleetHost fails closed on that host. Once Incus capacity
// succeeds, a failure or timeout in the optional boxd probes leaves the Incus
// start capacity usable and only omits boxd queue or throwaway-pool capability.
func (a *App) probeFleetCandidates(ctx context.Context, targets []fleetHost, req pickRequest, repoOrigin string) []fleetCandidate {
	sz := req.Size
	if sz.Name == "" {
		sz, _ = lookupSize("")
	}
	targetBox := req.Box
	if targetBox == "" {
		targetBox = req.Repo
	}
	repoKey := ""
	if !req.Fresh && !sz.GPU && req.Repo != "" {
		repoKey = throwawayRepoKey(repoOrigin, sz)
	}

	candidates := make([]fleetCandidate, len(targets))
	var wg sync.WaitGroup
	for i, host := range targets {
		wg.Go(func() {
			candidates[i] = a.probeFleetCandidate(ctx, host, req, sz, targetBox, repoKey)
		})
	}
	wg.Wait()
	return candidates
}

func (a *App) probeFleetCandidate(ctx context.Context, host fleetHost, req pickRequest, sz mode.Size, targetBox, repoKey string) fleetCandidate {
	hostCtx, cancel := context.WithTimeout(ctx, fleetProbeTimeout)
	defer cancel()

	hostApp := a.jobHostApp(host)
	hostApp.Warn = func(string) {}

	p := hostApp.probeHostStatus(hostCtx, hostProbeOpts{
		repoName:   targetBox,
		gpuRequest: sz.GPU,
		exceptGPU:  req.Box,
		strictCaps: true,
	})
	// If the Incus probe timed out after connecting, capacity is incomplete:
	// fail closed rather than treating partial caps as unlimited.
	if p.report.Reachable && hostCtx.Err() != nil {
		return fleetCandidate{
			Host:      host,
			Priority:  host.Priority,
			Reachable: true,
			Error:     hostCtx.Err().Error(),
			Mode:      p.report.Mode,
		}
	}
	c := fleetCandidate{
		Host:             host,
		Priority:         host.Priority,
		Reachable:        p.report.Reachable,
		Error:            p.report.Error,
		Mode:             p.report.Mode,
		Sizes:            p.report.Sizes,
		HasGPU:           p.hasGPU,
		GPUHolders:       p.gpuHolders,
		RepoBox:          p.report.RepoBox,
		RepoBoxThrowaway: p.repoBoxThrowaway,
	}
	if !c.Reachable || c.Error != "" || hostApp.boxdDisabled {
		return c
	}

	// Optional boxd probes (mode.Project "agents" only). A failure or timeout
	// here leaves the already-verified Incus start capacity usable.
	queued, queueCapable, hostPrio := hostApp.fleetBoxdState(hostCtx, host.DashboardURL)
	if queueCapable {
		c.BoxdAvailable = true
		c.Queued = queued
	}
	if hostPrio != nil {
		c.Priority = *hostPrio
	}
	if wantsPooledThrowaway(req, sz, p.report.RepoBox, p.repoBoxThrowaway, targetBox) &&
		repoKey != "" &&
		hasClaimableThrowaway(p.keptThrowaways, repoKey, sz.Name, hostApp.Now()) &&
		hostApp.poolEnabled(hostCtx) {
		c.WarmThrowaway = true
	}
	return c
}

func wantsPooledThrowaway(req pickRequest, sz mode.Size, repoBox *RepoBox, repoBoxThrowaway bool, targetBox string) bool {
	if req.Fresh || sz.GPU {
		return false
	}
	if req.New {
		return true
	}
	return req.ExplicitSize && repoBox != nil && !repoBoxThrowaway && boxMatches(repoBox, targetBox) && repoBox.Size != sz.Name
}

// hasClaimableThrowaway checks the already-verified idle throwaways (which
// passed keptThrowawayConfig and hasEndedJob in probeHostStatus) for a concrete
// candidate matching repoKey, size, and unexpired TTL. The eventual boxd claim
// in Run remains authoritative.
func hasClaimableThrowaway(kept []instance, repoKey, sizeName string, now time.Time) bool {
	if repoKey == "" {
		return false
	}
	for _, in := range kept {
		if !validBox.MatchString(in.Name) || in.Config[jobrun.ThrowawayRepoKey] != repoKey ||
			mode.SizeOf(in.Config).Name != sizeName || !keptThrowawayConfig(in, now) {
			continue
		}
		expires, err := time.Parse(time.RFC3339, in.Config[expiresKey])
		if err == nil && expires.After(now) {
			return true
		}
	}
	return false
}
