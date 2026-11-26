package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/jobrun"
	"github.com/a-Fig/agent-box/internal/mode"
)

func TestRunClaimsMatchingThrowawayOrLaunches(t *testing.T) {
	const origin = "https://example.test/team/my-repo.git"
	for _, status := range []int{http.StatusNoContent, http.StatusConflict, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			dir := newRepo(t)
			gitT(t, dir, "remote", "add", "origin", origin)
			f := &fakePC{t: t, exit: "0", remotes: testRemotes}
			a, out := testApp(f)
			a.KeepEnded = jobrun.EndedKeep
			now := a.Now()
			sz := mustSize(t, "small")
			repoKey := throwawayRepoKey(origin, sz)
			f.insts = []instance{{Name: "my-repo-9999", Status: "Running", Config: map[string]string{
				jobrun.ThrowawayKey:     "true",
				jobrun.ThrowawayRepoKey: repoKey,
				jobrun.ExpiresKey:       now.Add(20 * time.Minute).Format(time.RFC3339),
				mode.SizeKey:            sz.Name,
			}}}
			claims := 0
			a.HTTP = func(r *http.Request) (*http.Response, error) {
				claims++
				var req dash.ClaimThrowawayBody
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RepoKey != repoKey || req.Size != sz.Name ||
					r.URL.Path != "/api/boxes/my-repo-9999/claim" || r.Method != http.MethodPost {
					t.Fatalf("claim request %s %s, body %+v, decode %v", r.Method, r.URL.Path, req, err)
				}
				if status == http.StatusNoContent {
					f.insts[0].Config[jobrun.ThrowawayClaimKey] = now.Add(jobrun.ThrowawayClaimTTL).Format(time.RFC3339)
				}
				return &http.Response{StatusCode: status, Body: http.NoBody}, nil
			}
			r, err := a.Run(context.Background(), RunOpts{Dir: dir, Cmd: []string{"true"}, New: true, Size: sz.Name})
			if err != nil || r.Code != 0 || claims != 1 {
				t.Fatalf("run = %+v, %v; %d claims\n%s", r, err, claims, out)
			}
			if status == http.StatusNoContent {
				if len(f.launches) != 0 || r.Box != "my-repo-9999" || f.insts[0].Config[jobrun.ThrowawayClaimKey] != "" ||
					!strings.Contains(out.String(), "reusing idle throwaway") {
					t.Fatalf("reuse result = %+v, launches = %d, config = %v\n%s", r, len(f.launches), f.insts[0].Config, out)
				}
			} else if len(f.launches) != 1 || r.Box == "my-repo-9999" {
				t.Fatalf("fallback result = %+v, launches = %d\n%s", r, len(f.launches), out)
			}
		})
	}
}
