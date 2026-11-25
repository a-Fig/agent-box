package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/a-Fig/agent-box/internal/mode"
	"github.com/a-Fig/agent-box/internal/version"
)

// StatusReport says whether offloading to the PC is worth it now.
type StatusReport struct {
	Advice    string `json:"advice"`
	Available bool   `json:"available"` // box_run can start a job now
	Remote    string `json:"remote"`
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"` // why the PC can't be reached
	Mode      string `json:"mode"`            // mypc, shared, full, or "" if unknown
	Boxes     int    `json:"boxes"`
	MaxBoxes  int    `json:"maxBoxes"`  // -1: no limit
	SlotsFree int    `json:"slotsFree"` // -1: no limit
	// Replaceable is how many boxes SlotsFree counts as free: expired, or throwaways whose job
	// ended, which the next launch deletes first.
	Replaceable int        `json:"replaceable,omitempty"`
	Sizes       []SizeRoom `json:"sizes"`
	Room        string     `json:"room"` // like "1 large, or 2 medium, or 4 small"; "none (disk: 205GiB of 210GiB allocated)"; "no limit"
	// GPU is "free", "free, but no room ...", "in use: job X" or
	// "unavailable: ..." (My PC).
	GPU     string   `json:"gpu"`
	Repo    string   `json:"repo,omitempty"`    // when run in a git repo
	RepoBox *RepoBox `json:"repoBox,omitempty"` // the repo's own box, when it exists
	Update  string   `json:"update,omitempty"`  // a newer stable release is out than this box
}

// SizeRoom is one box size, what a box of it gets now, and how many more
// fit in the project's caps.
type SizeRoom struct {
	mode.Size
	CPUs int `json:"cpus"` // CPUs a box of this size gets now
	Fits int `json:"fits"` // more boxes of this size that fit now; -1: no limit
}

// RepoBox is a repo's own box and the jobs running in it.
type RepoBox struct {
	Name    string    `json:"name"`
	Size    string    `json:"size"`
	Status  string    `json:"status"`
	Expires string    `json:"expires,omitempty"`
	Running []JobInfo `json:"running"`
}

// Status reports the PC's state and, when dir is in a git repo, the repo's
// box and its running jobs. An unreachable PC is included in the report.
func (a *App) Status(ctx context.Context, dir string) (s StatusReport) {
	s = StatusReport{Remote: a.Remote, MaxBoxes: -1, SlotsFree: -1}
	defer func() { s.Update = behind(a.StateDir, version.Tag) }()
	if dir != "" {
		if rp, err := openRepo(ctx, a.R, dir); err == nil {
			s.Repo = rp.Name
		}
	}
	insts, err := a.list(ctx)
	if err != nil {
		s.Error = firstLine(err.Error())
		s.Advice = fmt.Sprintf("unavailable: can't reach %s (%s): run locally", a.Remote, s.Error)
		return s
	}
	s.Reachable = true
	s.Boxes = len(insts)
	s.Mode = a.mode(ctx)
	kept := map[string]bool{}
	for _, in := range a.keptThrowaways(ctx, insts) {
		kept[in.Name] = true
	}
	evictable := func(in instance) bool { return reclaimable(in) || kept[in.Name] } // the next launch deletes it first
	if n, err := strconv.Atoi(a.projectKey(ctx, "limits.instances")); err == nil && n >= 0 {
		free := n - len(insts)
		for _, in := range insts {
			if evictable(in) {
				free++
				s.Replaceable++
			}
		}
		s.MaxBoxes, s.SlotsFree = n, max(free, 0)
	}
	now := mode.Mode(s.Mode)
	if s.Mode == "" { // no tray: boxes keep the Shared CPUs they launched with
		now = mode.Shared
	}
	s.Sizes, s.Room = a.room(ctx, insts, now, s.MaxBoxes, evictable)
	s.GPU = a.gpuStatus(ctx, s, insts)
	var repoBox *instance
	if s.Repo != "" {
		repoBox = find(insts, s.Repo)
	}
	if repoBox != nil {
		s.RepoBox = &RepoBox{Name: repoBox.Name, Size: mode.SizeOf(repoBox.Config).Name, Status: repoBox.Status, Expires: repoBox.Config[expiresKey], Running: []JobInfo{}}
		if repoBox.Status == "Running" {
			jobs, err := a.boxJobs(ctx, repoBox.Name)
			if err != nil {
				a.warn(fmt.Sprintf("box: listing jobs in %s: %v", repoBox.Name, err))
			}
			for _, j := range jobs {
				if j.State == jobRunning || j.State == jobStarting {
					s.RepoBox.Running = append(s.RepoBox.Running, j)
				}
			}
		}
	}
	s.Advice, s.Available = advice(s, repoBox)
	return s
}

