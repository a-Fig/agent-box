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
// ran on this instance: live ones, the history, and queued jobs a start has
// begun for. It reads current state, so a job that appeared after the page's
// last update is counted even when the page never saw it (#247).
func (w *Watcher) deleteRisk(b dash.Box) dash.DeleteWarning {
	now := w.Now().UTC()
	seen := map[string]bool{}
	var warn dash.DeleteWarning
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
	for _, j := range w.Hist.AllJobs() {
		add(j, ranOn(j, b))
	}
	if w.Queue != nil {
		for _, j := range w.Queue.StartingFor(b.Name) {
			add(j, j.Box == b.Name)
		}
	}
	return warn
}

// DeleteBox deletes a box after checking it is the one the page saw. Its
// running jobs become lost on the next poll. An unconfirmed delete is
// refused with the warning when the box has active or recent jobs (#247);
// a confirmed one deletes as before. What the guard can guarantee is
// bounded by #217: Incus deletes by name with no conditional delete, so the
// identity and warning are rechecked immediately before the delete and the
// remaining window is that one call — a box replaced, or a job starting, in
// that gap is not covered.
func (w *Watcher) DeleteBox(ctx context.Context, name string, createdAt time.Time, confirmed bool, by string) error {
	b, err := w.boxFor(name, createdAt)
	if err != nil {
		return err
	}
	if !confirmed {
		if warn := w.deleteRisk(b); warn.Any() {
			return &needsConfirmError{warn}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	// Incus has no compare-and-delete; check again just before, so the window
	// for a reuse of the name is the length of one call.
	if _, err := w.checkCreated(ctx, name, createdAt); err != nil {
		return err
	}
	if !confirmed {
		if warn := w.deleteRisk(b); warn.Any() {
			return &needsConfirmError{warn}
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
