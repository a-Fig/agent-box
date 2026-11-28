package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/a-Fig/agent-box/internal/jobrun"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
)

// Job states as box jobs reports them.
const (
	jobRunning  = "running"
	jobDone     = "done"
	jobDied     = "died"     // the wrapper is gone and left no exit code
	jobStarting = "starting" // the log exists but the wrapper hasn't published its PID yet
)

// JobInfo is one job in a box, from its files in /work/.box.
type JobInfo struct {
	Job      string    `json:"job"`
	Box      string    `json:"box"`
	Host     string    `json:"host,omitempty"`
	State    string    `json:"state"`
	Code     *int      `json:"exitCode,omitempty"` // when done
	Started  time.Time `json:"started"`
	LogBytes int64     `json:"logBytes"`
}

// jobStateFn defines jobstate JOB, run in /work/.box: it prints the job's
// state and exit code ("-" unless done). A log with no PID file for over a
// minute means the start failed.
const jobStateFn = `jobstate() {
  if [ -f "$1.exit" ]; then echo "done $(cat "$1.exit")"
  elif alive "$(cat "$1.pid" 2>/dev/null)" "$1"; then echo "running -"
  elif [ -f "$1.pid" ] || [ -n "$(find "$1.log" -mmin +1 2>/dev/null)" ]; then echo "died -"
  else echo "starting -"
  fi
}
`

// jobsScript prints "JOB STATE CODE LOGBYTES" for every job in a box.
func jobsScript() string {
	return "# jobs\n" + aliveFn + jobStateFn + fmt.Sprintf(`cd %s/.box 2>/dev/null || exit 0
for l in *.log; do
  [ -e "$l" ] || continue
  j=${l%%.log}
  echo "$j $(jobstate "$j") $(( $(wc -c < "$l") ))"
done
exit 0
`, workRoot)
}

// pageScript prints "STATE CODE LOGBYTES" and optional base64 metadata on
// the first line, then limit bytes of the log from offset.
func pageScript(job string, offset, limit int64) string {
	return "# page\n" + aliveFn + jobStateFn + fmt.Sprintf(`cd %s/.box 2>/dev/null || exit 3
test -f %[2]s.log || exit 3
printf '%%s %%s' "$(jobstate %[2]s)" "$(( $(wc -c < %[2]s.log) ))"
if [ -f %[2]s.meta ]; then
  printf '\t'
  timeout -k 1 1 head -c %[5]d %[2]s.meta 2>/dev/null | base64 -w0
fi
printf '\n'
tail -c +%[3]d %[2]s.log | head -c %[4]d
`, workRoot, job, offset+1, limit, dash.MaxJobMetaBytes)
}

// cancelScript stops a job's session and writes 130 to its .exit if the
// wrapper didn't (jobrun.CancelScript). It marks the job .cancelled: the agent
// asked, so its watcher stays quiet.
func cancelScript(job string) string { return jobrun.CancelScript(workRoot, job, exitDetached, "") }

// jobStart reads the start time from a job ID (box.YYYYMMDD-HHMMSS-xxxx, UTC).
func jobStart(job string) time.Time {
	_, id, _ := strings.Cut(job, ".")
	if len(id) < 15 {
		return time.Time{}
	}
	t, _ := time.Parse("20060102-150405", id[:15])
	return t
}

func parseJobs(box, out string) []JobInfo {
	var jobs []JobInfo
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) != 4 || !validJob.MatchString(f[0]) {
			continue
		}
		j := JobInfo{Job: f[0], Box: box, State: f[1], Started: jobStart(f[0])}
		if c, err := strconv.Atoi(f[2]); err == nil {
			j.Code = &c
		}
		j.LogBytes, _ = strconv.ParseInt(f[3], 10, 64)
		jobs = append(jobs, j)
	}
	return jobs
}

