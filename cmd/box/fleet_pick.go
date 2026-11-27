package main

import (
	"fmt"
	"strings"

	"github.com/a-Fig/agent-box/internal/mode"
)

// Fleet target selection (#163).
//
// Following Borg and Kubernetes (docs/prior-art.md), picking runs in two steps:
// filter hosts that can start the run now (reachability, mode, room for new
// allocations, and GPU capability/occupancy), then rank eligible hosts by
// repo-scoped affinity where concrete evidence exists, breaking ties by stable
// configuration order. Configurable per-host priority (#171) is a separate
// issue and is not modeled here.
//
// Target filtering (--host pinning, single-host no-registry bypass, and keeping
// explicit --box on defaultHost unless --host pins elsewhere) lives in
// resolveFleetTargets; pickFleetHost consumes the resolved candidate list.
//
// Queueing and throwaway pool claims go through boxd, which serves only
// mode.Project ("agents"), so both require Host.Project == mode.Project and
// positively established boxd capability (BoxdAvailable).

type pickAction string

const (
	pickStart  pickAction = "start"
	pickQueue  pickAction = "queue"
	pickRefuse pickAction = "refuse"
)

// pickAffinity orders repo-specific affinity among eligible hosts.
// Higher values are preferred; equal values break ties by configuration order.
type pickAffinity int

const (
	affinityCold pickAffinity = iota
	affinityWarmHost
	affinityRepoBusy
	affinityRepoStopped
	affinityDirectWarm
)

func (a pickAffinity) String() string {
	switch a {
	case affinityDirectWarm:
		return "direct-warm"
	case affinityRepoStopped:
		return "repo-stopped"
	case affinityRepoBusy:
		return "repo-busy"
	case affinityWarmHost:
		return "warm-host"
	default:
		return "cold"
	}
}

// pickRequest carries an already-normalized run request for target selection.
// CLI/MCP flag validation and --host filtering happen before pickFleetHost.
type pickRequest struct {
	// Size is the normalized requested box size. Zero defaults to DefaultSize.
	Size mode.Size
	// ExplicitSize is true when the caller explicitly requested Size, so an
	// existing repo box of another size falls back to a throwaway.
	ExplicitSize bool
	// New requests a throwaway box instead of reusing the repo box (includes Fresh).
	New bool
	// Fresh requests a fresh throwaway box, bypassing pooled throwaway reuse.
	Fresh bool
	// Box names an explicit box (--box). resolveFleetTargets keeps it on a
	// single target host (defaultHost unless --host pinned elsewhere).
	Box string
	// Repo is the repo's box name (openRepo().Name), used for repo affinity.
	Repo string
	// Queue allows choosing a queue destination when no host can start now.
	Queue bool
}

// fleetCandidate holds typed probe facts for one host. Callers populate these
// directly from Incus/boxd probes rather than parsing human status text.
type fleetCandidate struct {
	Host             fleetHost
	Reachable        bool
	Error            string // non-empty when the probe failed or timed out mid-probe
	Mode             string // "full", "shared", "mypc", or "" (no tray)
	Sizes            []SizeRoom
	HasGPU           bool     // typed host GPU capability from project config
	GPUHolders       []string // names of GPU boxes currently holding the GPU (running, frozen, or starting)
	RepoBox          *RepoBox // this repo's (or explicit --box's) box on this host, if present
	RepoBoxThrowaway bool     // true when RepoBox is a throwaway (not reusable)
	WarmThrowaway    bool     // positive probe evidence of an idle claimable throwaway + enabled pool for this repo and size
	BoxdAvailable    bool     // positively established boxd queue capability on this host
	Queued           *int     // optional boxd queue depth when BoxdAvailable is true; nil if depth unknown
}

// fleetPick is the pure picker's decision. Host is populated for pickStart and
// pickQueue, and empty for pickRefuse.
type fleetPick struct {
	Action   pickAction
	Host     fleetHost
	Affinity pickAffinity
	Why      string // refusedFull, refusedGPUBusy, refusedMyPC, or ""
	Reason   string
}

