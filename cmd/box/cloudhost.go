package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/mode"
)

const (
	cloudProbeTimeout  = 15 * time.Second
	cloudFollowWaitSec = 25
	cloudMaxBodyBytes  = 1 << 20
)

type cloudHost struct {
	host    fleetHost
	do      func(*http.Request) (*http.Response, error)
	headers map[string]string
	app     *App
}

type cloudTokenFile struct {
	Headers map[string]string `json:"headers"`
}

type cloudErrorEnvelope struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

type cloudBudget struct {
	Month      string  `json:"month"`
	SpentUSD   float64 `json:"spentUSD"`
	RunningUSD float64 `json:"runningUSD"`
	CapUSD     float64 `json:"capUSD"`
	CreditUSD  float64 `json:"creditUSD"`
	WarnAt     float64 `json:"warnAt"`
}

type cloudProbeResponse struct {
	Protocol      int         `json:"protocol"`
	Sizes         []string    `json:"sizes"`
	Running       int         `json:"running"`
	MaxConcurrent int         `json:"maxConcurrent"`
	Paused        bool        `json:"paused"`
	MaxTimeoutSec int         `json:"maxTimeoutSec"`
	Budget        cloudBudget `json:"budget"`
}

type cloudJobSpec struct {
	ID         string               `json:"id"`
	Repo       string               `json:"repo"`
	Ref        string               `json:"ref"`
	Cmd        []string             `json:"cmd"`
	Env        map[string]string    `json:"env"`
	TimeoutSec int                  `json:"timeoutSec"`
	Size       string               `json:"size"`
	Reason     string               `json:"reason"`
	Source     *dash.SourceRevision `json:"source"`
}

type cloudJobCost struct {
	USD    float64 `json:"usd"`
	CPUSec float64 `json:"cpuSec"`
	GPUSec float64 `json:"gpuSec"`
}

type cloudJob struct {
	ID        string        `json:"id"`
	State     string        `json:"state"`
	Exit      *int          `json:"exit"`
	Attempt   int           `json:"attempt"`
	StartedAt *time.Time    `json:"startedAt"`
	EndedAt   *time.Time    `json:"endedAt"`
	LogBytes  int64         `json:"logBytes"`
	Tail      string        `json:"tail"`
	Mode      string        `json:"mode"`
	Cost      *cloudJobCost `json:"cost,omitempty"`
	Budget    *cloudBudget  `json:"budget,omitempty"`
}

type cloudJobList struct {
	Jobs []cloudJob `json:"jobs"`
}

type cloudCancelResponse struct {
	Cancelled bool `json:"cancelled"`
	Exit      int  `json:"exit"`
}

type cloudPauseState struct {
	Paused bool `json:"paused"`
}

type cloudStopResponse struct {
	Stopped int `json:"stopped"`
}

type cloudHTTPStatusError struct {
	status int
	code   string
	msg    string
}

func (e *cloudHTTPStatusError) Error() string {
	return e.msg
}

func (a *App) newCloudHost(host fleetHost) (*cloudHost, error) {
	headers, err := loadCloudHeaders(host.TokenFile)
	if err != nil {
		return nil, err
	}
	do := a.HTTP
	if do == nil {
		do = http.DefaultClient.Do
	}
	return &cloudHost{
		host:    host,
		do:      do,
		headers: headers,
		app:     a,
	}, nil
}

func loadCloudHeaders(tokenFile string) (map[string]string, error) {
	if tokenFile == "" {
		return nil, errors.New("cloud host requires token_file")
	}
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read token file %s: %w", tokenFile, err)
	}
	var tf cloudTokenFile
	if err := json.Unmarshal(raw, &tf); err != nil {
		return nil, fmt.Errorf("parse token file %s: %s", tokenFile, err.Error())
	}
	headers := make(map[string]string, len(tf.Headers))
	for k, v := range tf.Headers {
		if strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("token file %s contains an empty header name", tokenFile)
		}
		headers[k] = v
	}
	return headers, nil
}

