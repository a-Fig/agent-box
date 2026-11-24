// Package mode applies agent-box's three sharing modes (My PC / Shared / Full)
// to the Incus instances in the "agents" project. It knows nothing about
// Windows: it talks to Incus through the Incus interface, so it is tested with
// fakes and builds everywhere.
package mode

import "strconv"

// Mode is how much of the PC agents get.
type Mode string

const (
	MyPC   Mode = "mypc"   // every instance paused; long-frozen ones stopped
	Shared Mode = "shared" // each instance may use 2 CPUs, normal priority
	Full   Mode = "full"   // each instance may use 4 CPUs, higher priority
)

// All lists the modes in menu order.
var All = []Mode{MyPC, Shared, Full}

// ProjectModeKey is the Incus project key `box` reads to learn the mode.
const ProjectModeKey = "user.agent-box.mode"

// Parse turns a stored string into a Mode.
func Parse(s string) (Mode, bool) {
	for _, m := range All {
		if string(m) == s {
			return m, true
		}
	}
	return "", false
}

// Label is the human name shown in the tray.
func (m Mode) Label() string {
	switch m {
	case MyPC:
		return "My PC"
	case Shared:
		return "Shared"
	case Full:
		return "Full"
	}
	return string(m)
}

// ProjectReasonKey is the project key the tray sets to why it is yielding
// (a game, say) and clears when it stops.
const ProjectReasonKey = "user.agent-box.reason"

// BoxCPUs is how many CPUs each box may use in this mode: 0 when boxes are
// paused (My PC) or the mode is unknown.
func (m Mode) BoxCPUs() int {
	switch m {
	case Shared, Full:
		n, _ := strconv.Atoi(m.limits().cpu)
		return n
	}
	return 0
}

// limits are the per-instance Incus settings a running mode (Shared, Full)
// enforces. limits.cpu=N is a real cap (N CPUs), unlike limits.cpu.allowance,
// which is only a weight between Linux processes and does nothing against
// Windows. Memory stays as the profile set it. Project limits are fixed at
// provisioning, because Incus refuses to lower them below what is in use.
type limits struct {
	cpu      string // limits.cpu
	priority string // limits.cpu.priority
}

// legacyKeys are instance keys earlier versions set; they are removed if found.
var legacyKeys = []string{"limits.cpu.allowance"}

func (m Mode) limits() limits {
	if m == Full {
		return limits{cpu: "4", priority: "10"}
	}
	return limits{cpu: "2", priority: "0"}
}
