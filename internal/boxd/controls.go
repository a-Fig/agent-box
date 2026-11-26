package boxd

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/jobrun"
	"github.com/a-Fig/agent-box/internal/mode"
)

// The page's controls (docs/ui.md, "Controls"): the mode request, cancel a
// job, delete a box and extend one. Each is recorded in the history with the
// device that asked.

const controlTimeout = 20 * time.Second

func (s *Server) controlRoutes(mux *http.ServeMux) {
	mux.Handle("POST /api/mode", control(http.HandlerFunc(s.mode)))
	mux.Handle("POST /api/jobs/{key}/cancel", control(http.HandlerFunc(s.cancelJob)))
	mux.Handle("POST /api/boxes/{name}/delete", control(http.HandlerFunc(s.deleteBox)))
	mux.Handle("POST /api/boxes/{name}/extend", control(http.HandlerFunc(s.extendBox)))
	mux.HandleFunc("GET /api/throwaway-pool", s.throwawayPool)
	mux.Handle("POST /api/boxes/{name}/claim", control(http.HandlerFunc(s.claimThrowaway)))
	mux.Handle("POST /api/boxes/{name}/evict-idle", control(http.HandlerFunc(s.evictThrowaway)))
	mux.Handle("POST /api/boxes/{name}/pool-expiry", control(http.HandlerFunc(s.poolExpiry)))
}

func (s *Server) throwawayPool(w http.ResponseWriter, _ *http.Request) {
	if !s.ReuseThrowaways {
		writeError(w, http.StatusNotFound, dash.CodeNotFound, "throwaway pooling is disabled")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) claimThrowaway(w http.ResponseWriter, r *http.Request) {
	if !s.ReuseThrowaways {
		s.throwawayPool(w, r)
		return
	}
	var req dash.ClaimThrowawayBody
	if !decode(w, r, &req) {
		return
	}
	box := r.PathValue("name")
	key, badKey := hex.DecodeString(req.RepoKey)
	sz, sized := mode.LookupSize(req.Size)
	if !poolJobMatches(box, req.Job) || badKey != nil || len(key) != 32 || !sized || sz.GPU {
		writeError(w, http.StatusBadRequest, dash.CodeBadRequest, "claim needs a job, repository key and CPU size")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), controlTimeout)
	defer cancel()
	claimed, err := s.W.claimThrowaway(ctx, box, req.RepoKey, sz.Name, req.Job)
	poolReply(w, claimed, err)
}

func (s *Server) evictThrowaway(w http.ResponseWriter, r *http.Request) {
	var req struct{ UID string }
	if !decode(w, r, &req) {
		return
	}
	box := r.PathValue("name")
	if !validBoxName.MatchString(box) || req.UID == "" {
		writeError(w, http.StatusBadRequest, dash.CodeBadRequest, "eviction needs a box name and UID")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), controlTimeout)
	defer cancel()
	evicted, err := s.W.evictPooled(ctx, box, req.UID)
	poolReply(w, evicted, err)
}

func (s *Server) poolExpiry(w http.ResponseWriter, r *http.Request) {
	var req dash.PoolExpiryBody
	if !decode(w, r, &req) {
		return
	}
	box := r.PathValue("name")
	if !poolJobMatches(box, req.Job) {
		writeError(w, http.StatusBadRequest, dash.CodeBadRequest, "expiry needs this box's job ID")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), controlTimeout)
	defer cancel()
	wrote, err := s.W.setPoolExpiry(ctx, box, req.Job, req.Ended)
	poolReply(w, wrote, err)
}

func poolJobMatches(box, job string) bool {
	name, _, started := jobStart(job)
	return validBoxName.MatchString(box) && validJob.MatchString(job) && name == box && !started.IsZero()
}

