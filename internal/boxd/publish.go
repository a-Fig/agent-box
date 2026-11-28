package boxd

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/a-Fig/agent-box/internal/controller"
	"github.com/a-Fig/agent-box/internal/dash"
)

const (
	defaultPublishMinGap    = 15 * time.Second
	defaultPublishHeartbeat = 60 * time.Second
	defaultPublishBackoff   = 60 * time.Second
	defaultPublishMaxBack   = 5 * time.Minute
)

// Publisher subscribes to a Watcher and pushes coalesced, trimmed state
// snapshots to a fleet controller (docs/controller-protocol.md).
type Publisher struct {
	Watcher     *Watcher
	Client      *controller.Client
	HostID      string
	HostName    string
	Now         func() time.Time
	MinGap      time.Duration
	Heartbeat   time.Duration
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	Logf        func(format string, args ...any)

	seq int64
}

// NewPublisherFromConfig builds a Publisher when cfg.Controller is valid
// and its token_file can be read. Unconfigured controllers return nil silently;
// invalid controller settings or missing/unreadable token files log a warning
// and return nil so boxd never fails to boot.
func NewPublisherFromConfig(cfg Config, w *Watcher, logf func(string, ...any)) *Publisher {
	if cfg.Controller == nil || cfg.Controller.URL == "" {
		return nil
	}
	log := func(format string, args ...any) {
		if logf != nil {
			logf(format, args...)
		}
	}
	if !controller.ValidControllerURL(cfg.Controller.URL) {
		log("boxd: WARNING: controller.url must be https:// or loopback http://; publisher disabled")
		return nil
	}
	hostID := strings.TrimSpace(cfg.Controller.ID)
	if hostID == "" {
		hostID = controller.SanitizeHostID(cfg.MachineID)
	} else if !controller.ValidHostID(hostID) {
		log("boxd: WARNING: controller.id %q must match ^[a-z0-9-]{1,40}$; publisher disabled", hostID)
		return nil
	}
	if cfg.Controller.TokenFile == "" {
		log("boxd: WARNING: controller configured without token_file; publisher disabled")
		return nil
	}
	raw, err := os.ReadFile(cfg.Controller.TokenFile)
	if err != nil {
		log("boxd: WARNING: controller token_file %s: %v; publisher disabled", cfg.Controller.TokenFile, err)
		return nil
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		log("boxd: WARNING: controller token_file %s is empty; publisher disabled", cfg.Controller.TokenFile)
		return nil
	}
	return &Publisher{
		Watcher:  w,
		Client:   &controller.Client{URL: cfg.Controller.URL, Token: token},
		HostID:   hostID,
		HostName: cfg.MachineName,
		Now:      time.Now,
		Logf:     logf,
	}
}

func (p *Publisher) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

func (p *Publisher) minGap() time.Duration {
	if p.MinGap > 0 {
		return p.MinGap
	}
	return defaultPublishMinGap
}

func (p *Publisher) heartbeat() time.Duration {
	if p.Heartbeat > 0 {
		return p.Heartbeat
	}
	return defaultPublishHeartbeat
}

func (p *Publisher) baseBackoff() time.Duration {
	if p.BaseBackoff > 0 {
		return p.BaseBackoff
	}
	return defaultPublishBackoff
}

func (p *Publisher) maxBackoff() time.Duration {
	if p.MaxBackoff > 0 {
		return p.MaxBackoff
	}
	return defaultPublishMaxBack
}

func (p *Publisher) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
	}
}