// gpuStatus says whether a GPU box could run now.
func (a *App) gpuStatus(ctx context.Context, s StatusReport, insts []instance) string {
	if s.Mode == string(mode.MyPC) {
		return "unavailable: the PC is in My PC mode"
	}
	for _, in := range insts {
		if mode.IsGPU(in.Config) && a.beingCreated(in) {
			return "in use: box " + in.Name + ", starting"
		}
		if mode.IsGPU(in.Config) && in.Status != "Stopped" {
			holder, _ := a.gpuHolder(ctx, in)
			return "in use: " + holder
		}
	}
	for _, sz := range s.Sizes {
		if sz.GPU && sz.Fits == 0 {
			return "free, but no room for a gpu box now"
		}
	}
	return "free"
}

// room works out how many more boxes of each size fit, from the project's
// caps minus what existing boxes (stopped ones too) are allocated: CPUs at
// at least their size's Full count, because the tray raises them in Full mode,
// memory, instances, and disk as Incus counts it (root disks, volumes,
// images). A cap that can't be read doesn't limit.
//
// A launch the caps refuse deletes reclaimable boxes and tries again, so a
// size fits if it fits as things are, or once those boxes are gone. In the
// second case the memory cap is the one the tray applies to the boxes left,
// max(the mode's budget, what they hold), since a tick can lower it to that
// between the delete and the retry.
func (a *App) room(ctx context.Context, insts []instance, now mode.Mode, maxBoxes int, evictable func(instance) bool) ([]SizeRoom, string) {
	instCap := int64(maxBoxes)
	cpuCap, cpuErr := strconv.ParseInt(a.projectKey(ctx, "limits.cpu"), 10, 64)
	memCap, memKnown := mode.ParseBytes(a.projectKey(ctx, "limits.memory"))
	// held is what existing boxes allocate; evict is the reclaimable part.
	type alloc struct{ n, cpus, mem, disk, gpus int64 }
	var held, evict alloc
	for _, in := range insts {
		sz := mode.SizeOf(in.Config)
		var gpu int64
		if sz.GPU && in.Status != "Stopped" {
			gpu = 1
		}
		cfg := in.ExpandedConfig
		if cfg == nil {
			cfg = in.Config
		}
		cpu, _ := strconv.ParseInt(cfg["limits.cpu"], 10, 64)
		m, ok := mode.ParseBytes(cfg["limits.memory"])
		if !ok {
			m, _ = mode.ParseBytes(sz.Memory)
		}
		d, _ := mode.ParseBytes(sz.Disk)
		one := alloc{1, max(int64(sz.FullCPUs), cpu), m, d, gpu}
		held = alloc{held.n + one.n, held.cpus + one.cpus, held.mem + one.mem, held.disk + one.disk, held.gpus + one.gpus}
		if evictable(in) {
			evict = alloc{evict.n + one.n, evict.cpus + one.cpus, evict.mem + one.mem, evict.disk + one.disk, evict.gpus + one.gpus}
		}
	}
	resources := a.projectResources(ctx)
	diskFree, diskKnown := resources["disk"].remaining()
	containersFree, containersKnown := resources["containers"].remaining()
	evictMemCap := memCap
	if b, ok := mode.ParseBytes(mode.MemoryBudgets[now]); ok && evict.n > 0 {
		evictMemCap = min(memCap, max(b, held.mem-evict.mem))
	}

	sizes := make([]SizeRoom, len(mode.Sizes))
	limited := false
	var fullCaps []string
	for i, sz := range mode.Sizes {
		szMem, _ := mode.ParseBytes(sz.Memory)
		szDisk, _ := mode.ParseBytes(sz.Disk)
		// fitsWith is how many fit once the boxes in gone are deleted, under
		// memory cap mc; -1 when no cap limits. full collects the caps that
		// still leave no room for one, each with what is held now.
		var full []string
		fitsWith := func(gone alloc, mc int64) int64 {
			fits := int64(-1)
			full = nil
			limit := func(free, each int64, name string) {
				n := max(free, 0) / each
				if fits < 0 || n < fits {
					fits = n
				}
				if n == 0 {
					full = append(full, name)
				}
			}
			if instCap >= 0 {
				limit(instCap-held.n+gone.n, 1, fmt.Sprintf("box slots: %d of %d", held.n, instCap))
			}
			if containersKnown {
				limit(containersFree+gone.n, 1, "containers")
			}
			if cpuErr == nil && cpuCap >= 0 { // each box counts at its Full CPUs, which the tray may give it
				limit(cpuCap-held.cpus+gone.cpus, int64(sz.FullCPUs), fmt.Sprintf("CPUs: %d of %d reserved", held.cpus, cpuCap))
			}
			if memKnown {
				limit(mc-held.mem+gone.mem, szMem, fmt.Sprintf("memory: %s of %s allocated", gib(held.mem), gib(memCap)))
			}
			if diskKnown {
				d := resources["disk"]
				limit(diskFree+gone.disk, szDisk, fmt.Sprintf("disk: %s of %s allocated", gib(*d.Usage), gib(*d.Limit)))
			}
			if sz.GPU { // one at a time
				limit(1-held.gpus+gone.gpus, 1, "the GPU")
			}
			return fits
		}
		fits := fitsWith(alloc{}, memCap)
		if evict.n > 0 { // a launch deletes these first: name only the caps it would still hit
			if f := fitsWith(evict, evictMemCap); f > fits || f < 0 {
				fits = f
			}
		}
		sizes[i] = SizeRoom{Size: sz, CPUs: sz.CPUs(now), Fits: int(fits)}
		limited = limited || (fits >= 0 && !sz.GPU)
		if i == 0 { // the smallest size: what leaves no room at all (#92)
			fullCaps = full
		}
	}
	var parts []string
	for i := len(sizes) - 1; i >= 0; i-- { // largest first
		if sizes[i].Fits > 0 && !sizes[i].GPU { // the gpu line covers the GPU
			parts = append(parts, fmt.Sprintf("%d %s", sizes[i].Fits, sizes[i].Name))
		}
	}
	switch {
	case !limited:
		return sizes, "no limit"
	case len(parts) == 0: // some cap gave the small size 0, so fullCaps names it
		return sizes, "none (" + strings.Join(fullCaps, "; ") + ")"
	}
	return sizes, strings.Join(parts, ", or ")
}