// resolveFleetTargets determines which hosts to probe and whether fleet probing
// should be bypassed entirely.
//
// bypassFleet is true when:
//   - No host registry is configured and no --host pin was given (preserves
//     single-host defaults with zero fleet probes).
//   - An explicit --box was given without --host (keeps the named box on
//     defaultHost so it cannot silently select a same-name box on another host).
//
// Compatible aliases sharing the same (Remote, Project) tuple collapse to one
// destination in registry order with the tuple's consistent DashboardURL. An
// explicit --host pin selects that alias with the same tuple-resolved
// DashboardURL. Conflicting tuples are rejected before launch, including when
// pinned.
func resolveFleetTargets(defaultHost fleetHost, configured []fleetHost, pinnedHost, explicitBox string) (targets []fleetHost, bypassFleet bool, err error) {
	if pinnedHost != "" && strings.TrimSpace(pinnedHost) == "" {
		return nil, false, fmt.Errorf("host name cannot be empty")
	}
	pin := strings.TrimSpace(pinnedHost)
	if len(configured) == 0 {
		if pin == "" {
			return []fleetHost{defaultHost}, true, nil
		}
		name := defaultHost.Name
		if name == "" {
			name = defaultHost.Remote
		}
		if pin == name {
			return []fleetHost{defaultHost}, true, nil
		}
		return nil, false, fmt.Errorf("host %q is not configured", pin)
	}
	if pin == "" {
		if explicitBox != "" {
			return []fleetHost{defaultHost}, true, nil
		}
		seen := make(map[[2]string]bool, len(configured))
		out := make([]fleetHost, 0, len(configured))
		for _, host := range configured {
			resolved, err := resolveConfiguredTuple(host, configured)
			if err != nil {
				return nil, false, err
			}
			key := [2]string{host.Remote, host.Project}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, fleetHost{
				Name:         host.Name,
				Remote:       host.Remote,
				Project:      host.Project,
				DashboardURL: resolved.DashboardURL,
			})
		}
		return out, false, nil
	}
	names := make([]string, len(configured))
	for i, host := range configured {
		names[i] = host.Name
		if host.Name == pin {
			resolved, err := resolveConfiguredTuple(host, configured)
			if err != nil {
				return nil, false, err
			}
			return []fleetHost{{
				Name:         host.Name,
				Remote:       host.Remote,
				Project:      host.Project,
				DashboardURL: resolved.DashboardURL,
			}}, false, nil
		}
	}
	return nil, false, fmt.Errorf("host %q is not configured (available: %s)", pin, strings.Join(names, ", "))
}

func resolveConfiguredTuple(host fleetHost, configured []fleetHost) (fleetHost, error) {
	tag, err := jobHostTag(host)
	if err != nil {
		return fleetHost{}, err
	}
	return resolveJobHost(tag, fleetHost{}, configured)
}

// pickFleetHost selects a target host from candidates (already filtered by
// resolveFleetTargets) to start or queue a run on, or refuses when no host is
// eligible. It does no I/O and never mutates candidates.
func pickFleetHost(candidates []fleetCandidate, req pickRequest) fleetPick {
	sz := req.Size
	if sz.Name == "" {
		sz, _ = lookupSize("")
	}
	if len(candidates) == 0 {
		return fleetPick{Action: pickRefuse, Reason: "no configured hosts"}
	}
	if req.Box != "" && len(candidates) > 1 {
		return fleetPick{Action: pickRefuse, Reason: fmt.Sprintf("--box %q requires a single target host", req.Box)}
	}

	evals := make([]hostEval, len(candidates))
	bestStart := -1
	for i, c := range candidates {
		ev := evaluateCandidate(i, c, req, sz)
		evals[i] = ev
		if ev.canStart && (bestStart < 0 || betterStart(ev, evals[bestStart])) {
			bestStart = i
		}
	}
	if bestStart >= 0 {
		ev := evals[bestStart]
		return fleetPick{
			Action:   pickStart,
			Host:     ev.candidate.Host,
			Affinity: ev.startAff,
			Reason:   ev.startReason,
		}
	}

	if req.Queue {
		bestQueue := -1
		for i, ev := range evals {
			if ev.canQueue && (bestQueue < 0 || betterQueue(ev, evals[bestQueue])) {
				bestQueue = i
			}
		}
		if bestQueue >= 0 {
			ev := evals[bestQueue]
			return fleetPick{
				Action:   pickQueue,
				Host:     ev.candidate.Host,
				Affinity: ev.queueAff,
				Why:      ev.queueWhy,
				Reason:   fmt.Sprintf("queue on %s: %s", hostLabel(ev.candidate.Host), waitText(ev.queueWhy)),
			}
		}
	}

	return buildRefusal(evals)
}

