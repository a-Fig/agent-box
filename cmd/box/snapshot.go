package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// repo is the local git worktree a run starts from.
type repo struct {
	Top  string // worktree root
	Name string // box-safe name: the origin repo's name, else the folder's
}

func git(ctx context.Context, r Runner, dir string, env []string, args ...string) (string, error) {
	res, err := r.Run(ctx, Cmd{Name: "git", Args: args, Dir: dir, Env: env})
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout), nil
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

// snapshot commits the worktree as it is, untracked files included and
// ignored files excluded, without touching the user's index, and writes a
// bundle of that commit to a temporary file. The commit is reachable in the
// bundle as refs/agent-box/<id>. The caller removes the bundle.
func snapshot(ctx context.Context, r Runner, top, id string) (bundle string, err error) {
	tmp, err := os.MkdirTemp("", "box-")
	if err != nil {
		return "", err
	}
	defer func() {
		os.Remove(filepath.Join(tmp, "index"))
		if err != nil {
			os.RemoveAll(tmp)
		}
	}()

	// Start from HEAD's tree, not a copy of the real index: a copy would keep
	// assume-unchanged and skip-worktree flags, and add -A would then skip
	// changed files. The price is rehashing the worktree (no stat cache).
	index := filepath.Join(tmp, "index")
	head, _ := git(ctx, r, top, nil, "rev-parse", "-q", "--verify", "HEAD^{commit}")
	env := []string{
		"GIT_INDEX_FILE=" + index,
		"GIT_AUTHOR_NAME=box", "GIT_AUTHOR_EMAIL=box@agent-box",
		"GIT_COMMITTER_NAME=box", "GIT_COMMITTER_EMAIL=box@agent-box",
	}
	if head != "" {
		if _, err := git(ctx, r, top, env, "read-tree", "--no-sparse-checkout", head); err != nil {
			return "", err
		}
	}
	if _, err := git(ctx, r, top, env, "add", "-A"); err != nil {
		return "", err
	}
	tree, err := git(ctx, r, top, env, "write-tree")
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", tree, "-m", "box snapshot " + id}
	// A shallow clone lacks the history below its cut, so a bundle of it
	// can't be fetched elsewhere: snapshot it without history.
	if shallow, _ := git(ctx, r, top, nil, "rev-parse", "--is-shallow-repository"); head != "" && shallow != "true" {
		args = append(args, "-p", head)
	}
	commit, err := git(ctx, r, top, env, args...)
	if err != nil {
		return "", err
	}

	// git bundle needs a ref; keep it only as long as the bundle takes.
	ref := "refs/agent-box/" + id
	if _, err := git(ctx, r, top, nil, "update-ref", ref, commit); err != nil {
		return "", err
	}
	defer git(context.WithoutCancel(ctx), r, top, nil, "update-ref", "-d", ref)
	bundle = filepath.Join(tmp, id+".bundle")
	if _, err := git(ctx, r, top, nil, "bundle", "create", "-q", bundle, ref); err != nil {
		return "", err
	}
	return bundle, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
