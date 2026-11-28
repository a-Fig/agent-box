package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/mode"
)

// The queue, box's side (docs/queue.md). A box_run that can't start now (no
// room, the GPU taken, My PC) hands the run to boxd's queue
// instead of refusing it: box sends the worktree's snapshot, the same git
// bundle a run pushes, and boxd starts the job when it can. The watcher
// (push.go) follows the job through the queue, nudges the agent once at
// dash.QueueNudge, and pushes the result as for any other job.

// stateQueued is a box_run whose job waits in boxd's queue.
const stateQueued = "queued"

// queueTimeout bounds asking boxd about a queued job; an upload has none,
// since a snapshot can be large.
const queueTimeout = 15 * time.Second

// boxdDo sends a request to the selected boxd and returns the status
// and body. A transport failure, or no host, is an error.
func (a *App) boxdDo(ctx context.Context, method, path, contentType string, body io.Reader) (int, []byte, error) {
	address := a.boxdURL(ctx, path)
	if address == "" {
		return 0, nil, errors.New("no address for boxd (incus remote " + a.Remote + ")")
	}
	return a.boxdRequest(ctx, method, address, contentType, body)
}

func (a *App) boxdRequest(ctx context.Context, method, url, contentType string, body io.Reader) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return 0, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set(dash.ControlHeader, "1")
	do := a.HTTP
	if do == nil {
		do = http.DefaultClient.Do
	}
	resp, err := do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, err
}

// boxdError is boxd's message in an error body, or the status.
func boxdError(code int, b []byte) string {
	var eb dash.ErrorBody
	if json.Unmarshal(b, &eb) == nil && eb.Error != "" {
		return eb.Error
	}
	return fmt.Sprintf("boxd answered %d", code)
}

// queueable says whether a run turned away for why may wait in the queue.
// A GPU run doesn't wait out My PC, which can last hours
// (docs/queue.md); boxd refuses it too.
func queueable(why string, sz mode.Size) bool {
	switch why {
	case refusedFull, refusedGPUBusy:
		return true
	case refusedMyPC:
		return !sz.GPU
	}
	return false
}

// queueRun is what enqueue needs from Run.
type queueRun struct {
	o      RunOpts
	rp     repo
	box    string // the repo's box, or with new the base of a throwaway's name
	new    bool
	sz     mode.Size
	info   jobMeta
	snap   worktreeSnapshot // a snapshot made already, or zero to make one
	ref    string
	host   *fleetHost // non-nil for fleet dispatch (qualifies returned job ID)
	limits []string   // why the host could not start the run immediately
}

type queueFailure uint8

const (
	queueUnconfirmed queueFailure = iota
	queueRejected
	queueInvalid
)