type hostEval struct {
	index        int
	candidate    fleetCandidate
	canStart     bool
	startAff     pickAffinity
	startReason  string
	canQueue     bool
	queueWhy     string
	queueAff     pickAffinity
	refuseWhy    string
	refuseReason string
}

// markUnavailable records why a host cannot start now and centralizes queue
// eligibility and refusal wording for queueable causes.
func (ev *hostEval) markUnavailable(why, reason string, req pickRequest, sz mode.Size) {
	ev.refuseWhy = why
	if why == "" {
		ev.refuseReason = reason
		return
	}
	if ok, block := canQueueOnHost(ev.candidate, why, sz); ok {
		ev.refuseReason = reason
		ev.canQueue = true
		ev.queueWhy = why
		ev.queueAff = candidateQueueAffinity(ev.candidate, req, sz)
		return
	} else {
		ev.refuseReason = fmt.Sprintf("%s (%s)", reason, block)
	}
}

func evaluateCandidate(index int, c fleetCandidate, req pickRequest, sz mode.Size) hostEval {
	ev := hostEval{index: index, candidate: c}
	if !c.Reachable {
		reason := "unreachable"
		if c.Error != "" {
			reason += ": " + c.Error
		}
		ev.markUnavailable("", reason, req, sz)
		return ev
	}
	if c.Error != "" {
		ev.markUnavailable("", "capacity unknown: "+c.Error, req, sz)
		return ev
	}
	if c.Mode == string(mode.MyPC) {
		ev.markUnavailable(refusedMyPC, "in My PC mode", req, sz)
		return ev
	}
	if !startableMode(c.Mode) {
		ev.markUnavailable("", fmt.Sprintf("unrecognized mode %q", c.Mode), req, sz)
		return ev
	}
	if sz.GPU {
		return evaluateGPUCandidate(ev, req, sz)
	}
	return evaluateCPUCandidate(ev, req, sz)
}

func evaluateGPUCandidate(ev hostEval, req pickRequest, sz mode.Size) hostEval {
	c := ev.candidate
	if !c.HasGPU {
		ev.markUnavailable("", "no GPU on this host", req, sz)
		return ev
	}
	if req.Box != "" {
		if c.RepoBox == nil || !boxMatches(c.RepoBox, req.Box) || c.RepoBox.Size != mode.GPUSize || c.RepoBoxThrowaway {
			ev.markUnavailable("", fmt.Sprintf("box %s is not a reusable GPU box on this host", req.Box), req, sz)
			return ev
		}
		switch c.RepoBox.Status {
		case "Running", "Stopped":
		case "Frozen":
			ev.markUnavailable("", fmt.Sprintf("box %s is paused", boxLabel(c.RepoBox, req.Box)), req, sz)
			return ev
		default:
			ev.markUnavailable("", fmt.Sprintf("box %s has unrecognized status %q", boxLabel(c.RepoBox, req.Box), c.RepoBox.Status), req, sz)
			return ev
		}
	}
	// Matches claimGPU(ctx, insts, except): a named reusable GPU box does not
	// block itself from running; only another GPU holder makes the GPU busy.
	if gpuBusyFor(c.GPUHolders, req.Box) {
		ev.markUnavailable(refusedGPUBusy, "GPU is in use", req, sz)
		return ev
	}
	if req.Box != "" {
		// An existing running or stopped GPU box is already counted in Incus
		// project caps, so ensureBox starts/reuses it without needing new-box Fits.
		ev.canStart = true
		ev.startAff = repoBoxAffinity(c.RepoBox)
		if c.RepoBox.Status == "Stopped" {
			ev.startReason = "starting stopped GPU box"
		} else {
			ev.startReason = "reusing running GPU box"
		}
		return ev
	}
	if sizeFits(c.Sizes, mode.GPUSize) {
		ev.canStart = true
		ev.startAff = repoHostAffinity(c, req)
		ev.startReason = "GPU free with room"
		return ev
	}
	ev.markUnavailable(refusedFull, "no room for a gpu box", req, sz)
	return ev
}