func poolReply(w http.ResponseWriter, changed bool, err error) {
	switch {
	case err != nil:
		writeError(w, http.StatusBadGateway, dash.CodeIncus, err.Error())
	case !changed:
		w.WriteHeader(http.StatusConflict)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// device names the asking device for the history and the mode request: its
// tailnet name from `tailscale whois`, else the login.
func (s *Server) device(r *http.Request) string {
	if s.W.Whois != nil {
		// The last address is the one the nearest proxy (tailscale serve) set.
		hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		ip := strings.TrimSpace(hops[len(hops)-1])
		if net.ParseIP(ip) != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if d := s.W.Whois(ctx, ip); d != "" {
				return d
			}
		}
	}
	return by(r)
}

// TailscaleWhois is Watcher.Whois outside tests: the device's name.
func TailscaleWhois(ctx context.Context, ip string) string {
	out, err := exec.CommandContext(ctx, "tailscale", "whois", "--json", ip).Output()
	if err != nil {
		return ""
	}
	var w struct {
		Node struct {
			ComputedName string
			Name         string
		}
	}
	if json.Unmarshal(out, &w) != nil {
		return ""
	}
	if w.Node.ComputedName != "" {
		return w.Node.ComputedName
	}
	return strings.SplitN(w.Node.Name, ".", 2)[0]
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, dash.CodeBadRequest, "bad JSON: "+err.Error())
		return false
	}
	return true
}

// mode writes the mode request for the tray, which stays the only writer of
// the mode itself (docs/ui.md, "The mode handshake").
func (s *Server) mode(w http.ResponseWriter, r *http.Request) {
	var b dash.ModeBody
	if !decode(w, r, &b) {
		return
	}
	if !slices.Contains(dash.Modes, b.Mode) {
		writeError(w, http.StatusBadRequest, dash.CodeBadRequest, "mode is one of "+strings.Join(dash.Modes, ", "))
		return
	}
	req := dash.ModeRequest{Mode: b.Mode, At: s.W.Now().UTC().Truncate(time.Second), By: s.device(r)}
	m, err := s.W.RequestMode(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadGateway, dash.CodeIncus, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, m)
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.W.CancelJob(r.Context(), r.PathValue("key"), s.device(r))
	writeControl(w, err, func() { writeJSON(w, 200, j) })
}

func (s *Server) deleteBox(w http.ResponseWriter, r *http.Request) {
	var b dash.DeleteBody
	if !decode(w, r, &b) {
		return
	}
	if b.CreatedAt.IsZero() {
		writeError(w, http.StatusBadRequest, dash.CodeBadRequest, "createdAt names the box")
		return
	}
	err := s.W.DeleteBox(r.Context(), r.PathValue("name"), b.CreatedAt, b.Confirmed, s.device(r))
	var nc *needsConfirmError
	if errors.As(err, &nc) {
		writeJSON(w, http.StatusConflict, dash.DeleteConflictBody{Error: nc.warning.Message(r.PathValue("name")), Code: dash.CodeNeedsConfirm, Warning: nc.warning})
		return
	}
	writeControl(w, err, func() { w.WriteHeader(http.StatusNoContent) })
}

// needsConfirmError is an unconfirmed delete the guard refused (#247): the
// box has work the page had not shown when it asked.
type needsConfirmError struct{ warning dash.DeleteWarning }

func (e *needsConfirmError) Error() string { return "delete needs confirmation" }

func (s *Server) extendBox(w http.ResponseWriter, r *http.Request) {
	var b dash.ExtendBody
	if !decode(w, r, &b) {
		return
	}
	if b.CreatedAt.IsZero() {
		writeError(w, http.StatusBadRequest, dash.CodeBadRequest, "createdAt names the box")
		return
	}
	if !slices.Contains(dash.ExtendHours, b.Hours) {
		writeError(w, http.StatusBadRequest, dash.CodeBadRequest, "hours is 1, 4 or 24")
		return
	}
	box, err := s.W.ExtendBox(r.Context(), r.PathValue("name"), b.CreatedAt, b.Hours, s.device(r))
	writeControl(w, err, func() { writeJSON(w, 200, box) })
}

// controlError is a control that can't be applied, with its status and code.
type controlError struct {
	status int
	code   string
	msg    string
}

func (e *controlError) Error() string { return e.msg }

func writeControl(w http.ResponseWriter, err error, ok func()) {
	var ce *controlError
	switch {
	case errors.As(err, &ce):
		writeError(w, ce.status, ce.code, ce.msg)
	case err != nil:
		writeError(w, http.StatusBadGateway, dash.CodeIncus, err.Error())
	default:
		ok()
	}
}

var (
	errJobGone = &controlError{http.StatusNotFound, dash.CodeNotFound, "no such job"}
	errBoxGone = &controlError{http.StatusNotFound, dash.CodeNotFound, "no such box"}
	errNameNew = &controlError{http.StatusConflict, dash.CodeGone, "the box of that name is another one now"}
	errEnded   = &controlError{http.StatusConflict, dash.CodeNotRunning, "the job has already ended"}
)