// enqueue sends the run to boxd. Only a confirmed host rejection can be retried.
func (a *App) enqueue(ctx context.Context, q queueRun) (r JobResult, msg string, failure queueFailure) {
	snap, ref := q.snap, q.ref
	if snap.bundle == "" {
		ref = a.Now().UTC().Format("20060102-150405") + "-" + a.Rand()
		s, err := snapshot(ctx, a.R, q.rp.Top, ref)
		if err != nil {
			return r, "can't snapshot the worktree for the queue: " + err.Error(), queueInvalid
		}
		defer os.RemoveAll(filepath.Dir(s.bundle))
		snap = s
	}
	q.info.setSource(snap.source)
	meta, err := json.Marshal(q.info)
	if err != nil {
		return r, err.Error(), queueInvalid
	}
	timeout := int(q.o.Timeout.Round(time.Second) / time.Second)
	if q.o.Timeout > 0 && timeout == 0 {
		timeout = 1
	}
	req := dash.QueueRequest{Box: q.box, New: q.new, Size: q.o.Size, GPU: q.sz.GPU, Checkout: q.rp.Name, Ref: ref,
		Command: q.o.Cmd, Env: q.o.Env, Timeout: timeout, Meta: meta}
	if q.new && req.Size == "" {
		req.Size = q.sz.Name
	}
	f, err := os.Open(snap.bundle)
	if err != nil {
		return r, err.Error(), queueInvalid
	}
	defer f.Close()

	// Stream the body: a snapshot can be large.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			w, err := mw.CreateFormField("request")
			if err != nil {
				return err
			}
			if err := json.NewEncoder(w).Encode(req); err != nil {
				return err
			}
			if w, err = mw.CreateFormFile("bundle", "snapshot.bundle"); err != nil {
				return err
			}
			if _, err := io.Copy(w, f); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()
	code, b, err := a.boxdDo(ctx, http.MethodPost, "/api/queue", mw.FormDataContentType(), pr)
	pr.Close()
	switch {
	case err != nil:
		return r, "can't reach boxd to queue the run: " + firstLine(err.Error()), queueUnconfirmed
	case code != http.StatusCreated:
		msg := boxdError(code, b)
		return r, "boxd didn't queue the run: " + msg, classifyQueueRejection(code, msg)
	}
	var rep dash.QueueReply
	if err := json.Unmarshal(b, &rep); err != nil || !validJob.MatchString(rep.Job) {
		return r, "boxd's answer to the queue: " + string(b), queueUnconfirmed
	}
	box, _, _ := strings.Cut(rep.Job, ".")
	pubID, err := runJobID(q.host, rep.Job, box)
	if err != nil {
		return r, err.Error(), queueUnconfirmed
	}
	hostName := a.jobHostName
	// The launch above, if any, was refused: say which box the job will be
	// on, since boxd names a throwaway of its own (#89).
	on := "on " + box
	if q.new {
		on = "on a new box, " + box + ","
	}
	fmt.Fprintf(a.Err, "box: queued job %s%s, %s (%s); it starts %s when it can\n", pubID.ID, onHost(hostName), place(rep.Position), waitText(rep.Why), on)
	detail := a.capacityHint(ctx, q.sz, q.o.Box, q.limits)
	a.printCapacity(detail)
	return JobResult{Job: pubID.ID, Box: box, Host: hostName, State: stateQueued, Code: exitPaused, Position: rep.Position, Why: rep.Why, Capacity: detail, Source: snap.source, throwaway: q.new, fleetHost: q.host}, "", 0
}

// Boxd uses 400 for both request validation and failures
// storing a snapshot. The latter have a distinct error message.
func classifyQueueRejection(status int, message string) queueFailure {
	switch status {
	case http.StatusNotFound, http.StatusConflict, http.StatusInsufficientStorage:
		return queueRejected
	case http.StatusBadRequest:
		if strings.HasPrefix(message, "storing the snapshot: ") {
			return queueRejected
		}
		return queueInvalid
	case http.StatusForbidden:
		return queueInvalid
	default:
		return queueUnconfirmed
	}
}

// place says where a queued job is in line.
func place(n int) string {
	if n < 1 {
		return "starting now"
	}
	return ordinal(n) + " in line"
}

// ordinal is 1st, 2nd, 3rd, 4th...
func ordinal(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return fmt.Sprint(n) + suffix
}

// waitText says what a queued job waits for.
func waitText(why string) string {
	switch why {
	case refusedFull:
		return "waiting for room"
	case refusedGPUBusy:
		return "waiting for the GPU"
	case refusedMyPC:
		return "waiting while the PC is in My PC mode"
	case "":
		return "about to start"
	}
	return "waiting: " + why
}

// queuedJob asks boxd about a job by name: queued, started or ended. found
// is false for a job boxd doesn't know (404); err is a failure to ask.
func (a *App) queuedJob(ctx context.Context, job string) (j dash.Job, found bool, err error) {
	if a.Project != mode.Project {
		return j, false, nil // boxd only records jobs in the agents project
	}
	ctx, cancel := context.WithTimeout(ctx, queueTimeout)
	defer cancel()
	code, b, err := a.boxdDo(ctx, http.MethodGet, "/api/queue/"+url.PathEscape(job), "", nil)
	switch {
	case err != nil:
		return j, false, err
	case code == http.StatusNotFound:
		return j, false, nil
	case code != http.StatusOK:
		return j, false, errors.New(boxdError(code, b))
	}
	err = json.Unmarshal(b, &j)
	if !j.Source.Valid() {
		j.Source = nil
	}
	return j, true, err
}

// errNotQueued is cancelQueued's answer for a job that isn't waiting.
var errNotQueued = errors.New("not waiting in the queue")