func (a *App) boxJobs(ctx context.Context, box string) ([]JobInfo, error) {
	out, err := a.incusOK(ctx, "exec", a.target(box), "--", "bash", "-c", jobsScript())
	if err != nil {
		return nil, err
	}
	return parseJobs(box, out), nil
}

// Jobs lists the jobs in one box, or in every running box when box is "",
// newest first. Boxes that aren't running are skipped with a warning.
func (a *App) Jobs(ctx context.Context, box string) ([]JobInfo, error) {
	if box != "" && !validBox.MatchString(box) {
		return nil, fmt.Errorf("bad box name %q", box)
	}
	insts, err := a.list(ctx)
	if err != nil {
		return nil, err
	}
	if box != "" {
		inst := find(insts, box)
		if inst == nil {
			return nil, fmt.Errorf("no box %s", box)
		}
		insts = []instance{*inst}
	}
	var jobs []JobInfo
	for _, in := range insts {
		if in.Status != "Running" {
			a.warn(fmt.Sprintf("box: %s is %s; its jobs aren't listed", in.Name, in.Status))
			continue
		}
		js, err := a.boxJobs(ctx, in.Name)
		if err != nil {
			if box != "" {
				return nil, err
			}
			a.warn(fmt.Sprintf("box: listing jobs in %s: %v", in.Name, err))
			continue
		}
		jobs = append(jobs, js...)
	}
	sort.SliceStable(jobs, func(i, j int) bool {
		_, x, _ := strings.Cut(jobs[i].Job, ".")
		_, y, _ := strings.Cut(jobs[j].Job, ".")
		return x > y
	})
	return jobs, nil
}

func (j JobInfo) stateText() string {
	if j.Code != nil {
		return fmt.Sprintf("%s (exit %d)", j.State, *j.Code)
	}
	return j.State
}

