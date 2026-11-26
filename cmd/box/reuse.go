package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/jobrun"
	"github.com/a-Fig/agent-box/internal/mode"
)

// throwawayRepoKey scopes reuse to a credential-free origin and keeps the
// URL out of shared Incus config. Local paths do not identify a repo across
// machines and are left out of the pool.
func throwawayRepoKey(origin string, sz mode.Size) string {
	if origin == "" || sz.GPU {
		return ""
	}
	if parsed, err := url.Parse(origin); err != nil || parsed.Host == "" {
		host, repoPath, scp := strings.Cut(origin, ":")
		if !scp || len(host) < 2 || repoPath == "" || strings.ContainsAny(host, `/\`) {
			return ""
		}
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(origin)))
}

func (a *App) claimIdleThrowaway(ctx context.Context, insts []instance, repoKey string, sz mode.Size) string {
	if repoKey == "" {
		return ""
	}
	now := a.Now()
	body, _ := json.Marshal(dash.ClaimThrowawayBody{RepoKey: repoKey, Size: sz.Name})
	for _, in := range insts {
		if !validBox.MatchString(in.Name) || in.Config[jobrun.ThrowawayRepoKey] != repoKey ||
			mode.SizeOf(in.Config).Name != sz.Name || !keptThrowawayConfig(in, now) {
			continue
		}
		expires, _ := time.Parse(time.RFC3339, in.Config[expiresKey])
		if !expires.After(now) {
			continue
		}
		probe, cancel := context.WithTimeout(ctx, refusalTimeout)
		status, _, err := a.boxdDo(probe, http.MethodPost, "/api/boxes/"+in.Name+"/claim", "application/json", bytes.NewReader(body))
		cancel()
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