// cancelQueued removes a waiting job from boxd's queue, which deletes its
// snapshot. errNotQueued means it has started or isn't boxd's.
func (a *App) cancelQueued(ctx context.Context, job string) error {
	ctx, cancel := context.WithTimeout(ctx, queueTimeout)
	defer cancel()
	// boxd's controls take only JSON, so an empty object it is.
	code, b, err := a.boxdDo(ctx, http.MethodPost, "/api/queue/"+url.PathEscape(job)+"/cancel", "application/json", strings.NewReader("{}"))
	switch {
	case err != nil:
		return err
	case code == http.StatusNoContent:
		return nil
	case code == http.StatusConflict, code == http.StatusNotFound:
		return errNotQueued
	}
	return errors.New(boxdError(code, b))
}

// wasQueued says whether this laptop queued job, from its push record.
func (a *App) wasQueued(job string) bool {
	if a.StateDir == "" {
		return false
	}
	var rec jobRecord
	id, err := parseJobID(job)
	return err == nil && readJSON(a.statePath(job, ".json"), &rec) == nil && rec.Queued &&
		(rec.Host == nil || a.jobSourceMatchesHost(id, rec.Host))
}

// nudgeMessage is the one push a job gets once it has waited dash.QueueNudge.
func nudgeMessage(r jobRecord, waited time.Duration, why string) string {
	return fmt.Sprintf("box: job %s still queued after %s (%s). It runs when there is room; box_cancel takes it off the queue. %s\n%s",
		r.noticeJob(), elapsed(waited), waitText(why), fromBox, r.context())
}

// droppedMessage is the push for a queued job that never ran: boxd dropped
// it after dash.QueueMaxAge, or couldn't start it.
func droppedMessage(r jobRecord) string {
	return fmt.Sprintf("box: job %s left the queue without running (it waited %s at most, or couldn't start), exit 75. %s\n%s",
		r.noticeJob(), elapsed(dash.QueueMaxAge), fromBox, r.context())
}

// queueOutcome is how a queued job left the queue: started is when it began
// running in its box (zero when only the box can say), or end is how it ended
// when boxd already knows: its exit code, or that it never ran.
type queueOutcome struct {
	started   time.Time
	end       *jobEnd
	source    *dash.SourceRevision
	cancelled bool
}

// jobInBox says whether the job's log exists in its box: it started there.
func (a *App) jobInBox(ctx context.Context, job string) bool {
	look, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	_, err := a.LogPage(look, job, 0, 1)
	return err == nil
}

// waitQueuedJob follows the selected host. A watcher stops once the job
// was acknowledged. A manual follow uses a local cancel marker only to
// explain why the job is gone after checking the host.
func (a *App) waitQueuedJob(ctx context.Context, rec jobRecord, id jobID, seen, watching bool) (out queueOutcome, ok bool) {
	bare := id.Bare
	queuedAt := jobStart(bare) // boxd names a queued job by when it was queued
	out.source = rec.Source
	dropped := func() (queueOutcome, bool) {
		out.end = &jobEnd{Code: exitPaused, Lost: true, Dropped: true, Took: int64(a.Now().Sub(queuedAt) / time.Second), Source: out.source}
		return out, true
	}
	backoff := 15 * time.Second
	gone := 0 // 404s in a row since boxd answered queued
	for ctx.Err() == nil {
		if watching && a.exists(rec.Job, ".notified") {
			return out, false
		}
		j, found, err := a.queuedJob(ctx, bare)
		if err == nil && found && out.source == nil {
			out.source = j.Source
		}
		switch {
		case err != nil:
			// Out of reach: keep waiting, but not past the queue's own limit.
			if a.Now().Sub(queuedAt) > dash.QueueMaxAge+time.Hour {
				if a.jobInBox(ctx, bare) {
					return out, true
				}
				return dropped()
			}
			a.sleep(ctx, backoff)
			backoff = min(backoff*2, 2*time.Minute)
			continue
		case !found:
			// Before boxd has answered queued, or for a job in its box (a
			// restart forgets a start it hasn't scanned yet), the box says the
			// rest. A job boxd had queued that is in no box was taken off the
			// queue (from the page): it never ran.
			cancelled := !watching && a.cancelledOnHost(id)
			if (seen || cancelled) && a.jobInBox(ctx, bare) {
				return out, true
			}
			if cancelled {
				out.cancelled = true
				return out, true
			}
			if !seen {
				return out, true
			}
			if gone++; gone >= 2 {
				return dropped()
			}
			a.sleep(ctx, backoff)
			continue
		case j.State == dash.JobQueued:
			seen, gone, backoff = true, 0, 15*time.Second
			if j.QueuedAt != nil {
				queuedAt = *j.QueuedAt
			}
			if waited := a.Now().Sub(queuedAt); rec.Thread != "" && waited >= dash.QueueNudge {
				if won, _ := a.claimBy(rec.Job, ".nudged", "watcher"); won {
					a.send(ctx, rec.Thread, nudgeMessage(rec, waited, j.Why))
				}
			}
			a.sleep(ctx, backoff)
			continue
		case j.State == dash.JobLost && (j.QueuedAt == nil || !j.StartedAt.After(*j.QueuedAt)):
			return dropped()
		case j.State == dash.JobDone && j.Exit != nil:
			// It ran and ended while nobody watched: boxd has the result, and
			// its kept box may be gone already.
			e := &jobEnd{Code: *j.Exit, Tail: lastLines(j.Tail, tailLines), Source: out.source}
			if j.EndedAt != nil {
				e.Took = int64(j.EndedAt.Sub(j.StartedAt) / time.Second)
			}
			out.end = e
			return out, true
		}
		out.started = j.StartedAt
		return out, true // running or paused: the box has it
	}
	return out, false
}

