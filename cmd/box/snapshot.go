package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/a-Fig/agent-box/internal/dash"
)

// repo is the local git worktree a run starts from.
type repo struct {
	Top  string // worktree root
	Name string // box-safe name: the origin repo's name, else the folder's
}

func git(ctx context.Context, r Runner, dir string, env []string, args ...string) (string, error) {
	out, err := runGit(ctx, r, Cmd{Args: args, Dir: dir, Env: env})
	return strings.TrimSpace(out), err
}

func runGit(ctx context.Context, r Runner, cmd Cmd) (string, error) {
	cmd.Name = "git"
	res, err := r.Run(ctx, cmd)
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		return "", fmt.Errorf("git %s: %s", cmd.Args[0], strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

func openRepo(ctx context.Context, r Runner, dir string) (repo, error) {
	top, err := git(ctx, r, dir, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return repo{}, err
	}
	base := filepath.Base(top)
	// Worktrees often have meaningless folder names; the origin URL names the repo.
	if url, err := git(ctx, r, top, nil, "remote", "get-url", "origin"); err == nil && url != "" {
		base = path.Base(strings.ReplaceAll(strings.TrimSuffix(url, "/"), ":", "/"))
	}
	return repo{Top: top, Name: boxName(strings.TrimSuffix(base, ".git"))}, nil
}

var nonName = regexp.MustCompile(`[^a-z0-9]+`)

// boxName turns s into a valid Incus instance name: lowercase letters,
// digits and hyphens, starting with a letter, at most 63 characters.
func boxName(s string) string {
	s = strings.Trim(nonName.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		s = "box-" + s
	}
	if len(s) > 50 { // leave room for a "-xxxx" throwaway suffix
		s = strings.TrimRight(s[:50], "-")
	}
	return strings.TrimSuffix(s, "-")
}

type worktreeSnapshot struct {
	bundle string
	source *dash.SourceRevision
}

// snapshot commits the worktree as it is, untracked files included and
// ignored files excluded, without touching the user's index, and writes a
// bundle of that commit to a temporary file. The commit is reachable in the
// bundle as refs/agent-box/<id>. The caller removes the bundle.
func snapshot(ctx context.Context, r Runner, top, id string) (_ worktreeSnapshot, err error) {
	tmp, err := os.MkdirTemp("", "box-")
	if err != nil {
		return worktreeSnapshot{}, err
	}
	defer func() {
		os.Remove(filepath.Join(tmp, "index"))
		if err != nil {
			os.RemoveAll(tmp)
		}
	}()

	// Copy the real index's entries so files tracked with git add -f stay
	// tracked. Rebuilding the index drops its flags and stale stat cache.
	index := filepath.Join(tmp, "index")
	entries, err := runGit(ctx, r, Cmd{Args: []string{"ls-files", "--stage", "-z"}, Dir: top})
	if err != nil {
		return worktreeSnapshot{}, err
	}
	head, _ := git(ctx, r, top, nil, "rev-parse", "-q", "--verify", "HEAD^{commit}")
	env := []string{
		"GIT_INDEX_FILE=" + index,
		"GIT_AUTHOR_NAME=box", "GIT_AUTHOR_EMAIL=box@agent-box",
		"GIT_COMMITTER_NAME=box", "GIT_COMMITTER_EMAIL=box@agent-box",
	}
	if entries != "" {
		if _, err := runGit(ctx, r, Cmd{Args: []string{"update-index", "-z", "--index-info"}, Dir: top, Env: env, Stdin: strings.NewReader(entries)}); err != nil {
			return worktreeSnapshot{}, err
		}
	}
	if _, err := git(ctx, r, top, env, "add", "-A"); err != nil {
		return worktreeSnapshot{}, err
	}
	tree, err := git(ctx, r, top, env, "write-tree")
	if err != nil {
		return worktreeSnapshot{}, err
	}
	args := []string{"commit-tree", tree, "-m", "box snapshot " + id}
	// A shallow clone lacks the history below its cut, so a bundle of it
	// can't be fetched elsewhere: snapshot it without history.
	if shallow, _ := git(ctx, r, top, nil, "rev-parse", "--is-shallow-repository"); head != "" && shallow != "true" {
		args = append(args, "-p", head)
	}
	commit, err := git(ctx, r, top, env, args...)
	if err != nil {
		return worktreeSnapshot{}, err
	}
	source := &dash.SourceRevision{Commit: head, SnapshotCommit: commit}
	if head != "" {
		if headTree, err := git(ctx, r, top, nil, "rev-parse", "--verify", head+"^{tree}"); err == nil {
			dirty := tree != headTree
			source.Dirty = &dirty
		}
	}

	// git bundle needs a ref; keep it only as long as the bundle takes.
	ref := "refs/agent-box/" + id
	if _, err := git(ctx, r, top, nil, "update-ref", ref, commit); err != nil {
		return worktreeSnapshot{}, err
	}
	defer git(context.WithoutCancel(ctx), r, top, nil, "update-ref", "-d", ref)
	bundle := filepath.Join(tmp, id+".bundle")
	if _, err := git(ctx, r, top, nil, "bundle", "create", "-q", bundle, ref); err != nil {
		return worktreeSnapshot{}, err
	}
	return worktreeSnapshot{bundle: bundle, source: source}, nil
}