type projectResource struct{ Limit, Usage *int64 }

func (r projectResource) remaining() (int64, bool) {
	if r.Limit == nil || r.Usage == nil || *r.Limit < 0 || *r.Usage < 0 {
		return 0, false
	}
	return *r.Limit - *r.Usage, true
}

// projectResources reads Incus's allocation counters, when available.
func (a *App) projectResources(ctx context.Context) map[string]projectResource {
	out, err := a.incusOK(ctx, "project", "info", a.target(a.Project), "--format", "json")
	if err != nil {
		return nil
	}
	var st struct {
		Resources map[string]projectResource `json:"resources"`
	}
	if json.Unmarshal([]byte(out), &st) != nil {
		return nil
	}
	return st.Resources
}

// sizeText is "medium: 2 CPUs/4GiB" for a size as it is now.
func sizeText(s StatusReport, name string) string {
	for _, sz := range s.Sizes {
		if sz.Name == name {
			return fmt.Sprintf("%s: %d CPUs/%s", sz.Name, sz.CPUs, sz.Memory)
		}
	}
	return name
}

// waitsAdvice ends a status that says a run can't start now but would queue:
// box run and a pushed box_run wait in the PC's queue (#93); box_run without
// push still gets 75.
const waitsAdvice = ": box run, or box_run with push, would wait in the PC's queue; run locally if you need it now"

