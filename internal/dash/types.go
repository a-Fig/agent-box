// Package dash is the contract between the dashboard's server (boxd), its page
// and its devserver: the JSON shapes of docs/ui.md, and the table that sorts a
// command into a kind. Nothing here reads Incus or the clock.
//
// Conventions: times are RFC3339 UTC, sizes are bytes, cpuPct is the share of
// the box's own CPU limit from 0 to 100. A time that may be missing is a
// pointer, so it is null in JSON; so is a missing exit code.
package dash

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Modes a machine can be in, as the tray writes user.agent-box.mode. It writes
// the effective mode: while a game runs that is MyPC, with Yielding set.
const (
	ModeFull   = "full"
	ModeShared = "shared"
	ModeMyPC   = "mypc"
)

// Modes lists the modes a person can ask for, in the switch's order.
var Modes = []string{ModeFull, ModeShared, ModeMyPC}

// TrayStaleAfter is how old Machine.TrayLastSeen may be before the tray counts
// as down. The tray writes user.agent-box.tray-seen every 30s.
const TrayStaleAfter = 90 * time.Second

// ModeRequest is a mode someone asked for from the page, waiting for the tray
// to apply it (user.agent-box.mode-request). By is the asking device.
type ModeRequest struct {
	Mode string    `json:"mode"`
	At   time.Time `json:"at"`
	By   string    `json:"by"`
}

// Resources is a set of allocations: the project's caps, or what existing
// boxes hold of them. CPU counts each box's Full CPUs, as box status does.
type Resources struct {
	CPU       int   `json:"cpu"`
	Memory    int64 `json:"memory"`
	Instances int   `json:"instances"`
	Disk      int64 `json:"disk"`
}

// Room is how many boxes of each size fit now; GPU is 0 or 1.
type Room struct {
	Small  int `json:"small"`
	Medium int `json:"medium"`
	Large  int `json:"large"`
	GPU    int `json:"gpu"`
}

// Machine is a PC that runs boxes. Today there is one; ID lets a second
// machine be a second entry.
type Machine struct {
	ID            string       `json:"id"`
	Name          string       `json:"name"`
	Reachable     bool         `json:"reachable"`
	Mode          string       `json:"mode"`
	ModeReason    string       `json:"modeReason"` // why, while yielding: "gaming: steam:570"
	ModeRequested *ModeRequest `json:"modeRequested"`
	Yielding      bool         `json:"yielding"`
	TrayLastSeen  *time.Time   `json:"trayLastSeen"` // null: never seen
	Caps          Resources    `json:"caps"`
	Allocated     Resources    `json:"allocated"`
	Room          Room         `json:"room"`
	WSLMemory     int64        `json:"wslMemory"` // bytes the distro's VM holds now
}

// TrayAlive reports whether the tray has written its heartbeat lately.
func (m Machine) TrayAlive(now time.Time) bool {
	return m.TrayLastSeen != nil && now.Sub(*m.TrayLastSeen) < TrayStaleAfter
}

// Box states, from Incus's status in lower case.
const (
	BoxRunning = "running"
	BoxFrozen  = "frozen"
	BoxStopped = "stopped"
)

// Box is one Incus container in project agents.
type Box struct {
	Name      string     `json:"name"`
	Machine   string     `json:"machine"`
	Size      string     `json:"size"`
	GPU       bool       `json:"gpu"`
	State     string     `json:"state"`
	Throwaway bool       `json:"throwaway"`
	Repo      string     `json:"repo"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt"`
	MemUsed   int64      `json:"memUsed"`
	MemLimit  int64      `json:"memLimit"`
	CPUs      int        `json:"cpus"` // the box's limits.cpu now
	CPUPct    float64    `json:"cpuPct"`
}

// Job states.
const (
	JobRunning = "running"
	JobDone    = "done"
	JobPaused  = "paused" // its box is frozen
	JobLost    = "lost"   // its box went before an exit code was read
)

// Results are what filters and the history group finished jobs by.
const (
	ResultPass      = "pass"
	ResultFail      = "fail"
	ResultCancelled = "cancelled" // exit 130, from box cancel or the page
	ResultLost      = "lost"
)

// Results lists them in display order.
var Results = []string{ResultPass, ResultFail, ResultCancelled, ResultLost}

// Agent is who ran a job. It is self-reported by the agent's own cert, so it
// is for display, never for decisions.
type Agent struct {
	Client   string `json:"client"`   // claude-code, codex, cli
	Session  string `json:"session"`  // the Murmur name, else the worktree's folder
	Host     string `json:"host"`     // the laptop's hostname
	Worktree string `json:"worktree"` // the worktree's path
}

