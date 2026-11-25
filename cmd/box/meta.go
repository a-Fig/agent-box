package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/mode"
)

// A run's reason and who asked are recorded for the PC owner's dashboard
// (docs/ui.md, "Agent identity"). All of it is self-reported, for display
// only: nothing here decides anything.

const (
	reasonMin = 10
	reasonMax = 300
	// reasonKey holds box new's reason on the instance.
	reasonKey = "user.box.reason"
)

// checkReason trims a reason and checks its length in characters. what names
// the flag or argument in the message.
func checkReason(s string, required bool, what string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" && !required {
		return "", nil
	}
	help := fmt.Sprintf("give %s one or two sentences (%d to %d characters) on why this needs a box, like %q", what, reasonMin, reasonMax,
		"Full go test -race suite for the scheduler change; too heavy for the laptop.")
	if s == "" {
		return "", errors.New(what + " is required: " + help)
	}
	if n := utf8.RuneCountInString(s); n < reasonMin || n > reasonMax {
		return "", fmt.Errorf("%s is %d characters: %s", what, n, help)
	}
	return s, nil
}

// clientInfo is the MCP client that started box mcp, from initialize.
type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// agentInfo says who asked for a run.
type agentInfo struct {
	Client        string `json:"client"`
	ClientVersion string `json:"clientVersion"`
	Session       string `json:"session"`
	SessionSource string `json:"sessionSource"`
	Host          string `json:"host"`
	Worktree      string `json:"worktree"`
	// Display is what the dashboard calls the agent: the Murmur session
	// name, which box can't learn (see session), so the worktree's folder.
	Display string `json:"display"`
}

// jobMeta is /work/.box/<job>.meta, which boxd reads.
type jobMeta struct {
	V         int       `json:"v"`
	Reason    string    `json:"reason"`
	Command   string    `json:"command"`
	Repo      string    `json:"repo"`
	Branch    string    `json:"branch"`
	Commit    string    `json:"commit"`
	Agent     agentInfo `json:"agent"`
	Size      string    `json:"size"`
	GPU       bool      `json:"gpu"`
	EnvNames  []string  `json:"envNames"`
	StartedAt string    `json:"startedAt"`
}

// The bodies box posts to boxd are the dashboard's contract, internal/dash.
type refusal = dash.RefusalBody

const (
	refusedMyPC    = dash.RefusedMyPC
	refusedFull    = dash.RefusedFull
	refusedGPUBusy = dash.RefusedGPUBusy
)

func (a *App) getenv(k string) string {
	if a.Getenv != nil {
		return a.Getenv(k)
	}
	return os.Getenv(k)
}

// session names the agent's session, best first. Murmur has no variable for
// its session name (its names live in its daemon; read 2026-11-24), so it is
// skipped. Then:
//   - T3CODE_THREAD_ID: the T3 Code thread, set for Claude Code and for the
//     commands Codex runs. Codex starts MCP servers with a bare environment,
//     so for box mcp under Codex the thread comes from a tool call's
//     _meta.threadId instead, when T3 sends one (a.Thread).
//   - CLAUDE_CODE_SESSION_ID: Claude Code's own session.
//   - CODEX_THREAD_ID: Codex's thread, set for the commands it runs.
func (a *App) session() (id, source string) {
	if v := a.getenv("T3CODE_THREAD_ID"); v != "" {
		return v, "t3"
	}
	if a.Thread != "" {
		return a.Thread, "t3"
	}
	for _, s := range []struct{ env, source string }{
		{"CLAUDE_CODE_SESSION_ID", "claude-code"},
		{"CODEX_THREAD_ID", "codex"},
	} {
		if v := a.getenv(s.env); v != "" {
			return v, s.source
		}
	}
	return "", ""
}

func (a *App) agent(worktree string) agentInfo {
	ag := agentInfo{Client: a.Client.Name, ClientVersion: a.Client.Version, Worktree: filepath.FromSlash(worktree),
		Display: filepath.Base(filepath.FromSlash(worktree))}
	if ag.Client == "" {
		ag.Client, ag.ClientVersion = "cli", ""
	}
	ag.Session, ag.SessionSource = a.session()
	ag.Host, _ = os.Hostname()
	return ag
}

// runInfo is what a run records: its .meta, and the parts a refusal sends.
func (a *App) runInfo(ctx context.Context, rp repo, o RunOpts, sz mode.Size) jobMeta {
	origin, _ := git(ctx, a.R, rp.Top, nil, "remote", "get-url", "origin")
	branch, _ := git(ctx, a.R, rp.Top, nil, "symbolic-ref", "-q", "--short", "HEAD")
	commit, _ := git(ctx, a.R, rp.Top, nil, "rev-parse", "-q", "--verify", "HEAD^{commit}")
	names := []string{}
	for k := range o.Env {
		names = append(names, k)
	}
	sort.Strings(names)
	return jobMeta{V: 1, Reason: o.Reason, Command: displayCommand(o.Cmd), Repo: stripCredentials(origin),
		Branch: branch, Commit: commit, Agent: a.agent(rp.Top), Size: sz.Name, GPU: sz.GPU, EnvNames: names}
}

// stripCredentials drops the userinfo from a remote URL, where tokens go
// ("https://x-access-token:...@github.com/..."). An scp-style remote keeps
// only the user git, since that form can't carry a password but its user
// could still be a token.
func stripCredentials(remote string) string {
	if remote == "" {
		return ""
	}
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil {
			return ""
		}
		u.User, u.RawQuery, u.Fragment = nil, "", "" // a query can carry a token too
		return u.String()
	}
	if user, rest, ok := strings.Cut(remote, "@"); ok && user != "git" {
		return rest
	}
	return remote
}

