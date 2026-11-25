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

// Modes a machine can be in, as the tray writes user.agent-box.mode.
const (
	ModeFull   = "full"
	ModeShared = "shared"
	ModeMyPC   = "mypc"
)

// Modes lists the modes a person can ask for, in the switch's order.
var Modes = []string{ModeFull, ModeShared, ModeMyPC}

// TrayStaleAfter is how old Machine.TrayLastSeen may be before the enforcer
// counts as down. The active enforcer writes user.agent-box.tray-seen every 30s.
const TrayStaleAfter = 90 * time.Second

// Enforcers: which process applies the mode, Machine.Enforcer, from
// user.agent-box.enforcer as boxtray writes it. The tray runs while someone is
// logged on to the PC; the headless enforcer (boxtray --headless) runs while
// nobody is.
const (
	EnforcerTray     = "tray"
	EnforcerHeadless = "headless"
)

// ChosenMode is the mode the enforcers act on, the project key user.agent-box.chosen
// ({mode, at} JSON): the last pick, a tray click or an adopted request, and At when it was made.
type ChosenMode struct {
	Mode string    `json:"mode"`
	At   time.Time `json:"at"`
}

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
	ModeRequested *ModeRequest `json:"modeRequested"`
	Chosen        *ChosenMode  `json:"chosen"`       // null until a pick is stored
	TrayLastSeen  *time.Time   `json:"trayLastSeen"` // the active enforcer's heartbeat; null: never seen
	Enforcer      string       `json:"enforcer"`     // who wrote it: EnforcerTray, EnforcerHeadless, or "" if unknown
	Caps          Resources    `json:"caps"`
	Allocated     Resources    `json:"allocated"`
	Room          Room         `json:"room"`
	WSLMemory     int64        `json:"wslMemory"` // bytes the distro's VM holds now
	Boxd          *Version     `json:"boxd"`      // the boxd serving this; null if its build has no commit
	Tray          *Version     `json:"tray"`      // the enforcer's build, user.agent-box.tray-version; null: unknown
}

// Version is the commit a binary was built from, so the page can show when
// boxd and the tray were built from different commits.
type Version struct {
	Commit   string     `json:"commit"`        // the full hash
	Time     *time.Time `json:"time"`          // the commit's time; null if the build didn't record it
	Modified bool       `json:"modified"`      // built with uncommitted changes
	Tag      string     `json:"tag,omitempty"` // the release, like v0.3.0; "" for a build that isn't one
}

// String is the short commit, with "+" for a modified build, or "unknown".
func (v Version) String() string {
	c := v.Commit
	if len(c) > 7 {
		c = c[:7]
	}
	if c == "" {
		c = "unknown"
	}
	if v.Modified {
		c += "+"
	}
	return c
}

// TrayAlive reports whether the active enforcer, tray or headless, has written its heartbeat
// lately.
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
	JobQueued  = "queued" // waiting in boxd's queue (docs/queue.md)
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
	// QueuedAt is when a job that went through the queue was queued; null
	// for one box started at once. While it waits, StartedAt is QueuedAt
	// too, Position is its place in line from 1, and Why is what it waits
	// for, one of the Refused* causes.
	QueuedAt *time.Time `json:"queuedAt"`
	Position int        `json:"position"`
	Why      string     `json:"why"`
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
	Queued        QueueCounts   `json:"queued"`
}

// QueueCounts is a bucket's jobs queued (by when they were queued, cancelled
// and dropped ones too), and the queued jobs that started in it with their
// median wait.
type QueueCounts struct {
	Jobs              int     `json:"jobs"`
	Started           int     `json:"started"`
	MedianWaitSeconds float64 `json:"medianWaitSeconds"`
}

// Segment is a stretch of one mode, for the timeline.
type Segment struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Mode  string    `json:"mode"`
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
	RefusedMyPC    = "mypc"
	RefusedFull    = "full" // the caps have no room for its size
	RefusedGPUBusy = "gpu-busy"
)

// Refusals lists the causes in display order.
var Refusals = []string{RefusedMyPC, RefusedFull, RefusedGPUBusy}

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
	CodeFrozen     = "frozen"      // 409: the box is frozen for My PC
	CodeNotRunning = "not-running" // 409: the job has already ended
	CodeGone       = "gone"        // 409: the box of that name is another one now
	CodeIncus      = "incus"       // 502: Incus failed
	CodeQueueFull  = "queue-full"  // 507: the queue's snapshots would pass QueueBytes
)

// SSE event names on GET /api/events. The first event is EventState; each
// after it carries one whole object that replaces the old one.
//
// The queue rides on these: a job's queuing, and every change to a waiting
// job's Position or Why, is an EventJob with the whole job, so the page can
// keep the line from events alone. When it starts or is cancelled, an
// EventJobGone takes its waiting key away; a started job then comes as an
// EventJob under its running key (its StartedAt is the start), with the same
// ID and QueuedAt.
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

// QueueRequest is the "request" part of POST /api/queue (multipart/form-data);
// the "bundle" part is the snapshot: box's git bundle of the worktree, with
// the commit at refs/agent-box/<Ref>. boxd starts the job as box run would
// (internal/jobrun) when it can, on that snapshot (docs/queue.md).
type QueueRequest struct {
	// Box is the repo's box, or with New the base of a throwaway's name,
	// which boxd completes with a random suffix.
	Box      string            `json:"box"`
	New      bool              `json:"new"`
	Size     string            `json:"size"` // "" is medium for a new box, any size for an existing one
	GPU      bool              `json:"gpu"`
	Checkout string            `json:"checkout"` // the repo's name: /work/<checkout> in the box
	Ref      string            `json:"ref"`
	Command  []string          `json:"command"` // as box run takes it: one string runs as shell
	Env      map[string]string `json:"env"`
	Timeout  int               `json:"timeout"` // seconds; 0 is none
	// Meta is the .meta box would write (cmd/box). boxd sets its queuedAt
	// and startedAt and writes it next to the job's log.
	Meta json.RawMessage `json:"meta"`
}

// QueueReply answers POST /api/queue (201). Job is "<box>.<id>", as box
// names jobs; Key the job's key while it waits.
type QueueReply struct {
	Job      string `json:"job"`
	Key      string `json:"key"`
	Position int    `json:"position"`
	Why      string `json:"why"`
}

// QueuePage is GET /api/queue: the waiting jobs in the order they start.
type QueuePage struct {
	Jobs []Job `json:"jobs"`
}

// The queue's limits (docs/queue.md).
const (
	QueueBytes  = 2 << 30          // all waiting snapshots together
	QueueMaxAge = 24 * time.Hour   // a job waiting longer is dropped
	QueueNudge  = 20 * time.Minute // when box watch nudges the agent
)
