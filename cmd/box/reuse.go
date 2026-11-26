package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
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
	body, err := json.Marshal(req)
	if err != nil {
		return 0, err
	}
	probe, cancel := context.WithTimeout(ctx, refusalTimeout)
	defer cancel()
	status, _, err := a.boxdDo(probe, http.MethodPost, "/api/boxes/"+box+"/"+action, "application/json", bytes.NewReader(body))
	return status, err
}

func (a *App) evictPooled(ctx context.Context, in instance) error {
	uid := in.Config["volatile.uuid"]
	if uid == "" {
		uid = in.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	status, err := a.poolPost(ctx, in.Name, "evict-idle", struct{ UID string }{uid})
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent:
		return nil
	case http.StatusConflict:
		return errNotIdle
	default:
		return fmt.Errorf("boxd eviction returned HTTP %d", status)
	}
}

func (a *App) poolExpiry(ctx context.Context, box, job string, ended bool) {
	// boxd also observes the job files, so losing this hint is safe.
	a.poolPost(ctx, box, "pool-expiry", dash.PoolExpiryBody{Job: job, Ended: ended})
}