var plainWord = regexp.MustCompile(`^[A-Za-z0-9_./=:@%+,-]+$`)

// displayCommand is the command as a person would type it: one argument as
// is, several joined with only the words that need it quoted.
func displayCommand(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	w := make([]string, len(args))
	for i, s := range args {
		w[i] = s
		if !plainWord.MatchString(s) {
			w[i] = shq(s)
		}
	}
	return strings.Join(w, " ")
}

// pushMeta writes the job's .meta in the box, next to its log.
func (a *App) pushMeta(ctx context.Context, box, remote, tmp string, m jobMeta) error {
	m.StartedAt = a.Now().UTC().Format(time.RFC3339)
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	local := filepath.Join(tmp, "job.meta")
	if err := os.WriteFile(local, b, 0o644); err != nil {
		return err
	}
	_, err = a.incusOK(ctx, "file", "push", "--create-dirs", local, a.target(box)+remote+".meta")
	return err
}

// refusalTimeout bounds posting a refusal, finding the host included.
const refusalTimeout = 2 * time.Second

// errCause is the refusal cause of err: no room or the GPU taken.
func errCause(err error) func(context.Context) string {
	return func(context.Context) string {
		var g *errGPUBusy
		switch {
		case errors.As(err, &g):
			return refusedGPUBusy
		case isNoRoom(err):
			return refusedFull
		}
		return ""
	}
}

// pausedCause is the refusal cause of paused, a reach or check result. Only
// My PC has a cause boxd counts. An unreachable PC or a frozen box in another
// mode sends nothing.
func (a *App) pausedCause(paused string) func(context.Context) string {
	return func(ctx context.Context) string {
		if !strings.HasSuffix(paused, " is in My PC mode") {
			return ""
		}
		return refusedMyPC
	}
}

// turnAway queues a run that can't start now when q.o.Queue allows and the
// cause is one the queue waits out (queueable); otherwise, or if boxd won't
// take it, it refuses the run as before: exit 75.
func (a *App) turnAway(ctx context.Context, q queueRun, msg string, cause func(context.Context) string) JobResult {
	if q.o.Queue {
		cctx, cancel := context.WithTimeout(ctx, refusalTimeout)
		why := cause(cctx)
		cancel()
		if queueable(why, q.sz) {
			r, ok, qmsg := a.enqueue(ctx, q)
			if ok {
				return r
			}
			msg += "; not queued: " + qmsg
		}
		cause = func(context.Context) string { return why }
	}
	a.refuse(ctx, msg, q.info, cause)
	return JobResult{State: statePaused, Code: exitPaused}
}

// refuse prints msg and tells boxd the cause, as a best effort: errors are
// dropped, so the agent only hears the refusal. cause runs only when there
// is somewhere to post; "" sends nothing.
func (a *App) refuse(ctx context.Context, msg string, m jobMeta, cause func(context.Context) string) {
	fmt.Fprintf(a.Err, "box: %s\n", msg)
	if a.Post == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, refusalTimeout)
	defer cancel()
	why := cause(ctx)
	if why == "" {
		return
	}
	host := a.pcHost(ctx)
	if host == "" {
		return
	}
	b, err := json.Marshal(refusal{Why: why, Reason: m.Reason, Size: m.Size, GPU: m.GPU, Repo: m.Repo, Agent: m.Agent.Display})
	if err != nil {
		return
	}
	a.Post(ctx, "https://"+host+"/api/refusals", b)
}

// tailWriter passes writes through and keeps the last dash.TailBytes, the tail boxd keeps.
type tailWriter struct {
	w   io.Writer
	buf []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > dash.TailBytes {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-dash.TailBytes:]...)
	}
	return t.w.Write(p)
}

// String is the kept tail from a whole character's start, without invalid bytes.
func (t *tailWriter) String() string {
	b := t.buf
	for len(b) > 0 && !utf8.RuneStart(b[0]) {
		b = b[1:]
	}
	return strings.ToValidUTF8(string(b), "") // dropped, not replaced, so it stays within TailBytes
}

// reportEnded tells boxd how a job ended, as a best effort. A throwaway box
// goes right after its exit code is read, usually before boxd's next look,
// so without this its job would be recorded as lost. Errors are dropped.
func (a *App) reportEnded(ctx context.Context, job string, code int, ended time.Time, tail string) {
	if a.Post == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refusalTimeout)
	defer cancel()
	host := a.pcHost(ctx)
	if host == "" {
		return
	}
	box, id, _ := strings.Cut(job, ".")
	b, err := json.Marshal(dash.JobEndedBody{Box: box, ID: id, Exit: code, EndedAt: ended.UTC().Truncate(time.Second), Tail: tail})
	if err != nil {
		return
	}
	a.Post(ctx, "https://"+host+"/api/jobs/ended", b)
}

// pcHost is the host of the Incus remote, where boxd also answers (behind
// tailscale serve, on 443), or "" if it can't be read (DashURL).
func (a *App) pcHost(ctx context.Context) string {
	u, err := a.DashURL(ctx)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(u, "https://"), "/")
}

// postJSON is App.Post outside tests.
func postJSON(ctx context.Context, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-Box", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("POST %s: %s", url, resp.Status)
	}
	return nil
}
