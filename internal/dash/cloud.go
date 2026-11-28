package dash

import (
	"errors"
	"math"
	"time"
	"unicode/utf8"
)

// Cloud hosts (Modal, Cloudflare Workers) have no boxd of their own, so the page learns about them
// from box: after it probes a cloud host or follows a cloud job to its end, box posts what the
// runner said to the default host's boxd (POST /api/cloud/usage), as a best effort. boxd keeps the
// last report of each host in memory and shows it on the host's page, with how long ago it came.
//
// The page's pause switch (POST /api/cloud/pause) is boxd's, not the runner's: box reads
// State.CloudPaused when it probes the fleet and launches nothing on a cloud host while it is set.
// Jobs already running go on; `box cloud stop` stops them.

// CloudBudget is a cloud runner's month of spend, in the runner protocol's names
// (docs/runner-protocol.md). WarnAt is the share of CapUSD (0 to 1) past which spend warns; a
// CapUSD of 0 means no cap.
type CloudBudget struct {
	Month      string  `json:"month"`
	SpentUSD   float64 `json:"spentUSD"`
	RunningUSD float64 `json:"runningUSD"`
	CapUSD     float64 `json:"capUSD"`
	CreditUSD  float64 `json:"creditUSD"`
	WarnAt     float64 `json:"warnAt"`
}

// CloudHost is one cloud host as box last reported it. Kind is the runner ("modal",
// "cloudflare"), Mode how it runs a job ("function" or "sandbox"), Paused the runner's own pause
// (`box cloud pause`). ReportedAt and ReportedBy are boxd's: when the report came and from which
// device.
type CloudHost struct {
	Name          string       `json:"name"`
	Kind          string       `json:"kind"`
	Mode          string       `json:"mode"`
	Paused        bool         `json:"paused"`
	Running       int          `json:"running"`
	MaxConcurrent int          `json:"maxConcurrent"`
	Budget        *CloudBudget `json:"budget"`
	ReportedAt    time.Time    `json:"reportedAt"`
	ReportedBy    string       `json:"reportedBy"`
}

// ControllerUsage is the fleet controller's requests today against its daily limit, as its
// GET /v1/usage answers (docs/controller-protocol.md).
type ControllerUsage struct {
	Day      string `json:"day"`
	Requests int    `json:"requests"`
	Limit    int    `json:"limit"`
}

// CloudUsageBody is POST /api/cloud/usage: one cloud host's report, the controller's usage, or
// both. boxd sets the host's ReportedAt and ReportedBy itself.
type CloudUsageBody struct {
	Host       *CloudHost       `json:"host,omitempty"`
	Controller *ControllerUsage `json:"controller,omitempty"`
}

// CloudPauseBody is POST /api/cloud/pause. The reply is the CloudState.
type CloudPauseBody struct {
	Paused bool `json:"paused"`
}

// CloudState is EventCloud's data and POST /api/cloud/pause's reply: State's three cloud fields.
type CloudState struct {
	Cloud       []CloudHost      `json:"cloud"`
	CloudPaused bool             `json:"cloudPaused"`
	Controller  *ControllerUsage `json:"controller,omitempty"`
}

// MaxCloudHosts is how many cloud hosts boxd keeps reports of; a report of one more is refused.
const MaxCloudHosts = 32

// Validate checks a usage report before boxd keeps it.
func (b CloudUsageBody) Validate() error {
	if b.Host == nil && b.Controller == nil {
		return errors.New("the body is {host} or {controller}, or both")
	}
	if h := b.Host; h != nil {
		switch {
		case h.Name == "" || utf8.RuneCountInString(h.Name) > 64:
			return errors.New("host.name is 1 to 64 characters")
		case h.Kind == "" || utf8.RuneCountInString(h.Kind) > 32:
			return errors.New("host.kind is 1 to 32 characters")
		case h.Mode != "" && h.Mode != "function" && h.Mode != "sandbox":
			return errors.New("host.mode is function or sandbox")
		case h.Running < 0 || h.MaxConcurrent < 0 || h.Running > 1e6 || h.MaxConcurrent > 1e6:
			return errors.New("host.running and host.maxConcurrent are counts")
		}
		if g := h.Budget; g != nil {
			if utf8.RuneCountInString(g.Month) > 16 {
				return errors.New("host.budget.month is YYYY-MM")
			}
			for _, v := range []float64{g.SpentUSD, g.RunningUSD, g.CapUSD, g.CreditUSD} {
				if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1e9 {
					return errors.New("host.budget amounts are dollars, 0 or more")
				}
			}
			if math.IsNaN(g.WarnAt) || g.WarnAt < 0 || g.WarnAt > 1 {
				return errors.New("host.budget.warnAt is a share from 0 to 1")
			}
		}
	}
	if c := b.Controller; c != nil {
		if c.Requests < 0 || c.Limit < 0 || utf8.RuneCountInString(c.Day) > 16 {
			return errors.New("controller is {day, requests, limit}")
		}
	}
	return nil
}
