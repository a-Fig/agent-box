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
	inner := fmt.Sprintf(`echo "$$" > %[2]s.pid.tmp && mv %[2]s.pid.tmp %[2]s.pid || exit 1
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
