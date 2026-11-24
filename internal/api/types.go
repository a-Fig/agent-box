// Package api holds the boxd HTTP contract shared by boxd, box, boxtray and
// boxhost. It must agree with docs/DESIGN.md; change both together.
package api

import "time"

// Modes.
const (
	ModeMyPC   = "mypc"
	ModeShared = "shared"
	ModeFull   = "full"
)

// Box states.
const (
	BoxStarting = "starting"
	BoxRunning  = "running"
	BoxFrozen   = "frozen"
	BoxStopped  = "stopped"
	BoxError    = "error"
)

// Job states.
const (
	JobRunning = "running"
	JobPaused  = "paused"
	JobExited  = "exited"
	JobKilled  = "killed"
	JobTimeout = "timeout"
)

// Error codes.
const (
	CodeBadRequest   = "bad_request"
	CodeUnauthorized = "unauthorized"
	CodeForbidden    = "forbidden"
	CodeNotFound     = "not_found"
	CodeConflict     = "conflict"
	CodeModePaused   = "mode_paused"
	CodeCapacity     = "capacity"
	CodeTimeout      = "timeout"
	CodeInternal     = "internal"
)

// Error is the body of every non-2xx response.
type Error struct {
	Message           string `json:"error"`
	Code              string `json:"code"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

type Health struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
}

type Status struct {
	Version           string    `json:"version"`
	Mode              string    `json:"mode"`
	EffectiveMode     string    `json:"effective_mode"`
	Yield             *Yield    `json:"yield,omitempty"`
	Available         bool      `json:"available"`
	RetryAfterSeconds int       `json:"retry_after_seconds,omitempty"`
	Capacity          Capacity  `json:"capacity"`
	Host              HostInfo  `json:"host"`
	Boxes             int       `json:"boxes"`
	RunningJobs       int       `json:"running_jobs"`
	AwakeUntil        time.Time `json:"awake_until"`
}

type Yield struct {
	Active bool      `json:"active"`
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

type Capacity struct {
	CPUs         float64 `json:"cpus"`
	MemoryBytes  int64   `json:"memory_bytes"`
	MaxBoxes     int     `json:"max_boxes"`
	RunningBoxes int     `json:"running_boxes"`
}

type HostInfo struct {
	CPUs            int     `json:"cpus"`
	MemoryBytes     int64   `json:"memory_bytes"`
	CPUPercent      float64 `json:"cpu_percent"`
	MemoryUsedBytes int64   `json:"memory_used_bytes"`
	DiskFreeBytes   int64   `json:"disk_free_bytes"`
}

type SetModeRequest struct {
	Mode string `json:"mode"`
}

type YieldRequest struct {
	Active bool   `json:"active"`
	Reason string `json:"reason,omitempty"`
}

type Box struct {
	Name            string    `json:"name"`
	State           string    `json:"state"`
	Image           string    `json:"image"`
	IPv4            string    `json:"ipv4,omitempty"`
	Owner           string    `json:"owner"`
	Repo            string    `json:"repo,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	Keep            bool      `json:"keep"`
	Busy            bool      `json:"busy"`
	IdleSince       time.Time `json:"idle_since,omitempty"`
	CPUPercent      float64   `json:"cpu_percent"`
	MemoryUsedBytes int64     `json:"memory_used_bytes"`
}

type CreateBoxRequest struct {
	Name  string            `json:"name,omitempty"`
	Image string            `json:"image,omitempty"`
	TTL   string            `json:"ttl,omitempty"`
	Owner string            `json:"owner,omitempty"`
	Env   map[string]string `json:"env,omitempty"`
	Wait  bool              `json:"wait,omitempty"`
}

type UpdateBoxRequest struct {
	TTL  string `json:"ttl,omitempty"`
	Keep *bool  `json:"keep,omitempty"`
}

type SyncResponse struct {
	Path   string `json:"path"`
	Commit string `json:"commit"`
}

type HaveResponse struct {
	Commits []string `json:"commits"`
}

type JobRequest struct {
	Command []string          `json:"command"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Timeout string            `json:"timeout,omitempty"`
	Wait    bool              `json:"wait,omitempty"`
}

type Job struct {
	ID        string     `json:"id"`
	Box       string     `json:"box"`
	Command   []string   `json:"command"`
	State     string     `json:"state"`
	ExitCode  *int       `json:"exit_code,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	LogBytes  int64      `json:"log_bytes"`
}

// LogLine is one NDJSON line of GET /v1/jobs/{id}/logs. Exactly one of
// Data (with Stream and Offset), Heartbeat, Event or Exit is set.
type LogLine struct {
	Stream    string `json:"stream,omitempty"` // stdout|stderr
	Offset    int64  `json:"offset,omitempty"` // byte offset of Data in the job log
	Data      string `json:"data,omitempty"`
	Heartbeat bool   `json:"heartbeat,omitempty"`
	Event     string `json:"event,omitempty"` // paused|resumed
	Exit      *int   `json:"exit,omitempty"`
}
