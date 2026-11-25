// Package jobrun is how a job starts in a box: the shell that checks a
// snapshot out in /work/<repo> and runs the command detached, logging to
// /work/.box/<job>.log and leaving its exit code in <job>.exit. box run uses
// it, and boxd uses it to start queued jobs (docs/queue.md), so a job looks
// the same whoever started it.
package jobrun

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/a-Fig/agent-box/internal/digest"
)

// AliveFn defines alive PID JOB: whether PID is still JOB's wrapper and not
// a later process that got the same PID after a restart.
const AliveFn = `alive() { [ -n "$1" ] && kill -0 "$1" 2>/dev/null && { [ ! -r /proc/"$1"/cmdline ] || grep -aqF "$2" /proc/"$1"/cmdline; }; }
`

// JobPIDFn defines jobpid JOB, which waits for the detached wrapper to publish its actual PID.
const JobPIDFn = `jobpid() {
  local pid deadline=$((SECONDS + 15))
  pid=$(cat "$1.pid" 2>/dev/null)
  while [ -z "$pid" ] && [ ! -f "$1.exit" ] && [ "$SECONDS" -lt "$deadline" ]; do
    sleep 0.1
    pid=$(cat "$1.pid" 2>/dev/null)
  done
  printf '%s' "$pid"
}
`

// Payload is the shell code a job runs in its checkout: the exports, then the
// command, under coreutils timeout when there is one.
func Payload(env map[string]string, args []string, timeout time.Duration) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "export %s=%s || exit 2\n", k, Shq(env[k]))
	}
	cmd := ShellCommand(args)
	if len(args) == 1 {
		// "go test ./... | tail" exits with go test's code, not tail's (#6).
		cmd = "set -o pipefail\n" + cmd
	}
	// Define wrappers for go test and vitest so a test run leaves its JSON
	// report beside the log for the failure digest (internal/digest). The
	// wrappers pass through any command shape they don't handle and live
	// only in this shell, so child scripts run the real tools. Always load
	// them rather than gating on dash.Classify, so commands like
	// `npm run test:unit` or `npm -w packages/a test` get a digest too.
	cmd = digest.Wrappers + cmd
	if timeout > 0 {
		secs := strconv.FormatFloat(timeout.Seconds(), 'f', -1, 64)
		// A hard timeout can kill the timeout process itself. Read its private
		// diagnostic to distinguish that from a payload that simply exits 137.
		cmd = fmt.Sprintf(`exec 3>&1 4>&2
diagnostic=$(/usr/bin/timeout --verbose -k 10s %ss /bin/bash -c %s 2>&1 1>&3) 2>/dev/null
status=$?
if [[ $status = 137 && $diagnostic == *" KILL "* ]]; then exit 124; fi
if [[ $status != 0 && $status != 124 && -n $diagnostic ]]; then echo "$diagnostic" >&2; fi
exit "$status"`, secs, Shq("exec 2>&4 3>&- 4>&-\n"+cmd))
	}
	b.WriteString(cmd)
	return b.String()
}

// StartScript checks the bundle out in /work/<repo> and starts the payload
// detached, logging to /work/.box/<job>.log and writing its exit code to
// <job>.exit. <job>.repo names the checkout, for box pull.
// ref names the snapshot in the bundle (refs/agent-box/<ref>). Jobs on the
// same repo in the same box take turns (flock), since they share the
// checkout. The payload runs without the lock's FD, so a background child it
// leaves behind can't keep holding the lock.
func StartScript(root, job, ref, repo, payload string) string {
	w := root
	f := w + "/.box/" + job
	inner := fmt.Sprintf(`export `+digest.JobVar+`=%[2]s
echo "$$" > %[2]s.pid.tmp && mv %[2]s.pid.tmp %[2]s.pid || exit 1
exec 9>%[1]s/.box/%[3]s.lock
flock 9 || exit 1
if git init -q %[1]s/%[3]s && cd %[1]s/%[3]s &&
   git fetch -q %[2]s.bundle refs/agent-box/%[4]s &&
   git checkout -q --force --detach FETCH_HEAD && git clean -fdq; then
  (
%[5]s
  ) 9>&-
  code=$?
else
  echo "box: checkout failed" >&2
  code=1
fi
rm -f %[2]s.bundle
echo $code > %[2]s.exit.tmp && ln %[2]s.exit.tmp %[2]s.exit 2>/dev/null
rm -f %[2]s.exit.tmp
`, w, f, repo, ref, payload)
	// The log is created before the job starts, so a follow that begins at
	// once never sees "no job".
	return JobPIDFn + fmt.Sprintf("mkdir -p %[1]s/.box && : > %[3]s.log && echo %[4]s > %[3]s.repo && { setsid nohup bash -lc %[2]s >> %[3]s.log 2>&1 < /dev/null & } && [ -n \"$(jobpid %[3]s)\" ]",
		w, Shq(inner), f, Shq(repo))
}