func (c *cloudHost) doRequest(ctx context.Context, method, reqPath, contentType string, body io.Reader) (*http.Response, error) {
	endpoint := strings.TrimRight(c.host.Endpoint, "/")
	req, err := http.NewRequestWithContext(ctx, method, endpoint+reqPath, body)
	if err != nil {
		return nil, err
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return c.do(req)
}

func (c *cloudHost) doJSON(ctx context.Context, method, reqPath string, reqBody, out any) error {
	var body io.Reader
	var contentType string
	if reqBody != nil {
		raw, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
		contentType = "application/json"
	}
	resp, err := c.doRequest(ctx, method, reqPath, contentType, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, cloudMaxBodyBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		env := readCloudError(resp.StatusCode, raw)
		return &cloudHTTPStatusError{status: resp.StatusCode, code: env.Error, msg: env.Message}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func readCloudError(status int, body []byte) cloudErrorEnvelope {
	var env cloudErrorEnvelope
	if json.Unmarshal(body, &env) != nil {
		env = cloudErrorEnvelope{}
	}
	if env.Message == "" {
		if env.Error != "" {
			env.Message = env.Error
		} else {
			env.Message = fmt.Sprintf("runner answered %d", status)
		}
	}
	return env
}

func (c *cloudHost) out() io.Writer {
	if c.app != nil && c.app.Out != nil {
		return c.app.Out
	}
	return io.Discard
}

func (c *cloudHost) errOut() io.Writer {
	if c.app != nil && c.app.Err != nil {
		return c.app.Err
	}
	return io.Discard
}

func (c *cloudHost) probe(ctx context.Context) fleetCandidate {
	probeCtx, cancel := context.WithTimeout(ctx, cloudProbeTimeout)
	defer cancel()

	cand := fleetCandidate{
		Host:          c.host,
		Priority:      c.host.Priority,
		BoxdAvailable: false,
	}
	var p cloudProbeResponse
	if err := c.doJSON(probeCtx, http.MethodGet, "/v1/probe", nil, &p); err != nil {
		cand.Error = firstLine(err.Error())
		return cand
	}
	if p.Protocol != 1 {
		cand.Reachable = true
		cand.Error = fmt.Sprintf("unsupported runner protocol %d", p.Protocol)
		return cand
	}

	cand.Reachable = true
	cand.Mode = string(mode.Full)
	room := max(0, p.MaxConcurrent-p.Running)
	for _, name := range p.Sizes {
		sz, ok := mode.LookupSize(name)
		if !ok {
			continue
		}
		if sz.GPU {
			cand.HasGPU = true
		}
		cand.Sizes = append(cand.Sizes, SizeRoom{
			Size: sz,
			CPUs: sz.FullCPUs,
			Fits: room,
		})
	}
	spent := p.Budget.SpentUSD + p.Budget.RunningUSD
	switch {
	case p.Paused:
		cand.Unavailable = "cloud paused"
	case p.Budget.CapUSD >= 0 && spent >= p.Budget.CapUSD:
		cand.Unavailable = fmt.Sprintf("monthly cap reached ($%.2f of $%.2f)", spent, p.Budget.CapUSD)
	case p.Running >= p.MaxConcurrent:
		cand.Unavailable = fmt.Sprintf("cloud busy (%d/%d running)", p.Running, p.MaxConcurrent)
	}
	return cand
}

func (c *cloudHost) start(ctx context.Context, o RunOpts, sz mode.Size, prepared *fleetRun) (JobResult, *turnAwayInfo, error) {
	if o.Box != "" {
		return JobResult{Code: 2}, nil, errors.New("--box needs an Incus host")
	}
	if prepared == nil || prepared.snap.bundle == "" {
		return JobResult{Code: 1}, nil, errors.New("cloud run requires a prepared worktree snapshot")
	}
	bareID := prepared.repo.Name + "." + prepared.ref
	pubID, err := qualifyJobID(c.host, bareID)
	if err != nil {
		return JobResult{Box: prepared.repo.Name, Code: 1}, nil, err
	}

	env := o.Env
	if env == nil {
		env = map[string]string{}
	}
	timeoutSec := int(o.Timeout.Round(time.Second) / time.Second)
	if o.Timeout > 0 && timeoutSec == 0 {
		timeoutSec = 1
	}
	spec := cloudJobSpec{
		ID:         bareID,
		Repo:       prepared.repo.Name,
		Ref:        prepared.ref,
		Cmd:        o.Cmd,
		Env:        env,
		TimeoutSec: timeoutSec,
		Size:       sz.Name,
		Reason:     o.Reason,
		Source:     prepared.snap.source,
	}

	f, err := os.Open(prepared.snap.bundle)
	if err != nil {
		return JobResult{Code: 1}, nil, err
	}
	defer f.Close()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			h := make(textproto.MIMEHeader)
			h.Set("Content-Disposition", `form-data; name="spec"`)
			h.Set("Content-Type", "application/json")
			w, err := mw.CreatePart(h)
			if err != nil {
				return err
			}
			if err := json.NewEncoder(w).Encode(spec); err != nil {
				return err
			}
			if w, err = mw.CreateFormFile("bundle", filepath.Base(prepared.snap.bundle)); err != nil {
				return err
			}
			if _, err := io.Copy(w, f); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()

	resp, err := c.doRequest(ctx, http.MethodPost, "/v1/jobs", mw.FormDataContentType(), pr)
	pr.Close()
	hostCopy := c.host
	if err != nil {
		if ctx.Err() != nil {
			return JobResult{Code: 1}, nil, ctx.Err()
		}
		return JobResult{Job: pubID.ID, Box: pubID.Box, Host: c.host.Name, Code: 1, Source: prepared.snap.source, fleetHost: &hostCopy},
			nil, fmt.Errorf("%s: submission could not be confirmed; check box jobs: %w", hostLabel(c.host), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, cloudMaxBodyBytes))
	if err != nil {
		if ctx.Err() != nil {
			return JobResult{Code: 1}, nil, ctx.Err()
		}
		return JobResult{Job: pubID.ID, Box: pubID.Box, Host: c.host.Name, Code: 1, Source: prepared.snap.source, fleetHost: &hostCopy},
			nil, fmt.Errorf("%s: submission could not be confirmed; check box jobs: %w", hostLabel(c.host), err)
	}

	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK:
		var submitted cloudJob
		if err := json.Unmarshal(body, &submitted); err != nil {
			return JobResult{Job: pubID.ID, Box: pubID.Box, Host: c.host.Name, Code: 1, Source: prepared.snap.source, fleetHost: &hostCopy},
				nil, fmt.Errorf("%s: submission could not be confirmed; check box jobs: %w", hostLabel(c.host), err)
		}
		fmt.Fprintf(c.errOut(), "box: job %s on %s%s\n", pubID.ID, pubID.Box, onHost(c.host.Name))
		if c.app != nil {
			c.app.notify(jobNotice{Event: "starting_job", Box: pubID.Box, Job: pubID.ID, Host: c.host.Name})
		}
		r, err := c.follow(ctx, pubID, o.Wait, prepared.snap.source)
		return r, nil, err
	case http.StatusPaymentRequired, http.StatusConflict, http.StatusLocked:
		env := readCloudError(resp.StatusCode, body)
		return JobResult{State: statePaused, Code: exitPaused}, &turnAwayInfo{msg: env.Message}, nil
	case http.StatusBadRequest:
		env := readCloudError(resp.StatusCode, body)
		return JobResult{Code: 2}, nil, fmt.Errorf("%s: %s", hostLabel(c.host), env.Message)
	default:
		env := readCloudError(resp.StatusCode, body)
		if resp.StatusCode >= 500 {
			return JobResult{Job: pubID.ID, Box: pubID.Box, Host: c.host.Name, Code: 1, Source: prepared.snap.source, fleetHost: &hostCopy},
				nil, fmt.Errorf("%s: submission could not be confirmed; check box jobs (%s)", hostLabel(c.host), env.Message)
		}
		return JobResult{Code: 1}, nil, fmt.Errorf("%s: %s", hostLabel(c.host), env.Message)
	}
}

func (c *cloudHost) follow(ctx context.Context, pubID jobID, wait time.Duration, source *dash.SourceRevision) (JobResult, error) {
	hostCopy := c.host
	r := JobResult{
		Job:       pubID.ID,
		Box:       pubID.Box,
		Host:      c.host.Name,
		Code:      1,
		Source:    source,
		throwaway: true,
		fleetHost: &hostCopy,
	}

	followCtx := ctx
	var deadline time.Time
	if wait > 0 {
		var cancel context.CancelFunc
		followCtx, cancel = context.WithTimeout(ctx, wait)
		defer cancel()
		deadline = time.Now().Add(wait)
	}

	var logOffset int64
	out := c.out()
	drain := func(dctx context.Context) error {
		for {
			chunk, err := c.logChunk(dctx, pubID.Bare, logOffset, dash.MaxLogChunk)
			if err != nil {
				var se *cloudHTTPStatusError
				if errors.As(err, &se) && se.status == http.StatusNotFound {
					return nil
				}
				return err
			}
			raw, err := decodeLogChunkData(chunk)
			if err != nil {
				return err
			}
			if len(raw) > 0 {
				if _, err := out.Write(raw); err != nil {
					return err
				}
			}
			if chunk.Offset > logOffset {
				logOffset = chunk.Offset
			} else {
				logOffset += int64(len(raw))
			}
			if chunk.EOF || len(raw) == 0 {
				return nil
			}
		}
	}

	for followCtx.Err() == nil {
		waitSec := cloudFollowWaitSec
		if wait > 0 {
			rem := time.Until(deadline)
			if rem <= 0 {
				break
			}
			waitSec = min(cloudFollowWaitSec, max(1, int((rem+time.Second-1)/time.Second)))
		}
		startedCall := time.Now()
		j, err := c.job(followCtx, pubID.Bare, waitSec)
		if err != nil {
			if followCtx.Err() != nil {
				break
			}
			return r, err
		}

		switch j.State {
		case "done", "cancelled":
			drainCtx := ctx
			var cancelDrain context.CancelFunc
			if c.app != nil {
				drainCtx, cancelDrain = c.app.probeCtx(ctx)
			} else {
				drainCtx, cancelDrain = context.WithTimeout(context.WithoutCancel(ctx), probeTimeout)
			}
			if j.LogBytes > logOffset {
				if derr := drain(drainCtx); derr != nil && logOffset == 0 && j.Tail != "" {
					_, _ = io.WriteString(out, j.Tail)
				}
			} else if logOffset == 0 && j.Tail != "" {
				_, _ = io.WriteString(out, j.Tail)
			}
			cancelDrain()

			code := 0
			if j.Exit != nil {
				code = *j.Exit
			} else if j.State == "cancelled" {
				code = exitDetached
			}
			r.State, r.Code = stateDone, code
			return r, nil
		case "lost":
			if j.LogBytes > logOffset {
				drainCtx, cancelDrain := context.WithTimeout(context.WithoutCancel(ctx), probeTimeout)
				_ = drain(drainCtx)
				cancelDrain()
			} else if logOffset == 0 && j.Tail != "" {
				_, _ = io.WriteString(out, j.Tail)
			}
			r.State, r.Code = statePaused, exitPaused
			return r, fmt.Errorf("lost job %s (reattach with: box job %s)", pubID.ID, pubID.ID)
		}

		if j.LogBytes > logOffset {
			if err := drain(followCtx); err != nil && followCtx.Err() == nil {
				return r, err
			}
		}
		if time.Since(startedCall) < 50*time.Millisecond {
			poll := 50 * time.Millisecond
			if c.app != nil && c.app.WatchPoll > 0 {
				poll = c.app.WatchPoll
			}
			select {
			case <-followCtx.Done():
			case <-time.After(poll):
			}
		}
	}

	if ctx.Err() != nil {
		return r, ctx.Err()
	}
	fmt.Fprintf(c.errOut(), "box: job %s is still running; reattach with: box job %s\n", pubID.ID, pubID.ID)
	r.State, r.Code = stateDetached, exitDetached
	return r, nil
}

func (c *cloudHost) job(ctx context.Context, bareID string, waitSec int) (cloudJob, error) {
	reqPath := "/v1/jobs/" + url.PathEscape(bareID)
	if waitSec > 0 {
		waitSec = min(waitSec, cloudFollowWaitSec)
		reqPath += "?wait=" + strconv.Itoa(waitSec)
	}
	var j cloudJob
	err := c.doJSON(ctx, http.MethodGet, reqPath, nil, &j)
	var se *cloudHTTPStatusError
	if errors.As(err, &se) && se.status == http.StatusNotFound {
		return cloudJob{}, fmt.Errorf("no job %s", bareID)
	}
	return j, err
}

func (c *cloudHost) logChunk(ctx context.Context, bareID string, offset, limit int64) (dash.LogChunk, error) {
	reqPath := fmt.Sprintf("/v1/jobs/%s/log?offset=%d&limit=%d", url.PathEscape(bareID), offset, limit)
	var chunk dash.LogChunk
	err := c.doJSON(ctx, http.MethodGet, reqPath, nil, &chunk)
	return chunk, err
}

func decodeLogChunkData(chunk dash.LogChunk) ([]byte, error) {
	if chunk.DataBase64 != "" {
		return base64.StdEncoding.DecodeString(chunk.DataBase64)
	}
	return []byte(chunk.Data), nil
}

func (c *cloudHost) page(ctx context.Context, bareID string, offset, limit int64) (Page, error) {
	if !validJob.MatchString(bareID) {
		return Page{}, fmt.Errorf("bad job id %q", bareID)
	}
	if offset < 0 || limit < 0 {
		return Page{}, errors.New("offset and limit can't be negative")
	}
	if offset > math.MaxInt64-maxPage {
		return Page{}, errors.New("offset is too large")
	}
	if limit == 0 {
		limit = defaultPage
	}
	limit = min(limit, int64(dash.MaxLogChunk))

	j, err := c.job(ctx, bareID, 0)
	if err != nil {
		return Page{}, err
	}

	chunk, err := c.logChunk(ctx, bareID, offset, limit)
	if err != nil {
		var se *cloudHTTPStatusError
		if errors.As(err, &se) && se.status == http.StatusNotFound && j.State == "queued" {
			chunk = dash.LogChunk{Offset: offset, EOF: true}
		} else {
			return Page{}, err
		}
	}
	raw, err := decodeLogChunkData(chunk)
	if err != nil {
		return Page{}, err
	}
	text := string(raw)

	state := mapCloudJobState(j.State)
	started := jobStart(bareID)
	if j.StartedAt != nil && !j.StartedAt.IsZero() {
		started = j.StartedAt.UTC()
	}
	nextOffset := chunk.Offset
	if nextOffset < offset {
		nextOffset = offset + int64(len(text))
	}
	logBytes := max(chunk.LogBytes, j.LogBytes, nextOffset)
	more := !chunk.EOF || nextOffset < logBytes || state == jobRunning || state == jobStarting

	box, _, _ := strings.Cut(bareID, ".")
	return Page{
		JobInfo: JobInfo{
			Job:      bareID,
			Box:      box,
			State:    state,
			Code:     j.Exit,
			Started:  started,
			LogBytes: logBytes,
		},
		Offset:     offset,
		NextOffset: nextOffset,
		More:       more,
		Text:       text,
	}, nil
}

func mapCloudJobState(state string) string {
	switch state {
	case "done", "cancelled":
		return jobDone
	case "lost":
		return jobDied
	case "running":
		return jobRunning
	case "queued":
		return jobStarting
	default:
		return state
	}
}

func (c *cloudHost) list(ctx context.Context, limit int) ([]cloudJob, error) {
	if limit <= 0 {
		limit = 20
	}
	var out cloudJobList
	if err := c.doJSON(ctx, http.MethodGet, "/v1/jobs?limit="+strconv.Itoa(limit), nil, &out); err != nil {
		return nil, err
	}
	return out.Jobs, nil
}

func (c *cloudHost) cancel(ctx context.Context, bareID string) (cloudCancelResponse, error) {
	var out cloudCancelResponse
	err := c.doJSON(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(bareID)+"/cancel", nil, &out)
	var se *cloudHTTPStatusError
	if errors.As(err, &se) && se.status == http.StatusNotFound {
		return cloudCancelResponse{}, fmt.Errorf("no job %s", bareID)
	}
	return out, err
}

func (c *cloudHost) pull(ctx context.Context, bareID, relPath string) (io.ReadCloser, error) {
	reqPath := fmt.Sprintf("/v1/jobs/%s/file?path=%s", url.PathEscape(bareID), url.QueryEscape(relPath))
	resp, err := c.doRequest(ctx, http.MethodGet, reqPath, "", nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		return resp.Body, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, cloudMaxBodyBytes))
	env := readCloudError(resp.StatusCode, raw)
	return nil, fmt.Errorf("pull %s from %s: %s", relPath, bareID, env.Message)
}

func (c *cloudHost) pause(ctx context.Context, paused bool) (bool, error) {
	var out cloudPauseState
	if err := c.doJSON(ctx, http.MethodPost, "/v1/pause", cloudPauseState{Paused: paused}, &out); err != nil {
		return false, err
	}
	return out.Paused, nil
}

func (c *cloudHost) stop(ctx context.Context) (int, error) {
	var out cloudStopResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/stop", nil, &out); err != nil {
		return 0, err
	}
	return out.Stopped, nil
}
