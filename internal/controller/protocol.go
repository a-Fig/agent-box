// Package controller implements the controller-v1 HTTP+JSON protocol
// (docs/controller-protocol.md) for publishing and reading cached host state.
package controller

import (
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/a-Fig/agent-box/internal/dash"
)

const (
	// ProtocolVersion is the controller-v1 protocol version reported by GET /v1/health.
	ProtocolVersion = 1

	// MaxBodyBytes is the maximum PUT /v1/hosts/{id}/state request body size (256 KiB).
	MaxBodyBytes = 256 * 1024

	// StaleAfter is how old a host's seenAt may be before it counts as stale.
	StaleAfter = 180 * time.Second

	// MaxFinishedJobs is the maximum number of finished jobs kept by TrimState.
	MaxFinishedJobs = 20

	// MaxHostIDLength is the maximum length of a controller host ID.
	MaxHostIDLength = 40
)

// HealthResponse is returned by GET /v1/health.
type HealthResponse struct {
	OK       bool `json:"ok"`
	Protocol int  `json:"protocol"`
}

// PutStateRequest is the body for PUT /v1/hosts/{id}/state.
type PutStateRequest struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	Seq    int64      `json:"seq"`
	SentAt time.Time  `json:"sentAt"`
	State  dash.State `json:"state"`
}

// HostRecord is one host entry in GET /v1/hosts.
type HostRecord struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	SeenAt time.Time  `json:"seenAt"`
	Stale  bool       `json:"stale"`
	State  dash.State `json:"state"`
}

// HostsResponse is returned by GET /v1/hosts.
type HostsResponse struct {
	Now   time.Time    `json:"now"`
	Hosts []HostRecord `json:"hosts"`
}

// UsageResponse is returned by GET /v1/usage.
type UsageResponse struct {
	Day      string `json:"day"`
	Requests int    `json:"requests"`
	Limit    int    `json:"limit"`
}

// ErrorResponse is the JSON error envelope returned by non-2xx controller responses.
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// APIError is returned by Client when the controller responds with a non-2xx status.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Code != "" && e.Message != "" {
		return fmt.Sprintf("controller HTTP %d (%s): %s", e.Status, e.Code, e.Message)
	}
	if e.Message != "" {
		return fmt.Sprintf("controller HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("controller HTTP %d", e.Status)
}

// ValidHostID reports whether id matches ^[a-z0-9-]{1,40}$.
func ValidHostID(id string) bool {
	if len(id) < 1 || len(id) > MaxHostIDLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// SanitizeHostID converts an arbitrary machine ID into a valid ^[a-z0-9-]{1,40}$ ID.
// If no ASCII alphanumeric characters remain, it falls back to "pc".
func SanitizeHostID(raw string) string {
	var b strings.Builder
	prevDash := true // trim leading dashes
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if len(out) > MaxHostIDLength {
		out = strings.TrimRight(out[:MaxHostIDLength], "-")
	}
	if out == "" {
		return "pc"
	}
	return out
}

// ValidControllerURL reports whether raw is an https:// URL or a loopback
// http:// URL (for local tests), with a hostname and no credentials, query, or fragment.
func ValidControllerURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	default:
		return false
	}
}

// TrimState returns a copy of st with Jobs trimmed to all active jobs
// (running, paused, queued) plus at most the last 20 finished jobs.
func TrimState(st dash.State) dash.State {
	out := st
	out.Machines = slices.Clone(st.Machines)
	out.Boxes = slices.Clone(st.Boxes)
	if st.Jobs == nil {
		return out
	}

	var active []dash.Job
	var finished []dash.Job
	for _, j := range st.Jobs {
		if j.State == dash.JobDone || j.State == dash.JobLost {
			finished = append(finished, j)
		} else {
			active = append(active, j)
		}
	}

	if len(finished) > MaxFinishedJobs {
		slices.SortStableFunc(finished, func(a, b dash.Job) int {
			ta := jobFinishedTime(a)
			tb := jobFinishedTime(b)
			if !ta.Equal(tb) {
				return tb.Compare(ta)
			}
			return b.StartedAt.Compare(a.StartedAt)
		})
		finished = finished[:MaxFinishedJobs]
	}

	jobs := make([]dash.Job, 0, len(active)+len(finished))
	jobs = append(jobs, active...)
	jobs = append(jobs, finished...)
	out.Jobs = jobs
	return out
}

func jobFinishedTime(j dash.Job) time.Time {
	if j.EndedAt != nil {
		return *j.EndedAt
	}
	return j.StartedAt
}