func evaluateCPUCandidate(ev hostEval, req pickRequest, sz mode.Size) hostEval {
	c := ev.candidate
	isNew := req.New || req.Fresh
	targetBox := req.Box
	if targetBox == "" {
		targetBox = req.Repo
	}
	hasMatchingBox := targetBox != "" && c.RepoBox != nil && boxMatches(c.RepoBox, targetBox)
	usesThrowaway := isNew

	if !isNew && hasMatchingBox {
		if c.RepoBoxThrowaway {
			ev.markUnavailable("", fmt.Sprintf("box %s is a throwaway and cannot be reused", boxLabel(c.RepoBox, targetBox)), req, sz)
			return ev
		}
		if req.ExplicitSize && c.RepoBox.Size != sz.Name {
			// Run falls back to a throwaway of the requested size when the
			// existing repo box has a different size (box.go:reusableBox).
			usesThrowaway = true
		} else {
			switch c.RepoBox.Status {
			case "Running":
				ev.canStart = true
				ev.startAff = repoBoxAffinity(c.RepoBox)
				if len(c.RepoBox.Running) == 0 {
					ev.startReason = "reusing idle repo box"
				} else {
					ev.startReason = "reusing busy repo box"
				}
				return ev
			case "Stopped":
				// SizeRoom.Fits measures room for an additional allocation and
				// already counts stopped boxes; ensureBox starts an existing
				// stopped box without allocating a new one, and Run revalidates.
				ev.canStart = true
				ev.startAff = affinityRepoStopped
				ev.startReason = "starting stopped repo box"
				return ev
			case "Frozen":
				ev.markUnavailable("", fmt.Sprintf("box %s is paused", boxLabel(c.RepoBox, targetBox)), req, sz)
				return ev
			default:
				ev.markUnavailable("", fmt.Sprintf("box %s has unrecognized status %q", boxLabel(c.RepoBox, targetBox), c.RepoBox.Status), req, sz)
				return ev
			}
		}
	}

	// Claiming an idle pooled throwaway requires positive evidence for this repo
	// and goes through boxd (/api/boxes/.../claim), which handles mode.Project only.
	if usesThrowaway && !req.Fresh && hasRepoWarmThrowaway(c, req) {
		ev.canStart = true
		ev.startAff = affinityDirectWarm
		ev.startReason = "reusing warm throwaway"
		return ev
	}

	if sizeFits(c.Sizes, sz.Name) {
		ev.canStart = true
		ev.startAff = repoHostAffinity(c, req)
		ev.startReason = "room for " + sz.Name
		return ev
	}

	ev.markUnavailable(refusedFull, fmt.Sprintf("no room for a %s box", sz.Name), req, sz)
	return ev
}

// canQueueOnHost enforces that queueing happens only where the cause is
// queueable, the host targets mode.Project ("agents"), and boxd capability was
// positively established.
func canQueueOnHost(c fleetCandidate, why string, sz mode.Size) (bool, string) {
	if !queueable(why, sz) {
		if why == refusedMyPC && sz.GPU {
			return false, "a GPU run doesn't wait"
		}
		return false, "not queueable"
	}
	if c.Host.Project != mode.Project {
		return false, fmt.Sprintf("project %q has no boxd queue; boxd handles project %q only", c.Host.Project, mode.Project)
	}
	if !c.BoxdAvailable {
		return false, "boxd queue capability not established"
	}
	return true, ""
}

func betterStart(a, b hostEval) bool {
	if a.startAff != b.startAff {
		return a.startAff > b.startAff
	}
	return a.index < b.index
}

// betterQueue orders queue-eligible hosts (all verified as BoxdAvailable on
// mode.Project):
//  1. active wait (room or GPU) before My PC (owner-paused wait)
//  2. known queue depth before unknown depth
//  3. shorter queue depth when both hosts reported one
//  4. higher repo affinity
//  5. stable configuration order
func betterQueue(a, b hostEval) bool {
	aActive := a.queueWhy != refusedMyPC
	bActive := b.queueWhy != refusedMyPC
	if aActive != bActive {
		return aActive
	}
	aKnown, aLen := queueDepth(a.candidate.Queued)
	bKnown, bLen := queueDepth(b.candidate.Queued)
	if aKnown != bKnown {
		return aKnown
	}
	if aKnown && bKnown && aLen != bLen {
		return aLen < bLen
	}
	if a.queueAff != b.queueAff {
		return a.queueAff > b.queueAff
	}
	return a.index < b.index
}