// recordQueued writes the record of a job box run queued, so box job and box
// cancel know it waits in boxd's queue (wasQueued). It has no thread: a CLI
// run follows its own job, so no watcher or push is started for it.
func (a *App) recordQueued(info jobMeta, job string, gpu, throwaway bool) {
	a.recordQueuedOnHost(info, job, nil, gpu, throwaway)
}

func (a *App) recordQueuedOnHost(info jobMeta, job string, host *fleetHost, gpu, throwaway bool) {
	if a.StateDir == "" || os.MkdirAll(a.StateDir, 0o755) != nil {
		return
	}
	loc := &jobLocator{Remote: a.Remote, Project: a.Project}
	if host != nil {
		loc = locateJobHost(*host)
	}
	rec := jobRecord{Job: job, Host: loc, Source: info.Source, Reason: info.Reason, Command: info.Command, Worktree: info.Agent.Worktree, GPU: gpu, New: throwaway, Queued: true}
	if err := writeJSON(a.statePath(job, ".json"), rec); err != nil {
		a.warn("box: warning: recording queued job " + job + ": " + err.Error())
	}
}

// followQueued is box run's and box job's wait for a job in boxd's queue
// (#93): it waits until the job leaves the queue, then follows it in its box
// as box job does. Interrupted, it leaves the job queued and says how to
// get back to it.
func (a *App) followQueued(ctx context.Context, job string) (JobResult, error) {
	box, _, _ := strings.Cut(job, ".")
	return a.followQueuedJob(ctx, jobID{ID: job, Bare: job, Box: box}, false)
}

func (a *App) followQueuedJob(ctx context.Context, id jobID, seen bool) (JobResult, error) {
	job, box := id.ID, id.Box
	rec := jobRecord{Job: job, Queued: true}
	readJSON(a.statePath(job, ".json"), &rec)
	rec.Job = job
	rec.Thread = ""  // whoever follows here sees the job; no nudge is pushed
	rec.Source = nil // a manual follow takes its source from the host first
	out, ok := a.waitQueuedJob(ctx, rec, id, seen, false)
	r := JobResult{Job: job, Box: box, Host: a.jobHostName, State: stateDone, Source: out.source}
	if r.Source == nil {
		r.Source = a.savedJobSource(id)
	}
	switch {
	case out.cancelled:
		fmt.Fprintf(a.Err, "box: job %s was taken off the queue; it never ran\n", job)
		r.Code = exitDetached
		return r, nil
	case !ok:
		// Interrupted: 130 as for a detach from a running job, not 75, which
		// says nothing started, though the job will still run on the PC.
		fmt.Fprintf(a.Err, "box: job %s is still queued; box job %s follows it, box cancel %s takes it off\n", job, job, job)
		r.State, r.Code = stateQueued, exitDetached
		return r, nil
	case out.end != nil && out.end.Dropped:
		fmt.Fprintf(a.Err, "box: job %s left the queue without running (it waited %s at most, or couldn't start)\n", job, elapsed(dash.QueueMaxAge))
		r.Code = exitPaused
		return r, nil
	case out.end != nil:
		// It ran and ended between two looks: its kept box has the full log
		// and digest; boxd's tail is the fallback once the box is gone.
		if r, err := a.job(ctx, id, 0, out.source); err == nil && r.State == stateDone {
			return r, nil // not a pause: a kept box frozen by My PC doesn't undo an end boxd has
		}
		io.WriteString(a.Out, out.end.Tail+"\n")
		r.Code = out.end.Code
		return r, nil
	}
	fmt.Fprintf(a.Err, "box: job %s left the queue and started on %s\n", job, box+onHost(a.jobHostName))
	return a.job(ctx, id, 0, out.source)
}