func printJobs(a *App, jobs []JobInfo) error {
	if len(jobs) == 0 {
		fmt.Fprintln(a.Out, "no jobs")
		return nil
	}
	w := tabwriter.NewWriter(a.Out, 0, 4, 2, ' ', 0)
	fleet := jobs[0].Host != ""
	if fleet {
		fmt.Fprint(w, "HOST\t")
	}
	fmt.Fprintln(w, "JOB\tSTATE\tSTARTED\tLOG")
	for _, j := range jobs {
		if fleet {
			fmt.Fprintf(w, "%s\t", j.Host)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%dB\n", j.Job, j.stateText(), j.Started.Format(time.RFC3339), j.LogBytes)
	}
	return w.Flush()
}

// Page is a piece of a job's log.
type Page struct {
	JobInfo
	Source     *dash.SourceRevision `json:"source"`
	Offset     int64                `json:"offset"`
	NextOffset int64                `json:"nextOffset"`
	More       bool                 `json:"more"` // the log goes on past this page, or the job is still writing it
	Text       string               `json:"-"`
}

const (
	defaultPage = 32 << 10
	maxPage     = 1 << 20
)

// LogPage reads limit bytes of a job's log from offset, without waiting.
func (a *App) LogPage(ctx context.Context, job string, offset, limit int64) (Page, error) {
	if !validJob.MatchString(job) {
		return Page{}, fmt.Errorf("bad job id %q", job)
	}
	if offset < 0 || limit < 0 {
		return Page{}, errors.New("offset and limit can't be negative")
	}
	if offset > math.MaxInt64-maxPage {
		return Page{}, errors.New("offset is too large")
	}
	if limit == 0 {
		limit = defaultPage
	}
	limit = min(limit, maxPage)
	box, _, _ := strings.Cut(job, ".")
	res, err := a.incus(ctx, Cmd{Args: []string{"exec", a.target(box), "--", "bash", "-c", pageScript(job, offset, limit)}})
	if err != nil {
		return Page{}, err
	}
	if res.Code == 3 {
		return Page{}, fmt.Errorf("no job %s", job)
	}
	if res.Code != 0 {
		return Page{}, fmt.Errorf("reading job %s: %s", job, strings.TrimSpace(res.Stderr))
	}
	head, text, _ := strings.Cut(res.Stdout, "\n")
	head, meta, _ := strings.Cut(head, "\t")
	jobs := parseJobs(box, job+" "+head)
	if len(jobs) != 1 {
		return Page{}, fmt.Errorf("reading job %s: bad state %q", job, head)
	}
	p := Page{JobInfo: jobs[0], Source: decodeSource(meta), Offset: offset, Text: text}
	p.NextOffset = offset + int64(len(text))
	if len(text) != 0 {
		p.LogBytes = max(p.LogBytes, p.NextOffset) // the log grew while it was read
	}
	p.More = p.NextOffset < p.LogBytes || p.State == jobRunning || p.State == jobStarting
	return p, nil
}

// pageHead is the one-line summary of a log page.
func (p Page) pageHead() string {
	location := p.Box
	if p.Host != "" {
		location += " on " + p.Host
	}
	s := fmt.Sprintf("job %s on box %s: %s, log bytes %d-%d of %d", p.Job, location, p.stateText(), p.Offset, p.NextOffset, p.LogBytes)
	if p.More {
		s += fmt.Sprintf("; next offset %d", p.NextOffset)
	}
	return s
}

// Cancel stops a running job. It reports the job's exit code: 130 when box
// stopped it, or the code it had already finished with.
func (a *App) Cancel(ctx context.Context, job string) (cancelled bool, code int, err error) {
	if !validJob.MatchString(job) {
		return false, 0, fmt.Errorf("bad job id %q", job)
	}
	box, _, _ := strings.Cut(job, ".")
	return a.cancelJob(ctx, jobID{ID: job, Bare: job, Box: box})
}

func (a *App) cancelJob(ctx context.Context, id jobID) (cancelled bool, code int, err error) {
	job, box := id.ID, id.Box
	res, err := a.incus(ctx, Cmd{Args: []string{"exec", a.target(box), "--", "bash", "-c", cancelScript(id.Bare)}})
	if err != nil {
		return false, 0, err
	}
	switch res.Code {
	case 0:
	case 3:
		return false, 0, fmt.Errorf("no job %s", job)
	default:
		return false, 0, fmt.Errorf("cancel %s: %s", job, strings.TrimSpace(res.Stderr))
	}
	f := strings.Fields(res.Stdout)
	if len(f) != 2 || (f[0] != "cancelled" && f[0] != "finished") {
		return false, 0, fmt.Errorf("cancel %s: unexpected output %q", job, res.Stdout)
	}
	code, err = strconv.Atoi(f[1])
	if err != nil {
		return false, 0, fmt.Errorf("cancel %s: bad exit code %q", job, f[1])
	}
	if f[0] == "cancelled" {
		a.markIdle(ctx, box, id.Bare)
		a.markNotified(job, "cancel") // the agent cancelled it: no push of its end
	}
	return f[0] == "cancelled", code, nil
}

// wasCancelled says whether box cancel stopped the job (its .cancelled mark
// in the box). Any failure to look says no: a duplicate notice beats a lost one.
func (a *App) wasCancelled(ctx context.Context, job string) bool {
	box, _, _ := strings.Cut(job, ".")
	script := fmt.Sprintf("# cancelled\ntest -f %s/.box/%s.cancelled && echo yes\nexit 0\n", workRoot, job)
	res, err := a.incus(ctx, Cmd{Args: []string{"exec", a.target(box), "--", "bash", "-c", script}})
	return err == nil && strings.TrimSpace(res.Stdout) == "yes"
}

// checkoutOwnerCheck is the shell fragment that checks /work/.box/$r.checkout
// against job: if another job owns /work/$r, it explains so on stderr and exits 4.
func checkoutOwnerCheck(job string) string {
	return fmt.Sprintf(`cur=$(cat %[1]s/.box/"$r".checkout 2>/dev/null)
if [ -n "$cur" ] && [ "$cur" != %[2]s ]; then
  if [ -f %[1]s/.box/%[2]s.exit ] || [ -f %[1]s/.box/"$cur".exit ]; then
    echo "job %[3]s's checkout %[1]s/$r was replaced by job $cur; pull right after a job, or run with --fresh (fresh: true)" >&2
  else
    echo "checkout %[1]s/$r is in use by job $cur, not %[3]s yet" >&2
  fi
  exit 4
fi
`, workRoot, jobrun.Shq(job), job)
}

// checkoutScript prints the checkout a relative path in box pull starts
// from: the job's /work/<repo>, or the only checkout in the box. When job is
// set, it also checks that another job hasn't replaced that checkout (#197).
func checkoutScript(job string) string {
	s := "# checkout\nr=\n"
	if job != "" {
		s += fmt.Sprintf("test -f %[1]s/.box/%[2]s.log || { echo 'no job %[2]s' >&2; exit 3; }\nr=$(cat %[1]s/.box/%[2]s.repo 2>/dev/null)\n", workRoot, job)
	}
	s += fmt.Sprintf(`if [ -z "$r" ]; then
  set --
  for d in %[1]s/*/; do [ -d "$d" ] && set -- "$@" "$(basename "$d")"; done
  [ $# = 1 ] || { echo "box: $# checkouts in %[1]s; give an absolute path" >&2; exit 3; }
  r=$1
fi
`, workRoot)
	if job != "" {
		s += checkoutOwnerCheck(job)
	}
	return s + fmt.Sprintf("echo %s/\"$r\"\n", workRoot)
}

// checkoutOwnerScript checks whether job's .repo matches repo and, if so,
// verifies that job still owns /work/<repo> (#197). It prints repo when job's
// checkout is repo and intact, prints nothing when repo is a different
// directory, exits 3 if job doesn't exist, and exits 4 if another job owns it.
func checkoutOwnerScript(job, repo string) string {
	return "# checkout\n" + fmt.Sprintf(`test -f %[1]s/.box/%[2]s.log || { echo 'no job %[2]s' >&2; exit 3; }
r=$(cat %[1]s/.box/%[2]s.repo 2>/dev/null)
if [ -z "$r" ] || [ "$r" != %[3]s ]; then
  exit 0
fi
`, workRoot, job, jobrun.Shq(repo)) + checkoutOwnerCheck(job) + "echo \"$r\"\n"
}

// isWindowsPath says whether p looks like a Windows path (C:/..., C:\..., or
// \...), as when a Windows path is passed as a remote path or Git Bash rewrites
// a /... argument before box sees it (#197).
func isWindowsPath(p string) bool {
	if strings.HasPrefix(p, `\`) {
		return true
	}
	return len(p) >= 3 && ((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z')) &&
		p[1] == ':' && (p[2] == '/' || p[2] == '\\')
}

func (a *App) checkFileOwner(ctx context.Context, box, job, repo string) error {
	cur, err := a.incusOK(ctx, "file", "pull", a.target(box)+workRoot+"/.box/"+repo+".checkout", "-")
	if err != nil {
		switch err.Error() {
		case "incus file: Error: file does not exist", "incus file: Error: not found":
			return nil // older boxes have no .checkout file
		default:
			return fmt.Errorf("read checkout owner for %s: %w", job, err)
		}
	}
	if cur = strings.TrimSpace(cur); cur != "" && cur != job {
		return fmt.Errorf("job %s's checkout %s/%s was replaced by job %s; pull right after a job, or run with --fresh (fresh: true)", job, workRoot, repo, cur)
	}
	return nil
}

// checkCheckoutOwner verifies that job still owns /work/<repo> if repo is job's
// checkout. owned is true when repo matches job's checkout (so callers recheck
// it after transferring files into staging).
func (a *App) checkCheckoutOwner(ctx context.Context, box, job, repo string) (owned bool, err error) {
	out, filesOnly, err := a.execCheckoutScript(ctx, box, checkoutOwnerScript(job, repo))
	if err != nil && filesOnly {
		jobRepo, rerr := a.incusOK(ctx, "file", "pull", a.target(box)+workRoot+"/.box/"+job+".repo", "-")
		if rerr != nil {
			return false, rerr
		}
		if strings.TrimSpace(jobRepo) != repo {
			return false, nil
		}
		return true, a.checkFileOwner(ctx, box, job, repo)
	}
	if err != nil {
		return false, err
	}
	return out != "", nil
}

func inCheckout(cleaned, base string) bool {
	base = path.Clean(base)
	return cleaned == base || strings.HasPrefix(cleaned, base+"/")
}

func (a *App) execCheckoutScript(ctx context.Context, box, script string) (stdout string, filesOnly bool, err error) {
	res, err := a.incus(ctx, Cmd{Args: []string{"exec", a.target(box), "--", "bash", "-c", script}})
	if err != nil {
		return "", false, err
	}
	switch res.Code {
	case 0:
		return strings.TrimSpace(res.Stdout), false, nil
	case 3, 4:
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("checkout lookup failed for %s", box)
		}
		return "", false, errors.New(msg)
	default:
		msg := strings.TrimSpace(res.Stderr)
		filesOnly := strings.Contains(msg, "Instance is not running") || strings.Contains(msg, "Instance is frozen")
		return "", filesOnly, fmt.Errorf("incus exec: %s", msg)
	}
}

var windowsDeviceName = regexp.MustCompile(`(?i)^(CON|PRN|AUX|NUL|CONIN\$|CONOUT\$|(?:COM|LPT)[1-9¹²³])$`)

func localPullName(name string) bool {
	if !filepath.IsLocal(name) || filepath.Base(name) != name {
		return false
	}
	if runtime.GOOS != "windows" {
		return true
	}
	stem, _, _ := strings.Cut(name, ".")
	return strings.TrimRight(name, " .") == name && !windowsDeviceName.MatchString(strings.TrimRight(stem, " "))
}

// Pull copies a file or directory from a box into the local directory
// local, as local/<base name>. target is a qualified or bare job ID, or a box name. A relative
// remote path starts at the job's checkout (or the box's only checkout).
// Nothing is written outside local: the copy lands in a staging directory
// inside it, symlinks and special files are dropped, and an existing
// local/<base name> is never replaced.
func (a *App) Pull(ctx context.Context, target, remote, local string) (string, error) {
	if validBox.MatchString(target) {
		return a.pull(ctx, target, "", remote, local)
	}
	if !validJob.MatchString(target) && !strings.ContainsRune(target, '~') {
		return "", fmt.Errorf("bad job id or box name %q", target)
	}
	lookup, err := a.lookupJob(target)
	if err != nil {
		return "", err
	}
	if lookup.cloud != nil {
		return lookup.cloud.pullToDir(ctx, lookup.id.Bare, remote, local)
	}
	return lookup.app.pull(ctx, lookup.id.Box, lookup.id.Bare, remote, local)
}

func (a *App) pull(ctx context.Context, box, job, remote, local string) (string, error) {
	if remote == "" || strings.ContainsAny(remote, "\x00\n") {
		return "", errors.New("give a remote path")
	}
	if isWindowsPath(remote) {
		return "", fmt.Errorf("remote path %q is a Windows path, not a path in the Linux box (in Git Bash, pass a relative path or set MSYS_NO_PATHCONV=1 for /... paths)", remote)
	}
	if local == "" {
		return "", errors.New("give a local directory")
	}
	// A job's box the queue took for room says so, not Incus's "Instance not found" (#143).
	gone := func(err error) error {
		if job != "" && isGone(err) {
			if by := a.evictedFor(ctx, job); by != "" {
				return errors.New(evictedText(box, by))
			}
		}
		return err
	}
	var checkedRepo string
	if !path.IsAbs(remote) {
		base, filesOnly, err := a.execCheckoutScript(ctx, box, checkoutScript(job))
		if err != nil && job != "" && filesOnly {
			// Use the job files directly when a frozen or stopped box
			// cannot execute the checkout script.
			var repo string
			repo, err = a.incusOK(ctx, "file", "pull", a.target(box)+workRoot+"/.box/"+job+".repo", "-")
			if repo = strings.TrimSpace(repo); err == nil && (repo == "" || strings.ContainsAny(repo, "/\x00\n") || repo == "." || repo == "..") {
				err = fmt.Errorf("job %s names no checkout; give an absolute path", job)
			}
			if err == nil {
				err = a.checkFileOwner(ctx, box, job, repo)
			}
			base = workRoot + "/" + repo
		}
		if err != nil {
			return "", gone(err)
		}
		remote = path.Join(base, remote)
		if job != "" && inCheckout(path.Clean(remote), base) {
			if repo := path.Base(path.Clean(base)); validBox.MatchString(repo) {
				checkedRepo = repo
			}
		}
	} else if job != "" {
		cleaned := path.Clean(remote)
		if rest, ok := strings.CutPrefix(cleaned, workRoot+"/"); ok {
			repo, _, _ := strings.Cut(rest, "/")
			if validBox.MatchString(repo) {
				owned, err := a.checkCheckoutOwner(ctx, box, job, repo)
				if err != nil {
					return "", gone(err)
				}
				if owned {
					checkedRepo = repo
				}
			}
		}
	}
	remote = path.Clean(remote)
	name := path.Base(remote)
	if name == "/" || name == "." || name == ".." {
		return "", fmt.Errorf("can't pull %s", remote)
	}
	if !localPullName(name) {
		return "", fmt.Errorf("can't use %q as a local file name", name)
	}

	local, err := filepath.Abs(local)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(local, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(local, name)
	if _, err := os.Lstat(dest); err == nil {
		return "", fmt.Errorf("%s already exists; remove it or pull into another directory", dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	staging, err := os.MkdirTemp(local, ".box-pull-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	if _, err := a.incusOK(ctx, "file", "pull", "-r", a.target(box)+remote, staging); err != nil {
		if job != "" && isGone(err) {
			if by := a.evictedFor(ctx, job); by != "" {
				return "", errors.New(evictedText(box, by))
			}
		}
		return "", fmt.Errorf("pull %s from %s: %w", remote, box, err)
	}
	// Recheck checkout ownership after transferring into staging and before
	// publishing to dest: another job could have replaced /work/<repo> while
	// incus file pull was reading it (#197).
	if checkedRepo != "" {
		if _, err := a.checkCheckoutOwner(ctx, box, job, checkedRepo); err != nil {
			return "", gone(err)
		}
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		return "", err
	}
	if len(entries) != 1 {
		return "", fmt.Errorf("pull of %s left %d entries, want 1", remote, len(entries))
	}
	if entries[0].Name() != name || (!entries[0].Type().IsRegular() && !entries[0].IsDir()) {
		return "", fmt.Errorf("pull of %s did not produce a regular file or directory named %q", remote, name)
	}
	got := filepath.Join(staging, entries[0].Name())
	var dropped []string
	err = filepath.WalkDir(got, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(staging, p)
		dropped = append(dropped, filepath.ToSlash(rel))
		return os.Remove(p)
	})
	if err != nil {
		return "", err
	}
	if len(dropped) > 0 {
		a.warn("box: warning: dropped links and special files: " + strings.Join(dropped, " "))
	}
	// Reserve a directory name before copying it. CopyFS alone would merge
	// into an existing directory. For files it already uses exclusive creation.
	if !entries[0].IsDir() {
		err = os.CopyFS(local, os.DirFS(staging))
	} else if err = os.Mkdir(dest, 0o755); err == nil {
		err = os.CopyFS(dest, os.DirFS(got))
		if err != nil {
			err = errors.Join(err, os.RemoveAll(dest))
		}
	}
	if err != nil {
		return "", err
	}
	return dest, nil
}