// RequestMode writes user.agent-box.mode-request and announces the machine
// with it pending.
func (w *Watcher) RequestMode(ctx context.Context, req dash.ModeRequest) (dash.Machine, error) {
	b, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	// The tray writes the project's keys every few seconds (jobrun.RetryETag).
	if err := jobrun.RetryETag(ctx, func() error {
		_, err := w.Incus.CLI(ctx, "", "project", "set", mode.Project, mode.ProjectModeRequestKey+"="+string(b))
		return err
	}); err != nil {
		return dash.Machine{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.record(record{Type: recControl, At: req.At, Control: &controlRec{Action: "mode " + req.Mode, Target: w.MachineID, By: req.By}})
	w.machine.ModeRequested = &req
	w.emit(dash.Event{Name: dash.EventMachine, Data: w.machine})
	return w.machine, nil
}

// CancelJob does what box cancel does, then records the job ended with 130.
func (w *Watcher) CancelJob(ctx context.Context, key, by string) (dash.Job, error) {
	w.mu.Lock()
	j, live := w.live[key]
	var box dash.Box
	var job dash.Job
	if live {
		job = *j
		box = w.boxes[j.Box]
	}
	w.mu.Unlock()
	if !live {
		if _, ok := w.Hist.Job(key); ok {
			return dash.Job{}, errEnded
		}
		return dash.Job{}, errJobGone
	}
	if box.State == dash.BoxFrozen {
		return dash.Job{}, &controlError{http.StatusConflict, dash.CodeFrozen, "the box is frozen for My PC; it can't run the kill"}
	}
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	who := by
	if who == "" {
		who = "an unknown device" // never empty: empty is box cancel's own
	}
	out, err := w.Incus.Exec(ctx, job.Box, jobrun.CancelScript(workRoot, job.Box+"."+job.ID, 130, who))
	var ee *ExitError
	if errors.As(err, &ee) && ee.Code == 3 {
		return dash.Job{}, errJobGone
	}
	if err != nil {
		return dash.Job{}, err
	}
	f := strings.Fields(string(out))
	if len(f) == 2 && f[0] == "finished" {
		return dash.Job{}, errEnded // it ended on its own first; the next poll records how
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.record(record{Type: recControl, Control: &controlRec{Action: "cancel", Target: key, By: by}})
	j, ok := w.live[key]
	if !ok { // a poll saw it end meanwhile
		done, _ := w.Hist.Job(key)
		return done, nil
	}
	before, now, code := *j, w.Now().UTC(), 130
	j.State, j.Exit, j.EndedAt = dash.JobDone, &code, &now
	delete(w.died, key)
	w.settle(key, j, before)
	return *j, nil
}

// boxFor finds the box of a name made at createdAt.
func (w *Watcher) boxFor(name string, createdAt time.Time) (dash.Box, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	b, ok := w.boxes[name]
	switch {
	case !ok:
		return b, errBoxGone
	case !b.CreatedAt.Equal(createdAt):
		return b, errNameNew
	}
	return b, nil
}

// deleteRisk is the warning for deleting b now, from the jobs boxd knows
// ran on this instance: live ones, the history's bounded check, and queued
// jobs a start has begun for (#247). Call it once the box has been probed,
// so it also counts a job that started after the last poll.
func (w *Watcher) deleteRisk(b dash.Box) dash.DeleteWarning {
	now := w.Now().UTC()
	var warn dash.DeleteWarning
	if w.Hist != nil {
		warn = w.Hist.DeleteRiskOn(b, now)
	}
	seen := map[string]bool{}
	for _, j := range warn.Active {
		seen[j.Key] = true
	}
	for _, j := range warn.Recent {
		seen[j.Key] = true
	}
	add := func(j dash.Job, onBox bool) {
		if !onBox || seen[j.Key] {
			return
		}
		switch active, recent := j.DeleteClass(now); {
		case active:
			seen[j.Key] = true
			warn.Active = append(warn.Active, j)
		case recent:
			seen[j.Key] = true
			warn.Recent = append(warn.Recent, j)
		}
	}
	w.mu.Lock()
	live := make([]dash.Job, 0, len(w.live))
	for _, j := range w.live {
		live = append(live, *j)
	}
	w.mu.Unlock()
	for _, j := range live {
		add(j, ranOn(j, b))
	}
	if w.Queue != nil {
		for _, j := range w.Queue.StartingFor(b.Name) {
			add(j, j.Box == b.Name)
		}
	}
	return warn
}

var errProbe = errors.New("the box's jobs could not be read")

// classifyScan turns one box's fresh scan into the delete warning for this
// request only. It writes nothing: folding the scan into live and the
// history (applyJobs) would let an earlier poll whose scan predates a
// newly started job settle that job as lost afterwards.
func (w *Watcher) classifyScan(in Instance, jobs []scanned) dash.DeleteWarning {
	now := w.Now().UTC()
	var warn dash.DeleteWarning
	for _, s := range jobs {
		name, id, started := jobStart(s.Job)
		if s.Meta != nil && s.Meta.QueuedAt != "" {
			if t, err := time.Parse(time.RFC3339, s.Meta.StartedAt); err == nil {
				started = t.UTC()
			}
		}
		if name != in.Name || started.IsZero() || !holds(in.CreatedAt, started) {
			continue
		}
		j := w.newJob(dash.JobKey(in.Name, id, started), id, in.Name, started, in, s.Meta)
		if s.Tail != "" {
			j.Tail = s.Tail
		}
		switch s.State {
		case "done":
			j.State, j.Exit, j.EndedAt = dash.JobDone, s.Exit, s.EndedAt
			if j.EndedAt == nil {
				j.EndedAt = &now
			}
		case "running", "starting":
			j.State = dash.JobRunning
			if in.Status == "Frozen" {
				j.State = dash.JobPaused
			}
		// A "died" job's wrapper is gone with no exit written: it is not
		// active, however old its log is. Only live and history classify a
		// loss as recent, so a dead log left on a repo box can't warn forever.
		default:
			continue
		}
		switch active, recent := j.DeleteClass(now); {
		case active:
			warn.Active = append(warn.Active, *j)
		case recent:
			warn.Recent = append(warn.Recent, *j)
		}
	}
	return warn
}

// probeDelete scans one box fresh for a delete and classifies the scan
// (classifyScan); it changes no job state. Running boxes go through
// scanAll, which drops a scan whose box was replaced while it was read.
// A Frozen box's processes are paused, so it is read with a short exec
// directly; failing that, its state is unknown. A Stopped box starts no
// jobs and needs no probe. Any other status is transient or unknown, so
// it is errProbe too: never a warningless delete.
func (w *Watcher) probeDelete(ctx context.Context, in Instance) (dash.DeleteWarning, error) {
	switch in.Status {
	case "Running":
		scans := w.scanAll(ctx, []Instance{in})
		jobs, ok := scans[in.Name]
		if !ok {
			return dash.DeleteWarning{}, errProbe
		}
		return w.classifyScan(in, jobs), nil
	case "Frozen":
		var skip map[string]bool
		if w.Hist != nil {
			skip = w.Hist.EndedOn(in)
		}
		look, cancel := context.WithTimeout(ctx, execTimeout)
		defer cancel()
		out, err := w.Incus.Exec(look, in.Name, scanScript, slices.Collect(mapKeys(skip))...)
		if err != nil {
			return dash.DeleteWarning{}, errProbe
		}
		return w.classifyScan(in, parseScan(out, skip)), nil
	case "Stopped":
		return dash.DeleteWarning{}, nil
	}
	return dash.DeleteWarning{}, errProbe
}

func mergeWarning(warn *dash.DeleteWarning, more dash.DeleteWarning) {
	seen := map[string]bool{}
	for _, j := range warn.Active {
		seen[j.Key] = true
	}
	for _, j := range warn.Recent {
		seen[j.Key] = true
	}
	for _, j := range more.Active {
		if !seen[j.Key] {
			seen[j.Key] = true
			warn.Active = append(warn.Active, j)
		}
	}
	for _, j := range more.Recent {
		if !seen[j.Key] {
			seen[j.Key] = true
			warn.Recent = append(warn.Recent, j)
		}
	}
}

// DeleteBox deletes a box after checking it is the one the page saw. Its
// running jobs become lost on the next poll. An unconfirmed delete is
// refused with the warning when the box has active or recent jobs (#247);
// a confirmed one deletes as before.
//
// The warning is not read off the last poll: a direct box run can start
// after it, and a job can freeze before it. Before a warningless delete,
// DeleteBox scans a Running or Frozen box fresh and classifies the scan
// for this request (probeDelete — read-only, no live or history writes),
// rechecks the box's identity after a successful probe, and merges the
// scan with the known jobs. Known work warns. When the box's job state
// cannot be verified at all, the answer is a retryable error: no delete,
// and no warning to confirm past.
//
// What the guard can guarantee is still bounded by #217: Incus deletes by
// name with no conditional delete. A job or a replacement can arrive after
// the last check and before or during the delete call; that arrival is
// not covered.
func (w *Watcher) DeleteBox(ctx context.Context, name string, createdAt time.Time, confirmed bool, by string) error {
	b, err := w.boxFor(name, createdAt)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	// Incus has no compare-and-delete; check identity again just before, so
	// the window for a reuse of the name is the length of one call.
	in, err := w.checkCreated(ctx, name, createdAt)
	if err != nil {
		return err
	}
	if !confirmed {
		probed, probeErr := w.probeDelete(ctx, in)
		if probeErr == nil && (in.Status == "Running" || in.Status == "Frozen") {
			// The probe took time; the box it read must still be this one.
			if _, err := w.checkCreated(ctx, name, createdAt); err != nil {
				return err
			}
		}
		warn := w.deleteRisk(b)
		mergeWarning(&warn, probed)
		if warn.Any() {
			return &needsConfirmError{warning: warn}
		}
		if probeErr != nil {
			return &controlError{http.StatusBadGateway, dash.CodeIncus, "couldn't check " + name + " for jobs, so nothing was deleted; try again"}
		}
	}
	if err := w.delete(ctx, name); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.record(record{Type: recControl, Control: &controlRec{Action: "delete", Target: name, By: by}})
	return nil
}

// checkCreated reads the box from Incus now and checks its creation time.
func (w *Watcher) checkCreated(ctx context.Context, name string, createdAt time.Time) (Instance, error) {
	ins, err := w.Incus.Instances(ctx)
	if err != nil {
		return Instance{}, err
	}
	for _, in := range ins {
		if in.Name == name {
			if !in.CreatedAt.Equal(createdAt) {
				return in, errNameNew
			}
			return in, nil
		}
	}
	return Instance{}, errBoxGone
}

// ExtendBox adds hours to the later of now and the box's expiry, and holds
// the box until then (mode.HoldKey): a job's start or end, or a launch short
// of room, won't take that time away.
func (w *Watcher) ExtendBox(ctx context.Context, name string, createdAt time.Time, hours int, by string) (dash.Box, error) {
	b, err := w.boxFor(name, createdAt)
	if err != nil {
		return b, err
	}
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	// Each try reads the box again: the write that beat ours may have moved
	// the expiry (a job starting there, or another extend), and the new
	// expiry is counted from the one Incus holds now.
	var until time.Time
	var replaced bool
	wrote, err := w.Incus.SetConfigIf(ctx, name, func(in Instance) map[string]string {
		replaced = !in.CreatedAt.Equal(createdAt)
		if replaced {
			return nil
		}
		from := w.Now().UTC()
		if t, err := time.Parse(time.RFC3339, in.Config[jobrun.ExpiresKey]); err == nil && t.After(from) {
			from = t.UTC()
		}
		until = mode.Held(in.Config, from).UTC().Add(time.Duration(hours) * time.Hour).Truncate(time.Second)
		at := until.Format(time.RFC3339)
		return map[string]string{jobrun.ExpiresKey: at, mode.HoldKey: at}
	})
	if err != nil {
		return b, err
	}
	if replaced {
		return b, errNameNew
	}
	if !wrote {
		return b, errBoxGone
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.record(record{Type: recControl, Control: &controlRec{Action: "extend " + itoa(hours) + "h", Target: name, By: by}})
	if cur, ok := w.boxes[name]; ok && cur.CreatedAt.Equal(createdAt) {
		// Only protect a deadline published while this request was in flight.
		// The original cache entry may predate a job's expiry shortening.
		if cur.ExpiresAt != nil && (b.ExpiresAt == nil || !cur.ExpiresAt.Equal(*b.ExpiresAt)) &&
			!until.After(*cur.ExpiresAt) {
			return cur, nil
		}
		cur.ExpiresAt = &until
		w.boxes[name] = cur
		w.emit(dash.Event{Name: dash.EventBox, Data: cur})
		return cur, nil
	}
	b.ExpiresAt = &until
	return b, nil
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// delete deletes a box, running or not, waiting out another operation on
// it such as the tray's limit write (#71).
func (w *Watcher) delete(ctx context.Context, name string) error {
	return jobrun.RetryETag(ctx, func() error {
		_, err := w.Incus.CLI(ctx, "", "delete", name, "--force")
		return err
	})
}
