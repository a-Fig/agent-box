package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/jobrun"
	"github.com/a-Fig/agent-box/internal/mode"
)

// throwawayRepoKey scopes reuse to a credential-free origin and keeps the
// URL out of shared Incus config. SSH and local paths can resolve differently
// for different users even when display metadata shows the same origin.
func throwawayRepoKey(origin string, sz mode.Size) string {
	parsed, err := url.Parse(origin)
	if sz.GPU || err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(origin)))
}

func (a *App) claimIdleThrowaway(ctx context.Context, insts []instance, repoKey, ref string, sz mode.Size) string {
	if repoKey == "" {
		return ""
	}
	now := a.Now()
	for _, in := range insts {
		if !validBox.MatchString(in.Name) || in.Config[jobrun.ThrowawayRepoKey] != repoKey ||
			mode.SizeOf(in.Config).Name != sz.Name || !keptThrowawayConfig(in, now) {
			continue
		}
		expires, _ := time.Parse(time.RFC3339, in.Config[expiresKey])
		if !expires.After(now) {
			continue
		}
		req := dash.ClaimThrowawayBody{RepoKey: repoKey, Size: sz.Name, Job: in.Name + "." + ref}
		status, err := a.poolPost(ctx, in.Name, "claim", req)
		switch {
		case err == nil && status == http.StatusNoContent:
			return in.Name
		case err == nil && status == http.StatusConflict:
			continue // another run or a hold took the box
		default:
			return "" // older or unreachable boxd: launch as before
		}
	}
	return ""
}

func (a *App) poolEnabled(ctx context.Context) bool {
	probe, cancel := context.WithTimeout(ctx, refusalTimeout)
	defer cancel()
	status, _, err := a.boxdDo(probe, http.MethodGet, "/api/throwaway-pool", "", nil)
	return err == nil && status == http.StatusNoContent
}

func (a *App) poolPost(ctx context.Context, box, action string, req any) (int, error) {
	status, _, err := a.poolRequest(ctx, box, action, req)
	return status, err
}

func (a *App) poolRequest(ctx context.Context, box, action string, req any) (int, []byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return 0, nil, err
	}
	probe, cancel := context.WithTimeout(ctx, refusalTimeout)
	defer cancel()
	return a.boxdDo(probe, http.MethodPost, "/api/boxes/"+box+"/"+action, "application/json", bytes.NewReader(body))
}

func (a *App) evictPooled(ctx context.Context, in instance) error {
	return a.evictThroughBoxd(ctx, in, "evict-idle")
}

var errDirectEviction = errors.New("kept box eviction can use Incus directly")

func (a *App) evictKept(ctx context.Context, in instance) error {
	return a.evictThroughBoxd(ctx, in, "evict-kept")
}

func (a *App) evictThroughBoxd(ctx context.Context, in instance, action string) error {
	uid := in.Config["volatile.uuid"]
	if uid == "" {
		uid = in.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	status, body, err := a.poolRequest(ctx, in.Name, action, struct{ UID string }{uid})
	canUseIncus := action == "evict-kept" && in.Status == "Running" && in.Config[jobrun.ThrowawayStopKey] == ""
	if err != nil {
		if canUseIncus && ctx.Err() == nil {
			return errDirectEviction
		}
		return err
	}
	switch status {
	case http.StatusNoContent:
		return nil
	case http.StatusConflict:
		return errNotIdle
	case http.StatusForbidden:
		if canUseIncus {
			return errDirectEviction
		}
	case http.StatusNotFound:
		var legacy dash.ErrorBody
		if canUseIncus &&
			json.Unmarshal(body, &legacy) == nil && legacy.Code == dash.CodeNotFound && legacy.Error == "no such endpoint" {
			return errDirectEviction
		}
		return errNotIdle
	}
	return fmt.Errorf("boxd eviction returned HTTP %d", status)
}

// The CLI can use Incus even when boxd is unavailable. Its short reservation
// keeps cooling out, and a fresh read catches a stop that reserved first.
func (a *App) evictKeptDirect(ctx context.Context, in instance) error {
	ctx, cancel := context.WithTimeout(ctx, jobrun.EvictTTL/2)
	defer cancel()
	until := a.Now().Add(jobrun.EvictTTL).UTC()
	lease := until.Format(time.RFC3339Nano)
	err := jobrun.RetryETag(ctx, func() error {
		if !a.stillIdle(ctx, in, "") {
			return errNotIdle
		}
		_, err := a.incusOK(ctx, "config", "set", a.target(in.Name), jobrun.DirectEvictKey+"="+lease)
		return err
	})
	if err != nil {
		return err
	}
	return jobrun.RetryETag(ctx, func() error {
		if ctx.Err() != nil || !until.After(a.Now()) || !a.stillIdle(ctx, in, lease) {
			return errNotIdle
		}
		_, err := a.incusOK(ctx, "delete", a.target(in.Name), "--force")
		return err
	})
}

func (a *App) poolExpiry(ctx context.Context, box, job string, ended bool) {
	// boxd also observes the job files, so losing this hint is safe.
	a.poolPost(ctx, box, "pool-expiry", dash.PoolExpiryBody{Job: job, Ended: ended})
}