func advice(s StatusReport, repoBox *instance) (string, bool) {
	switch {
	case s.Mode == string(mode.MyPC):
		return "unavailable: PC is in My PC mode" + waitsAdvice + " (a GPU run doesn't wait)", false
	case repoBox != nil && repoBox.Status == "Frozen":
		return fmt.Sprintf("unavailable: this repo's box %s is frozen: run locally", repoBox.Name), false
	case repoBox != nil && repoBox.Config[throwawayKey] != "true":
		busy := ""
		if n := len(s.RepoBox.Running); n > 0 {
			busy = fmt.Sprintf(", %d job(s) running there (runs take turns)", n)
		}
		more := "; room for " + s.Room + " more"
		switch {
		case noRoom(s.Room):
			more = "; no room for more boxes" + strings.TrimPrefix(s.Room, "none")
		case s.Room == "no limit":
			more = ""
		}
		return fmt.Sprintf("available: this repo's box %s (%s) is %s%s%s", repoBox.Name, sizeText(s, s.RepoBox.Size), strings.ToLower(repoBox.Status), busy, more) + modeNote(s), true
	case s.SlotsFree == 0:
		return fmt.Sprintf("busy: all %d box slots are taken", s.MaxBoxes) + waitsAdvice, false
	case noRoom(s.Room):
		return "busy: no room for another box " + strings.TrimPrefix(s.Room, "none ") + waitsAdvice, false
	case s.Room == "no limit":
		return "available: " + sizeText(s, mode.DefaultSize) + modeNote(s), true
	}
	return "available: room for " + s.Room + modeNote(s), true
}

// gib writes n in GiB to a tenth: Incus's disk usage is rarely whole GiB.
func gib(n int64) string {
	return strings.TrimSuffix(strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64), ".0") + "GiB"
}

// noRoom is a Room that fits no box: "none (<the full caps>)".
func noRoom(room string) bool { return strings.HasPrefix(room, "none (") }

func modeNote(s StatusReport) string {
	if s.Mode == "" {
		return " (mode unknown: the tray hasn't set it)"
	}
	return ""
}

// projectKey reads one key of the box project's config; "" if unset or unreadable.
func (a *App) projectKey(ctx context.Context, key string) string {
	out, err := a.incusOK(ctx, "project", "get", a.target(a.Project), key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

// writeStatus prints a report: the advice first, then the details.
func writeStatus(w io.Writer, s StatusReport) {
	fmt.Fprintln(w, s.Advice)
	if s.Update != "" {
		fmt.Fprintln(w, s.Update)
	}
	if !s.Reachable {
		return
	}
	m := s.Mode
	if m == "" {
		m = "unknown"
	}
	fmt.Fprintf(w, "mode: %s\n", m)
	if s.MaxBoxes >= 0 {
		free := fmt.Sprintf("%d free", s.SlotsFree)
		if s.Replaceable > 0 { // "4 of 10 (8 free)" read as a bug (#82)
			free += fmt.Sprintf(", %d of them taken by finished throwaways or expired boxes a launch replaces", s.Replaceable)
		}
		fmt.Fprintf(w, "boxes: %d of %d (%s)\n", s.Boxes, s.MaxBoxes, free)
	} else {
		fmt.Fprintf(w, "boxes: %d\n", s.Boxes)
	}
	fmt.Fprintf(w, "room for: %s\n", s.Room)
	var sizes []string
	for _, sz := range s.Sizes {
		if !sz.GPU { // the gpu line covers it
			sizes = append(sizes, fmt.Sprintf("%s %d CPUs/%s/%s disk", sz.Name, sz.CPUs, sz.Memory, sz.Disk))
		}
	}
	fmt.Fprintf(w, "sizes now: %s (default %s)\n", strings.Join(sizes, ", "), mode.DefaultSize)
	fmt.Fprintf(w, "gpu: %s\n", s.GPU)
	switch {
	case s.RepoBox != nil:
		fmt.Fprintf(w, "repo box: %s, %s, %s, expires %s\n", s.RepoBox.Name, s.RepoBox.Size, s.RepoBox.Status, s.RepoBox.Expires)
		for _, j := range s.RepoBox.Running {
			fmt.Fprintf(w, "  %s %s since %s\n", j.Job, j.State, j.Started.Format("2006-01-02 15:04:05Z"))
		}
	case s.Repo != "":
		fmt.Fprintf(w, "repo box: %s (not created yet)\n", s.Repo)
	}
}