// Job is one command run on a box.
type Job struct {
	Key       string     `json:"key"` // box/id/startedAt: names and IDs get reused
	ID        string     `json:"id"`
	Box       string     `json:"box"`
	Machine   string     `json:"machine"`
	State     string     `json:"state"`
	Exit      *int       `json:"exit"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt"`
	// Reason is the agent's one or two sentences on why this runs on a box,
	// the job's headline. box run requires it; a job from before that has "".
	Reason  string `json:"reason"`
	Command string `json:"command"`
	Kind    string `json:"kind"`
	Repo    string `json:"repo"`
	Branch  string `json:"branch"`
	Commit  string `json:"commit"`
	Agent   Agent  `json:"agent"`
	Size    string `json:"size"`
	GPU     bool   `json:"gpu"`
	Tail    string `json:"tail"` // the log's last TailBytes, kept after the box goes
}

// TailBytes is how much of a job's log its Tail keeps.
const TailBytes = 2 << 10

// JobBoxGrace is how much later than a job's start its box may have been made. box run --new
// makes the job's ID, and its start, before it launches the box, so a throwaway's createdAt is a
// few seconds after its job's startedAt. A job's box is the box of its name made no later than
// startedAt + JobBoxGrace; a box of that name made later is a reuse of the name.
const JobBoxGrace = 10 * time.Minute

// JobKey makes a job's key from its box, ID and start.
func JobKey(box, id string, started time.Time) string {
	return box + "/" + id + "/" + started.UTC().Format(time.RFC3339)
}

// Result is how a finished job went, or "" while it runs or is paused.
func (j Job) Result() string {
	switch {
	case j.State == JobLost:
		return ResultLost
	case j.State != JobDone || j.Exit == nil:
		return ""
	case *j.Exit == 0:
		return ResultPass
	case *j.Exit == 130:
		return ResultCancelled
	}
	return ResultFail
}

// State is GET /api/state and the first SSE event. Jobs holds the running and
// paused jobs plus the last 50 finished.
type State struct {
	Machines []Machine `json:"machines"`
	Boxes    []Box     `json:"boxes"`
	Jobs     []Job     `json:"jobs"`
	Now      time.Time `json:"now"`
}

// FinishedInState is how many finished jobs State.Jobs keeps.
const FinishedInState = 50

// JobsPage is GET /api/jobs: finished jobs, newest first. More is true when
// asking again with before set to the last job's startedAt returns more.
type JobsPage struct {
	Jobs []Job `json:"jobs"`
	More bool  `json:"more"`
}

// LogChunk is GET /api/jobs/{key}/log?offset=: at most MaxLogChunk bytes
// from offset. Offset is where to ask next. EOF means the log has nothing
// more now; a running job's may grow, so the page asks again. Paused means
// the box is frozen, so the log can't be read now; that is not an error.
type LogChunk struct {
	Offset int64  `json:"offset"`
	Data   string `json:"data"`
	EOF    bool   `json:"eof"`
	Paused bool   `json:"paused"`
}

// MaxLogChunk is the most bytes a LogChunk's Data holds.
const MaxLogChunk = 64 << 10

// JobCounts groups a bucket's jobs, by when they started.
type JobCounts struct {
	ByResult map[string]int `json:"byResult"`
	ByKind   map[string]int `json:"byKind"`
	ByRepo   map[string]int `json:"byRepo"`
	ByAgent  map[string]int `json:"byAgent"`
}

// BoxHours is a bucket's box-hours: how long boxes existed in it, by size.
type BoxHours struct {
	BySize map[string]float64 `json:"bySize"`
}

// RefusalCounts is a bucket's refusals by their cause (RefusalBody.Why).
type RefusalCounts struct {
	ByWhy map[string]int `json:"byWhy"`
}

// Medians is a bucket's median run time of finished jobs, by repo.
type Medians struct {
	ByRepo map[string]float64 `json:"byRepo"`
}

// Bucket is one hour (range=day) or one day (range=week). MemUsed is the
// mean of the minute samples; MemCap the project's memory cap.
type Bucket struct {
	Start         time.Time     `json:"start"`
	Jobs          JobCounts     `json:"jobs"`
	BoxHours      BoxHours      `json:"boxHours"`
	Refusals      RefusalCounts `json:"refusals"`
	MedianSeconds Medians       `json:"medianSeconds"`
	MemUsed       int64         `json:"memUsed"`
	MemCap        int64         `json:"memCap"`
}

// Segment is a stretch of one effective mode, for the timeline. A yield is a
// MyPC segment whose Reason says why ("gaming: steam:570"); the others have
// none.
type Segment struct {
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	Mode   string    `json:"mode"`
	Reason string    `json:"reason"`
}