// ShellCommand runs a single argument as a shell string ("go test ./... && x")
// and quotes several arguments word by word.
func ShellCommand(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	q := make([]string, len(args))
	for i, s := range args {
		q[i] = Shq(s)
	}
	return strings.Join(q, " ")
}

// Shq quotes s for the shell.
func Shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// CancelScript stops a job's whole session (the wrapper leads one, from
// setsid): TERM, then KILL after 5 seconds. It writes code (box uses 130) to the job's
// .exit if the wrapper didn't, so followers stop. It prints "cancelled" or
// "finished", then the exit code, and exits 3 for an unknown job.
//
// by is empty for box cancel, the agent's own: the script then leaves a
// .cancelled file before the kill, and box's watchers stay quiet about the
// job. From the page, by names the device that asked: the script leaves no
// mark, so the agent hears that its job ended, and it adds a line saying who
// cancelled it to the log, so the agent can see it was on purpose.
func CancelScript(root, job string, code int, by string) string {
	before := `: > "$j.cancelled"`
	if by != "" {
		before = "echo " + Shq("box: cancelled from the dashboard by "+by) + ` >> "$j.log"`
	}
	return "# cancel\n" + AliveFn + JobPIDFn + fmt.Sprintf(`cd %s/.box 2>/dev/null || exit 3
j=%s
test -f "$j.log" || exit 3
pid=$(jobpid "$j")
if [ -f "$j.exit" ]; then echo "finished $(cat "$j.exit")"; exit 0; fi
case "$pid" in ''|*[!0-9]*) echo "job $j has no wrapper PID" >&2; exit 4 ;; esac
if ! alive "$pid" "$j" || [ "$(ps -o sid= -p "$pid" | tr -d ' ')" != "$pid" ]; then
  if [ -f "$j.exit" ]; then echo "finished $(cat "$j.exit")"; exit 0; fi
  echo "cannot identify the session for job $j" >&2
  exit 4
fi
# Before the exit file can be there: the agent's .cancelled mark, which its
# watcher reads to stay quiet, or the page's line in the log.
%s
if ! pkill -TERM -s "$pid" && pgrep -s "$pid" >/dev/null; then rm -f "$j.cancelled"; exit 4; fi
for i in $(seq 50); do pgrep -s "$pid" >/dev/null || break; sleep 0.1; done
if ! pkill -KILL -s "$pid" && pgrep -s "$pid" >/dev/null; then rm -f "$j.cancelled"; exit 4; fi
if [ -f "$j.exit" ]; then echo "finished $(cat "$j.exit")"; exit 0; fi
verdict=finished
if echo %d > "$j.cancel.$$" && ln "$j.cancel.$$" "$j.exit" 2>/dev/null; then verdict=cancelled; fi
rm -f "$j.cancel.$$"
test -f "$j.exit" || exit 4
echo "$verdict $(cat "$j.exit")"
`, root, Shq(job), before, code)
}

// RunningScript prints the jobs still running in a box, one name a line.
// box and boxd both use it.
func RunningScript(root string) string {
	return "# running jobs\n" + AliveFn + fmt.Sprintf(`cd %s/.box 2>/dev/null || exit 0
for p in *.pid; do
  [ -e "$p" ] || continue
  j=${p%%.pid}
  [ -e "$j.exit" ] && continue
  alive "$(cat "$p" 2>/dev/null)" "$j" && echo "$j"
done
exit 0
`, root)
}
