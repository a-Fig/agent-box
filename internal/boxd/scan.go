package boxd

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/a-Fig/agent-box/internal/dash"
	"github.com/a-Fig/agent-box/internal/jobrun"
)

// The job files `box run` leaves in a box, /work/.box/<job>.{log,pid,exit,meta}
// where job is "<box>.<id>" (cmd/box/jobs.go). These scripts read them the same
// way box does; alive and jobstate follow cmd/box's, reading at most a few
// bytes of each file and checking .exit again once the wrapper is gone.
const jobDir = "/work/.box"

const jobStateFn = `jobstate() {
  if [ -f "$1.exit" ]; then echo "done $(head -c 16 "$1.exit")"
  elif alive "$(head -c 16 "$1.pid" 2>/dev/null)" "$1"; then echo "running -"
  elif [ -f "$1.exit" ]; then echo "done $(head -c 16 "$1.exit")"
  elif [ -f "$1.pid" ] || [ -n "$(find "$1.log" -mmin +1 2>/dev/null)" ]; then echo "died -"
  else echo "starting -"
  fi
}
`

// scanScript prints one line per job in the box:
//
//	J <job> <state> <exit|-> <logbytes> <exit mtime|0> <meta base64|-> <tail base64|->
//
// It looks at the newest maxScanJobs jobs only (job names sort by start).
// Jobs named in its arguments (ones boxd has already recorded as ended) get
// "-" for meta and tail, so a long-lived box costs little to scan.
var scanScript = "# boxd scan\n" + jobrun.AliveFn + jobStateFn + fmt.Sprintf(`cd %s 2>/dev/null || exit 0
declare -A skip; for s in "$@"; do skip[$s]=1; done
logs=(*.log); [ -e "${logs[0]}" ] || exit 0
n=%d; first=$(( ${#logs[@]} > n ? ${#logs[@]} - n : 0 ))
for l in "${logs[@]:first}"; do
  [ -f "$l" ] || continue # a FIFO or a device would hang the reads
  j=${l%%.log}
  st=$(jobstate "$j")
  mt=0; [ -f "$j.exit" ] && mt=$(stat -c %%Y "$j.exit")
  meta=-; tail=-
  if [ -z "${skip[$j]}" ]; then
    [ -f "$j.meta" ] && meta=$(head -c 16384 "$j.meta" | base64 -w0)
    tail=$(tail -c %d "$l" | base64 -w0); [ -n "$tail" ] || tail=-
    [ -n "$meta" ] || meta=-
  fi
  echo "J $j $st $(( $(wc -c < "$l") )) $mt $meta $tail"
done
exit 0
`, jobDir, maxScanJobs, dash.TailBytes)

// logScript prints the log's size, then up to limit bytes of it from offset.
// It exits 3 when the job isn't there.
func logScript(job string, offset int64) string {
	return fmt.Sprintf(`cd %s 2>/dev/null || exit 3
test -f %[2]s.log || exit 3
echo $(( $(wc -c < %[2]s.log) ))
tail -c +%[3]d %[2]s.log | head -c %[4]d
`, jobDir, job, offset+1, dash.MaxLogChunk)
}

// maxScanJobs bounds a scan of a long-lived box.
const maxScanJobs = 200

var validJob = regexp.MustCompile(`^[a-z][a-z0-9-]*\.[0-9]{8}-[0-9]{6}-[0-9a-f]+$`)

// scanned is one job as a scan saw it.
type scanned struct {
	Job      string // <box>.<id>
	State    string // running, done, died, starting
	Exit     *int
	LogBytes int64
	EndedAt  *time.Time // the .exit file's mtime
	Meta     *meta      // nil when skipped or missing
	Tail     string     // "" when skipped
	Skipped  bool
}

// meta is /work/.box/<job>.meta as box writes it (cmd/box/meta.go).
type meta struct {
	V       int                  `json:"v"`
	Reason  string               `json:"reason"`
	Command string               `json:"command"`
	Repo    string               `json:"repo"`
	Branch  string               `json:"branch"`
	Commit  string               `json:"commit"`
	Source  *dash.SourceRevision `json:"source"`
	Agent   struct {
		Client   string `json:"client"`
		Session  string `json:"session"`
		Display  string `json:"display"`
		Host     string `json:"host"`
		Worktree string `json:"worktree"`
	} `json:"agent"`
	Size string `json:"size"`
	GPU  bool   `json:"gpu"`
	// Set by boxd for a job it started from the queue.
	QueuedAt  string `json:"queuedAt"`
	StartedAt string `json:"startedAt"`
}

func parseScan(out []byte, skipped map[string]bool) []scanned {
	var jobs []scanned
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) != 8 || f[0] != "J" || !validJob.MatchString(f[1]) {
			continue
		}
		s := scanned{Job: f[1], State: f[2], Skipped: skipped[f[1]]}
		if c, err := strconv.Atoi(f[3]); err == nil {
			s.Exit = &c
		}
		s.LogBytes, _ = strconv.ParseInt(f[4], 10, 64)
		if mt, err := strconv.ParseInt(f[5], 10, 64); err == nil && mt > 0 {
			t := time.Unix(mt, 0).UTC()
			s.EndedAt = &t
		}
		if b, err := base64.StdEncoding.DecodeString(f[6]); err == nil && f[6] != "-" {
			var m meta
			if json.Unmarshal(b, &m) == nil {
				s.Meta = &m
			}
		}
		if b, err := base64.StdEncoding.DecodeString(f[7]); err == nil && f[7] != "-" {
			s.Tail = tailOf(b)
		}
		jobs = append(jobs, s)
	}
	return jobs
}

// tailOf trims a log's last bytes to start at a line (when one starts in
// them) and at a whole UTF-8 character.
func tailOf(b []byte) string {
	if len(b) > dash.TailBytes {
		b = b[len(b)-dash.TailBytes:]
	}
	if len(b) >= dash.TailBytes {
		if i := strings.IndexByte(string(b), '\n'); i >= 0 && i < len(b)-1 {
			b = b[i+1:]
		}
	}
	for len(b) > 0 && !utf8.RuneStart(b[0]) {
		b = b[1:]
	}
	return strings.ToValidUTF8(string(b), "�")
}

// jobStart reads the start from a job name (box.YYYYMMDD-HHMMSS-xxxx, UTC).
func jobStart(job string) (box, id string, started time.Time) {
	box, id, _ = strings.Cut(job, ".")
	if len(id) >= 15 {
		started, _ = time.Parse("20060102-150405", id[:15])
	}
	return box, id, started
}

// cutUTF8 drops an incomplete character at the end of b, so a log chunk ends
// where the next one can start.
func cutUTF8(b []byte) []byte {
	for i := 1; i <= utf8.UTFMax && i <= len(b); i++ {
		if utf8.RuneStart(b[len(b)-i]) {
			if r, _ := utf8.DecodeRune(b[len(b)-i:]); r == utf8.RuneError && !utf8.FullRune(b[len(b)-i:]) {
				return b[:len(b)-i]
			}
			return b
		}
	}
	return b
}