// Run subscribes to Watcher and publishes snapshots until ctx is cancelled.
func (p *Publisher) Run(ctx context.Context) {
	if p.Watcher == nil || p.Client == nil {
		return
	}
	_, ch, cancel := p.Watcher.Subscribe()
	defer func() { cancel() }()

	dirty := true
	var lastAttempt, lastSuccess, backoffUntil time.Time
	var failures int
	var lastErr string

	for {
		if ctx.Err() != nil {
			return
		}
		var closed bool
		dirty, closed = drainWatcherEvents(ch, dirty)
		if closed {
			cancel()
			_, ch, cancel = p.Watcher.Subscribe()
			dirty = true
		}

		now := p.now()
		due := p.nextDue(dirty, lastAttempt, lastSuccess, backoffUntil)
		if !now.Before(due) {
			err := p.pushOnce(ctx, now)
			doneAt := p.now()
			lastAttempt = doneAt
			if err == nil {
				if failures > 0 {
					p.logf("boxd: controller push recovered after %d failure(s)", failures)
				}
				failures = 0
				lastErr = ""
				backoffUntil = time.Time{}
				lastSuccess = doneAt
				dirty = false
			} else {
				if ctx.Err() != nil {
					return
				}
				failures++
				dirty = true
				delay := backoffDelay(p.baseBackoff(), p.maxBackoff(), failures)
				backoffUntil = doneAt.Add(delay)
				msg := err.Error()
				if failures == 1 || msg != lastErr {
					p.logf("boxd: controller push failed (retry in %s): %v", delay, err)
					lastErr = msg
				}
			}
			continue
		}

		timer := time.NewTimer(due.Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case _, ok := <-ch:
			timer.Stop()
			if !ok {
				cancel()
				_, ch, cancel = p.Watcher.Subscribe()
			}
			dirty = true
		case <-timer.C:
		}
	}
}

func (p *Publisher) nextDue(dirty bool, lastAttempt, lastSuccess, backoffUntil time.Time) time.Time {
	var due time.Time
	if dirty {
		if !lastSuccess.IsZero() {
			due = lastSuccess.Add(p.minGap())
		}
	} else {
		if !lastAttempt.IsZero() {
			due = lastAttempt.Add(p.heartbeat())
		}
	}
	if backoffUntil.After(due) {
		due = backoffUntil
	}
	return due
}

func (p *Publisher) pushOnce(ctx context.Context, now time.Time) error {
	p.seq++
	req, ok := fitPutStateRequest(p.HostID, p.HostName, p.seq, now, controller.TrimState(p.Watcher.State()))
	if !ok {
		p.logf("boxd: controller state exceeded %d bytes after trimming; skipping seq %d", controller.MaxBodyBytes, p.seq)
		return nil
	}
	return p.Client.PutState(ctx, p.HostID, req)
}

func fitPutStateRequest(id, name string, seq int64, now time.Time, st dash.State) (controller.PutStateRequest, bool) {
	req := controller.PutStateRequest{
		ID:     id,
		Name:   name,
		Seq:    seq,
		SentAt: now.UTC(),
		State:  st,
	}
	if fitsBodyCap(req) {
		return req, true
	}

	// Step 1: drop finished jobs while keeping active jobs.
	active := make([]dash.Job, 0, len(req.State.Jobs))
	for _, j := range req.State.Jobs {
		if j.State != dash.JobDone && j.State != dash.JobLost {
			active = append(active, j)
		}
	}
	req.State.Jobs = active
	if fitsBodyCap(req) {
		return req, true
	}

	// Step 2: strip job tails.
	for i := range req.State.Jobs {
		req.State.Jobs[i].Tail = ""
	}
	if fitsBodyCap(req) {
		return req, true
	}

	// Step 3: drop excess jobs until within MaxBodyBytes.
	for len(req.State.Jobs) > 0 {
		req.State.Jobs = req.State.Jobs[:len(req.State.Jobs)/2]
		if fitsBodyCap(req) {
			return req, true
		}
	}
	return req, fitsBodyCap(req)
}

func fitsBodyCap(req controller.PutStateRequest) bool {
	b, err := json.Marshal(req)
	return err == nil && len(b) <= controller.MaxBodyBytes
}

func backoffDelay(base, maxDelay time.Duration, failures int) time.Duration {
	d := base
	for i := 1; i < failures; i++ {
		if d >= maxDelay/2 {
			return maxDelay
		}
		d *= 2
	}
	if d > maxDelay {
		return maxDelay
	}
	return d
}

func drainWatcherEvents(ch <-chan dash.Event, dirty bool) (newDirty, closed bool) {
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return true, true
			}
			dirty = true
		default:
			return dirty, false
		}
	}
}
