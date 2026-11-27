package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestFollowMetadataRequiresFullGitObjectIDs(t *testing.T) {
	head := strings.Repeat("a", 40)
	snap := strings.Repeat("b", 40)
	for _, tc := range []struct {
		name, meta string
		want       bool
	}{
		{"legacy", `{"commit":"` + head + `"}`, false},
		{"valid SHA-1", `{"source":{"commit":"` + head + `","dirty":false,"snapshotCommit":"` + snap + `"}}`, true},
		{"valid SHA-256", `{"source":{"commit":"` + strings.Repeat("a", 64) + `","dirty":true,"snapshotCommit":"` + strings.Repeat("b", 64) + `"}}`, true},
		{"unknown HEAD", `{"source":{"commit":"","dirty":null,"snapshotCommit":"` + snap + `"}}`, true},
		{"unproven dirty state", `{"source":{"commit":"","dirty":false,"snapshotCommit":"` + snap + `"}}`, false},
		{"arbitrary source", `{"source":{"commit":"` + strings.Repeat("z", 40) + `","dirty":false,"snapshotCommit":"` + snap + `"}}`, false},
		{"short snapshot", `{"source":{"commit":"` + head + `","dirty":false,"snapshotCommit":"1234567"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ef := &exitFilter{meta: base64.StdEncoding.EncodeToString([]byte(tc.meta))}
			if source := ef.source(); (source != nil) != tc.want {
				t.Fatalf("marker metadata decoded to %+v", source)
			}
		})
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
			checkResult := func(text string, result map[string]any) {
				t.Helper()
				src, ok := result["source"].(map[string]any)
				if !ok || src["commit"] != head || src["dirty"] != true || src["snapshotCommit"] != synced ||
					!strings.Contains(text, sourceLine(meta.Source)) {
					t.Fatalf("queued result lost its saved source: %v\n%s", result, text)
				}
			}
			checkResult(text, st)
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
			b.views = []dash.Job{{State: dash.JobQueued, Position: 1, Source: meta.Source}}
			text, st = e.call(t, "box_job", map[string]any{"id": queuedJobName})
			checkResult(text, st)
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
			if e.push.count() != 1 || !strings.Contains(e.push.sent[0].msg, sourceLine(meta.Source)) {
				t.Fatalf("completion notice lost its saved source: %v", e.push.sent)
			}
			text, st = e.call(t, "box_job", map[string]any{"id": queuedJobName, "limit": 1})
			checkResult(text, st)
		})
	}
}

func TestPagedJobSourceKeepsOffsetsAndLogBytes(t *testing.T) {
	bash := gitBash(t)
	old := workRoot
	workRoot = filepath.ToSlash(t.TempDir())
	defer func() { workRoot = old }()
	const job = "fixture.20261123-120000-0001"
	file := filepath.Join(workRoot, ".box", job)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, file+".log", "ABC\n")
	write(t, file+".exit", "0\n")

	dirty := false
	src := &dash.SourceRevision{Commit: strings.Repeat("a", 40), Dirty: &dirty, SnapshotCommit: strings.Repeat("b", 40)}
	meta, err := json.Marshal(jobMeta{Source: src})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakePC{t: t, bash: bash}
	a, _ := testApp(f)
	var out, stderr bytes.Buffer
	a.Out, a.Err = &out, &stderr
	e := pushEnv{a: a}
	for _, known := range []bool{false, true} {
		want := "source unknown"
		if known {
			write(t, file+".meta", string(meta))
			want = sourceLine(src)
		}
		text, st := e.call(t, "box_job", map[string]any{"id": job, "offset": 1, "limit": 2})
		if st["log"] != "BC" || st["nextOffset"] != 3.0 || st["logBytes"] != 4.0 ||
			(st["source"] != nil) != known || !strings.Contains(text, "\n"+want+"\nBC") {
			t.Fatalf("MCP page = %v\n%s", st, text)
		}
		out.Reset()
		stderr.Reset()
		if code := run(context.Background(), a, []string{"job", job, "--offset", "1", "--limit", "2"}, nil); code != 0 ||
			out.String() != "BC" || !strings.Contains(stderr.String(), "box: "+want+"\n") {
			t.Fatalf("CLI page = %d, stdout %q, stderr %q", code, &out, &stderr)
		}
	}
}