// Ranges of GET /api/history.
const (
	RangeDay  = "day"
	RangeWeek = "week"
)

// History is GET /api/history?range=day|week: 24 hourly buckets or 7 daily
// ones, oldest first, the last one holding now.
type History struct {
	Range    string    `json:"range"`
	Buckets  []Bucket  `json:"buckets"`
	Segments []Segment `json:"segments"`
}

// Why a run was turned away with exit 75: RefusalBody.Why and the key of a
// bucket's refusals.byWhy.
const (
	RefusedYielding = "yielding" // a game is running
	RefusedMyPC     = "mypc"
	RefusedFull     = "full" // the caps have no room for its size
	RefusedGPUBusy  = "gpu-busy"
)

// Refusals lists the causes in display order.
var Refusals = []string{RefusedYielding, RefusedMyPC, RefusedFull, RefusedGPUBusy}

// RefusalBody is POST /api/refusals, sent by box as a best effort. Why is the
// refusal's cause; Reason the agent's own reason for the run.
type RefusalBody struct {
	Why    string `json:"why"`
	Reason string `json:"reason"`
	Size   string `json:"size"`
	GPU    bool   `json:"gpu"`
	Repo   string `json:"repo"`
	Agent  string `json:"agent"`
}

// JobEndedBody is POST /api/jobs/ended, sent by box as a best effort (2s timeout) once it has
// read a run's exit and before it deletes a throwaway box, so boxd records the job as ended
// rather than lost when its box vanishes between polls. boxd takes it only for a job it knows is
// running (404 otherwise), masks the tail as it masks every tail, and answers 204.
type JobEndedBody struct {
	Box     string    `json:"box"`
	ID      string    `json:"id"`
	Exit    int       `json:"exit"`
	EndedAt time.Time `json:"endedAt"`
	Tail    string    `json:"tail"`
}

// ModeBody is POST /api/mode. The reply is the Machine, with ModeRequested
// set until the tray applies it.
type ModeBody struct {
	Mode string `json:"mode"`
}

// BoxBody names which box a control is for: the box of the path's name made at CreatedAt.
// Names are reused, so a page that saw an older box of the name must not act on a newer one;
// boxd compares CreatedAt with the box's own and answers 409 CodeGone when they differ.
type BoxBody struct {
	CreatedAt time.Time `json:"createdAt"`
}

// DeleteBody is POST /api/boxes/{name}/delete; the reply is 204.
type DeleteBody = BoxBody

// ExtendBody is POST /api/boxes/{name}/extend; the reply is the Box.
type ExtendBody struct {
	Hours     int       `json:"hours"`
	CreatedAt time.Time `json:"createdAt"`
}

// ExtendHours are the extensions the page offers and boxd accepts.
var ExtendHours = []int{1, 4, 24}

// ControlHeader must be on every POST, with the value "1": another site can't
// send it without a CORS preflight, which boxd never grants.
const ControlHeader = "X-Agent-Box"

// ErrorBody is every non-2xx reply.
type ErrorBody struct {
	Error string `json:"error"` // a sentence for a person
	Code  string `json:"code"`  // one of the Code constants
}

// Error codes, with the status each goes with.
const (
	CodeBadRequest = "bad-request" // 400
	CodeForbidden  = "forbidden"   // 403: the caller isn't an owner
	CodeNotFound   = "not-found"   // 404: the box or job is gone
	CodeYielding   = "yielding"    // 409: the box is frozen for a game
	CodeFrozen     = "frozen"      // 409: the box is frozen for My PC
	CodeNotRunning = "not-running" // 409: the job has already ended
	CodeGone       = "gone"        // 409: the box of that name is another one now
	CodeIncus      = "incus"       // 502: Incus failed
)

// SSE event names on GET /api/events. The first event is EventState; each
// after it carries one whole object that replaces the old one.
const (
	EventState   = "state"    // State
	EventMachine = "machine"  // Machine
	EventBox     = "box"      // Box
	EventJob     = "job"      // Job
	EventBoxGone = "box-gone" // BoxGone
	EventJobGone = "job-gone" // JobGone
)

// BoxGone and JobGone name what went.
type (
	BoxGone struct {
		Name    string `json:"name"`
		Machine string `json:"machine"`
	}
	JobGone struct {
		Key string `json:"key"`
	}
)

// Event is one server-sent event: its name and its data, which is written as
// one line of JSON.
type Event struct {
	Name string `json:"event"`
	Data any    `json:"data"`
}

// WriteTo writes e in the text/event-stream format.
func (e Event) WriteTo(w io.Writer) (int64, error) {
	b, err := json.Marshal(e.Data)
	if err != nil {
		return 0, err
	}
	n, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Name, b)
	return int64(n), err
}
