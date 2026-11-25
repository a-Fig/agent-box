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
// room, the GPU taken, My PC or a game) hands the run to boxd's queue
// instead of refusing it: box sends the worktree's snapshot, the same git
// bundle a run pushes, and boxd starts the job when it can. The watcher
// (push.go) follows the job through the queue, nudges the agent once at
// dash.QueueNudge, and pushes the result as for any other job.

// stateQueued is a box_run whose job waits in boxd's queue.
const stateQueued = "queued"

// queueTimeout bounds asking boxd about a queued job; an upload has none,
// since a snapshot can be large.
const queueTimeout = 15 * time.Second

// boxdDo sends a request to boxd on the PC (pcHost) and returns the status
// and body. A transport failure, or no host, is an error.
func (a *App) boxdDo(ctx context.Context, method, path, contentType string, body io.Reader) (int, []byte, error) {
	host := a.pcHost(ctx)
	if host == "" {
		return 0, nil, errors.New("no address for boxd (incus remote " + a.Remote + ")")
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+host+path, body)
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
// A GPU run doesn't wait out My PC or a game, which can last hours
// (docs/queue.md); boxd refuses it too.
func queueable(why string, sz mode.Size) bool {
	switch why {
	case refusedFull, refusedGPUBusy:
		return true
	case refusedMyPC, refusedYielding:
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
	bundle string // a snapshot made already, or "" to make one
	ref    string
}

// enqueue sends the run to boxd's queue. ok is false when it isn't queued;
// msg then says why, for the refusal.
func (a *App) enqueue(ctx context.Context, q queueRun) (r JobResult, ok bool, msg string) {
	bundle, ref := q.bundle, q.ref
	if bundle == "" {
		ref = a.Now().UTC().Format("20060102-150405") + "-" + a.Rand()
		b, err := snapshot(ctx, a.R, q.rp.Top, ref)
		if err != nil {
			return r, false, "can't snapshot the worktree for the queue: " + err.Error()
		}
		defer os.RemoveAll(filepath.Dir(b))
		bundle = b
	}
	meta, err := json.Marshal(q.info)
	if err != nil {
		return r, false, err.Error()
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
	f, err := os.Open(bundle)
	if err != nil {
		return r, false, err.Error()
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
		return r, false, "can't reach boxd to queue the run: " + firstLine(err.Error())
	case code != http.StatusCreated:
		return r, false, "boxd didn't queue the run: " + boxdError(code, b)
	}
	var rep dash.QueueReply
	if err := json.Unmarshal(b, &rep); err != nil || !validJob.MatchString(rep.Job) {
		return r, false, "boxd's answer to the queue: " + string(b)
	}
	box, _, _ := strings.Cut(rep.Job, ".")
	fmt.Fprintf(a.Err, "box: queued job %s, %s in line (%s); it starts when it can\n", rep.Job, ordinal(rep.Position), waitText(rep.Why))
	return JobResult{Job: rep.Job, Box: box, State: stateQueued, Code: exitPaused, Position: rep.Position, Why: rep.Why, throwaway: q.new}, true, ""
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
	case refusedYielding:
		return "waiting while the PC's owner plays a game"
	case "":
		return "about to start"
	}
	return "waiting: " + why
}

// queuedJob asks boxd about a job by name: queued, started or ended. found
// is false for a job boxd doesn't know (404); err is a failure to ask.
func (a *App) queuedJob(ctx context.Context, job string) (j dash.Job, found bool, err error) {
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
	return j, true, json.Unmarshal(b, &j)
}

// errNotQueued is cancelQueued's answer for a job that isn't waiting.
var errNotQueued = errors.New("not waiting in the queue")

// cancelQueued removes a waiting job from boxd's queue, which deletes its
// snapshot. errNotQueued means it has started or isn't boxd's.
func (a *App) cancelQueued(ctx context.Context, job string) error {
	ctx, cancel := context.WithTimeout(ctx, queueTimeout)
	defer cancel()
	code, b, err := a.boxdDo(ctx, http.MethodPost, "/api/queue/"+url.PathEscape(job)+"/cancel", "", nil)
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
	return readJSON(a.statePath(job, ".json"), &rec) == nil && rec.Queued
}

// nudgeMessage is the one push a job gets once it has waited dash.QueueNudge.
func nudgeMessage(r jobRecord, waited time.Duration, why string) string {
	return fmt.Sprintf("box: job %s still queued after %s (%s). Can't wait? Run it locally and box_cancel this job. %s\n%s",
		r.Job, elapsed(waited), waitText(why), fromBox, r.context())
}

// droppedMessage is the push for a queued job that never ran: boxd dropped
// it after dash.QueueMaxAge, or couldn't start it.
func droppedMessage(r jobRecord) string {
	return fmt.Sprintf("box: job %s left the queue without running (it waited %s at most, or couldn't start), exit 75: run it locally. %s\n%s",
		r.Job, elapsed(dash.QueueMaxAge), fromBox, r.context())
}

// waitQueued follows a queued job until it starts. started is when it did;
// ended is set when it left the queue without running. It nudges once at
// dash.QueueNudge. ok is false when ctx ended or the agent cancelled it.
func (a *App) waitQueued(ctx context.Context, rec jobRecord) (started time.Time, dropped bool, ok bool) {
	queuedAt := jobStart(rec.Job) // boxd names a queued job by when it was queued
	backoff := 15 * time.Second
	for ctx.Err() == nil {
		if a.exists(rec.Job, ".notified") {
			return time.Time{}, false, false // box_cancel took it off the queue
		}
		j, found, err := a.queuedJob(ctx, rec.Job)
		switch {
		case err != nil:
			// Out of reach: keep waiting, but not past the queue's own limit.
			if a.Now().Sub(queuedAt) > dash.QueueMaxAge+time.Hour {
				return time.Time{}, true, true
			}
			a.sleep(ctx, backoff)
			backoff = min(backoff*2, 2*time.Minute)
			continue
		case !found:
			// Not boxd's to report (a restart forgets a start it hasn't
			// scanned yet): the box itself says the rest.
			return time.Time{}, false, true
		case j.State == dash.JobQueued:
			backoff = 15 * time.Second
			if j.QueuedAt != nil {
				queuedAt = *j.QueuedAt
			}
			if waited := a.Now().Sub(queuedAt); waited >= dash.QueueNudge {
				if won, _ := a.claimBy(rec.Job, ".nudged", "watcher"); won {
					a.send(ctx, rec.Thread, nudgeMessage(rec, waited, j.Why))
				}
			}
			a.sleep(ctx, backoff)
			continue
		case j.State == dash.JobLost && (j.QueuedAt == nil || !j.StartedAt.After(*j.QueuedAt)):
			return time.Time{}, true, true
		}
		return j.StartedAt, false, true // running, paused or ended: the box has it
	}
	return time.Time{}, false, false
}