// notifiedBy is who wrote job's .notified ("cancel", "watcher", "box run",
// ...), or "" when there is none.
func (a *App) notifiedBy(job string) string {
	who, _ := a.notification(job)
	return who
}

// A cancellation marker can carry the writing host's tag on its second line.
func (a *App) notification(job string) (who, tag string) {
	if a.StateDir == "" {
		return "", ""
	}
	b, err := os.ReadFile(a.statePath(job, ".notified"))
	if err != nil {
		return "", ""
	}
	who, tag, _ = strings.Cut(strings.TrimSpace(string(b)), "\n")
	return who, tag
}

func (a *App) cancelledOnHost(id jobID) bool {
	who, tag := a.notification(id.ID)
	if who != "cancel" {
		return false
	}
	if id.hostTag != "" {
		return tag == "" || tag == id.hostTag // a legacy qualified ID already names its host
	}
	// A legacy bare marker still acknowledges delivery. Its target is
	// unknown, so a manual follow relies on the selected host's queue.
	want, err := jobHostTag(fleetHost{Remote: a.Remote, Project: a.Project})
	return err == nil && tag == want
}

// lastLines is the last n lines of s.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// CancelAny cancels a job in the selected host's queue or box. A qualified ID
// can come from another client, so its queue is checked without a local record.
// Legacy IDs without a queue marker still try the default host's box first.
func (a *App) CancelAny(ctx context.Context, job string) (removed, cancelled bool, code int, err error) {
	lookup, err := a.lookupJob(job)
	if err != nil {
		return false, false, 0, err
	}
	if lookup.cloud != nil {
		res, err := lookup.cloud.cancel(ctx, lookup.id.Bare)
		if err != nil {
			return false, false, 0, err
		}
		if res.Cancelled {
			lookup.app.markNotified(lookup.id.ID, "cancel")
		}
		return false, res.Cancelled, res.Exit, nil
	}
	target, id := lookup.app, lookup.id
	if id.hostTag != "" || target.wasQueued(id.ID) {
		removed, err = target.cancelQueuedOrStarted(ctx, id.Bare)
		if err != nil {
			return false, false, 0, fmt.Errorf("cancel %s: %w", id.ID, err)
		}
	}
	if !removed {
		cancelled, code, err = target.cancelJob(ctx, id)
		if err != nil && target.cancelQueued(ctx, id.Bare) == nil {
			removed, err = true, nil
		}
	}
	if removed {
		target.markNotified(id.ID, "cancel")
	}
	return removed, cancelled, code, err
}

// cancelQueuedOrStarted tries to remove a job from the queue. A job boxd is
// starting right now can't be taken off the queue, nor stopped in its box
// yet, so it waits for the start, up to a minute, then stops it there.
// removed is false when the job wasn't boxd's to cancel; the caller then
// stops it in its box.

func (a *App) cancelQueuedOrStarted(ctx context.Context, job string) (removed bool, err error) {
	deadline := a.Now().Add(time.Minute)
	for {
		err := a.cancelQueued(ctx, job)
		switch {
		case err == nil:
			return true, nil
		case !errors.Is(err, errNotQueued):
			return false, nil // boxd out of reach: try the box
		}
		j, found, qerr := a.queuedJob(ctx, job)
		if qerr != nil || !found || j.State != dash.JobQueued || a.Now().After(deadline) {
			return false, nil // started, or gone: the box has it
		}
		if !a.sleep(ctx, 2*time.Second) { // still starting
			return false, ctx.Err()
		}
	}
}