func queueDepth(q *int) (bool, int) {
	if q == nil || *q < 0 {
		return false, 0
	}
	return true, *q
}

func buildRefusal(evals []hostEval) fleetPick {
	if len(evals) == 1 {
		ev := evals[0]
		return fleetPick{
			Action: pickRefuse,
			Why:    ev.refuseWhy,
			Reason: fmt.Sprintf("%s: %s", hostLabel(ev.candidate.Host), ev.refuseReason),
		}
	}
	parts := make([]string, len(evals))
	sharedWhy := evals[0].refuseWhy
	for i, ev := range evals {
		parts[i] = fmt.Sprintf("%s: %s", hostLabel(ev.candidate.Host), ev.refuseReason)
		if ev.refuseWhy != sharedWhy {
			sharedWhy = ""
		}
	}
	return fleetPick{
		Action: pickRefuse,
		Why:    sharedWhy,
		Reason: "no eligible host (" + strings.Join(parts, "; ") + ")",
	}
}

func startableMode(m string) bool {
	return m == "" || m == string(mode.Shared) || m == string(mode.Full)
}

// gpuBusyFor reports whether any GPU holder other than except currently holds
// the host's GPU, matching claimGPU(ctx, insts, except).
func gpuBusyFor(holders []string, except string) bool {
	for _, name := range holders {
		if except == "" || name != except {
			return true
		}
	}
	return false
}

// sizeFits reports whether sizes indicates room for at least one additional box
// of name. Fits < 0 means no cap limits the size; missing size entries fail closed.
func sizeFits(sizes []SizeRoom, name string) bool {
	for _, sz := range sizes {
		if sz.Name == name {
			return sz.Fits != 0
		}
	}
	return false
}

func boxMatches(rb *RepoBox, want string) bool {
	return rb != nil && want != "" && rb.Name == want
}

func boxLabel(rb *RepoBox, fallback string) string {
	if rb != nil && rb.Name != "" {
		return rb.Name
	}
	if fallback != "" {
		return fallback
	}
	return "repo"
}

func repoBoxAffinity(rb *RepoBox) pickAffinity {
	if rb == nil {
		return affinityCold
	}
	switch rb.Status {
	case "Running":
		if len(rb.Running) == 0 {
			return affinityDirectWarm
		}
		return affinityRepoBusy
	case "Stopped":
		return affinityRepoStopped
	default:
		return affinityWarmHost
	}
}

func hasRepoWarmThrowaway(c fleetCandidate, req pickRequest) bool {
	return req.Repo != "" && c.WarmThrowaway && c.Host.Project == mode.Project
}

// repoHostAffinity returns affinityWarmHost only when there is concrete
// repo-scoped evidence on the host (an existing repo box or a warm pooled
// throwaway for this repo).
func repoHostAffinity(c fleetCandidate, req pickRequest) pickAffinity {
	if hasRepoWarmThrowaway(c, req) {
		return affinityWarmHost
	}
	if req.Repo != "" && c.RepoBox != nil && !c.RepoBoxThrowaway && boxMatches(c.RepoBox, req.Repo) {
		return affinityWarmHost
	}
	return affinityCold
}

func candidateQueueAffinity(c fleetCandidate, req pickRequest, sz mode.Size) pickAffinity {
	isNew := req.New || req.Fresh
	targetBox := req.Box
	if targetBox == "" {
		targetBox = req.Repo
	}
	if !isNew && !sz.GPU && targetBox != "" && c.RepoBox != nil && !c.RepoBoxThrowaway &&
		boxMatches(c.RepoBox, targetBox) && (!req.ExplicitSize || c.RepoBox.Size == sz.Name) {
		return repoBoxAffinity(c.RepoBox)
	}
	if (isNew || (req.ExplicitSize && targetBox != "" && c.RepoBox != nil && boxMatches(c.RepoBox, targetBox) && c.RepoBox.Size != sz.Name)) &&
		!req.Fresh && !sz.GPU && hasRepoWarmThrowaway(c, req) {
		return affinityDirectWarm
	}
	return repoHostAffinity(c, req)
}

func hostLabel(h fleetHost) string {
	if h.Name != "" {
		return h.Name
	}
	if h.Remote != "" {
		return h.Remote
	}
	return "unknown"
}
