package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/a-Fig/agent-box/internal/dash"
)

func requireSourceRevision(t *testing.T, got *dash.SourceRevision, head string, dirty bool, snapshot string) {
	t.Helper()
	if got == nil || got.Commit != head || got.SnapshotCommit != snapshot || got.Dirty == nil || *got.Dirty != dirty {
		t.Fatalf("source = %+v, want commit %s, dirty %t, snapshot %s", got, head, dirty, snapshot)
	}
}

func TestSnapshotReportsWhetherTheBundledTreeDiffers(t *testing.T) {
	dir := newRepo(t)
	// A staged change that git add -A replaces with HEAD's bytes is not in
	// the snapshot. The remaining ignored file is not in it either.
	gitT(t, dir, "add", "a.txt")
	write(t, filepath.Join(dir, "a.txt"), "one\n")
	if err := os.Remove(filepath.Join(dir, "untracked.txt")); err != nil {
		t.Fatal(err)
	}
	const ref = "clean-tree"
	snap, err := snapshot(context.Background(), execRunner{}, dir, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(snap.bundle))
	clone := fetchSnapshot(t, snap.bundle, ref)
	requireSourceRevision(t, snap.source, gitT(t, dir, "rev-parse", "HEAD"), false, gitT(t, clone, "rev-parse", "FETCH_HEAD"))
	if got, want := gitT(t, clone, "rev-parse", "FETCH_HEAD^{tree}"), gitT(t, dir, "rev-parse", "HEAD^{tree}"); got != want {
		t.Fatalf("bundled tree = %s, want %s", got, want)
	}
}

func TestSnapshotIncludesUntrackedChangesInDirtyState(t *testing.T) {
	dir := newRepo(t)
	write(t, filepath.Join(dir, "a.txt"), "one\n")
	const ref = "untracked-tree"
	snap, err := snapshot(context.Background(), execRunner{}, dir, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(snap.bundle))
	clone := fetchSnapshot(t, snap.bundle, ref)
	requireSourceRevision(t, snap.source, gitT(t, dir, "rev-parse", "HEAD"), true, gitT(t, clone, "rev-parse", "FETCH_HEAD"))
	if got := gitT(t, clone, "show", "FETCH_HEAD:untracked.txt"); got != "new" {
		t.Fatalf("bundled untracked file = %q", got)
	}
}

func TestQueuedBundleKeepsSourceWhenHeadAdvances(t *testing.T) {
	for _, mode := range []string{"build at enqueue", "reuse the prebuilt bundle"} {
		t.Run(mode, func(t *testing.T) {
			f := &fakePC{refuse: memoryRefusal}
			if mode == "build at enqueue" {
				f.mode = "mypc"
			}
			e, b := newQueueEnv(t, f)
			if f.mode == "mypc" {
				b.reply.Why = dash.RefusedMyPC
			}
			head := gitT(t, e.repo, "rev-parse", "HEAD")
			base := e.a.R
			bundles := 0
			e.a.R = lookupRunner(func(ctx context.Context, cmd Cmd) (Result, error) {
				res, err := base.Run(ctx, cmd)
				if cmd.Name == "git" && len(cmd.Args) > 1 && cmd.Args[0] == "bundle" && cmd.Args[1] == "create" && err == nil && res.Code == 0 {
					bundles++
					// Commit a fix after Git wrote the bundle but before Box
					// uploads it or copies its revision into the watcher.
					write(t, filepath.Join(e.repo, "a.txt"), "fixed\n")
					gitT(t, e.repo, "commit", "-qam", "fix after snapshot")
				}
				return res, err
			})
			text, st := e.call(t, "box_run", e.runArgs(map[string]any{"new": true}, "true"))
			if st["state"] != stateQueued || bundles != 1 || gitT(t, e.repo, "rev-parse", "HEAD") == head {
				t.Fatalf("queue = %v, bundle builds %d\n%s", st, bundles, text)
			}
			var meta jobMeta
			if err := json.Unmarshal(b.req.Meta, &meta); err != nil || meta.Commit != head {
				t.Fatalf("queued metadata = %s, %v", b.req.Meta, err)
			}
			bundle := filepath.Join(t.TempDir(), "queued.bundle")
			if err := os.WriteFile(bundle, b.bundle, 0o600); err != nil {
				t.Fatal(err)
			}
			clone := fetchSnapshot(t, bundle, b.req.Ref)
			synced := gitT(t, clone, "rev-parse", "FETCH_HEAD")
			requireSourceRevision(t, meta.Source, head, true, synced)
			if got := gitT(t, clone, "show", "FETCH_HEAD:a.txt"); got != "two" {
				t.Fatalf("queued tracked contents = %q", got)
			}
			if got := gitT(t, clone, "show", "FETCH_HEAD:untracked.txt"); got != "new" {
				t.Fatalf("queued untracked contents = %q", got)
			}
			rec, err := e.a.readWatchRecord(queuedJobName)
			if err != nil {
				t.Fatal(err)
			}
			requireSourceRevision(t, rec.Source, head, true, synced)
			exit := 7
			b.views = []dash.Job{{State: dash.JobDone, Exit: &exit, Source: meta.Source}}
			if err := e.a.Watch(context.Background(), queuedJobName); err != nil {
				t.Fatal(err)
			}
			end, ok := e.a.recordedEnd(queuedJobName)
			if !ok || end.Code != exit {
				t.Fatalf("retained queued end = %+v, present %t", end, ok)
			}
			requireSourceRevision(t, end.Source, head, true, synced)
		})
	}
}
